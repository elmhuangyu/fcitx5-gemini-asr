package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestListRemoteModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "secret-list-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v1beta/models" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{
					"name":                       "models/gemini-2.5-pro",
					"displayName":                "Gemini 2.5 Pro",
					"supportedGenerationMethods": []string{"generateContent"},
				},
				{
					"name":                       "models/text-embedding-004",
					"displayName":                "Text Embedding 004",
					"supportedGenerationMethods": []string{"embedContent"},
				},
				{
					"name":                       "models/gemini-3.5-transcribe-live",
					"displayName":                "Gemini 3.5 Transcribe Live",
					"supportedGenerationMethods": []string{"bidiGenerateContent"},
				},
				{
					"name":                       "models/gemini-2.5-flash",
					"displayName":                "Gemini 2.5 Flash",
					"supportedGenerationMethods": []string{"generateContent", "bidiGenerateContent"},
				},
			},
		})
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Connection.APIBaseURL = srv.URL

	models, err := ListRemoteModels(context.Background(), cfg, "secret-list-key")
	if err != nil {
		t.Fatalf("ListRemoteModels: %v", err)
	}
	if len(models) != 3 {
		t.Fatalf("got %d models, want 3: %+v", len(models), models)
	}
	// Sorted alphabetically by short name.
	if models[0].Name != "gemini-2.5-flash" || !models[0].SupportsLive || !models[0].SupportsGenerate {
		t.Fatalf("unexpected model[0]: %+v", models[0])
	}
	if models[1].Name != "gemini-2.5-pro" || models[1].SupportsLive || !models[1].SupportsGenerate {
		t.Fatalf("unexpected model[1]: %+v", models[1])
	}
	if models[2].Name != "gemini-3.5-transcribe-live" || !models[2].SupportsLive || models[2].SupportsGenerate {
		t.Fatalf("unexpected model[2]: %+v", models[2])
	}

	// Verify API error redacts secret if returned in body.
	_, err = ListRemoteModels(context.Background(), cfg, "wrong-key-secret")
	if err == nil || strings.Contains(err.Error(), "wrong-key-secret") {
		t.Fatalf("expected sanitized error, got %v", err)
	}
}

func TestListRemoteModels_PaginationDeduplicationAndEscaping(t *testing.T) {
	const specialToken = "tok+with/slash=equals=="
	var pageCalls atomic.Int32
	var sawEscapedQuery atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("pageSize") != "200" {
			http.Error(w, "bad pageSize", http.StatusBadRequest)
			return
		}
		call := pageCalls.Add(1)
		tok := r.URL.Query().Get("pageToken")
		switch call {
		case 1:
			if tok != "" {
				http.Error(w, "expected empty initial pageToken", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{
					{
						"name":                       "models/gemini-2.5-pro",
						"displayName":                "Gemini 2.5 Pro",
						"supportedGenerationMethods": []string{"generateContent"},
					},
					{
						"name":                       "models/text-embedding-004",
						"displayName":                "Text Embedding",
						"supportedGenerationMethods": []string{"embedContent"},
					},
				},
				"nextPageToken": specialToken,
			})
		case 2:
			if tok != specialToken {
				http.Error(w, "wrong pageToken: "+tok, http.StatusBadRequest)
				return
			}
			if strings.Contains(r.URL.RawQuery, "tok%2Bwith%2Fslash%3Dequals%3D%3D") {
				sawEscapedQuery.Store(true)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{
					{
						"name":                       "models/gemini-3.5-transcribe-live",
						"displayName":                "Gemini 3.5 Transcribe Live",
						"supportedGenerationMethods": []string{"bidiGenerateContent"},
					},
					{
						// Duplicate across pages must be deduplicated.
						"name":                       "models/gemini-2.5-pro",
						"displayName":                "Gemini 2.5 Pro Duplicate",
						"supportedGenerationMethods": []string{"generateContent"},
					},
				},
				"nextPageToken": "page-3",
			})
		case 3:
			if tok != "page-3" {
				http.Error(w, "wrong pageToken page 3: "+tok, http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{
					{
						"name":                       "models/gemini-2.5-flash",
						"displayName":                "Gemini 2.5 Flash",
						"supportedGenerationMethods": []string{"generateContent", "bidiGenerateContent"},
					},
				},
			})
		default:
			http.Error(w, "too many pages requested", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Connection.APIBaseURL = srv.URL

	models, err := ListRemoteModels(context.Background(), cfg, "list-key")
	if err != nil {
		t.Fatalf("ListRemoteModels: %v", err)
	}
	if got := pageCalls.Load(); got != 3 {
		t.Fatalf("pageCalls = %d, want 3", got)
	}
	if !sawEscapedQuery.Load() {
		t.Fatal("expected nextPageToken special characters (+, /, =) to be URL-escaped in query")
	}
	if len(models) != 3 {
		t.Fatalf("got %d models, want 3: %+v", len(models), models)
	}
	if models[0].Name != "gemini-2.5-flash" || models[1].Name != "gemini-2.5-pro" || models[2].Name != "gemini-3.5-transcribe-live" {
		t.Fatalf("unexpected sorted models: %+v", models)
	}
	if models[1].DisplayName != "Gemini 2.5 Pro" {
		t.Fatalf("expected first occurrence preserved on duplicate, got %+v", models[1])
	}
}

func TestListRemoteModels_LoopGuard(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{
					"name":                       "models/gemini-2.5-flash",
					"displayName":                "Gemini 2.5 Flash",
					"supportedGenerationMethods": []string{"generateContent"},
				},
			},
			"nextPageToken": "repeating-token",
		})
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Connection.APIBaseURL = srv.URL

	models, err := ListRemoteModels(context.Background(), cfg, "list-key")
	if err != nil {
		t.Fatalf("ListRemoteModels: %v", err)
	}
	if len(models) != 1 || models[0].Name != "gemini-2.5-flash" {
		t.Fatalf("unexpected models: %+v", models)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected loop guard to stop after 2 requests, got %d", got)
	}
}

func TestListRemoteModels_HTTP200ErrorSanitized(t *testing.T) {
	const secret = "super-secret-models-key"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    403,
				"status":  "PERMISSION_DENIED",
				"message": "denied at https://example.com/v1beta/models?key=leaked-url-token using " + secret,
			},
		})
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Connection.APIBaseURL = srv.URL

	_, err := ListRemoteModels(context.Background(), cfg, secret)
	if err == nil {
		t.Fatal("expected error for HTTP 200 JSON error body")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "leaked-url-token") {
		t.Fatalf("error leaked secret: %v", err)
	}
	if !strings.Contains(err.Error(), "REDACTED") {
		t.Fatalf("expected REDACTED in sanitized error, got %v", err)
	}
}
