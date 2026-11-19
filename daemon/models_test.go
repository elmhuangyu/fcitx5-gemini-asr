package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
