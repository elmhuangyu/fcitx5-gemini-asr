package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

const (
	DefaultLiveWSURL         = "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"
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
	VADSilenceMs     int
	VADThreshold     float64
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

type liveSetupMessage struct {
	Setup liveSetupPayload `json:"setup"`
}

type liveSetupPayload struct {
	Model                   string                        `json:"model"`
	GenerationConfig        liveGenerationConfig          `json:"generationConfig"`
	SystemInstruction       *liveSystemInstruction        `json:"systemInstruction,omitempty"`
	InputAudioTranscription *liveAudioTranscriptionConfig `json:"inputAudioTranscription,omitempty"`
}

type liveGenerationConfig struct {
	ResponseModalities []string `json:"responseModalities"`
	Temperature        *float64 `json:"temperature,omitempty"`
}

type liveSystemInstruction struct {
	Parts []liveTextPart `json:"parts"`
}

type liveTextPart struct {
	Text string `json:"text"`
}

type liveAudioTranscriptionConfig struct {
	LanguageCodes    []string `json:"languageCodes"`
	CustomVocabulary []string `json:"customVocabulary,omitempty"`
	Mode             string   `json:"mode,omitempty"`
}

type liveRealtimeInputMessage struct {
	RealtimeInput liveRealtimeInputPayload `json:"realtimeInput"`
}

type liveRealtimeInputPayload struct {
	Audio          *liveBlob `json:"audio,omitempty"`
	AudioStreamEnd bool      `json:"audioStreamEnd,omitempty"`
}

type liveBlob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type liveClientContentMessage struct {
	ClientContent liveClientContentPayload `json:"clientContent"`
}

type liveClientContentPayload struct {
	TurnComplete bool `json:"turnComplete"`
}

type liveTranscription struct {
	Text string `json:"text"`
}

type liveServerMessage struct {
	SetupComplete json.RawMessage `json:"setupComplete"`
	ServerContent *struct {
		ModelTurn *struct {
			Parts []struct {
				Text    string `json:"text"`
				Thought bool   `json:"thought"`
			} `json:"parts"`
		} `json:"modelTurn"`
		InterimInputTranscription *liveTranscription `json:"interimInputTranscription"`
		InputTranscription        *liveTranscription `json:"inputTranscription"`
		TurnComplete              bool               `json:"turnComplete"`
		GenerationComplete        bool               `json:"generationComplete"`
	} `json:"serverContent"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
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

	rawWSURL := strings.TrimSpace(opts.LiveWSURL)
	if rawWSURL == "" {
		rawWSURL = DefaultLiveWSURL
	}
	u, err := url.Parse(rawWSURL)
	if err != nil {
		return "", fmt.Errorf("invalid Live WebSocket URL %q: %s", RedactURL(rawWSURL), SanitizeError(err, opts.APIKey))
	}
	q := u.Query()
	q.Set("key", opts.APIKey)
	u.RawQuery = q.Encode()
	wsURL := u.String()

	dialer, err := BuildWSDialer(opts.Proxy)
	if err != nil {
		return "", err
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

	conn, resp, err := dialer.DialContext(ctx, wsURL, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return "", fmt.Errorf("connect Gemini Live (%s): %s", RedactURL(wsURL), SanitizeError(err, opts.APIKey))
	}

	var closeConnOnce sync.Once
	closeConn := func() {
		closeConnOnce.Do(func() {
			_ = conn.Close()
		})
	}
	defer closeConn()

	// Ensure context cancellation immediately unblocks WebSocket ReadMessage.
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

	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = DefaultLiveModel
	}
	if !strings.HasPrefix(model, "models/") {
		model = "models/" + model
	}

	isTranscribe := isTranscribeLiveModel(model)
	langCodes := NormalizeLanguageCodes(opts.Language)

	var writeMu sync.Mutex
	writeJSON := func(v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return conn.WriteJSON(v)
	}

	setupPayload := liveSetupPayload{
		Model: model,
		GenerationConfig: liveGenerationConfig{
			ResponseModalities: []string{"TEXT"},
		},
		InputAudioTranscription: &liveAudioTranscriptionConfig{
			LanguageCodes:    langCodes,
			CustomVocabulary: vocab,
		},
	}
	if isTranscribe {
		setupPayload.InputAudioTranscription.Mode = mode
	} else {
		temp := 0.0
		setupPayload.GenerationConfig.Temperature = &temp
		instruction := DefaultSystemInstruction
		if len(langCodes) > 0 {
			instruction += " Primary language: " + strings.Join(langCodes, ", ") + "."
		}
		setupPayload.SystemInstruction = &liveSystemInstruction{
			Parts: []liveTextPart{{Text: instruction}},
		}
	}

	if err := writeJSON(liveSetupMessage{Setup: setupPayload}); err != nil {
		return "", fmt.Errorf("send Gemini Live setup: %s", SanitizeError(err, opts.APIKey))
	}

	setupTimeout := c.SetupTimeout
	if setupTimeout <= 0 {
		setupTimeout = 10 * time.Second
	}
	_ = conn.SetReadDeadline(time.Now().Add(setupTimeout))
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", fmt.Errorf("await Gemini Live setupComplete: %s", SanitizeError(err, opts.APIKey))
		}
		var msg liveServerMessage
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		if msg.Error != nil {
			return "", errors.New(SanitizeError(fmt.Errorf("gemini live error (%d %s): %s", msg.Error.Code, msg.Error.Status, msg.Error.Message), opts.APIKey))
		}
		if len(msg.SetupComplete) > 0 {
			break
		}
	}
	_ = conn.SetReadDeadline(time.Time{})

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
	)

	go func() {
		defer close(readerDoneCh)
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				textMu.Lock()
				readerErr = err
				textMu.Unlock()
				return
			}
			var msg liveServerMessage
			if err := json.Unmarshal(payload, &msg); err != nil {
				continue
			}
			if msg.Error != nil {
				textMu.Lock()
				apiErr = errors.New(SanitizeError(fmt.Errorf("gemini live error (%d %s): %s", msg.Error.Code, msg.Error.Status, msg.Error.Message), opts.APIKey))
				textMu.Unlock()
				return
			}
			if msg.ServerContent == nil {
				continue
			}

			sc := msg.ServerContent
			var (
				gotFinal    bool
				turnDone    = sc.TurnComplete || sc.GenerationComplete
				snapshot    string
				emitPartial bool
				shouldDone  bool
				shouldGrace bool
			)

			textMu.Lock()
			if sc.InterimInputTranscription != nil {
				interim = sc.InterimInputTranscription.Text
				if interim != "" {
					speechSinceLastFinal = true
				}
			}
			if sc.InputTranscription != nil {
				sawInputTranscription = true
				interim = ""
				speechSinceLastFinal = false
				gotFinal = true
				text := sc.InputTranscription.Text
				if isTranscribe && mode == "SMART" {
					committed = text
				} else if text != "" {
					committed = joinTranscriptSegments(committed, text)
				}
			}
			if sc.ModelTurn != nil && !sawInputTranscription && sc.InputTranscription == nil && sc.InterimInputTranscription == nil {
				var modelDelta strings.Builder
				for _, part := range sc.ModelTurn.Parts {
					if part.Thought || part.Text == "" {
						continue
					}
					modelDelta.WriteString(part.Text)
				}
				if modelDelta.Len() > 0 {
					committed += modelDelta.String()
				}
			}

			curSnapshot := joinTranscriptSegments(committed, interim)
			if curSnapshot != lastSnapshot {
				lastSnapshot = curSnapshot
				snapshot = curSnapshot
				emitPartial = true
			}

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

	vadThreshold := opts.VADThreshold
	if vadThreshold <= 0 {
		vadThreshold = DefaultVADThreshold
	}
	vad := NewVADDetector(vadThreshold, opts.VADSilenceMs)
	var (
		sendErr      error
		sentAudio    bool
		vadTriggered bool
	)

	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		select {
		case <-readerDoneCh:
			goto finalize
		default:
		}

		chunk, err := bufferedAudio.ReadChunkContext(ctx, readerDoneCh)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if len(chunk) > 0 {
			frame := liveRealtimeInputMessage{
				RealtimeInput: liveRealtimeInputPayload{
					Audio: &liveBlob{
						MimeType: fmt.Sprintf("audio/pcm;rate=%d", SampleRate),
						Data:     base64.StdEncoding.EncodeToString(chunk),
					},
				},
			}
			if wErr := writeJSON(frame); wErr != nil {
				sendErr = wErr
				break
			}
			sentAudio = true
			if ComputeRMS(chunk) >= vadThreshold {
				textMu.Lock()
				speechSinceLastFinal = true
				textMu.Unlock()
			}
			if !vadTriggered && vad.Feed(chunk) {
				// VAD silence auto-stop: stop capturing new audio but keep
				// looping so chunks already buffered are drained to the
				// WebSocket before audioStreamEnd, matching the explicit
				// stop (stopCh) path.
				vadTriggered = true
				bufferedAudio.StopCapture()
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				sendErr = err
			}
			break
		}
	}

finalize:
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
			if wErr := writeJSON(liveRealtimeInputMessage{
				RealtimeInput: liveRealtimeInputPayload{
					AudioStreamEnd: true,
				},
			}); wErr != nil && sendErr == nil {
				sendErr = wErr
			}
			if !isTranscribe {
				if wErr := writeJSON(liveClientContentMessage{
					ClientContent: liveClientContentPayload{
						TurnComplete: true,
					},
				}); wErr != nil && sendErr == nil {
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

	writeMu.Lock()
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(250*time.Millisecond))
	writeMu.Unlock()
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
