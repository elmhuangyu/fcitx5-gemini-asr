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
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	DefaultLiveWSURL         = "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"
	DefaultSystemInstruction = "Verbatim speech-to-text transcriber. Output ONLY exact transcribed text with proper punctuation. Never answer questions or converse."
)

type LiveSessionOptions struct {
	APIKey       string
	Proxy        string
	LiveWSURL    string
	Model        string
	Language     string
	VADSilenceMs int
	VADThreshold float64
}

type LiveTranscriber interface {
	Run(ctx context.Context, audio AudioSource, stopCh <-chan struct{}, opts LiveSessionOptions, onPartial func(string)) (string, error)
}

type GeminiLiveClient struct {
	SetupTimeout    time.Duration
	FinalizeTimeout time.Duration
}

func NewGeminiLiveClient() *GeminiLiveClient {
	return &GeminiLiveClient{
		SetupTimeout:    10 * time.Second,
		FinalizeTimeout: 5 * time.Second,
	}
}

type liveSetupMessage struct {
	Setup liveSetupPayload `json:"setup"`
}

type liveSetupPayload struct {
	Model             string                `json:"model"`
	GenerationConfig  liveGenerationConfig  `json:"generationConfig"`
	SystemInstruction liveSystemInstruction `json:"systemInstruction"`
}

type liveGenerationConfig struct {
	ResponseModalities []string `json:"responseModalities"`
	Temperature        float64  `json:"temperature"`
}

type liveSystemInstruction struct {
	Parts []liveTextPart `json:"parts"`
}

type liveTextPart struct {
	Text string `json:"text"`
}

type liveRealtimeInputMessage struct {
	RealtimeInput liveRealtimeInputPayload `json:"realtimeInput"`
}

type liveRealtimeInputPayload struct {
	MediaChunks    []liveMediaChunk `json:"mediaChunks,omitempty"`
	AudioStreamEnd bool             `json:"audioStreamEnd,omitempty"`
}

type liveMediaChunk struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type liveClientContentMessage struct {
	ClientContent liveClientContentPayload `json:"clientContent"`
}

type liveClientContentPayload struct {
	TurnComplete bool `json:"turnComplete"`
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
		InputTranscription *struct {
			Text string `json:"text"`
		} `json:"inputTranscription"`
		TurnComplete       bool `json:"turnComplete"`
		GenerationComplete bool `json:"generationComplete"`
	} `json:"serverContent"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func (c *GeminiLiveClient) Run(ctx context.Context, audio AudioSource, stopCh <-chan struct{}, opts LiveSessionOptions, onPartial func(string)) (string, error) {
	if audio == nil {
		return "", errors.New("audio source is nil")
	}

	rawWSURL := strings.TrimSpace(opts.LiveWSURL)
	if rawWSURL == "" {
		rawWSURL = DefaultLiveWSURL
	}
	u, err := url.Parse(rawWSURL)
	if err != nil {
		return "", fmt.Errorf("invalid Live WebSocket URL %q: %w", RedactURL(rawWSURL), err)
	}
	q := u.Query()
	q.Set("key", opts.APIKey)
	u.RawQuery = q.Encode()
	wsURL := u.String()

	dialer, err := BuildWSDialer(opts.Proxy)
	if err != nil {
		return "", err
	}

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

	// Ensure context cancellation immediately unblocks ReadChunk and WebSocket ReadMessage.
	sessionDone := make(chan struct{})
	defer close(sessionDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = audio.Close()
			closeConn()
		case <-stopCh:
			_ = audio.Close()
			select {
			case <-ctx.Done():
				closeConn()
			case <-sessionDone:
			}
		case <-sessionDone:
		}
	}()

	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = "gemini-3.5-transcribe-live"
	}
	if !strings.HasPrefix(model, "models/") {
		model = "models/" + model
	}

	instruction := DefaultSystemInstruction
	lang := strings.TrimSpace(opts.Language)
	if lang != "" && !strings.EqualFold(lang, "auto") {
		instruction += " Primary language: " + lang + "."
	}

	var writeMu sync.Mutex
	writeJSON := func(v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return conn.WriteJSON(v)
	}

	setupMsg := liveSetupMessage{
		Setup: liveSetupPayload{
			Model: model,
			GenerationConfig: liveGenerationConfig{
				ResponseModalities: []string{"TEXT"},
				Temperature:        0.0,
			},
			SystemInstruction: liveSystemInstruction{
				Parts: []liveTextPart{{Text: instruction}},
			},
		},
	}
	if err := writeJSON(setupMsg); err != nil {
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
			return "", fmt.Errorf("gemini live error (%d %s): %s", msg.Error.Code, msg.Error.Status, msg.Error.Message)
		}
		if len(msg.SetupComplete) > 0 {
			break
		}
	}
	_ = conn.SetReadDeadline(time.Time{})

	var (
		textMu            sync.Mutex
		accText           strings.Builder
		readerErr         error
		awaitingFinalTurn atomic.Bool
		finalTurnOnce     sync.Once
		finalTurnCh       = make(chan struct{})
		readerDoneCh      = make(chan struct{})
	)
	signalFinalTurn := func() {
		finalTurnOnce.Do(func() {
			close(finalTurnCh)
		})
	}

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
				readerErr = fmt.Errorf("gemini live error (%d %s): %s", msg.Error.Code, msg.Error.Status, msg.Error.Message)
				textMu.Unlock()
				return
			}
			if msg.ServerContent == nil {
				continue
			}

			var delta strings.Builder
			if msg.ServerContent.ModelTurn != nil {
				for _, part := range msg.ServerContent.ModelTurn.Parts {
					if part.Thought || part.Text == "" {
						continue
					}
					delta.WriteString(part.Text)
				}
			}
			if delta.Len() == 0 && msg.ServerContent.InputTranscription != nil && msg.ServerContent.InputTranscription.Text != "" {
				delta.WriteString(msg.ServerContent.InputTranscription.Text)
			}

			if delta.Len() > 0 {
				textMu.Lock()
				accText.WriteString(delta.String())
				snapshot := accText.String()
				textMu.Unlock()
				if onPartial != nil && ctx.Err() == nil {
					onPartial(snapshot)
				}
			}

			if (msg.ServerContent.TurnComplete || msg.ServerContent.GenerationComplete) && awaitingFinalTurn.Load() {
				signalFinalTurn()
			}
		}
	}()
	defer func() {
		closeConn()
		<-readerDoneCh
	}()

	vad := NewVADDetector(opts.VADThreshold, opts.VADSilenceMs)
	var sendErr error

	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		select {
		case <-stopCh:
			goto finalize
		case <-readerDoneCh:
			goto finalize
		default:
		}

		chunk, err := audio.ReadChunk()
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if len(chunk) > 0 {
			frame := liveRealtimeInputMessage{
				RealtimeInput: liveRealtimeInputPayload{
					MediaChunks: []liveMediaChunk{
						{
							MimeType: fmt.Sprintf("audio/pcm;rate=%d", SampleRate),
							Data:     base64.StdEncoding.EncodeToString(chunk),
						},
					},
				},
			}
			if wErr := writeJSON(frame); wErr != nil {
				sendErr = wErr
				break
			}
			if vad.Feed(chunk) {
				break
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
	_ = audio.Close()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	awaitingFinalTurn.Store(true)
	_ = writeJSON(liveRealtimeInputMessage{
		RealtimeInput: liveRealtimeInputPayload{
			AudioStreamEnd: true,
		},
	})
	_ = writeJSON(liveClientContentMessage{
		ClientContent: liveClientContentPayload{
			TurnComplete: true,
		},
	})

	finalizeTimeout := c.FinalizeTimeout
	if finalizeTimeout <= 0 {
		finalizeTimeout = 5 * time.Second
	}
	timer := time.NewTimer(finalizeTimeout)
	defer timer.Stop()

	select {
	case <-finalTurnCh:
	case <-readerDoneCh:
	case <-timer.C:
	case <-ctx.Done():
		return "", ctx.Err()
	}

	writeMu.Lock()
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(250*time.Millisecond))
	writeMu.Unlock()
	closeConn()

	textMu.Lock()
	finalText := strings.TrimSpace(accText.String())
	rErr := readerErr
	textMu.Unlock()

	if finalText == "" {
		if sendErr != nil {
			return "", fmt.Errorf("audio streaming failed: %s", SanitizeError(sendErr, opts.APIKey))
		}
		if rErr != nil && !isExpectedWSClose(rErr) {
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
