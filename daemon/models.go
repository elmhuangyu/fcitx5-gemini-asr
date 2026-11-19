package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const maxModelListPages = 50

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
	NextPageToken string `json:"nextPageToken"`
	Error         *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func fetchModelsPage(ctx context.Context, client *http.Client, baseURL, pageToken, apiKey string) (listModelsResponse, error) {
	u, err := url.Parse(baseURL + "/v1beta/models")
	if err != nil {
		return listModelsResponse{}, fmt.Errorf("parse models URL: %s", SanitizeError(err, apiKey))
	}
	q := u.Query()
	q.Set("pageSize", "200")
	if pageToken != "" {
		q.Set("pageToken", pageToken)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return listModelsResponse{}, fmt.Errorf("build models request: %s", SanitizeError(err, apiKey))
	}
	req.Header.Set("x-goog-api-key", apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return listModelsResponse{}, fmt.Errorf("list models HTTP request failed: %s", SanitizeError(err, apiKey))
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return listModelsResponse{}, fmt.Errorf("read models response: %s", SanitizeError(err, apiKey))
	}

	var parsed listModelsResponse
	unmarshalErr := json.Unmarshal(body, &parsed)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if unmarshalErr == nil && parsed.Error != nil && parsed.Error.Message != "" {
			return listModelsResponse{}, errors.New(SanitizeError(fmt.Errorf("gemini API error (%d %s): %s", resp.StatusCode, parsed.Error.Status, parsed.Error.Message), apiKey))
		}
		return listModelsResponse{}, fmt.Errorf("gemini API HTTP %d: %s", resp.StatusCode, SanitizeError(errors.New(strings.TrimSpace(string(body))), apiKey))
	}

	if unmarshalErr != nil {
		return listModelsResponse{}, fmt.Errorf("decode models response: %s", SanitizeError(unmarshalErr, apiKey))
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return listModelsResponse{}, errors.New(SanitizeError(fmt.Errorf("gemini API error (%d %s): %s", parsed.Error.Code, parsed.Error.Status, parsed.Error.Message), apiKey))
	}
	return parsed, nil
}

// ListRemoteModels queries Gemini /v1beta/models and returns Gemini models supporting Live or generateContent.
func ListRemoteModels(ctx context.Context, cfg Config, apiKey string) ([]RemoteModelInfo, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.Connection.APIBaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultAPIBaseURL
	}

	client, err := BuildHTTPClient(cfg.Connection.Proxy, 20*time.Second)
	if err != nil {
		return nil, err
	}

	var (
		models     []RemoteModelInfo
		pageToken  string
		seenTokens = make(map[string]struct{})
		seenModels = make(map[string]struct{})
	)

	for page := 0; page < maxModelListPages; page++ {
		parsed, err := fetchModelsPage(ctx, client, baseURL, pageToken, apiKey)
		if err != nil {
			return nil, err
		}

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
			if _, dup := seenModels[shortName]; dup {
				continue
			}
			seenModels[shortName] = struct{}{}
			models = append(models, RemoteModelInfo{
				Name:             shortName,
				DisplayName:      m.DisplayName,
				SupportsLive:     live,
				SupportsGenerate: gen,
			})
		}

		if parsed.NextPageToken == "" {
			break
		}
		if _, seen := seenTokens[parsed.NextPageToken]; seen {
			break
		}
		seenTokens[parsed.NextPageToken] = struct{}{}
		pageToken = parsed.NextPageToken
	}

	sort.Slice(models, func(i, j int) bool {
		return models[i].Name < models[j].Name
	})
	return models, nil
}
