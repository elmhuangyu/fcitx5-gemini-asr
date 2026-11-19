package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

type RemoteModelInfo struct {
	Name             string
	DisplayName      string
	SupportsLive     bool
	SupportsGenerate bool
}

type listModelsResponse struct {
	Models []struct {
		Name                       string   `json:"name"`
		DisplayName                string   `json:"displayName"`
		SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
	} `json:"models"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// ListRemoteModels queries Gemini /v1beta/models and returns Gemini models supporting Live or generateContent.
func ListRemoteModels(ctx context.Context, cfg Config, apiKey string) ([]RemoteModelInfo, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.Connection.APIBaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultAPIBaseURL
	}
	endpoint := fmt.Sprintf("%s/v1beta/models?pageSize=200", baseURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build models request: %s", SanitizeError(err, apiKey))
	}
	req.Header.Set("x-goog-api-key", apiKey)

	client, err := BuildHTTPClient(cfg.Connection.Proxy, 20*time.Second)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list models HTTP request failed: %s", SanitizeError(err, apiKey))
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read models response: %s", SanitizeError(err, apiKey))
	}

	var parsed listModelsResponse
	_ = json.Unmarshal(body, &parsed)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if parsed.Error != nil && parsed.Error.Message != "" {
			return nil, fmt.Errorf("gemini API error (%d %s): %s", resp.StatusCode, parsed.Error.Status, SanitizeError(errors.New(parsed.Error.Message), apiKey))
		}
		return nil, fmt.Errorf("gemini API HTTP %d: %s", resp.StatusCode, SanitizeError(errors.New(strings.TrimSpace(string(body))), apiKey))
	}

	var models []RemoteModelInfo
	for _, m := range parsed.Models {
		shortName := strings.TrimPrefix(m.Name, "models/")
		if !strings.Contains(strings.ToLower(shortName), "gemini") {
			continue
		}
		var live, gen bool
		for _, method := range m.SupportedGenerationMethods {
			switch method {
			case "bidiGenerateContent":
				live = true
			case "generateContent":
				gen = true
			}
		}
		if !live && !gen {
			continue
		}
		models = append(models, RemoteModelInfo{
			Name:             shortName,
			DisplayName:      m.DisplayName,
			SupportsLive:     live,
			SupportsGenerate: gen,
		})
	}

	sort.Slice(models, func(i, j int) bool {
		return models[i].Name < models[j].Name
	})
	return models, nil
}
