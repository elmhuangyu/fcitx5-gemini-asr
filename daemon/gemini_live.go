package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"google.golang.org/genai"
)

const (
	// DefaultLiveBaseURL / DefaultLiveAPIVersion are what the genai SDK expands
	// into wss://<host>/ws/google.ai.generativelanguage.<version>.GenerativeService.BidiGenerateContent.
	DefaultLiveBaseURL       = "https://generativelanguage.googleapis.com"
	DefaultLiveAPIVersion    = "v1beta"
	DefaultSystemInstruction = "Verbatim speech-to-text transcriber. Output ONLY exact transcribed text with proper punctuation. Never answer questions or converse."
)

type LiveSessionOptions struct {
	APIKey           string
	Proxy            string
	LiveWSURL        string
	Model            string
	Language         string
	Mode             string
	CustomVocabulary []string
	// VADSilenceMs drives server-side automatic activity detection
	// (silenceDurationMs). 0 disables it and capture only ends on manual stop.
	VADSilenceMs int
}

type LiveTranscriber interface {
	Run(ctx context.Context, audio AudioSource, stopCh <-chan struct{}, opts LiveSessionOptions, onPartial func(string)) (string, error)
}

type GeminiLiveClient struct {
	SetupTimeout        time.Duration
	FinalizeTimeout     time.Duration
	FinalizeGracePeriod time.Duration
}

func NewGeminiLiveClient() *GeminiLiveClient {
	return &GeminiLiveClient{
		SetupTimeout:        10 * time.Second,
		FinalizeTimeout:     5 * time.Second,
		FinalizeGracePeriod: 300 * time.Millisecond,
	}
}

var languageCodeAliases = map[string]string{
	"zh":          "cmn-Hans-CN",
	"zh-cn":       "cmn-Hans-CN",
	"zh-hans":     "cmn-Hans-CN",
	"zh-hans-cn":  "cmn-Hans-CN",
	"cmn":         "cmn-Hans-CN",
	"cmn-cn":      "cmn-Hans-CN",
	"cmn-hans":    "cmn-Hans-CN",
	"cmn-hans-cn": "cmn-Hans-CN",
	"zh-hk":       "yue-Hant-HK",
	"yue":         "yue-Hant-HK",
	"yue-hk":      "yue-Hant-HK",
	"yue-hant":    "yue-Hant-HK",
	"yue-hant-hk": "yue-Hant-HK",
}

// NormalizeLanguageCodes converts user language settings (including "auto" and
// common aliases such as "zh-CN") into a non-nil slice of BCP-47 hints for
// AudioTranscriptionConfig.languageCodes.
func NormalizeLanguageCodes(raw string) []string {
	out := []string{}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return out
	}
	seen := make(map[string]struct{})
	for _, part := range strings.Split(trimmed, ",") {
		tok := strings.ReplaceAll(strings.TrimSpace(part), "_", "-")
		if tok == "" {
			continue
		}
		if strings.EqualFold(tok, "auto") {
			return []string{}
		}
		if canonical, ok := languageCodeAliases[strings.ToLower(tok)]; ok {
			tok = canonical
		}
		if _, dup := seen[tok]; dup {
			continue
		}
		seen[tok] = struct{}{}
		out = append(out, tok)
	}
	return out
}

func isTranscribeLiveModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "transcribe")
}

func isASCIIAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func isASCIIPunct(r rune) bool {
	switch r {
	case '.', ',', '!', '?', ';', ':':
		return true
	default:
		return false
	}
}

func joinTranscriptSegments(base, next string) string {
	if base == "" {
		return next
	}
	if next == "" {
		return base
	}
	last, _ := utf8.DecodeLastRuneInString(base)
	first, _ := utf8.DecodeRuneInString(next)
	if unicode.IsSpace(last) || unicode.IsSpace(first) {
		return base + next
	}
	if (isASCIIAlnum(last) || isASCIIPunct(last)) && isASCIIAlnum(first) {
		return base + " " + next
	}
	return base + next
}

func notifySignal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

var liveWSPathRe = regexp.MustCompile(`/ws/google\.ai\.generativelanguage\.([A-Za-z0-9]+)\.GenerativeService\.BidiGenerateContent/?$`)

// resolveLiveEndpoint turns connection.live_ws_url into the base URL and API
// version the genai SDK needs. The SDK always appends
// "/ws/google.ai.generativelanguage.<version>.GenerativeService.BidiGenerateContent"
// itself, so a full endpoint URL has that suffix stripped and any other value
// is treated as a base URL (scheme://host[/prefix]). Query strings are dropped:
// the API key is sent in the x-goog-api-key header, never in the URL.
func resolveLiveEndpoint(raw string) (baseURL, apiVersion string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultLiveBaseURL, DefaultLiveAPIVersion, nil
	}
	u, perr := url.Parse(raw)
	if perr != nil || u.Host == "" {
		return "", "", fmt.Errorf("invalid Live WebSocket URL %q", RedactURL(raw))
	}
	apiVersion = DefaultLiveAPIVersion
	p := u.Path
	if m := liveWSPathRe.FindStringSubmatch(p); m != nil {
		apiVersion = m[1]
		p = liveWSPathRe.ReplaceAllString(p, "")
	}
	p = strings.TrimRight(p, "/")
	return u.Scheme + "://" + u.Host + p, apiVersion, nil
}

// liveDialMu serialises genai Live connects: the SDK dials with the global
// websocket.DefaultDialer and offers no per-client dialer hook, so we swap in
// our proxy-aware dialer for the duration of the handshake.
var liveDialMu sync.Mutex

// rawConnTracker remembers the TCP connection opened by the dialer so a hung
// connect/setup (which the SDK performs without context or deadlines) can be
// aborted by closing it.
type rawConnTracker struct {
	mu     sync.Mutex
	conn   net.Conn
	closed bool
}

func (t *rawConnTracker) set(c net.Conn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		_ = c.Close()
		return
	}
	t.conn = c
}

func (t *rawConnTracker) abort() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	if t.conn != nil {
		_ = t.conn.Close()
	}
}

type liveConnectResult struct {
	session *genai.Session
	err     error
}

func (c *GeminiLiveClient) connect(ctx context.Context, opts LiveSessionOptions, model string, cfg *genai.LiveConnectConfig) (*genai.Session, error) {
	baseURL, apiVersion, err := resolveLiveEndpoint(opts.LiveWSURL)
	if err != nil {
		return nil, err
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:      opts.APIKey,
		Backend:     genai.BackendGeminiAPI,
		HTTPOptions: genai.HTTPOptions{BaseURL: baseURL, APIVersion: apiVersion},
	})
	if err != nil {
		return nil, fmt.Errorf("create Gemini Live client: %s", SanitizeError(err, opts.APIKey))
	}

	setupTimeout := c.SetupTimeout
	if setupTimeout <= 0 {
		setupTimeout = 10 * time.Second
	}
	const handshakeTimeout = 10 * time.Second

	tracker := &rawConnTracker{}
	resCh := make(chan liveConnectResult, 1)
	go func() {
		liveDialMu.Lock()
		defer liveDialMu.Unlock()
		dialer, derr := BuildWSDialer(opts.Proxy)
		if derr != nil {
			resCh <- liveConnectResult{err: derr}
			return
		}
		dialer.HandshakeTimeout = handshakeTimeout
		base := dialer.NetDialContext
		if base == nil {
			base = (&net.Dialer{Timeout: handshakeTimeout}).DialContext
		}
		dialer.NetDialContext = func(dctx context.Context, network, addr string) (net.Conn, error) {
			conn, cerr := base(dctx, network, addr)
			if cerr == nil {
				tracker.set(conn)
			}
			return conn, cerr
		}
		orig := websocket.DefaultDialer
		websocket.DefaultDialer = dialer
		defer func() { websocket.DefaultDialer = orig }()

		s, cerr := client.Live.Connect(ctx, model, cfg)
		resCh <- liveConnectResult{session: s, err: cerr}
	}()

	timer := time.NewTimer(handshakeTimeout + setupTimeout)
	defer timer.Stop()
	abandon := func() {
		tracker.abort()
		go func() {
			if r := <-resCh; r.session != nil {
				_ = r.session.Close()
			}
		}()
	}
	select {
	case r := <-resCh:
		if r.err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("connect Gemini Live: %s", SanitizeError(r.err, opts.APIKey))
		}
		return r.session, nil
	case <-ctx.Done():
		abandon()
		return nil, ctx.Err()
	case <-timer.C:
		abandon()
		return nil, errors.New("connect Gemini Live: timed out awaiting setupComplete")
	}
}

// isEndOfSpeechSignal reports whether a server message marks the end of a
// detected user utterance.
func isEndOfSpeechSignal(msg *genai.LiveServerMessage) bool {
	if msg.VoiceActivity != nil && msg.VoiceActivity.VoiceActivityType == genai.VoiceActivityTypeActivityEnd {
		return true
	}
	return msg.VoiceActivityDetectionSignal != nil && msg.VoiceActivityDetectionSignal.VADSignalType == genai.VADSignalTypeEos
}

func isStartOfSpeechSignal(msg *genai.LiveServerMessage) bool {
	if msg.VoiceActivity != nil && msg.VoiceActivity.VoiceActivityType == genai.VoiceActivityTypeActivityStart {
		return true
	}
	return msg.VoiceActivityDetectionSignal != nil && msg.VoiceActivityDetectionSignal.VADSignalType == genai.VADSignalTypeSos
}

func (c *GeminiLiveClient) Run(ctx context.Context, audio AudioSource, stopCh <-chan struct{}, opts LiveSessionOptions, onPartial func(string)) (string, error) {
	if audio == nil {
		return "", errors.New("audio source is nil")
	}

	mode, err := NormalizeLiveMode(opts.Mode)
	if err != nil {
		return "", err
	}
	vocab := pruneCustomVocabulary(opts.CustomVocabulary)
	if len(vocab) > MaxCustomVocabulary {
		return "", fmt.Errorf("custom_vocabulary has %d entries (max %d)", len(vocab), MaxCustomVocabulary)
	}
	if _, _, err := resolveLiveEndpoint(opts.LiveWSURL); err != nil {
		return "", errors.New(SanitizeError(err, opts.APIKey))
	}

	bufferedAudio := NewBufferedAudioSource(audio, DefaultPreRollMaxChunks)
	defer func() { _ = bufferedAudio.Close() }()

	sessionDone := make(chan struct{})
	var closeSessionOnce sync.Once
	endSession := func() {
		closeSessionOnce.Do(func() {
			close(sessionDone)
		})
	}
	stopWatcherDone := make(chan struct{})
	go func() {
		defer close(stopWatcherDone)
		select {
		case <-ctx.Done():
			_ = bufferedAudio.Close()
		case <-stopCh:
			bufferedAudio.StopCapture()
		case <-sessionDone:
		}
	}()
	defer func() {
		endSession()
		<-stopWatcherDone
	}()

	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = DefaultLiveModel
	}
	if !strings.HasPrefix(model, "models/") {
		model = "models/" + model
	}

	isTranscribe := isTranscribeLiveModel(model)
	langCodes := NormalizeLanguageCodes(opts.Language)

	// Server-side VAD: vad_silence_ms > 0 enables automatic activity detection
	// with that trailing-silence duration; 0 disables it (manual stop only, the
	// client then brackets the audio with activityStart/activityEnd).
	serverVAD := opts.VADSilenceMs > 0
	aad := &genai.AutomaticActivityDetection{Disabled: !serverVAD}
	if serverVAD {
		aad.SilenceDurationMs = genai.Ptr(int32(opts.VADSilenceMs))
		aad.StartOfSpeechSensitivity = genai.StartSensitivityHigh
		aad.EndOfSpeechSensitivity = genai.EndSensitivityHigh
	}

	liveCfg := &genai.LiveConnectConfig{
		ResponseModalities: []genai.Modality{genai.ModalityText},
		InputAudioTranscription: &genai.AudioTranscriptionConfig{
			LanguageCodes:    langCodes,
			CustomVocabulary: vocab,
		},
		RealtimeInputConfig: &genai.RealtimeInputConfig{AutomaticActivityDetection: aad},
	}
	if isTranscribe {
		liveCfg.InputAudioTranscription.Mode = genai.AudioTranscriptionConfigMode(mode)
	} else {
		liveCfg.Temperature = genai.Ptr(float32(0))
		instruction := DefaultSystemInstruction
		if len(langCodes) > 0 {
			instruction += " Primary language: " + strings.Join(langCodes, ", ") + "."
		}
		liveCfg.SystemInstruction = genai.NewContentFromText(instruction, genai.RoleUser)
	}

	session, err := c.connect(ctx, opts, model, liveCfg)
	if err != nil {
		return "", err
	}

	var closeConnOnce sync.Once
	closeConn := func() {
		closeConnOnce.Do(func() {
			_ = session.Close()
		})
	}
	defer closeConn()

	// Ensure context cancellation immediately unblocks session.Receive.
	connWatcherDone := make(chan struct{})
	go func() {
		defer close(connWatcherDone)
		select {
		case <-ctx.Done():
			closeConn()
		case <-sessionDone:
		}
	}()
	defer func() {
		endSession()
		<-connWatcherDone
	}()

	var writeMu sync.Mutex
	send := func(in genai.LiveRealtimeInput) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return session.SendRealtimeInput(in)
	}

	var (
		textMu                sync.Mutex
		committed             string
		interim               string
		lastSnapshot          string
		sawInputTranscription bool
		speechSinceLastFinal  bool
		awaitingFinalTurn     bool
		gotFinalAfterEnd      bool
		gotTurnDoneAfterEnd   bool
		readerErr             error
		apiErr                error
		finalDoneCh           = make(chan struct{}, 1)
		graceTriggerCh        = make(chan struct{}, 1)
		readerDoneCh          = make(chan struct{})
		serverVADStopOnce     sync.Once
		serverVADStopped      bool
	)
	serverStop := func(reason string) {
		serverVADStopOnce.Do(func() {
			textMu.Lock()
			serverVADStopped = true
			textMu.Unlock()
			log.Printf("live session server vad auto-stop (%s silence_ms=%d)", reason, opts.VADSilenceMs)
			bufferedAudio.StopCapture()
		})
	}

	go func() {
		defer close(readerDoneCh)
		for {
			msg, err := session.Receive()
			if err != nil {
				textMu.Lock()
				if strings.HasPrefix(err.Error(), "received error in response") {
					apiErr = errors.New(SanitizeError(fmt.Errorf("gemini live error: %w", err), opts.APIKey))
				} else {
					readerErr = err
				}
				textMu.Unlock()
				return
			}

			if serverVAD {
				if isStartOfSpeechSignal(msg) {
					textMu.Lock()
					speechSinceLastFinal = true
					textMu.Unlock()
				}
				if isEndOfSpeechSignal(msg) {
					serverStop("activity_end")
				}
			}
			sc := msg.ServerContent
			if sc == nil {
				continue
			}

			var (
				gotFinal    bool
				turnDone    = sc.TurnComplete || sc.GenerationComplete
				snapshot    string
				emitPartial bool
				shouldDone  bool
				shouldGrace bool
				heardSpeech bool
			)

			textMu.Lock()
			if sc.InterimInputTranscription != nil {
				interim = sc.InterimInputTranscription.Text
				if interim != "" {
					speechSinceLastFinal = true
					heardSpeech = true
				}
			}
			if sc.InputTranscription != nil {
				sawInputTranscription = true
				interim = ""
				speechSinceLastFinal = false
				gotFinal = true
				text := sc.InputTranscription.Text
				if text != "" {
					heardSpeech = true
				}
				if isTranscribe && mode == "SMART" {
					committed = text
				} else if text != "" {
					committed = joinTranscriptSegments(committed, text)
				}
			}
			if sc.ModelTurn != nil && !sawInputTranscription && sc.InputTranscription == nil && sc.InterimInputTranscription == nil {
				var modelDelta strings.Builder
				for _, part := range sc.ModelTurn.Parts {
					if part == nil || part.Thought || part.Text == "" {
						continue
					}
					modelDelta.WriteString(part.Text)
				}
				if modelDelta.Len() > 0 {
					committed += modelDelta.String()
					heardSpeech = true
				}
			}

			curSnapshot := joinTranscriptSegments(committed, interim)
			if curSnapshot != lastSnapshot {
				lastSnapshot = curSnapshot
				snapshot = curSnapshot
				emitPartial = true
			}

			// Fallback end-of-speech signal: if the server never sends explicit
			// activity events, the end of the server-detected turn (turnComplete /
			// generationComplete) after we have heard speech also ends capture.
			serverTurnEnded := serverVAD && !awaitingFinalTurn && turnDone && (heardSpeech || committed != "")

			if awaitingFinalTurn {
				if gotFinal {
					gotFinalAfterEnd = true
				}
				if turnDone {
					gotTurnDoneAfterEnd = true
				}
				if !isTranscribe {
					if gotTurnDoneAfterEnd {
						shouldDone = true
					}
				} else {
					if gotFinalAfterEnd && gotTurnDoneAfterEnd {
						shouldDone = true
					} else if gotFinal {
						shouldGrace = true
					} else if turnDone {
						if interim == "" && !speechSinceLastFinal && committed != "" {
							shouldDone = true
						} else {
							shouldGrace = true
						}
					}
				}
			}
			textMu.Unlock()

			if emitPartial && onPartial != nil && ctx.Err() == nil {
				onPartial(snapshot)
			}
			if serverTurnEnded {
				serverStop("turn_complete")
			}
			if shouldDone {
				notifySignal(finalDoneCh)
			} else if shouldGrace {
				notifySignal(graceTriggerCh)
			}
		}
	}()
	defer func() {
		closeConn()
		<-readerDoneCh
	}()

	log.Printf("live session started (model=%s vad_silence_ms=%d server_vad=%t)", model, opts.VADSilenceMs, serverVAD)
	var (
		sendErr    error
		sentAudio  bool
		chunksSent int
		stopReason = "unknown"
	)
	mimeType := fmt.Sprintf("audio/pcm;rate=%d", SampleRate)

	for {
		if ctx.Err() != nil {
			log.Printf("live session cancelled (chunks=%d)", chunksSent)
			return "", ctx.Err()
		}
		select {
		case <-readerDoneCh:
			stopReason = "ws_reader_closed"
			goto finalize
		default:
		}

		chunk, err := bufferedAudio.ReadChunkContext(ctx, readerDoneCh)
		if ctx.Err() != nil {
			log.Printf("live session cancelled (chunks=%d)", chunksSent)
			return "", ctx.Err()
		}
		if len(chunk) > 0 {
			if !sentAudio && !serverVAD {
				if wErr := send(genai.LiveRealtimeInput{ActivityStart: &genai.ActivityStart{}}); wErr != nil {
					sendErr = wErr
					stopReason = "ws_write_err"
					break
				}
			}
			if wErr := send(genai.LiveRealtimeInput{Audio: &genai.Blob{MIMEType: mimeType, Data: chunk}}); wErr != nil {
				sendErr = wErr
				stopReason = "ws_write_err"
				break
			}
			sentAudio = true
			chunksSent++
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				sendErr = err
				stopReason = "audio_err"
			} else if stopReason == "unknown" {
				textMu.Lock()
				vadStopped := serverVADStopped
				textMu.Unlock()
				if vadStopped {
					stopReason = "server_vad"
				} else {
					stopReason = "stop_cmd_or_eof"
				}
			}
			break
		}
	}

finalize:
	log.Printf("live session finalizing (reason=%s chunks=%d)", stopReason, chunksSent)
	bufferedAudio.StopCapture()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	finalizedCleanly := false
	if sentAudio {
		textMu.Lock()
		awaitingFinalTurn = true
		alreadySettled := isTranscribe && committed != "" && interim == "" && !speechSinceLastFinal
		textMu.Unlock()

		readerClosed := false
		select {
		case <-readerDoneCh:
			readerClosed = true
		default:
		}
		if !readerClosed {
			end := genai.LiveRealtimeInput{AudioStreamEnd: true}
			if !serverVAD {
				end = genai.LiveRealtimeInput{ActivityEnd: &genai.ActivityEnd{}}
			}
			if wErr := send(end); wErr != nil && sendErr == nil {
				sendErr = wErr
			}
			if !isTranscribe {
				writeMu.Lock()
				wErr := session.SendClientContent(genai.LiveClientContentInput{TurnComplete: genai.Ptr(true)})
				writeMu.Unlock()
				if wErr != nil && sendErr == nil {
					sendErr = wErr
				}
			}
		}

		finalizeTimeout := c.FinalizeTimeout
		if finalizeTimeout <= 0 {
			finalizeTimeout = 5 * time.Second
		}
		gracePeriod := c.FinalizeGracePeriod
		if gracePeriod <= 0 {
			gracePeriod = 300 * time.Millisecond
		}

		timeoutTimer := time.NewTimer(finalizeTimeout)
		defer timeoutTimer.Stop()

		var graceTimer *time.Timer
		var graceCh <-chan time.Time
		armGrace := func() {
			if graceTimer == nil {
				graceTimer = time.NewTimer(gracePeriod)
				graceCh = graceTimer.C
				return
			}
			if !graceTimer.Stop() {
				select {
				case <-graceTimer.C:
				default:
				}
			}
			graceTimer.Reset(gracePeriod)
			graceCh = graceTimer.C
		}
		defer func() {
			if graceTimer != nil {
				graceTimer.Stop()
			}
		}()

		if alreadySettled {
			armGrace()
		}

	waitLoop:
		for {
			select {
			case <-finalDoneCh:
				finalizedCleanly = true
				break waitLoop
			case <-readerDoneCh:
				break waitLoop
			case <-graceCh:
				finalizedCleanly = true
				break waitLoop
			case <-graceTriggerCh:
				armGrace()
			case <-timeoutTimer.C:
				break waitLoop
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
	}

	closeConn()

	textMu.Lock()
	finalText := strings.TrimSpace(joinTranscriptSegments(committed, interim))
	rErr := readerErr
	sErr := apiErr
	turnDoneCleanly := finalizedCleanly || (gotFinalAfterEnd && gotTurnDoneAfterEnd)
	textMu.Unlock()

	if finalText == "" {
		if sErr != nil {
			return "", sErr
		}
		if sendErr != nil {
			return "", fmt.Errorf("audio streaming failed: %s", SanitizeError(sendErr, opts.APIKey))
		}
		if rErr != nil && !isExpectedWSClose(rErr) && !turnDoneCleanly {
			return "", fmt.Errorf("gemini live stream failed: %s", SanitizeError(rErr, opts.APIKey))
		}
	}
	return finalText, nil
}

func isExpectedWSClose(err error) bool {
	if err == nil || errors.Is(err, io.EOF) {
		return true
	}
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		return ce.Code == websocket.CloseNormalClosure || ce.Code == websocket.CloseGoingAway
	}
	return false
}
