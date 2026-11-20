package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeTranscriber struct {
	mode string // "manual", "vad", "slow"
}

func (f *fakeTranscriber) Run(ctx context.Context, audio AudioSource, stopCh <-chan struct{}, opts LiveSessionOptions, onPartial func(string)) (string, error) {
	switch f.mode {
	case "vad":
		onPartial("自动断句测试")
		return "自动断句测试", nil
	case "slow":
		onPartial("正在录音")
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-stopCh:
			return "完成", nil
		}
	default:
		onPartial("今天下午三点")
		select {
		case <-stopCh:
			onPartial("今天下午三点我们开个会")
			return "今天下午三点我们开个会", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

type fakePostProcessor struct {
	mu        sync.Mutex
	calls     map[string]int
	lastLabel string
	delay     time.Duration
}

func newFakePostProcessor() *fakePostProcessor {
	return &fakePostProcessor{calls: make(map[string]int)}
}

func (f *fakePostProcessor) Generate(ctx context.Context, cfg Config, apiKey, action, rawText string) (string, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f.mu.Lock()
	f.calls[action]++
	f.mu.Unlock()
	return fmt.Sprintf("[%s] %s", action, rawText), nil
}

func (f *fakePostProcessor) GenerateWithLabel(ctx context.Context, cfg Config, apiKey, action, targetLabel, rawText string) (string, error) {
	f.mu.Lock()
	f.lastLabel = targetLabel
	f.mu.Unlock()
	return f.Generate(ctx, cfg, apiKey, action, rawText)
}

func startTestSocketServer(t *testing.T, tr LiveTranscriber, pp PostProcessRunner) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	// Use /tmp short path if t.TempDir() is long for AF_UNIX 108-byte limit, though Linux allows ~107 chars.
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")
	cfgPath := filepath.Join(dir, "config.toml")

	cfgMgr, err := NewConfigManager(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	var audioClosed atomic.Bool
	srv := NewSocketServer(sockPath, SocketServerDeps{
		ConfigManager: cfgMgr,
		KeyLoader: func(credName, fallbackEnv string) (string, error) {
			return "dummy-test-key", nil
		},
		AudioFactory: func(ctx context.Context) (AudioSource, error) {
			return &testAudioTracker{closed: &audioClosed}, nil
		},
		Transcriber:   tr,
		PostProcessor: pp,
	})

	ln, err := srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %o, want 0600", info.Mode().Perm())
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ServeListener(ctx, ln)
	}()
	return sockPath, cancel, errCh
}

type testAudioTracker struct {
	closed *atomic.Bool
}

func (a *testAudioTracker) ReadChunk() ([]byte, error) {
	return make([]byte, ChunkBytes), nil
}

func (a *testAudioTracker) Close() error {
	a.closed.Store(true)
	return nil
}

func readEvent(t *testing.T, scanner *bufio.Scanner) ServerEvent {
	t.Helper()
	if !scanner.Scan() {
		t.Fatalf("unexpected scanner EOF (err=%v)", scanner.Err())
	}
	var ev ServerEvent
	if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
		t.Fatalf("unmarshal event %q: %v", scanner.Text(), err)
	}
	return ev
}

func sendCmd(t *testing.T, conn net.Conn, cmd ClientCommand) {
	t.Helper()
	b, err := json.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	if _, err := conn.Write(b); err != nil {
		t.Fatalf("write cmd: %v", err)
	}
}

func TestSocketServer_FullFlowAndPrefetch(t *testing.T) {
	pp := newFakePostProcessor()
	sockPath, cancel, errCh := startTestSocketServer(t, &fakeTranscriber{mode: "manual"}, pp)
	defer func() {
		cancel()
		if err := <-errCh; err != nil {
			t.Fatalf("server exit err: %v", err)
		}
	}()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	scanner := bufio.NewScanner(conn)

	sendCmd(t, conn, ClientCommand{Cmd: "start_recording", Language: "zh-CN"})
	ev := readEvent(t, scanner)
	if ev.Event != "state_changed" || ev.State != "recording" {
		t.Fatalf("expected recording state, got %+v", ev)
	}
	ev = readEvent(t, scanner)
	if ev.Event != "partial_text" || ev.Text != "今天下午三点" {
		t.Fatalf("expected partial_text, got %+v", ev)
	}

	sendCmd(t, conn, ClientCommand{Cmd: "stop_recording"})
	ev = readEvent(t, scanner)
	if ev.Event != "partial_text" || ev.Text != "今天下午三点我们开个会" {
		t.Fatalf("expected second partial_text, got %+v", ev)
	}
	ev = readEvent(t, scanner)
	if ev.Event != "transcribe_done" || ev.RawText != "今天下午三点我们开个会" {
		t.Fatalf("expected transcribe_done, got %+v", ev)
	}

	// Wait briefly for prefetch to finish, then verify refine_formal hits prefetch cache.
	time.Sleep(50 * time.Millisecond)
	sendCmd(t, conn, ClientCommand{Cmd: "post_process", Action: "refine_formal"})
	ev = readEvent(t, scanner)
	if ev.Event != "state_changed" || ev.State != "processing" {
		t.Fatalf("expected processing state, got %+v", ev)
	}
	ev = readEvent(t, scanner)
	if ev.Event != "final_result" || ev.Text != "[refine_formal] 今天下午三点我们开个会" {
		t.Fatalf("expected final_result, got %+v", ev)
	}

	pp.mu.Lock()
	formalCalls := pp.calls["refine_formal"]
	typosCalls := pp.calls["refine_fix_typos"]
	pp.mu.Unlock()
	if formalCalls != 1 || typosCalls != 1 {
		t.Fatalf("expected 1 prefetch call each, got formal=%d typos=%d", formalCalls, typosCalls)
	}
}

func TestSocketServer_VADAndCancel(t *testing.T) {
	pp := newFakePostProcessor()
	sockPath, cancel, errCh := startTestSocketServer(t, &fakeTranscriber{mode: "vad"}, pp)
	defer func() {
		cancel()
		if err := <-errCh; err != nil {
			t.Fatalf("server exit err: %v", err)
		}
	}()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	scanner := bufio.NewScanner(conn)

	// VAD auto-completes without stop_recording.
	sendCmd(t, conn, ClientCommand{Cmd: "start_recording"})
	if ev := readEvent(t, scanner); ev.Event != "state_changed" || ev.State != "recording" {
		t.Fatalf("got %+v", ev)
	}
	if ev := readEvent(t, scanner); ev.Event != "partial_text" {
		t.Fatalf("got %+v", ev)
	}
	if ev := readEvent(t, scanner); ev.Event != "transcribe_done" || ev.RawText != "自动断句测试" {
		t.Fatalf("got %+v", ev)
	}

	// Cancel resets state to idle and clears raw_text.
	sendCmd(t, conn, ClientCommand{Cmd: "cancel"})
	if ev := readEvent(t, scanner); ev.Event != "state_changed" || ev.State != "idle" {
		t.Fatalf("expected idle after cancel, got %+v", ev)
	}
	sendCmd(t, conn, ClientCommand{Cmd: "post_process", Action: "refine_formal"})
	if ev := readEvent(t, scanner); ev.Event != "error" {
		t.Fatalf("expected error after cancel cleared session, got %+v", ev)
	}
}

func TestSocketServer_RefusesNonSocketPath(t *testing.T) {
	dir := t.TempDir()
	regularFile := filepath.Join(dir, "not-a-socket")
	if err := os.WriteFile(regularFile, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfgMgr, err := NewConfigManager(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	srv := NewSocketServer(regularFile, SocketServerDeps{ConfigManager: cfgMgr})
	if _, err := srv.Listen(); err == nil {
		t.Fatal("expected Listen to refuse overwriting regular file")
	}
	data, err := os.ReadFile(regularFile)
	if err != nil || string(data) != "keep me" {
		t.Fatalf("regular file was modified/removed: %v %q", err, string(data))
	}
}

func TestSocketServer_CancelMidRecordingAndPostProcess(t *testing.T) {
	pp := newFakePostProcessor()
	pp.delay = 250 * time.Millisecond

	sockPath, cancel, errCh := startTestSocketServer(t, &fakeTranscriber{mode: "slow"}, pp)
	defer func() {
		cancel()
		if err := <-errCh; err != nil {
			t.Fatalf("server exit err: %v", err)
		}
	}()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	scanner := bufio.NewScanner(conn)

	// 1. Cancel mid-recording: must receive state_changed=idle and no transcribe_done.
	sendCmd(t, conn, ClientCommand{Cmd: "start_recording"})
	if ev := readEvent(t, scanner); ev.Event != "state_changed" || ev.State != "recording" {
		t.Fatalf("got %+v", ev)
	}
	if ev := readEvent(t, scanner); ev.Event != "partial_text" {
		t.Fatalf("got %+v", ev)
	}
	sendCmd(t, conn, ClientCommand{Cmd: "cancel"})
	if ev := readEvent(t, scanner); ev.Event != "state_changed" || ev.State != "idle" {
		t.Fatalf("expected idle, got %+v", ev)
	}

	// 2. Complete recording, trigger slow post_process, then cancel mid-post-process.
	sendCmd(t, conn, ClientCommand{Cmd: "start_recording"})
	if ev := readEvent(t, scanner); ev.Event != "state_changed" || ev.State != "recording" {
		t.Fatalf("got %+v", ev)
	}
	if ev := readEvent(t, scanner); ev.Event != "partial_text" {
		t.Fatalf("got %+v", ev)
	}
	sendCmd(t, conn, ClientCommand{Cmd: "stop_recording"})
	if ev := readEvent(t, scanner); ev.Event != "transcribe_done" || ev.RawText != "完成" {
		t.Fatalf("got %+v", ev)
	}

	sendCmd(t, conn, ClientCommand{Cmd: "post_process", Action: "translate_en"})
	if ev := readEvent(t, scanner); ev.Event != "state_changed" || ev.State != "processing" {
		t.Fatalf("got %+v", ev)
	}
	sendCmd(t, conn, ClientCommand{Cmd: "cancel"})
	if ev := readEvent(t, scanner); ev.Event != "state_changed" || ev.State != "idle" {
		t.Fatalf("expected idle after cancel mid-post-process, got %+v", ev)
	}

	// Verify no stale final_result arrives after delay.
	_ = conn.SetReadDeadline(time.Now().Add(350 * time.Millisecond))
	if scanner.Scan() {
		t.Fatalf("unexpected stale event after cancel: %s", scanner.Text())
	}
}

func TestSocketServer_DynamicTranslateTargetLabel(t *testing.T) {
	sockPath, cancel, _ := startTestSocketServer(t, &fakeTranscriber{mode: "ok"}, newFakePostProcessor())
	defer cancel()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	scanner := bufio.NewScanner(conn)

	sendCmd(t, conn, ClientCommand{Cmd: "start_recording"})
	if ev := readEvent(t, scanner); ev.Event != "state_changed" || ev.State != "recording" {
		t.Fatalf("got %+v", ev)
	}
	if ev := readEvent(t, scanner); ev.Event != "partial_text" {
		t.Fatalf("got %+v", ev)
	}
	sendCmd(t, conn, ClientCommand{Cmd: "stop_recording"})
	if ev := readEvent(t, scanner); ev.Event != "partial_text" {
		t.Fatalf("got %+v", ev)
	}
	if ev := readEvent(t, scanner); ev.Event != "transcribe_done" {
		t.Fatalf("got %+v", ev)
	}

	// Frontend can request a language added in fcitx5-configtool without modifying config.toml or restarting daemon.
	sendCmd(t, conn, ClientCommand{Cmd: "post_process", Action: "translate_ja", TargetLabel: "Japanese"})
	if ev := readEvent(t, scanner); ev.Event != "state_changed" || ev.State != "processing" {
		t.Fatalf("got %+v", ev)
	}
	if ev := readEvent(t, scanner); ev.Event != "final_result" || ev.Text != "[translate_ja] 今天下午三点我们开个会" {
		t.Fatalf("got %+v", ev)
	}
}

func TestSocketServer_ClientDisconnectCancelsCapture(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "disc.sock")
	cfgMgr, err := NewConfigManager(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}

	var audioClosed atomic.Bool
	audioDone := make(chan struct{})
	srv := NewSocketServer(sockPath, SocketServerDeps{
		ConfigManager: cfgMgr,
		KeyLoader: func(credName, fallbackEnv string) (string, error) {
			return "dummy-key", nil
		},
		AudioFactory: func(ctx context.Context) (AudioSource, error) {
			return &blockingAudioSource{closed: &audioClosed, done: audioDone}, nil
		},
		Transcriber: &fakeTranscriber{mode: "slow"},
	})

	ln, err := srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = srv.ServeListener(ctx, ln)
	}()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(conn)
	sendCmd(t, conn, ClientCommand{Cmd: "start_recording"})
	if ev := readEvent(t, scanner); ev.Event != "state_changed" || ev.State != "recording" {
		t.Fatalf("got %+v", ev)
	}

	// Abruptly disconnect client.
	_ = conn.Close()

	select {
	case <-audioDone:
	case <-time.After(2 * time.Second):
		t.Fatal("expected audio capture to close upon client disconnect")
	}
	if !audioClosed.Load() {
		t.Fatal("audioClosed was false")
	}
}

type blockingAudioSource struct {
	closed *atomic.Bool
	done   chan struct{}
	once   sync.Once
}

func (b *blockingAudioSource) ReadChunk() ([]byte, error) {
	<-b.done
	return nil, fmt.Errorf("closed")
}

func (b *blockingAudioSource) Close() error {
	b.once.Do(func() {
		b.closed.Store(true)
		close(b.done)
	})
	return nil
}

func TestSocketServer_ShutdownClosesPersistentClient(t *testing.T) {
	t.Run("idle_client", func(t *testing.T) {
		sockPath, cancel, errCh := startTestSocketServer(t, &fakeTranscriber{mode: "slow"}, newFakePostProcessor())
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		scanner := bufio.NewScanner(conn)

		// Round-trip one command so the server has accepted and is parked in scanner.Scan().
		sendCmd(t, conn, ClientCommand{Cmd: "cancel"})
		if ev := readEvent(t, scanner); ev.Event != "state_changed" || ev.State != "idle" {
			cancel()
			t.Fatalf("expected idle state, got %+v", ev)
		}

		// Cancel server context WITHOUT closing client conn.
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("server exit err: %v", err)
			}
		case <-time.After(1 * time.Second):
			t.Fatal("server shutdown timed out with persistent idle client connected")
		}
	})

	t.Run("mid_recording", func(t *testing.T) {
		sockPath, cancel, errCh := startTestSocketServer(t, &fakeTranscriber{mode: "slow"}, newFakePostProcessor())
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		scanner := bufio.NewScanner(conn)

		sendCmd(t, conn, ClientCommand{Cmd: "start_recording"})
		if ev := readEvent(t, scanner); ev.Event != "state_changed" || ev.State != "recording" {
			cancel()
			t.Fatalf("expected recording state, got %+v", ev)
		}
		if ev := readEvent(t, scanner); ev.Event != "partial_text" {
			cancel()
			t.Fatalf("expected partial_text, got %+v", ev)
		}

		// Cancel server context mid-recording WITHOUT closing client conn.
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("server exit err: %v", err)
			}
		case <-time.After(1 * time.Second):
			t.Fatal("server shutdown timed out mid-recording with persistent client connected")
		}
	})
}

func TestSocketServer_StalledClientWriteDeadline(t *testing.T) {
	t.Run("sendUnchecked_net_pipe", func(t *testing.T) {
		serverConn, clientConn := net.Pipe()
		defer func() { _ = serverConn.Close() }()
		defer func() { _ = clientConn.Close() }()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var cancelled atomic.Bool

		cs := &clientSession{
			server: &SocketServer{
				deps: SocketServerDeps{
					WriteTimeout: 50 * time.Millisecond,
				},
			},
			conn:    serverConn,
			baseCtx: ctx,
			cancelConn: func() {
				cancelled.Store(true)
				cancel()
				_ = serverConn.Close()
			},
		}

		start := time.Now()
		cs.sendUnchecked(ServerEvent{Event: "state_changed", State: "recording"})
		elapsed := time.Since(start)

		if elapsed >= 500*time.Millisecond {
			t.Fatalf("sendUnchecked blocked too long on unread pipe: %v", elapsed)
		}
		if !cancelled.Load() {
			t.Fatal("expected cancelConn to be invoked on write timeout")
		}
	})

	t.Run("handleConnection_stalled_peer", func(t *testing.T) {
		dir := t.TempDir()
		cfgMgr, err := NewConfigManager(filepath.Join(dir, "config.toml"))
		if err != nil {
			t.Fatal(err)
		}

		var audioClosed atomic.Bool
		srv := NewSocketServer(filepath.Join(dir, "unused.sock"), SocketServerDeps{
			ConfigManager: cfgMgr,
			KeyLoader: func(credName, fallbackEnv string) (string, error) {
				return "dummy-key", nil
			},
			AudioFactory: func(ctx context.Context) (AudioSource, error) {
				return &testAudioTracker{closed: &audioClosed}, nil
			},
			Transcriber:  &fakeTranscriber{mode: "slow"},
			WriteTimeout: 50 * time.Millisecond,
		})

		serverConn, clientConn := net.Pipe()
		defer func() { _ = clientConn.Close() }()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		handleDone := make(chan struct{})
		go func() {
			defer close(handleDone)
			srv.handleConnection(ctx, serverConn)
		}()

		_ = clientConn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
		sendCmd(t, clientConn, ClientCommand{Cmd: "start_recording"})

		// Never read from clientConn so serverConn.Write stalls and hits WriteTimeout.
		select {
		case <-handleDone:
		case <-time.After(500 * time.Millisecond):
			t.Fatal("handleConnection did not close stalled client within 500ms")
		}
		if !audioClosed.Load() {
			t.Fatal("expected audio source to close when stalled client connection closed")
		}
	})
}

func TestServerEvent_TranslateTargetsJSON(t *testing.T) {
	ev := ServerEvent{
		Event:   "raw_transcript",
		RawText: "hello",
		TranslateTargets: []TranslateTarget{
			{Action: "translate_ja", Label: "Japanese"},
			{Action: "translate_en", Label: "English"},
		},
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal ServerEvent: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal ServerEvent to map: %v", err)
	}
	rawTargets, ok := decoded["translate_targets"].([]any)
	if !ok || len(rawTargets) != 2 {
		t.Fatalf("expected translate_targets array of len 2 in %s", string(b))
	}
	first, ok := rawTargets[0].(map[string]any)
	if !ok || first["action"] != "translate_ja" || first["label"] != "Japanese" {
		t.Fatalf("unexpected first translate_targets entry: %#v", rawTargets[0])
	}

	// Empty TranslateTargets should be omitted.
	emptyEv := ServerEvent{Event: "state_changed", State: "idle"}
	emptyBytes, err := json.Marshal(emptyEv)
	if err != nil {
		t.Fatalf("marshal empty ServerEvent: %v", err)
	}
	var emptyDecoded map[string]any
	if err := json.Unmarshal(emptyBytes, &emptyDecoded); err != nil {
		t.Fatalf("unmarshal empty ServerEvent: %v", err)
	}
	if _, exists := emptyDecoded["translate_targets"]; exists {
		t.Fatalf("expected translate_targets omitted when empty, got %s", string(emptyBytes))
	}
}
