package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestGeminiLiveClient_SetupStreamAndStop(t *testing.T) {
	var (
		mu             sync.Mutex
		gotAPIKey      string
		gotSetup       map[string]any
		gotAudioChunks int
		gotStreamEnd   bool
		gotTurnEnd     bool
	)

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAPIKey = r.URL.Query().Get("key")
		mu.Unlock()

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		// 1. Read setup frame
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
				if mc, ok := ri["mediaChunks"].([]any); ok && len(mc) > 0 {
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
								"inputTranscription": map[string]any{
									"text": "三点开会",
								},
							},
						})
					}
				}
				if end, ok := ri["audioStreamEnd"].(bool); ok && end {
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
	var partials []string
	var partialMu sync.Mutex

	client := NewGeminiLiveClient()
	client.FinalizeTimeout = 2 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		time.Sleep(150 * time.Millisecond)
		close(stopCh)
	}()

	finalText, err := client.Run(ctx, audio, stopCh, LiveSessionOptions{
		APIKey:       "test-live-key",
		LiveWSURL:    wsURL,
		Model:        "gemini-3.5-transcribe-live",
		Language:     "zh-CN",
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
	if gotAPIKey != "test-live-key" {
		t.Fatalf("key = %q", gotAPIKey)
	}
	if gotAudioChunks != 2 || !gotStreamEnd || !gotTurnEnd {
		t.Fatalf("chunks=%d streamEnd=%v turnEnd=%v", gotAudioChunks, gotStreamEnd, gotTurnEnd)
	}

	setupObj, _ := gotSetup["setup"].(map[string]any)
	if setupObj["model"] != "models/gemini-3.5-transcribe-live" {
		t.Fatalf("setup model = %v", setupObj["model"])
	}
	genCfg, _ := setupObj["generationConfig"].(map[string]any)
	if genCfg["temperature"] != float64(0) {
		t.Fatalf("temperature = %v", genCfg["temperature"])
	}
	mods, _ := genCfg["responseModalities"].([]any)
	if len(mods) != 1 || mods[0] != "TEXT" {
		t.Fatalf("responseModalities = %v", mods)
	}

	partialMu.Lock()
	defer partialMu.Unlock()
	if len(partials) < 2 || partials[0] != "今天下午" {
		t.Fatalf("unexpected partials: %#v", partials)
	}
	// Verify thought text never leaked into partials.
	for _, p := range partials {
		if strings.Contains(p, "thinking") {
			t.Fatalf("thought part leaked into partial: %q", p)
		}
	}
	_ = base64.StdEncoding // keep import usage clear
}
