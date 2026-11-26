package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type sliceAudioSource struct {
	mu     sync.Mutex
	chunks [][]byte
	idx    int
	closed bool
	waitCh chan struct{}
}

func newSliceAudioSource(chunks [][]byte, blockAfter bool) *sliceAudioSource {
	var ch chan struct{}
	if blockAfter {
		ch = make(chan struct{})
	}
	return &sliceAudioSource{
		chunks: chunks,
		waitCh: ch,
	}
}

func (s *sliceAudioSource) ReadChunk() ([]byte, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, io.EOF
	}
	if s.idx < len(s.chunks) {
		c := s.chunks[s.idx]
		s.idx++
		s.mu.Unlock()
		return c, nil
	}
	waitCh := s.waitCh
	s.mu.Unlock()

	if waitCh != nil {
		<-waitCh
	}
	return nil, io.EOF
}

func (s *sliceAudioSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		if s.waitCh != nil {
			close(s.waitCh)
		}
	}
	return nil
}

// isStreamEnd reports whether a realtimeInput frame ends the audio stream
// (audioStreamEnd with server VAD, activityEnd with manual activity control).
func isStreamEnd(ri map[string]any) bool {
	if end, ok := ri["audioStreamEnd"].(bool); ok && end {
		return true
	}
	_, ok := ri["activityEnd"]
	return ok
}

func TestResolveLiveEndpoint(t *testing.T) {
	cases := []struct {
		in, base, ver string
		wantErr       bool
	}{
		{"", "https://generativelanguage.googleapis.com", "v1beta", false},
		{"wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent", "wss://generativelanguage.googleapis.com", "v1beta", false},
		{"wss://proxy.example.com/prefix/ws/google.ai.generativelanguage.v1alpha.GenerativeService.BidiGenerateContent?key=abc", "wss://proxy.example.com/prefix", "v1alpha", false},
		{"ws://127.0.0.1:8080", "ws://127.0.0.1:8080", "v1beta", false},
		{"ws://127.0.0.1:8080/", "ws://127.0.0.1:8080", "v1beta", false},
		{"not a url", "", "", true},
	}
	for _, tc := range cases {
		base, ver, err := resolveLiveEndpoint(tc.in)
		if (err != nil) != tc.wantErr {
			t.Fatalf("resolveLiveEndpoint(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
		}
		if base != tc.base || ver != tc.ver {
			t.Fatalf("resolveLiveEndpoint(%q) = (%q, %q), want (%q, %q)", tc.in, base, ver, tc.base, tc.ver)
		}
	}
}

func TestNormalizeLanguageCodes(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", []string{}},
		{"   ", []string{}},
		{"auto", []string{}},
		{"AUTO", []string{}},
		{"en-US, auto", []string{}},
		{"zh-CN", []string{"cmn-Hans-CN"}},
		{"zh_cn", []string{"cmn-Hans-CN"}},
		{"zh", []string{"cmn-Hans-CN"}},
		{"zh-Hans", []string{"cmn-Hans-CN"}},
		{"cmn-Hans-CN", []string{"cmn-Hans-CN"}},
		{"zh-HK", []string{"yue-Hant-HK"}},
		{"yue", []string{"yue-Hant-HK"}},
		{"en_US, zh-CN, en-US", []string{"en-US", "cmn-Hans-CN"}},
		{"ja-JP", []string{"ja-JP"}},
	}

	for _, tc := range cases {
		got := NormalizeLanguageCodes(tc.in)
		if got == nil {
			t.Fatalf("NormalizeLanguageCodes(%q) returned nil slice (want non-nil)", tc.in)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("NormalizeLanguageCodes(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

func TestGeminiLiveClient_TranscribeLiveVerbatim(t *testing.T) {
	chunk1 := makePCMChunk(800, ChunkBytes)
	chunk2 := makePCMChunk(900, ChunkBytes)

	var (
		mu               sync.Mutex
		gotAPIKey        string
		gotSetup         map[string]any
		gotAudioPayloads [][]byte
		gotMediaChunks   bool
		gotStreamEnd     bool
		gotClientContent bool
	)

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAPIKey = r.Header.Get("x-goog-api-key")
		if r.URL.Query().Has("key") {
			gotAPIKey += "+query-key-leak"
		}
		mu.Unlock()

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var setup map[string]any
		if err := json.Unmarshal(payload, &setup); err != nil {
			return
		}
		mu.Lock()
		gotSetup = setup
		mu.Unlock()

		_ = conn.WriteJSON(map[string]any{"setupComplete": map[string]any{}})

		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var msg map[string]any
			if err := json.Unmarshal(payload, &msg); err != nil {
				continue
			}

			if _, ok := msg["clientContent"]; ok {
				mu.Lock()
				gotClientContent = true
				mu.Unlock()
			}

			if ri, ok := msg["realtimeInput"].(map[string]any); ok {
				if _, hasMC := ri["mediaChunks"]; hasMC {
					mu.Lock()
					gotMediaChunks = true
					mu.Unlock()
				}
				if audioObj, ok := ri["audio"].(map[string]any); ok {
					dataStr, _ := audioObj["data"].(string)
					decoded, decErr := base64.StdEncoding.DecodeString(dataStr)
					if decErr == nil {
						mu.Lock()
						gotAudioPayloads = append(gotAudioPayloads, decoded)
						n := len(gotAudioPayloads)
						mu.Unlock()
						switch n {
						case 1:
							_ = conn.WriteJSON(map[string]any{
								"serverContent": map[string]any{
									"interimInputTranscription": map[string]any{"text": "今天"},
								},
							})
							_ = conn.WriteJSON(map[string]any{
								"serverContent": map[string]any{
									"interimInputTranscription": map[string]any{"text": "今天下午"},
								},
							})
							_ = conn.WriteJSON(map[string]any{
								"serverContent": map[string]any{
									"inputTranscription": map[string]any{"text": "今天下午"},
								},
							})
						case 2:
							_ = conn.WriteJSON(map[string]any{
								"serverContent": map[string]any{
									"interimInputTranscription": map[string]any{"text": "三点"},
								},
							})
							_ = conn.WriteJSON(map[string]any{
								"serverContent": map[string]any{
									"inputTranscription": map[string]any{"text": "三点开会"},
								},
							})
						}
					}
				}
				if isStreamEnd(ri) {
					mu.Lock()
					gotStreamEnd = true
					mu.Unlock()
					_ = conn.WriteJSON(map[string]any{
						"serverContent": map[string]any{
							"inputTranscription": map[string]any{"text": "。"},
							"turnComplete":       true,
						},
					})
					return
				}
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	audio := newSliceAudioSource([][]byte{chunk1, chunk2}, true)

	stopCh := make(chan struct{})
	var partials []string
	var partialMu sync.Mutex

	client := NewGeminiLiveClient()
	client.FinalizeTimeout = 2 * time.Second
	client.FinalizeGracePeriod = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		time.Sleep(150 * time.Millisecond)
		close(stopCh)
	}()

	finalText, err := client.Run(ctx, audio, stopCh, LiveSessionOptions{
		APIKey:           "test-live-key",
		LiveWSURL:        wsURL,
		Model:            "gemini-3.5-transcribe-live",
		Language:         "zh-CN",
		Mode:             "VERBATIM",
		CustomVocabulary: []string{"Fcitx5", "PipeWire"},
		VADSilenceMs:     1200,
	}, func(p string) {
		partialMu.Lock()
		partials = append(partials, p)
		partialMu.Unlock()
	})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	if finalText != "今天下午三点开会。" {
		t.Fatalf("finalText = %q, want %q", finalText, "今天下午三点开会。")
	}

	mu.Lock()
	defer mu.Unlock()
	if gotAPIKey != "test-live-key" {
		t.Fatalf("key = %q", gotAPIKey)
	}
	if gotMediaChunks || gotClientContent || !gotStreamEnd {
		t.Fatalf("mediaChunks=%v clientContent=%v streamEnd=%v", gotMediaChunks, gotClientContent, gotStreamEnd)
	}
	if len(gotAudioPayloads) != 2 || !bytes.Equal(gotAudioPayloads[0], chunk1) || !bytes.Equal(gotAudioPayloads[1], chunk2) {
		t.Fatalf("unexpected decoded audio payloads: count=%d", len(gotAudioPayloads))
	}

	setupObj, _ := gotSetup["setup"].(map[string]any)
	if setupObj["model"] != "models/gemini-3.5-transcribe-live" {
		t.Fatalf("setup model = %v", setupObj["model"])
	}
	if _, hasSys := setupObj["systemInstruction"]; hasSys {
		t.Fatalf("transcribe-live setup must not send systemInstruction: %v", setupObj["systemInstruction"])
	}
	genCfg, _ := setupObj["generationConfig"].(map[string]any)
	if _, hasTemp := genCfg["temperature"]; hasTemp {
		t.Fatalf("transcribe-live generationConfig must not send temperature: %v", genCfg["temperature"])
	}
	mods, _ := genCfg["responseModalities"].([]any)
	if len(mods) != 1 || mods[0] != "TEXT" {
		t.Fatalf("responseModalities = %v", mods)
	}
	iat, ok := setupObj["inputAudioTranscription"].(map[string]any)
	if !ok {
		t.Fatalf("missing inputAudioTranscription in setup: %#v", setupObj)
	}
	if iat["mode"] != "VERBATIM" {
		t.Fatalf("mode = %v, want VERBATIM", iat["mode"])
	}
	wantLangs := []any{"cmn-Hans-CN"}
	if !reflect.DeepEqual(iat["languageCodes"], wantLangs) {
		t.Fatalf("languageCodes = %#v, want %#v", iat["languageCodes"], wantLangs)
	}
	wantVocab := []any{"Fcitx5", "PipeWire"}
	if !reflect.DeepEqual(iat["customVocabulary"], wantVocab) {
		t.Fatalf("customVocabulary = %#v, want %#v", iat["customVocabulary"], wantVocab)
	}

	partialMu.Lock()
	defer partialMu.Unlock()
	wantPartials := []string{
		"今天",
		"今天下午",
		"今天下午三点",
		"今天下午三点开会",
		"今天下午三点开会。",
	}
	if !reflect.DeepEqual(partials, wantPartials) {
		t.Fatalf("partials = %#v, want %#v", partials, wantPartials)
	}
}

func TestGeminiLiveClient_TranscribeLiveSmartMode(t *testing.T) {
	runSmartCase := func(t *testing.T, clearAtEnd bool) (string, []string, map[string]any) {
		t.Helper()
		var (
			mu       sync.Mutex
			gotSetup map[string]any
		)
		upgrader := websocket.Upgrader{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()

			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var setup map[string]any
			if err := json.Unmarshal(payload, &setup); err != nil {
				return
			}
			mu.Lock()
			gotSetup = setup
			mu.Unlock()
			_ = conn.WriteJSON(map[string]any{"setupComplete": map[string]any{}})

			sentChunk := false
			for {
				_, payload, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var msg map[string]any
				if err := json.Unmarshal(payload, &msg); err != nil {
					continue
				}
				ri, ok := msg["realtimeInput"].(map[string]any)
				if !ok {
					continue
				}
				if _, hasAudio := ri["audio"]; hasAudio && !sentChunk {
					sentChunk = true
					_ = conn.WriteJSON(map[string]any{
						"serverContent": map[string]any{
							"interimInputTranscription": map[string]any{"text": "um hello world"},
						},
					})
					_ = conn.WriteJSON(map[string]any{
						"serverContent": map[string]any{
							"inputTranscription": map[string]any{"text": "Hello world."},
						},
					})
					_ = conn.WriteJSON(map[string]any{
						"serverContent": map[string]any{
							"interimInputTranscription": map[string]any{"text": "make it two sentences"},
						},
					})
				}
				if isStreamEnd(ri) {
					finalText := "Hello. World."
					if clearAtEnd {
						finalText = ""
					}
					_ = conn.WriteJSON(map[string]any{
						"serverContent": map[string]any{
							"inputTranscription": map[string]any{"text": finalText},
							"turnComplete":       true,
						},
					})
					return
				}
			}
		}))
		defer srv.Close()

		wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
		audio := newSliceAudioSource([][]byte{makePCMChunk(800, ChunkBytes)}, true)
		stopCh := make(chan struct{})
		go func() {
			time.Sleep(120 * time.Millisecond)
			close(stopCh)
		}()

		var partials []string
		var partialMu sync.Mutex
		client := NewGeminiLiveClient()
		client.FinalizeTimeout = 2 * time.Second
		client.FinalizeGracePeriod = 80 * time.Millisecond

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		got, err := client.Run(ctx, audio, stopCh, LiveSessionOptions{
			APIKey:       "test-smart-key",
			LiveWSURL:    wsURL,
			Model:        "gemini-3.5-transcribe-live",
			Language:     "auto",
			Mode:         "SMART",
			VADSilenceMs: 1200,
		}, func(p string) {
			partialMu.Lock()
			partials = append(partials, p)
			partialMu.Unlock()
		})
		if err != nil {
			t.Fatalf("Run error: %v", err)
		}

		mu.Lock()
		setupCopy := gotSetup
		mu.Unlock()
		partialMu.Lock()
		partialsCopy := append([]string(nil), partials...)
		partialMu.Unlock()
		return got, partialsCopy, setupCopy
	}

	t.Run("cumulative_replacement", func(t *testing.T) {
		finalText, partials, gotSetup := runSmartCase(t, false)
		if finalText != "Hello. World." {
			t.Fatalf("finalText = %q, want %q", finalText, "Hello. World.")
		}
		wantPartials := []string{
			"um hello world",
			"Hello world.",
			"Hello world. make it two sentences",
			"Hello. World.",
		}
		if !reflect.DeepEqual(partials, wantPartials) {
			t.Fatalf("partials = %#v, want %#v", partials, wantPartials)
		}

		setupObj, _ := gotSetup["setup"].(map[string]any)
		iat, _ := setupObj["inputAudioTranscription"].(map[string]any)
		if iat["mode"] != "SMART" {
			t.Fatalf("mode = %v, want SMART", iat["mode"])
		}
		// The genai SDK omits empty languageCodes (omitempty); the API treats
		// absent/empty identically as automatic language detection.
		if lc, _ := iat["languageCodes"].([]any); len(lc) != 0 {
			t.Fatalf("languageCodes = %#v, want absent/empty", iat["languageCodes"])
		}
	})

	t.Run("voice_edit_clear_to_empty", func(t *testing.T) {
		finalText, partials, _ := runSmartCase(t, true)
		if finalText != "" {
			t.Fatalf("finalText = %q, want empty string", finalText)
		}
		if len(partials) == 0 || partials[len(partials)-1] != "" {
			t.Fatalf("expected final partial to reset to empty, got %#v", partials)
		}
	})
}

func TestGeminiLiveClient_TranscribeLiveFinalizeScenarios(t *testing.T) {
	t.Run("turnComplete_before_inputTranscription", func(t *testing.T) {
		upgrader := websocket.Upgrader{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()

			_, _, _ = conn.ReadMessage()
			_ = conn.WriteJSON(map[string]any{"setupComplete": map[string]any{}})

			for {
				_, payload, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var msg map[string]any
				if err := json.Unmarshal(payload, &msg); err != nil {
					continue
				}
				if ri, ok := msg["realtimeInput"].(map[string]any); ok {
					if isStreamEnd(ri) {
						_ = conn.WriteJSON(map[string]any{
							"serverContent": map[string]any{
								"turnComplete": true,
							},
						})
						time.Sleep(40 * time.Millisecond)
						_ = conn.WriteJSON(map[string]any{
							"serverContent": map[string]any{
								"inputTranscription": map[string]any{"text": "延迟到达的最终结果"},
							},
						})
						return
					}
				}
			}
		}))
		defer srv.Close()

		client := NewGeminiLiveClient()
		client.FinalizeTimeout = 1500 * time.Millisecond
		client.FinalizeGracePeriod = 120 * time.Millisecond

		stopCh := make(chan struct{})
		go func() {
			time.Sleep(80 * time.Millisecond)
			close(stopCh)
		}()

		audio := newSliceAudioSource([][]byte{makePCMChunk(800, ChunkBytes)}, true)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		got, err := client.Run(ctx, audio, stopCh, LiveSessionOptions{
			APIKey:    "k",
			LiveWSURL: "ws" + strings.TrimPrefix(srv.URL, "http"),
			Model:     "gemini-3.5-transcribe-live",
		}, nil)
		if err != nil {
			t.Fatalf("Run error: %v", err)
		}
		if got != "延迟到达的最终结果" {
			t.Fatalf("got = %q, want %q", got, "延迟到达的最终结果")
		}
	})

	t.Run("missing_turnComplete_completes_after_grace", func(t *testing.T) {
		upgrader := websocket.Upgrader{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()

			_, _, _ = conn.ReadMessage()
			_ = conn.WriteJSON(map[string]any{"setupComplete": map[string]any{}})

			for {
				_, payload, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var msg map[string]any
				if err := json.Unmarshal(payload, &msg); err != nil {
					continue
				}
				if ri, ok := msg["realtimeInput"].(map[string]any); ok {
					if isStreamEnd(ri) {
						_ = conn.WriteJSON(map[string]any{
							"serverContent": map[string]any{
								"inputTranscription": map[string]any{"text": "只有终稿没有turnComplete"},
							},
						})
						// Keep WS open without sending turnComplete.
						for {
							if _, _, rErr := conn.ReadMessage(); rErr != nil {
								return
							}
						}
					}
				}
			}
		}))
		defer srv.Close()

		client := NewGeminiLiveClient()
		client.FinalizeTimeout = 1500 * time.Millisecond
		client.FinalizeGracePeriod = 80 * time.Millisecond

		stopCh := make(chan struct{})
		var stopAt time.Time
		var stopMu sync.Mutex
		go func() {
			time.Sleep(80 * time.Millisecond)
			stopMu.Lock()
			stopAt = time.Now()
			stopMu.Unlock()
			close(stopCh)
		}()

		audio := newSliceAudioSource([][]byte{makePCMChunk(800, ChunkBytes)}, true)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		got, err := client.Run(ctx, audio, stopCh, LiveSessionOptions{
			APIKey:    "k",
			LiveWSURL: "ws" + strings.TrimPrefix(srv.URL, "http"),
			Model:     "gemini-3.5-transcribe-live",
		}, nil)
		elapsed := func() time.Duration {
			stopMu.Lock()
			defer stopMu.Unlock()
			return time.Since(stopAt)
		}()
		if err != nil {
			t.Fatalf("Run error: %v", err)
		}
		if got != "只有终稿没有turnComplete" {
			t.Fatalf("got = %q", got)
		}
		if elapsed >= 600*time.Millisecond {
			t.Fatalf("expected grace-period exit (<600ms), took %v", elapsed)
		}
	})

	t.Run("already_settled_before_stop", func(t *testing.T) {
		upgrader := websocket.Upgrader{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()

			_, _, _ = conn.ReadMessage()
			_ = conn.WriteJSON(map[string]any{"setupComplete": map[string]any{}})

			for {
				_, payload, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var msg map[string]any
				if err := json.Unmarshal(payload, &msg); err != nil {
					continue
				}
				if ri, ok := msg["realtimeInput"].(map[string]any); ok {
					if _, hasAudio := ri["audio"]; hasAudio {
						_ = conn.WriteJSON(map[string]any{
							"serverContent": map[string]any{
								"interimInputTranscription": map[string]any{"text": "已经"},
							},
						})
						_ = conn.WriteJSON(map[string]any{
							"serverContent": map[string]any{
								"inputTranscription": map[string]any{"text": "已经完成"},
							},
						})
					}
					if isStreamEnd(ri) {
						// Server VAD already finalized before stop; stay silent.
						for {
							if _, _, rErr := conn.ReadMessage(); rErr != nil {
								return
							}
						}
					}
				}
			}
		}))
		defer srv.Close()

		client := NewGeminiLiveClient()
		client.FinalizeTimeout = 1500 * time.Millisecond
		client.FinalizeGracePeriod = 80 * time.Millisecond

		stopCh := make(chan struct{})
		var stopAt time.Time
		var stopMu sync.Mutex
		go func() {
			time.Sleep(100 * time.Millisecond)
			stopMu.Lock()
			stopAt = time.Now()
			stopMu.Unlock()
			close(stopCh)
		}()

		audio := newSliceAudioSource([][]byte{makePCMChunk(800, ChunkBytes)}, true)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		got, err := client.Run(ctx, audio, stopCh, LiveSessionOptions{
			APIKey:    "k",
			LiveWSURL: "ws" + strings.TrimPrefix(srv.URL, "http"),
			Model:     "gemini-3.5-transcribe-live",
		}, nil)
		elapsed := func() time.Duration {
			stopMu.Lock()
			defer stopMu.Unlock()
			return time.Since(stopAt)
		}()
		if err != nil {
			t.Fatalf("Run error: %v", err)
		}
		if got != "已经完成" {
			t.Fatalf("got = %q, want %q", got, "已经完成")
		}
		if elapsed >= 600*time.Millisecond {
			t.Fatalf("expected settled grace exit (<600ms), took %v", elapsed)
		}
	})

	t.Run("interim_fallback_on_stream_close", func(t *testing.T) {
		upgrader := websocket.Upgrader{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()

			_, _, _ = conn.ReadMessage()
			_ = conn.WriteJSON(map[string]any{"setupComplete": map[string]any{}})

			for {
				_, payload, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var msg map[string]any
				if err := json.Unmarshal(payload, &msg); err != nil {
					continue
				}
				if ri, ok := msg["realtimeInput"].(map[string]any); ok {
					if _, hasAudio := ri["audio"]; hasAudio {
						_ = conn.WriteJSON(map[string]any{
							"serverContent": map[string]any{
								"interimInputTranscription": map[string]any{"text": "未定稿内容"},
							},
						})
					}
					if isStreamEnd(ri) {
						_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
						return
					}
				}
			}
		}))
		defer srv.Close()

		client := NewGeminiLiveClient()
		client.FinalizeTimeout = 1500 * time.Millisecond
		client.FinalizeGracePeriod = 80 * time.Millisecond

		stopCh := make(chan struct{})
		go func() {
			time.Sleep(100 * time.Millisecond)
			close(stopCh)
		}()

		audio := newSliceAudioSource([][]byte{makePCMChunk(800, ChunkBytes)}, true)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		got, err := client.Run(ctx, audio, stopCh, LiveSessionOptions{
			APIKey:    "k",
			LiveWSURL: "ws" + strings.TrimPrefix(srv.URL, "http"),
			Model:     "gemini-3.5-transcribe-live",
		}, nil)
		if err != nil {
			t.Fatalf("Run error: %v", err)
		}
		if got != "未定稿内容" {
			t.Fatalf("got = %q, want %q", got, "未定稿内容")
		}
	})
}

func TestGeminiLiveClient_GeneralLiveModelFallback(t *testing.T) {
	var (
		mu             sync.Mutex
		gotSetup       map[string]any
		gotAudioChunks int
		gotMediaChunks bool
		gotStreamEnd   bool
		gotTurnEnd     bool
	)

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var setup map[string]any
		if err := json.Unmarshal(payload, &setup); err != nil {
			return
		}
		mu.Lock()
		gotSetup = setup
		mu.Unlock()
		_ = conn.WriteJSON(map[string]any{"setupComplete": map[string]any{}})

		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var msg map[string]any
			if err := json.Unmarshal(payload, &msg); err != nil {
				continue
			}

			if ri, ok := msg["realtimeInput"].(map[string]any); ok {
				if _, hasMC := ri["mediaChunks"]; hasMC {
					mu.Lock()
					gotMediaChunks = true
					mu.Unlock()
				}
				if _, ok := ri["audio"].(map[string]any); ok {
					mu.Lock()
					gotAudioChunks++
					n := gotAudioChunks
					mu.Unlock()
					switch n {
					case 1:
						_ = conn.WriteJSON(map[string]any{
							"serverContent": map[string]any{
								"modelTurn": map[string]any{
									"parts": []map[string]any{
										{"text": "thinking...", "thought": true},
										{"text": "今天下午"},
									},
								},
							},
						})
					case 2:
						_ = conn.WriteJSON(map[string]any{
							"serverContent": map[string]any{
								"modelTurn": map[string]any{
									"parts": []map[string]any{
										{"text": "三点开会"},
									},
								},
							},
						})
					}
				}
				if isStreamEnd(ri) {
					mu.Lock()
					gotStreamEnd = true
					mu.Unlock()
				}
			}

			if cc, ok := msg["clientContent"].(map[string]any); ok {
				if tc, ok := cc["turnComplete"].(bool); ok && tc {
					mu.Lock()
					gotTurnEnd = true
					mu.Unlock()
					_ = conn.WriteJSON(map[string]any{
						"serverContent": map[string]any{
							"modelTurn": map[string]any{
								"parts": []map[string]any{{"text": "。"}},
							},
							"turnComplete": true,
						},
					})
					return
				}
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	audio := newSliceAudioSource([][]byte{
		makePCMChunk(800, ChunkBytes),
		makePCMChunk(900, ChunkBytes),
	}, true)

	stopCh := make(chan struct{})
	go func() {
		time.Sleep(150 * time.Millisecond)
		close(stopCh)
	}()

	var partials []string
	var partialMu sync.Mutex
	client := NewGeminiLiveClient()
	client.FinalizeTimeout = 2 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	finalText, err := client.Run(ctx, audio, stopCh, LiveSessionOptions{
		APIKey:       "test-fallback-key",
		LiveWSURL:    wsURL,
		Model:        "gemini-2.5-flash-live",
		Language:     "zh-CN",
		Mode:         "SMART",
		VADSilenceMs: 1200,
	}, func(p string) {
		partialMu.Lock()
		partials = append(partials, p)
		partialMu.Unlock()
	})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if finalText != "今天下午三点开会。" {
		t.Fatalf("finalText = %q, want %q", finalText, "今天下午三点开会。")
	}

	mu.Lock()
	defer mu.Unlock()
	if gotMediaChunks || gotAudioChunks != 2 || !gotStreamEnd || !gotTurnEnd {
		t.Fatalf("mediaChunks=%v chunks=%d streamEnd=%v turnEnd=%v", gotMediaChunks, gotAudioChunks, gotStreamEnd, gotTurnEnd)
	}

	setupObj, _ := gotSetup["setup"].(map[string]any)
	if setupObj["model"] != "models/gemini-2.5-flash-live" {
		t.Fatalf("setup model = %v", setupObj["model"])
	}
	sysInstr, ok := setupObj["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("expected systemInstruction for general live model")
	}
	parts, _ := sysInstr["parts"].([]any)
	firstPart, _ := parts[0].(map[string]any)
	if text, _ := firstPart["text"].(string); !strings.Contains(text, "cmn-Hans-CN") {
		t.Fatalf("expected canonical cmn-Hans-CN in systemInstruction, got %q", text)
	}
	genCfg, _ := setupObj["generationConfig"].(map[string]any)
	if genCfg["temperature"] != float64(0) {
		t.Fatalf("temperature = %v, want 0", genCfg["temperature"])
	}
	iat, _ := setupObj["inputAudioTranscription"].(map[string]any)
	if _, hasMode := iat["mode"]; hasMode {
		t.Fatalf("general live model must omit AudioTranscriptionConfig.mode: %#v", iat)
	}

	partialMu.Lock()
	defer partialMu.Unlock()
	for _, p := range partials {
		if strings.Contains(p, "thinking") {
			t.Fatalf("thought part leaked into partial: %q", p)
		}
	}
}

func TestGeminiLiveClient_ErrorSanitization(t *testing.T) {
	const secret = "super-secret-live-api-key"

	t.Run("setup_phase_error", func(t *testing.T) {
		upgrader := websocket.Upgrader{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()

			_, _, _ = conn.ReadMessage()
			_ = conn.WriteJSON(map[string]any{
				"error": map[string]any{
					"code":    400,
					"status":  "INVALID_ARGUMENT",
					"message": "setup failed for wss://example.com/ws?key=leaked-query-secret with key " + secret,
				},
			})
		}))
		defer srv.Close()

		client := NewGeminiLiveClient()
		audio := newSliceAudioSource(nil, true)
		stopCh := make(chan struct{})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, err := client.Run(ctx, audio, stopCh, LiveSessionOptions{
			APIKey:    secret,
			LiveWSURL: "ws" + strings.TrimPrefix(srv.URL, "http"),
			Model:     "gemini-3.5-transcribe-live",
		}, nil)
		if err == nil {
			t.Fatal("expected error for setup-phase JSON error")
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "leaked-query-secret") {
			t.Fatalf("setup error leaked secret: %v", err)
		}
		if !strings.Contains(err.Error(), "REDACTED") {
			t.Fatalf("expected REDACTED in sanitized setup error, got %v", err)
		}
	})

	t.Run("mid_stream_error", func(t *testing.T) {
		upgrader := websocket.Upgrader{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()

			_, _, _ = conn.ReadMessage()
			_ = conn.WriteJSON(map[string]any{"setupComplete": map[string]any{}})

			for {
				_, payload, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var msg map[string]any
				if err := json.Unmarshal(payload, &msg); err != nil {
					continue
				}
				if ri, ok := msg["realtimeInput"].(map[string]any); ok {
					if _, hasAudio := ri["audio"]; hasAudio {
						_ = conn.WriteJSON(map[string]any{
							"error": map[string]any{
								"code":    500,
								"status":  "INTERNAL",
								"message": "stream failed at wss://example.com/ws?key=leaked-query-secret using " + secret,
							},
						})
						return
					}
				}
			}
		}))
		defer srv.Close()

		client := NewGeminiLiveClient()
		client.FinalizeTimeout = 2 * time.Second
		audio := newSliceAudioSource([][]byte{makePCMChunk(800, ChunkBytes)}, true)
		stopCh := make(chan struct{})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, err := client.Run(ctx, audio, stopCh, LiveSessionOptions{
			APIKey:    secret,
			LiveWSURL: "ws" + strings.TrimPrefix(srv.URL, "http"),
			Model:     "gemini-3.5-transcribe-live",
		}, nil)
		if err == nil {
			t.Fatal("expected error for mid-stream JSON error")
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "leaked-query-secret") {
			t.Fatalf("mid-stream error leaked secret: %v", err)
		}
		if !strings.Contains(err.Error(), "REDACTED") {
			t.Fatalf("expected REDACTED in sanitized mid-stream error, got %v", err)
		}
	})
}

type preRollTestAudioSource struct {
	mu          sync.Mutex
	chunks      [][]byte
	idx         int
	closed      bool
	emittedCh   chan struct{}
	emittedOnce sync.Once
	waitCh      chan struct{}
}

func newPreRollTestAudioSource(chunks [][]byte) *preRollTestAudioSource {
	return &preRollTestAudioSource{
		chunks:    chunks,
		emittedCh: make(chan struct{}),
		waitCh:    make(chan struct{}),
	}
}

func (p *preRollTestAudioSource) ReadChunk() ([]byte, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, io.EOF
	}
	if p.idx < len(p.chunks) {
		c := p.chunks[p.idx]
		p.idx++
		p.mu.Unlock()
		return c, nil
	}
	p.mu.Unlock()

	p.emittedOnce.Do(func() {
		close(p.emittedCh)
	})
	<-p.waitCh
	return nil, io.EOF
}

func (p *preRollTestAudioSource) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		close(p.waitCh)
	}
	return nil
}

func TestGeminiLiveClient_PreRollBuffersAudioDuringSlowConnect(t *testing.T) {
	chunk1 := makePCMChunk(800, ChunkBytes)
	chunk2 := makePCMChunk(900, ChunkBytes)

	var (
		mu               sync.Mutex
		handshakeDoneAt  time.Time
		gotAudioPayloads [][]byte
		gotStreamEnd     bool
	)

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate slow TLS/WS connect + setup handshake (~200ms total).
		time.Sleep(120 * time.Millisecond)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		_, _, err = conn.ReadMessage()
		if err != nil {
			return
		}
		time.Sleep(80 * time.Millisecond)
		mu.Lock()
		handshakeDoneAt = time.Now()
		mu.Unlock()
		_ = conn.WriteJSON(map[string]any{"setupComplete": map[string]any{}})

		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var msg map[string]any
			if err := json.Unmarshal(payload, &msg); err != nil {
				continue
			}
			ri, ok := msg["realtimeInput"].(map[string]any)
			if !ok {
				continue
			}
			if audioObj, ok := ri["audio"].(map[string]any); ok {
				dataStr, _ := audioObj["data"].(string)
				decoded, decErr := base64.StdEncoding.DecodeString(dataStr)
				if decErr == nil {
					mu.Lock()
					gotAudioPayloads = append(gotAudioPayloads, decoded)
					mu.Unlock()
				}
			}
			if isStreamEnd(ri) {
				mu.Lock()
				gotStreamEnd = true
				mu.Unlock()
				_ = conn.WriteJSON(map[string]any{
					"serverContent": map[string]any{
						"inputTranscription": map[string]any{"text": "预录音测试成功"},
						"turnComplete":       true,
					},
				})
				return
			}
		}
	}))
	defer srv.Close()

	audio := newPreRollTestAudioSource([][]byte{chunk1, chunk2})
	stopCh := make(chan struct{})
	stopDoneCh := make(chan struct{})
	var stoppedAt time.Time

	go func() {
		defer close(stopDoneCh)
		// Wait until BufferedAudioSource has captured both chunks into its pre-roll buffer,
		// then close stopCh BEFORE the 200ms WebSocket handshake completes.
		<-audio.emittedCh
		stoppedAt = time.Now()
		close(stopCh)
	}()

	client := NewGeminiLiveClient()
	client.FinalizeTimeout = 2 * time.Second
	client.FinalizeGracePeriod = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	finalText, err := client.Run(ctx, audio, stopCh, LiveSessionOptions{
		APIKey:       "test-preroll-key",
		LiveWSURL:    "ws" + strings.TrimPrefix(srv.URL, "http"),
		Model:        "gemini-3.5-transcribe-live",
		Language:     "zh-CN",
		Mode:         "VERBATIM",
		VADSilenceMs: 1200,
	}, nil)
	<-stopDoneCh
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if finalText != "预录音测试成功" {
		t.Fatalf("finalText = %q, want %q", finalText, "预录音测试成功")
	}

	mu.Lock()
	defer mu.Unlock()
	if !stoppedAt.Before(handshakeDoneAt) {
		t.Fatalf("expected stopCh to close before handshake completed (stoppedAt=%v, handshakeDoneAt=%v)", stoppedAt, handshakeDoneAt)
	}
	if !gotStreamEnd {
		t.Fatal("expected audioStreamEnd to be sent after draining pre-rolled chunks")
	}
	if len(gotAudioPayloads) != 2 || !bytes.Equal(gotAudioPayloads[0], chunk1) || !bytes.Equal(gotAudioPayloads[1], chunk2) {
		t.Fatalf("expected both pre-rolled chunks to be delivered, got %d payloads", len(gotAudioPayloads))
	}
}

func TestGeminiLiveClient_VADAutoStopDrainsBufferedTailAudio(t *testing.T) {
	// The server reports end-of-speech (voiceActivity ACTIVITY_END) as soon as
	// it sees the first chunk, while the remaining chunks are still sitting in
	// the BufferedAudioSource queue. They must be drained to the WebSocket
	// before audioStreamEnd.
	chunks := [][]byte{makePCMChunk(800, ChunkBytes)}
	for i := 0; i < 5; i++ {
		chunks = append(chunks, makePCMChunk(0, ChunkBytes))
	}

	var (
		mu               sync.Mutex
		gotAudioPayloads [][]byte
		gotStreamEnd     bool
		payloadsAtEnd    int
	)

	audio := newPreRollTestAudioSource(chunks)

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		_, _, err = conn.ReadMessage()
		if err != nil {
			return
		}
		// Hold setupComplete until every chunk is already buffered, so the
		// VAD trigger below deterministically leaves tail chunks queued.
		<-audio.emittedCh
		_ = conn.WriteJSON(map[string]any{"setupComplete": map[string]any{}})

		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var msg map[string]any
			if err := json.Unmarshal(payload, &msg); err != nil {
				continue
			}
			ri, ok := msg["realtimeInput"].(map[string]any)
			if !ok {
				continue
			}
			if audioObj, ok := ri["audio"].(map[string]any); ok {
				dataStr, _ := audioObj["data"].(string)
				decoded, decErr := base64.StdEncoding.DecodeString(dataStr)
				if decErr == nil {
					mu.Lock()
					gotAudioPayloads = append(gotAudioPayloads, decoded)
					n := len(gotAudioPayloads)
					mu.Unlock()
					if n == 1 {
						_ = conn.WriteJSON(map[string]any{"voiceActivity": map[string]any{"voiceActivityType": "ACTIVITY_END"}})
					}
				}
			}
			if isStreamEnd(ri) {
				mu.Lock()
				gotStreamEnd = true
				payloadsAtEnd = len(gotAudioPayloads)
				mu.Unlock()
				_ = conn.WriteJSON(map[string]any{
					"serverContent": map[string]any{
						"inputTranscription": map[string]any{"text": "尾部音频已送达"},
						"turnComplete":       true,
					},
				})
				return
			}
		}
	}))
	defer srv.Close()

	// stopCh never closes: the session must end via server VAD auto-stop alone.
	stopCh := make(chan struct{})

	client := NewGeminiLiveClient()
	client.FinalizeTimeout = 2 * time.Second
	client.FinalizeGracePeriod = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	finalText, err := client.Run(ctx, audio, stopCh, LiveSessionOptions{
		APIKey:       "test-vad-tail-key",
		LiveWSURL:    "ws" + strings.TrimPrefix(srv.URL, "http"),
		Model:        "gemini-3.5-transcribe-live",
		Mode:         "VERBATIM",
		VADSilenceMs: 300,
	}, nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if finalText != "尾部音频已送达" {
		t.Fatalf("finalText = %q, want %q", finalText, "尾部音频已送达")
	}

	mu.Lock()
	defer mu.Unlock()
	if !gotStreamEnd {
		t.Fatal("expected audioStreamEnd to be sent after VAD auto-stop")
	}
	if payloadsAtEnd != len(chunks) {
		t.Fatalf("audioStreamEnd arrived after %d payloads, want %d (buffered tail chunks must be drained first)", payloadsAtEnd, len(chunks))
	}
	for i, want := range chunks {
		if i >= len(gotAudioPayloads) || !bytes.Equal(gotAudioPayloads[i], want) {
			t.Fatalf("payload %d mismatch: got %d bytes, want %d bytes", i, len(gotAudioPayloads[i]), len(want))
		}
	}
}
