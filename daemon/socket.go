package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const defaultSocketWriteTimeout = 5 * time.Second

type ClientCommand struct {
	Cmd         string `json:"cmd"`
	Language    string `json:"language,omitempty"`
	Action      string `json:"action,omitempty"`
	TargetLabel string `json:"target_label,omitempty"`
}

type ServerEvent struct {
	Event            string            `json:"event"`
	State            string            `json:"state,omitempty"`
	Text             string            `json:"text,omitempty"`
	RawText          string            `json:"raw_text,omitempty"`
	Message          string            `json:"message,omitempty"`
	TranslateTargets []TranslateTarget `json:"translate_targets,omitempty"`
}

type APIKeyLoader func(credName, fallbackEnv string) (string, error)
type AudioFactory func(ctx context.Context) (AudioSource, error)

type PostProcessRunner interface {
	Generate(ctx context.Context, cfg Config, apiKey, action, rawText string) (string, error)
	GenerateWithLabel(ctx context.Context, cfg Config, apiKey, action, targetLabel, rawText string) (string, error)
}

type SocketServerDeps struct {
	ConfigManager *ConfigManager
	KeyLoader     APIKeyLoader
	AudioFactory  AudioFactory
	Transcriber   LiveTranscriber
	PostProcessor PostProcessRunner
	WriteTimeout  time.Duration
}

type SocketServer struct {
	socketPath string
	deps       SocketServerDeps
}

func NewSocketServer(socketPath string, deps SocketServerDeps) *SocketServer {
	if deps.KeyLoader == nil {
		deps.KeyLoader = LoadAPIKey
	}
	if deps.AudioFactory == nil {
		deps.AudioFactory = func(ctx context.Context) (AudioSource, error) {
			return NewPipeWireCapture(ctx)
		}
	}
	if deps.Transcriber == nil {
		deps.Transcriber = NewGeminiLiveClient()
	}
	if deps.PostProcessor == nil {
		deps.PostProcessor = NewPostProcessor()
	}
	if deps.WriteTimeout <= 0 {
		deps.WriteTimeout = defaultSocketWriteTimeout
	}
	return &SocketServer{
		socketPath: filepath.Clean(socketPath),
		deps:       deps,
	}
}

func (s *SocketServer) Listen() (net.Listener, error) {
	dir := filepath.Dir(s.socketPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create runtime socket dir %q: %w", dir, err)
	}

	if info, err := os.Lstat(s.socketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refusing to remove non-socket path %q", s.socketPath)
		}
		if err := os.Remove(s.socketPath); err != nil {
			return nil, fmt.Errorf("remove stale socket %q: %w", s.socketPath, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat socket %q: %w", s.socketPath, err)
	}

	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen unix socket %q: %w", s.socketPath, err)
	}
	if err := os.Chmod(s.socketPath, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(s.socketPath)
		return nil, fmt.Errorf("chmod unix socket %q: %w", s.socketPath, err)
	}
	return ln, nil
}

func (s *SocketServer) Serve(ctx context.Context) error {
	ln, err := s.Listen()
	if err != nil {
		return err
	}
	return s.ServeListener(ctx, ln)
}

func (s *SocketServer) ServeListener(ctx context.Context, ln net.Listener) error {
	defer func() {
		_ = ln.Close()
		if info, err := os.Lstat(s.socketPath); err == nil && info.Mode()&os.ModeSocket != 0 {
			_ = os.Remove(s.socketPath)
		}
	}()

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	var connWG sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				connWG.Wait()
				return nil
			}
			return fmt.Errorf("accept unix socket connection: %w", err)
		}
		connWG.Add(1)
		go func(c net.Conn) {
			defer connWG.Done()
			s.handleConnection(ctx, c)
		}(conn)
	}
}

func (s *SocketServer) handleConnection(parentCtx context.Context, conn net.Conn) {
	var closeConnOnce sync.Once
	closeConn := func() {
		closeConnOnce.Do(func() {
			_ = conn.Close()
		})
	}

	connCtx, cancelConn := context.WithCancel(parentCtx)
	closeWatcherDone := make(chan struct{})
	go func() {
		defer close(closeWatcherDone)
		<-connCtx.Done()
		closeConn()
	}()

	cs := &clientSession{
		server:     s,
		conn:       conn,
		baseCtx:    connCtx,
		cancelConn: cancelConn,
		prefetch:   NewPrefetchSession(),
	}
	defer closeConn()
	defer cs.shutdown()
	defer func() {
		cancelConn()
		<-closeWatcherDone
	}()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var cmd ClientCommand
		if err := json.Unmarshal([]byte(line), &cmd); err != nil {
			cs.sendUnchecked(ServerEvent{
				Event:   "error",
				Message: fmt.Sprintf("invalid JSON command: %v", err),
			})
			continue
		}
		cs.dispatch(cmd)
	}
	if err := scanner.Err(); err != nil && connCtx.Err() == nil {
		log.Printf("socket client read error: %v", err)
	}
}

type clientSession struct {
	server     *SocketServer
	conn       net.Conn
	baseCtx    context.Context
	cancelConn context.CancelFunc

	writeMu sync.Mutex

	mu         sync.Mutex
	sessionID  uint64
	recCancel  context.CancelFunc
	stopOnce   *sync.Once
	stopCh     chan struct{}
	postCancel context.CancelFunc
	prefetch   *PrefetchSession

	wg sync.WaitGroup
}

func (cs *clientSession) shutdown() {
	cs.mu.Lock()
	cs.sessionID++
	if cs.recCancel != nil {
		cs.recCancel()
		cs.recCancel = nil
	}
	if cs.postCancel != nil {
		cs.postCancel()
		cs.postCancel = nil
	}
	cs.stopOnce = nil
	cs.stopCh = nil
	cs.mu.Unlock()

	cs.prefetch.Reset()
	cs.wg.Wait()
}

func (cs *clientSession) isCurrent(sid uint64) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.sessionID == sid
}

func (cs *clientSession) sendIfSession(sid uint64, ev ServerEvent) {
	if !cs.isCurrent(sid) {
		return
	}
	cs.sendUnchecked(ev)
}

func (cs *clientSession) sendUnchecked(ev ServerEvent) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ev); err != nil {
		return
	}

	cs.writeMu.Lock()
	defer cs.writeMu.Unlock()
	var timeout time.Duration
	if cs.server != nil {
		timeout = cs.server.deps.WriteTimeout
	}
	if timeout <= 0 {
		timeout = defaultSocketWriteTimeout
	}
	_ = cs.conn.SetWriteDeadline(time.Now().Add(timeout))
	if _, err := cs.conn.Write(buf.Bytes()); err != nil {
		if cs.baseCtx == nil || cs.baseCtx.Err() == nil {
			log.Printf("socket client write error (closing connection): %v", err)
		}
		if cs.cancelConn != nil {
			cs.cancelConn()
		}
	}
}

func (cs *clientSession) dispatch(cmd ClientCommand) {
	switch cmd.Cmd {
	case "start_recording":
		cs.handleStartRecording(cmd.Language)
	case "stop_recording":
		cs.handleStopRecording()
	case "cancel":
		cs.handleCancel()
	case "post_process":
		cs.handlePostProcess(cmd.Action, cmd.TargetLabel)
	default:
		cs.sendUnchecked(ServerEvent{
			Event:   "error",
			Message: fmt.Sprintf("unknown command %q", cmd.Cmd),
		})
	}
}

func (cs *clientSession) handleStartRecording(reqLang string) {
	cs.mu.Lock()
	cs.sessionID++
	sid := cs.sessionID
	if cs.recCancel != nil {
		cs.recCancel()
		cs.recCancel = nil
	}
	if cs.postCancel != nil {
		cs.postCancel()
		cs.postCancel = nil
	}
	recCtx, recCancel := context.WithCancel(cs.baseCtx)
	stopCh := make(chan struct{})
	stopOnce := &sync.Once{}
	cs.recCancel = recCancel
	cs.stopCh = stopCh
	cs.stopOnce = stopOnce
	cs.mu.Unlock()

	cs.prefetch.Reset()

	cfg := cs.server.deps.ConfigManager.Get()
	apiKey, err := cs.server.deps.KeyLoader(DefaultCredentialName, DefaultFallbackEnv)
	if err != nil {
		recCancel()
		cs.sendIfSession(sid, ServerEvent{Event: "error", Message: err.Error()})
		cs.sendIfSession(sid, ServerEvent{Event: "state_changed", State: "idle"})
		return
	}

	audio, err := cs.server.deps.AudioFactory(recCtx)
	if err != nil {
		recCancel()
		cs.sendIfSession(sid, ServerEvent{Event: "error", Message: err.Error()})
		cs.sendIfSession(sid, ServerEvent{Event: "state_changed", State: "idle"})
		return
	}

	lang := strings.TrimSpace(reqLang)
	if lang == "" {
		lang = cfg.LiveLanguage()
	}

	cs.sendIfSession(sid, ServerEvent{Event: "state_changed", State: "recording"})

	opts := LiveSessionOptions{
		APIKey:           apiKey,
		Proxy:            cfg.Connection.Proxy,
		LiveWSURL:        cfg.Connection.LiveWSURL,
		Model:            cfg.LiveTranscribe.Model,
		Language:         lang,
		Mode:             cfg.LiveTranscribe.Mode,
		CustomVocabulary: cfg.LiveTranscribe.CustomVocabulary,
		VADSilenceMs:     cfg.LiveTranscribe.VADSilenceMs,
	}

	cs.wg.Add(1)
	go func() {
		defer cs.wg.Done()
		defer func() {
			_ = audio.Close()
			recCancel()
		}()

		rawText, err := cs.server.deps.Transcriber.Run(recCtx, audio, stopCh, opts, func(partial string) {
			if recCtx.Err() != nil {
				return
			}
			cs.sendIfSession(sid, ServerEvent{
				Event: "partial_text",
				Text:  partial,
			})
		})

		if recCtx.Err() != nil || !cs.isCurrent(sid) {
			return
		}
		if err != nil {
			cs.sendIfSession(sid, ServerEvent{Event: "error", Message: SanitizeError(err, apiKey)})
			cs.sendIfSession(sid, ServerEvent{Event: "state_changed", State: "idle"})
			return
		}

		rawText = strings.TrimSpace(rawText)
		latestCfg := cs.server.deps.ConfigManager.Get()
		if rawText != "" {
			cs.prefetch.StartPrefetch(cs.baseCtx, rawText, latestCfg.PostProcess.Prefetch, cs.makePostProcessFn(latestCfg, apiKey))
		}

		cs.sendIfSession(sid, ServerEvent{
			Event:            "transcribe_done",
			RawText:          rawText,
			TranslateTargets: latestCfg.TranslateTargets(),
		})
	}()
}

func (cs *clientSession) handleStopRecording() {
	cs.mu.Lock()
	stopOnce := cs.stopOnce
	stopCh := cs.stopCh
	cs.mu.Unlock()

	if stopOnce != nil && stopCh != nil {
		stopOnce.Do(func() {
			close(stopCh)
		})
	}
}

func (cs *clientSession) handleCancel() {
	cs.mu.Lock()
	cs.sessionID++
	sid := cs.sessionID
	if cs.recCancel != nil {
		cs.recCancel()
		cs.recCancel = nil
	}
	if cs.postCancel != nil {
		cs.postCancel()
		cs.postCancel = nil
	}
	cs.stopOnce = nil
	cs.stopCh = nil
	cs.mu.Unlock()

	cs.prefetch.Reset()
	cs.sendIfSession(sid, ServerEvent{
		Event: "state_changed",
		State: "idle",
	})
}

func (cs *clientSession) handlePostProcess(action, targetLabel string) {
	action = strings.TrimSpace(action)
	if action == "" {
		cs.sendUnchecked(ServerEvent{
			Event:   "error",
			Message: "post_process action is required",
		})
		return
	}

	if cs.prefetch.RawText() == "" {
		cs.sendUnchecked(ServerEvent{
			Event:   "error",
			Message: "no transcribed text available for post_process",
		})
		return
	}

	cfg := cs.server.deps.ConfigManager.Get()
	if prompt, ok := cfg.PromptForActionWithLabel(action, targetLabel); !ok || strings.TrimSpace(prompt) == "" {
		cs.sendUnchecked(ServerEvent{
			Event:   "error",
			Message: fmt.Sprintf("unknown post_process action %q", action),
		})
		return
	}

	apiKey, err := cs.server.deps.KeyLoader(DefaultCredentialName, DefaultFallbackEnv)
	if err != nil {
		cs.sendUnchecked(ServerEvent{
			Event:   "error",
			Message: err.Error(),
		})
		return
	}

	cs.mu.Lock()
	sid := cs.sessionID
	if cs.postCancel != nil {
		cs.postCancel()
	}
	postCtx, postCancel := context.WithCancel(cs.baseCtx)
	cs.postCancel = postCancel
	cs.mu.Unlock()

	cs.sendIfSession(sid, ServerEvent{
		Event: "state_changed",
		State: "processing",
	})

	fn := cs.makePostProcessFn(cfg, apiKey, targetLabel)
	cs.wg.Add(1)
	go func() {
		defer cs.wg.Done()
		defer postCancel()

		res, err := cs.prefetch.Resolve(postCtx, action, targetLabel, fn)
		if postCtx.Err() != nil || !cs.isCurrent(sid) {
			return
		}
		if err != nil {
			cs.sendIfSession(sid, ServerEvent{
				Event:   "error",
				Message: SanitizeError(err, apiKey),
			})
			return
		}
		cs.sendIfSession(sid, ServerEvent{
			Event: "final_result",
			Text:  res,
		})
	}()
}

func (cs *clientSession) makePostProcessFn(cfg Config, apiKey string, targetLabels ...string) PostProcessFunc {
	targetLabel := ""
	if len(targetLabels) > 0 {
		targetLabel = targetLabels[0]
	}
	return func(ctx context.Context, action, rawText string) (string, error) {
		if targetLabel != "" {
			return cs.server.deps.PostProcessor.GenerateWithLabel(ctx, cfg, apiKey, action, targetLabel, rawText)
		}
		return cs.server.deps.PostProcessor.Generate(ctx, cfg, apiKey, action, rawText)
	}
}
