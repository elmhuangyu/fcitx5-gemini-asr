package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPostProcessor_GenerateAndPrefetch(t *testing.T) {
	var (
		mu       sync.Mutex
		calls    []string
		hitCount atomic.Int32
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "secret-pp-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/v1beta/models/gemini-3.5-flash-lite:generateContent") {
			http.Error(w, "wrong path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		var req generateContentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		promptText := req.Contents[0].Parts[0].Text
		hitCount.Add(1)
		mu.Lock()
		calls = append(calls, promptText)
		mu.Unlock()

		// Slight delay to exercise in-flight singleflight coalescing.
		time.Sleep(40 * time.Millisecond)

		var out string
		switch {
		case strings.Contains(promptText, "正式"):
			out = "今天下午三点我们将召开架构评审会议。"
		case strings.Contains(promptText, "错别字"):
			out = "今天下午三点，我们开个会讨论一下架构。"
		case strings.Contains(promptText, "English"):
			out = "We will meet at 3 PM today to discuss the architecture."
		default:
			out = "processed"
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{
				{
					"content": map[string]any{
						"parts": []map[string]any{
							{"text": "hidden thought", "thought": true},
							{"text": out},
						},
					},
				},
			},
		})
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Connection.APIBaseURL = srv.URL

	pp := NewPostProcessor()
	session := NewPrefetchSession()
	fn := func(ctx context.Context, action, rawText string) (string, error) {
		return pp.Generate(ctx, cfg, "secret-pp-key", action, rawText)
	}

	raw := "今天下午三点我们开个会讨论一下架构"
	session.StartPrefetch(context.Background(), raw, []string{"refine_fix_typos", "refine_formal"}, fn)

	// Immediate resolve while refine_formal is still in-flight should coalesce (no duplicate HTTP call).
	resFormal, err := session.Resolve(context.Background(), "refine_formal", fn)
	if err != nil {
		t.Fatalf("Resolve refine_formal: %v", err)
	}
	if resFormal != "今天下午三点我们将召开架构评审会议。" {
		t.Fatalf("unexpected refine_formal result: %q", resFormal)
	}

	// Second resolve of cached item must hit cache with zero extra HTTP requests.
	beforeHits := hitCount.Load()
	resTypos, err := session.Resolve(context.Background(), "refine_fix_typos", fn)
	if err != nil {
		t.Fatalf("Resolve refine_fix_typos: %v", err)
	}
	if resTypos != "今天下午三点，我们开个会讨论一下架构。" {
		t.Fatalf("unexpected refine_fix_typos result: %q", resTypos)
	}
	if hitCount.Load() != beforeHits {
		t.Fatalf("expected cache hit with 0 new HTTP calls, got %d -> %d", beforeHits, hitCount.Load())
	}

	// Non-prefetched action runs on demand.
	resEn, err := session.Resolve(context.Background(), "translate_en", fn)
	if err != nil {
		t.Fatalf("Resolve translate_en: %v", err)
	}
	if resEn != "We will meet at 3 PM today to discuss the architecture." {
		t.Fatalf("unexpected translate_en result: %q", resEn)
	}

	if got := hitCount.Load(); got != 3 {
		t.Fatalf("total HTTP calls = %d, want 3 (2 prefetch + 1 on-demand)", got)
	}

	// Unknown action fails closed.
	if _, err := pp.Generate(context.Background(), cfg, "secret-pp-key", "does_not_exist", raw); err == nil {
		t.Fatal("expected error for unknown action")
	}
}

func TestPostProcessor_PerActionModelOverride(t *testing.T) {
	var (
		mu    sync.Mutex
		paths []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{
				{
					"content": map[string]any{
						"parts": []map[string]any{{"text": "ok"}},
					},
				},
			},
		})
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Connection.APIBaseURL = srv.URL
	cfg.PostProcess.Model = "gemini-2.5-flash"
	cfg.PostProcess.Models = map[string]string{
		"as_prompt": "gemini-2.5-pro",
	}

	pp := NewPostProcessor()
	if _, err := pp.Generate(context.Background(), cfg, "k", "as_prompt", "make a cli"); err != nil {
		t.Fatalf("Generate as_prompt: %v", err)
	}
	if _, err := pp.Generate(context.Background(), cfg, "k", "refine_fix_typos", "hello"); err != nil {
		t.Fatalf("Generate refine_fix_typos: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 2 {
		t.Fatalf("got %d requests, want 2", len(paths))
	}
	if paths[0] != "/v1beta/models/gemini-2.5-pro:generateContent" {
		t.Fatalf("as_prompt hit %q", paths[0])
	}
	if paths[1] != "/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("refine_fix_typos hit %q", paths[1])
	}
}

func TestPostProcessor_HTTP200ErrorSanitized(t *testing.T) {
	const secret = "super-secret-postprocess-key"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    400,
				"status":  "INVALID_ARGUMENT",
				"message": "request to https://example.com/v1beta?key=leaked-query-secret failed for key " + secret,
			},
		})
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Connection.APIBaseURL = srv.URL

	pp := NewPostProcessor()
	_, err := pp.Generate(context.Background(), cfg, secret, "refine_fix_typos", "hello world")
	if err == nil {
		t.Fatal("expected error for HTTP 200 JSON error body")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "leaked-query-secret") {
		t.Fatalf("error leaked secret: %v", err)
	}
	if !strings.Contains(err.Error(), "REDACTED") {
		t.Fatalf("expected REDACTED in sanitized error, got %v", err)
	}
}
