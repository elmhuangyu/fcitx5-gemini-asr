package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// liveRecorderServer runs a fake Live API that records the setup frame and the
// kinds of realtimeInput frames it receives. onFrame may reply on the conn.
type liveRecorderServer struct {
	*httptest.Server
	mu     sync.Mutex
	setup  map[string]any
	frames []string
}

func newLiveRecorderServer(t *testing.T, onFrame func(conn *websocket.Conn, kind string, n int)) *liveRecorderServer {
	t.Helper()
	rs := &liveRecorderServer{}
	upgrader := websocket.Upgrader{}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		_ = json.Unmarshal(payload, &setup)
		rs.mu.Lock()
		rs.setup = setup
		rs.mu.Unlock()
		_ = conn.WriteJSON(map[string]any{"setupComplete": map[string]any{}})
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var msg map[string]any
			if json.Unmarshal(payload, &msg) != nil {
				continue
			}
			ri, ok := msg["realtimeInput"].(map[string]any)
			if !ok {
				continue
			}
			kind := ""
			for _, k := range []string{"audio", "activityStart", "activityEnd", "audioStreamEnd"} {
				if _, has := ri[k]; has {
					kind = k
				}
			}
			rs.mu.Lock()
			rs.frames = append(rs.frames, kind)
			n := len(rs.frames)
			rs.mu.Unlock()
			if onFrame != nil {
				onFrame(conn, kind, n)
			}
		}
	}))
	t.Cleanup(rs.Close)
	return rs
}

func (rs *liveRecorderServer) wsURL() string { return "ws" + strings.TrimPrefix(rs.URL, "http") }

func (rs *liveRecorderServer) activityConfig(t *testing.T) map[string]any {
	t.Helper()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	setupObj, _ := rs.setup["setup"].(map[string]any)
	ric, _ := setupObj["realtimeInputConfig"].(map[string]any)
	aad, _ := ric["automaticActivityDetection"].(map[string]any)
	if aad == nil {
		t.Fatalf("missing realtimeInputConfig.automaticActivityDetection in setup: %#v", setupObj)
	}
	return aad
}

func TestGeminiLiveClient_ServerVADConfigAndTurnCompleteFallback(t *testing.T) {
	// No explicit activity events: the server's turnComplete after speech must
	// still end capture (stopCh never closes).
	rs := newLiveRecorderServer(t, func(conn *websocket.Conn, kind string, n int) {
		switch kind {
		case "audio":
			if n == 1 {
				_ = conn.WriteJSON(map[string]any{"serverContent": map[string]any{
					"inputTranscription": map[string]any{"text": "你好"},
					"turnComplete":       true,
				}})
			}
		case "audioStreamEnd":
			_ = conn.WriteJSON(map[string]any{"serverContent": map[string]any{"turnComplete": true}})
		}
	})

	client := NewGeminiLiveClient()
	client.FinalizeTimeout = 2 * time.Second
	client.FinalizeGracePeriod = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	audio := newSliceAudioSource([][]byte{makePCMChunk(800, ChunkBytes)}, true)
	got, err := client.Run(ctx, audio, make(chan struct{}), LiveSessionOptions{
		APIKey: "k", LiveWSURL: rs.wsURL(), Model: "gemini-3.5-transcribe-live", VADSilenceMs: 900,
	}, nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if got != "你好" {
		t.Fatalf("got %q", got)
	}
	aad := rs.activityConfig(t)
	if aad["silenceDurationMs"] != float64(900) {
		t.Fatalf("silenceDurationMs = %v, want 900", aad["silenceDurationMs"])
	}
	if aad["disabled"] == true {
		t.Fatalf("automatic activity detection must be enabled: %#v", aad)
	}
	if aad["startOfSpeechSensitivity"] == nil || aad["endOfSpeechSensitivity"] == nil {
		t.Fatalf("expected start/end sensitivity: %#v", aad)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if last := rs.frames[len(rs.frames)-1]; last != "audioStreamEnd" {
		t.Fatalf("frames = %v, want trailing audioStreamEnd", rs.frames)
	}
}

func TestGeminiLiveClient_ManualModeDisablesServerVAD(t *testing.T) {
	rs := newLiveRecorderServer(t, func(conn *websocket.Conn, kind string, n int) {
		if kind == "activityEnd" {
			_ = conn.WriteJSON(map[string]any{"serverContent": map[string]any{
				"inputTranscription": map[string]any{"text": "manual"},
				"turnComplete":       true,
			}})
		}
	})

	client := NewGeminiLiveClient()
	client.FinalizeTimeout = 2 * time.Second
	client.FinalizeGracePeriod = 100 * time.Millisecond
	stopCh := make(chan struct{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(stopCh)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	audio := newSliceAudioSource([][]byte{makePCMChunk(800, ChunkBytes)}, true)
	got, err := client.Run(ctx, audio, stopCh, LiveSessionOptions{
		APIKey: "k", LiveWSURL: rs.wsURL(), Model: "gemini-3.5-transcribe-live", VADSilenceMs: 0,
	}, nil)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if got != "manual" {
		t.Fatalf("got %q", got)
	}
	if aad := rs.activityConfig(t); aad["disabled"] != true {
		t.Fatalf("vad_silence_ms=0 must disable automatic activity detection: %#v", aad)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	want := []string{"activityStart", "audio", "activityEnd"}
	if !reflect.DeepEqual(rs.frames, want) {
		t.Fatalf("frames = %v, want %v", rs.frames, want)
	}
}
