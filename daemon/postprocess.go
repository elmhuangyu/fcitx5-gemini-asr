package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const DefaultAPIBaseURL = "https://generativelanguage.googleapis.com"

type PostProcessFunc func(ctx context.Context, action, rawText string) (string, error)

type PostProcessor struct {
	mu      sync.Mutex
	clients map[string]*http.Client
}

func NewPostProcessor() *PostProcessor {
	return &PostProcessor{
		clients: make(map[string]*http.Client),
	}
}

func (p *PostProcessor) clientForProxy(proxyAddr string) (*http.Client, error) {
	key := strings.TrimSpace(proxyAddr)
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[key]; ok {
		return c, nil
	}
	c, err := BuildHTTPClient(key, 30*time.Second)
	if err != nil {
		return nil, err
	}
	p.clients[key] = c
	return c, nil
}

type generateContentRequest struct {
	Contents         []generateContentItem  `json:"contents"`
	GenerationConfig *generateContentConfig `json:"generationConfig,omitempty"`
}

type generateContentItem struct {
	Role  string                `json:"role,omitempty"`
	Parts []generateContentPart `json:"parts"`
}

type generateContentPart struct {
	Text string `json:"text"`
}

type generateContentConfig struct {
	Temperature float64 `json:"temperature"`
}

type generateContentResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text    string `json:"text"`
				Thought bool   `json:"thought"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func (p *PostProcessor) Generate(ctx context.Context, cfg Config, apiKey, action, rawText string) (string, error) {
	prompt, ok := cfg.Prompts[action]
	if !ok || strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("unknown or empty post_process action %q", action)
	}
	if strings.TrimSpace(rawText) == "" {
		return "", errors.New("raw_text is empty")
	}
	return p.GenerateRaw(ctx, cfg, apiKey, cfg.ModelForAction(action), prompt+"\n\n"+rawText, 0.2)
}

// GenerateRaw sends a single text prompt to the Gemini generateContent API using the given model.
func (p *PostProcessor) GenerateRaw(ctx context.Context, cfg Config, apiKey, model, userText string, temperature float64) (string, error) {
	if strings.TrimSpace(userText) == "" {
		return "", errors.New("prompt text is empty")
	}

	baseURL := strings.TrimRight(strings.TrimSpace(cfg.Connection.APIBaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultAPIBaseURL
	}
	trimmedModel := strings.TrimPrefix(strings.TrimSpace(model), "models/")
	if trimmedModel == "" {
		trimmedModel = DefaultPostProcessModel
	}

	endpoint := fmt.Sprintf("%s/v1beta/models/%s:generateContent", baseURL, trimmedModel)

	reqPayload := generateContentRequest{
		Contents: []generateContentItem{
			{
				Role: "user",
				Parts: []generateContentPart{
					{Text: userText},
				},
			},
		},
		GenerationConfig: &generateContentConfig{
			Temperature: temperature,
		},
	}

	bodyBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return "", fmt.Errorf("marshal generateContent request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("build generateContent request: %s", SanitizeError(err, apiKey))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", apiKey)

	client, err := p.clientForProxy(cfg.Connection.Proxy)
	if err != nil {
		return "", err
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("generateContent HTTP request failed: %s", SanitizeError(err, apiKey))
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("read generateContent response: %s", SanitizeError(err, apiKey))
	}

	var parsed generateContentResponse
	_ = json.Unmarshal(respBytes, &parsed)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if parsed.Error != nil && parsed.Error.Message != "" {
			return "", fmt.Errorf("gemini API error (%d %s): %s", resp.StatusCode, parsed.Error.Status, SanitizeError(errors.New(parsed.Error.Message), apiKey))
		}
		return "", fmt.Errorf("gemini API HTTP %d: %s", resp.StatusCode, SanitizeError(errors.New(strings.TrimSpace(string(respBytes))), apiKey))
	}

	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", errors.New(SanitizeError(fmt.Errorf("gemini API error (%d %s): %s", parsed.Error.Code, parsed.Error.Status, parsed.Error.Message), apiKey))
	}
	if len(parsed.Candidates) == 0 {
		return "", errors.New("gemini API returned no candidates")
	}

	var out strings.Builder
	for _, part := range parsed.Candidates[0].Content.Parts {
		if part.Thought || part.Text == "" {
			continue
		}
		out.WriteString(part.Text)
	}
	result := strings.TrimSpace(out.String())
	if result == "" {
		return "", errors.New("gemini API returned empty post-processed text")
	}
	return result, nil
}

type prefetchEntry struct {
	done chan struct{}
	text string
	err  error
}

type PrefetchSession struct {
	mu      sync.Mutex
	rawText string
	cancel  context.CancelFunc
	entries map[string]*prefetchEntry
}

func NewPrefetchSession() *PrefetchSession {
	return &PrefetchSession{
		entries: make(map[string]*prefetchEntry),
	}
}

func (s *PrefetchSession) StartPrefetch(parentCtx context.Context, rawText string, actions []string, fn PostProcessFunc) {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	ctx, cancel := context.WithCancel(parentCtx)
	s.cancel = cancel
	s.rawText = rawText
	s.entries = make(map[string]*prefetchEntry)

	seen := make(map[string]struct{}, len(actions))
	toRun := make([]struct {
		action string
		entry  *prefetchEntry
	}, 0, len(actions))

	if rawText != "" && fn != nil {
		for _, action := range actions {
			action = strings.TrimSpace(action)
			if action == "" {
				continue
			}
			if _, ok := seen[action]; ok {
				continue
			}
			seen[action] = struct{}{}
			entry := &prefetchEntry{done: make(chan struct{})}
			s.entries[action] = entry
			toRun = append(toRun, struct {
				action string
				entry  *prefetchEntry
			}{action: action, entry: entry})
		}
	}
	s.mu.Unlock()

	for _, item := range toRun {
		go func(action string, entry *prefetchEntry) {
			text, err := fn(ctx, action, rawText)
			entry.text = text
			entry.err = err
			close(entry.done)
		}(item.action, item.entry)
	}
}

func (s *PrefetchSession) Resolve(ctx context.Context, action string, fn PostProcessFunc) (string, error) {
	s.mu.Lock()
	rawText := s.rawText
	if rawText == "" {
		s.mu.Unlock()
		return "", errors.New("no transcribed text in current session")
	}
	entry, exists := s.entries[action]
	if !exists {
		if fn == nil {
			s.mu.Unlock()
			return "", errors.New("post-processor is not configured")
		}
		entry = &prefetchEntry{done: make(chan struct{})}
		s.entries[action] = entry
		s.mu.Unlock()

		text, err := fn(ctx, action, rawText)
		entry.text = text
		entry.err = err
		if err != nil {
			// Remove failed/cancelled on-demand entry so another attempt can retry cleanly.
			s.mu.Lock()
			if s.entries[action] == entry {
				delete(s.entries, action)
			}
			s.mu.Unlock()
		}
		close(entry.done)
		return text, err
	}
	s.mu.Unlock()

	select {
	case <-entry.done:
		return entry.text, entry.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *PrefetchSession) RawText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rawText
}

func (s *PrefetchSession) Reset() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.rawText = ""
	s.entries = make(map[string]*prefetchEntry)
	s.mu.Unlock()
}
