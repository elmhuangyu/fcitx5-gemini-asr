package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/fsnotify/fsnotify"
	"github.com/gorilla/websocket"
	"golang.org/x/net/proxy"
)

const (
	DefaultVADThreshold     = 300.0
	DefaultLiveModel        = "gemini-3.5-transcribe-live"
	DefaultPostProcessModel = "gemini-3.5-flash-lite"
	DefaultLiveLanguage     = "auto"
	DefaultLiveMode         = "VERBATIM"
	MaxCustomVocabulary     = 1000
)

// KnownActions lists built-in post-processing action keys in menu display order.
var KnownActions = []string{
	"refine_fix_typos",
	"refine_casual",
	"refine_formal",
	"as_prompt",
	"translate_en",
	"ask_ai",
}

type Config struct {
	Connection     ConnectionConfig     `toml:"connection"`
	LiveTranscribe LiveTranscribeConfig `toml:"live_transcribe"`
	PostProcess    PostProcessConfig    `toml:"post_process"`
	Prompts        map[string]string    `toml:"prompts"`
}

type ConnectionConfig struct {
	CredentialName string `toml:"credential_name"`
	FallbackEnv    string `toml:"fallback_env"`
	Proxy          string `toml:"proxy"`
	LiveWSURL      string `toml:"live_ws_url,omitempty"`
	APIBaseURL     string `toml:"api_base_url,omitempty"`
}

type LiveTranscribeConfig struct {
	Model            string   `toml:"model"`
	Language         string   `toml:"language"`
	Mode             string   `toml:"mode,omitempty"`
	CustomVocabulary []string `toml:"custom_vocabulary,omitempty"`
	VADSilenceMs     int      `toml:"vad_silence_ms"`
	VADThreshold     float64  `toml:"vad_threshold,omitempty"`
}

type TranslateTarget struct {
	Action string `json:"action"`
	Label  string `json:"label"`
}

type PostProcessConfig struct {
	Model              string            `toml:"model"`
	Models             map[string]string `toml:"models,omitempty"`
	Prefetch           []string          `toml:"prefetch"`
	TranslateLanguages []string          `toml:"translate_languages,omitempty"`
}

func DefaultConfig() Config {
	return Config{
		Connection: ConnectionConfig{
			CredentialName: DefaultCredentialName,
			FallbackEnv:    DefaultFallbackEnv,
			Proxy:          "",
		},
		LiveTranscribe: LiveTranscribeConfig{
			Model:        DefaultLiveModel,
			Language:     DefaultLiveLanguage,
			Mode:         DefaultLiveMode,
			VADSilenceMs: 1200,
			VADThreshold: DefaultVADThreshold,
		},
		PostProcess: PostProcessConfig{
			Model:              DefaultPostProcessModel,
			Prefetch:           []string{"refine_fix_typos", "refine_formal"},
			TranslateLanguages: []string{"en"},
		},
		Prompts: map[string]string{
			"refine_fix_typos": "仅修正以下语音转录文本中的错别字、同音字错误和标点符号。严禁增删词汇或改变原本的口语语气，直接输出结果：",
			"refine_casual":    "将以下文本润色为自然、流畅、轻松的日常对话语气（适合即时通讯聊天），保留原意，直接输出结果：",
			"refine_formal":    "将以下文本润色为清晰、专业、得体的正式书面语（适合工作邮件或技术文档），直接输出结果：",
			"as_prompt":        "将以下口述需求整理为结构清晰、条件明确的 AI Prompt，直接输出提示词内容：",
			"translate_en":     "Translate the following text into natural, idiomatic English. Output ONLY the translation:",
			"ask_ai":           "请直接将以下语音内容作为提示词或指令执行，并直接输出生成的最终内容，不要复述指令或添加多余解释：",
		},
	}
}

// DefaultConfigTOML returns the canonical config.toml template matching systemd/config.toml.
func DefaultConfigTOML() string {
	return `[connection]
credential_name = "gemini_api_key"
fallback_env = "GEMINI_API_KEY"
proxy = ""

[live_transcribe]
model = "gemini-3.5-transcribe-live"
language = "auto" # "auto" (85+ languages & code-switching), or comma-separated BCP-47 (e.g. "cmn-Hans-CN,en-US"; "zh-CN" aliases to "cmn-Hans-CN")
mode = "VERBATIM"        # "VERBATIM" (literal) or "SMART" (disfluency cleanup & formatting)
# custom_vocabulary = ["Fcitx5", "PipeWire", "Gemini"]
vad_silence_ms = 1200

[post_process]
model = "gemini-3.5-flash-lite"
prefetch = ["refine_fix_typos", "refine_formal"]
translate_languages = ["en"]

[post_process.models]
# Optional per-action model overrides (empty inherits post_process.model):
# refine_fix_typos = "gemini-2.5-flash-lite"
# refine_casual = "gemini-2.5-flash"
# refine_formal = "gemini-2.5-pro"
# as_prompt = "gemini-2.5-pro"
# translate_en = "gemini-2.5-flash"
# ask_ai = "gemini-2.5-pro"

[prompts]
refine_fix_typos = "仅修正以下语音转录文本中的错别字、同音字错误和标点符号。严禁增删词汇或改变原本的口语语气，直接输出结果："
refine_casual = "将以下文本润色为自然、流畅、轻松的日常对话语气（适合即时通讯聊天），保留原意，直接输出结果："
refine_formal = "将以下文本润色为清晰、专业、得体的正式书面语（适合工作邮件或技术文档），直接输出结果："
as_prompt = "将以下口述需求整理为结构清晰、条件明确的 AI Prompt，直接输出提示词内容："
translate_en = "Translate the following text into natural, idiomatic English. Output ONLY the translation:"
ask_ai = "请直接将以下语音内容作为提示词或指令执行，并直接输出生成的最终内容，不要复述指令或添加多余解释："
`
}

// NormalizeLiveMode validates and canonicalizes a Live transcription mode string.
func NormalizeLiveMode(raw string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "", "VERBATIM":
		return "VERBATIM", nil
	case "SMART":
		return "SMART", nil
	default:
		return "", fmt.Errorf("invalid live.mode %q (expected VERBATIM or SMART)", raw)
	}
}

func pruneCustomVocabulary(terms []string) []string {
	if len(terms) == 0 {
		return nil
	}
	out := make([]string, 0, len(terms))
	seen := make(map[string]struct{}, len(terms))
	for _, t := range terms {
		trimmed := strings.TrimSpace(t)
		if trimmed == "" {
			continue
		}
		if _, dup := seen[trimmed]; dup {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (c Config) Clone() Config {
	out := c
	if c.LiveTranscribe.CustomVocabulary != nil {
		out.LiveTranscribe.CustomVocabulary = append([]string(nil), c.LiveTranscribe.CustomVocabulary...)
	}
	if c.PostProcess.Prefetch != nil {
		out.PostProcess.Prefetch = append([]string(nil), c.PostProcess.Prefetch...)
	}
	if c.PostProcess.TranslateLanguages != nil {
		out.PostProcess.TranslateLanguages = append([]string(nil), c.PostProcess.TranslateLanguages...)
	}
	if c.PostProcess.Models != nil {
		out.PostProcess.Models = make(map[string]string, len(c.PostProcess.Models))
		for k, v := range c.PostProcess.Models {
			out.PostProcess.Models[k] = v
		}
	}
	if c.Prompts != nil {
		out.Prompts = make(map[string]string, len(c.Prompts))
		for k, v := range c.Prompts {
			out.Prompts[k] = v
		}
	}
	return out
}

// ModelForAction returns the configured model override for action, falling back to post_process.model.
func (c Config) ModelForAction(action string) string {
	if m := strings.TrimSpace(c.PostProcess.Models[action]); m != "" {
		return m
	}
	if m := strings.TrimSpace(c.PostProcess.Model); m != "" {
		return m
	}
	return DefaultPostProcessModel
}

var knownTranslateLanguages = map[string]TranslateTarget{
	"en":                  {Action: "translate_en", Label: "English"},
	"english":             {Action: "translate_en", Label: "English"},
	"zh":                  {Action: "translate_zh_cn", Label: "Chinese (Simplified)"},
	"zh-cn":               {Action: "translate_zh_cn", Label: "Chinese (Simplified)"},
	"zh-hans":             {Action: "translate_zh_cn", Label: "Chinese (Simplified)"},
	"chinese":             {Action: "translate_zh_cn", Label: "Chinese (Simplified)"},
	"simplified-chinese":  {Action: "translate_zh_cn", Label: "Chinese (Simplified)"},
	"zh-tw":               {Action: "translate_zh_tw", Label: "Chinese (Traditional)"},
	"zh-hant":             {Action: "translate_zh_tw", Label: "Chinese (Traditional)"},
	"traditional-chinese": {Action: "translate_zh_tw", Label: "Chinese (Traditional)"},
	"ja":                  {Action: "translate_ja", Label: "Japanese"},
	"jp":                  {Action: "translate_ja", Label: "Japanese"},
	"japanese":            {Action: "translate_ja", Label: "Japanese"},
	"ko":                  {Action: "translate_ko", Label: "Korean"},
	"korean":              {Action: "translate_ko", Label: "Korean"},
	"fr":                  {Action: "translate_fr", Label: "French"},
	"french":              {Action: "translate_fr", Label: "French"},
	"de":                  {Action: "translate_de", Label: "German"},
	"german":              {Action: "translate_de", Label: "German"},
	"es":                  {Action: "translate_es", Label: "Spanish"},
	"spanish":             {Action: "translate_es", Label: "Spanish"},
	"pt":                  {Action: "translate_pt", Label: "Portuguese"},
	"portuguese":          {Action: "translate_pt", Label: "Portuguese"},
	"ru":                  {Action: "translate_ru", Label: "Russian"},
	"russian":             {Action: "translate_ru", Label: "Russian"},
	"it":                  {Action: "translate_it", Label: "Italian"},
	"italian":             {Action: "translate_it", Label: "Italian"},
	"ar":                  {Action: "translate_ar", Label: "Arabic"},
	"arabic":              {Action: "translate_ar", Label: "Arabic"},
	"vi":                  {Action: "translate_vi", Label: "Vietnamese"},
	"vietnamese":          {Action: "translate_vi", Label: "Vietnamese"},
	"th":                  {Action: "translate_th", Label: "Thai"},
	"thai":                {Action: "translate_th", Label: "Thai"},
	"id":                  {Action: "translate_id", Label: "Indonesian"},
	"indonesian":          {Action: "translate_id", Label: "Indonesian"},
}

func slugifyTranslateTarget(s string) string {
	var b strings.Builder
	prevUnderscore := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevUnderscore = false
		case !prevUnderscore && b.Len() > 0:
			b.WriteByte('_')
			prevUnderscore = true
		}
	}
	slug := strings.Trim(b.String(), "_")
	if slug == "" {
		h := fnv.New32a()
		_, _ = h.Write([]byte(s))
		return fmt.Sprintf("u_%08x", h.Sum32())
	}
	return slug
}

// ResolveTranslateTarget normalizes a configured target language entry into its action key and display label.
func ResolveTranslateTarget(raw string) (TranslateTarget, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return TranslateTarget{}, false
	}
	alias := strings.ToLower(trimmed)
	alias = strings.ReplaceAll(alias, "_", "-")
	alias = strings.ReplaceAll(alias, " ", "-")
	if known, ok := knownTranslateLanguages[alias]; ok {
		return known, true
	}
	return TranslateTarget{
		Action: "translate_" + slugifyTranslateTarget(trimmed),
		Label:  trimmed,
	}, true
}

func normalizeTranslateLanguages(langs []string) []string {
	if len(langs) == 0 {
		return []string{"en"}
	}
	out := make([]string, 0, len(langs))
	seen := make(map[string]struct{}, len(langs))
	for _, raw := range langs {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		target, ok := ResolveTranslateTarget(trimmed)
		if !ok {
			continue
		}
		if _, dup := seen[target.Action]; dup {
			continue
		}
		seen[target.Action] = struct{}{}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return []string{"en"}
	}
	return out
}

// TranslateTargets returns the deduplicated list of configured translation targets, defaulting to English.
func (c Config) TranslateTargets() []TranslateTarget {
	out := make([]TranslateTarget, 0, len(c.PostProcess.TranslateLanguages))
	seen := make(map[string]struct{}, len(c.PostProcess.TranslateLanguages))
	for _, raw := range c.PostProcess.TranslateLanguages {
		target, ok := ResolveTranslateTarget(raw)
		if !ok {
			continue
		}
		if _, dup := seen[target.Action]; dup {
			continue
		}
		seen[target.Action] = struct{}{}
		out = append(out, target)
	}
	if len(out) == 0 {
		return []TranslateTarget{{Action: "translate_en", Label: "English"}}
	}
	return out
}

// PromptForAction returns the prompt for action, synthesizing a default translation prompt for configured target languages.
func (c Config) PromptForAction(action string) (string, bool) {
	return c.PromptForActionWithLabel(action, "")
}

// PromptForActionWithLabel returns the prompt for action, falling back to a
// synthesized translation prompt using targetLabel or known language aliases
// when action is a translate_* key.
func (c Config) PromptForActionWithLabel(action, targetLabel string) (string, bool) {
	if p, ok := c.Prompts[action]; ok && strings.TrimSpace(p) != "" {
		return p, true
	}
	for _, target := range c.TranslateTargets() {
		if target.Action == action {
			return fmt.Sprintf("Translate the following text into natural, idiomatic %s. Output ONLY the translation:", target.Label), true
		}
	}
	if strings.HasPrefix(action, "translate_") {
		label := strings.TrimSpace(targetLabel)
		if label == "" {
			for _, known := range knownTranslateLanguages {
				if known.Action == action {
					label = known.Label
					break
				}
			}
		}
		if label != "" {
			return fmt.Sprintf("Translate the following text into natural, idiomatic %s. Output ONLY the translation:", label), true
		}
	}
	return "", false
}

// EffectiveActionNames returns KnownActions (with translate_en expanded to configured TranslateTargets) followed by any extra sorted prompt keys.
func (c Config) EffectiveActionNames() []string {
	targets := c.TranslateTargets()
	out := make([]string, 0, len(KnownActions)+len(targets)+len(c.Prompts))
	seen := make(map[string]struct{}, len(KnownActions)+len(targets)+len(c.Prompts))
	for _, a := range KnownActions {
		if a == "translate_en" {
			for _, t := range targets {
				if _, dup := seen[t.Action]; !dup {
					out = append(out, t.Action)
					seen[t.Action] = struct{}{}
				}
			}
			continue
		}
		out = append(out, a)
		seen[a] = struct{}{}
	}
	var extra []string
	for k := range c.Prompts {
		if _, ok := seen[k]; !ok {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

// renderConfigTOML normalizes cfg and encodes it as TOML without writing anything.
func renderConfigTOML(cfg Config) ([]byte, error) {
	toWrite := cfg.Clone()
	toWrite.LiveTranscribe.CustomVocabulary = pruneCustomVocabulary(toWrite.LiveTranscribe.CustomVocabulary)
	if toWrite.PostProcess.Prefetch == nil {
		toWrite.PostProcess.Prefetch = []string{}
	}
	toWrite.PostProcess.TranslateLanguages = normalizeTranslateLanguages(toWrite.PostProcess.TranslateLanguages)
	if len(toWrite.PostProcess.Models) > 0 {
		pruned := make(map[string]string, len(toWrite.PostProcess.Models))
		for k, v := range toWrite.PostProcess.Models {
			if trimmed := strings.TrimSpace(v); trimmed != "" {
				pruned[k] = trimmed
			}
		}
		if len(pruned) > 0 {
			toWrite.PostProcess.Models = pruned
		} else {
			toWrite.PostProcess.Models = nil
		}
	}

	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	if err := enc.Encode(toWrite); err != nil {
		return nil, fmt.Errorf("encode config TOML: %w", err)
	}
	return buf.Bytes(), nil
}

// writeConfigFileAtomic creates path's parent directory if needed and writes
// data atomically with mode 0600; kind names the file in error messages.
func writeConfigFileAtomic(path string, data []byte, kind string) error {
	cleanPath := filepath.Clean(path)
	dir := filepath.Dir(cleanPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s directory %q: %w", kind, dir, err)
	}
	return writeFileAtomic(cleanPath, data, 0o600)
}

// SaveConfigFile atomically writes cfg as TOML with mode 0600.
func SaveConfigFile(path string, cfg Config) error {
	data, err := renderConfigTOML(cfg)
	if err != nil {
		return err
	}
	return writeConfigFileAtomic(path, data, "config")
}

func LoadConfigFile(path string) (Config, error) {
	cfg := DefaultConfig()
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(cfg.Connection.CredentialName) == "" {
		cfg.Connection.CredentialName = DefaultCredentialName
	}
	if strings.TrimSpace(cfg.Connection.FallbackEnv) == "" {
		cfg.Connection.FallbackEnv = DefaultFallbackEnv
	}
	if strings.TrimSpace(cfg.LiveTranscribe.Model) == "" {
		cfg.LiveTranscribe.Model = DefaultLiveModel
	}
	mode, err := NormalizeLiveMode(cfg.LiveTranscribe.Mode)
	if err != nil {
		return Config{}, err
	}
	cfg.LiveTranscribe.Mode = mode
	cfg.LiveTranscribe.CustomVocabulary = pruneCustomVocabulary(cfg.LiveTranscribe.CustomVocabulary)
	if len(cfg.LiveTranscribe.CustomVocabulary) > MaxCustomVocabulary {
		return Config{}, fmt.Errorf("live_transcribe.custom_vocabulary has %d entries (max %d)", len(cfg.LiveTranscribe.CustomVocabulary), MaxCustomVocabulary)
	}
	if cfg.LiveTranscribe.VADThreshold <= 0 {
		cfg.LiveTranscribe.VADThreshold = DefaultVADThreshold
	}
	if strings.TrimSpace(cfg.PostProcess.Model) == "" {
		cfg.PostProcess.Model = DefaultPostProcessModel
	}
	cfg.PostProcess.TranslateLanguages = normalizeTranslateLanguages(cfg.PostProcess.TranslateLanguages)
	return cfg, nil
}

type ConfigManager struct {
	path     string
	mu       sync.RWMutex
	cfg      Config
	onReload func(Config)
}

func NewConfigManager(path string) (*ConfigManager, error) {
	cleanPath := filepath.Clean(path)
	cfg, err := LoadConfigFile(cleanPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			cfg = DefaultConfig()
		} else {
			return nil, fmt.Errorf("load config %q: %w", cleanPath, err)
		}
	}
	return &ConfigManager{
		path: cleanPath,
		cfg:  cfg,
	}, nil
}

func (m *ConfigManager) Get() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg.Clone()
}

func (m *ConfigManager) SetOnReload(fn func(Config)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onReload = fn
}

func (m *ConfigManager) Reload() error {
	cfg, err := LoadConfigFile(m.path)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.cfg = cfg
	cb := m.onReload
	m.mu.Unlock()

	if cb != nil {
		cb(cfg.Clone())
	}
	return nil
}

// Watch monitors the parent directory of the config file so both in-place writes
// and atomic editor renames trigger debounced hot-reloads.
func (m *ConfigManager) Watch(ctx context.Context) error {
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("ensure config dir %q: %w", dir, err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create fsnotify watcher: %w", err)
	}
	defer func() { _ = watcher.Close() }()

	if err := watcher.Add(dir); err != nil {
		return fmt.Errorf("watch config dir %q: %w", dir, err)
	}

	var debounceTimer *time.Timer
	var timerCh <-chan time.Time
	resetTimer := func() {
		if debounceTimer == nil {
			debounceTimer = time.NewTimer(100 * time.Millisecond)
			timerCh = debounceTimer.C
			return
		}
		if !debounceTimer.Stop() {
			select {
			case <-debounceTimer.C:
			default:
			}
		}
		debounceTimer.Reset(100 * time.Millisecond)
		timerCh = debounceTimer.C
	}

	target := filepath.Clean(m.path)

	for {
		select {
		case <-ctx.Done():
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			return nil
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if filepath.Clean(event.Name) != target {
				continue
			}
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) != 0 {
				resetTimer()
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			log.Printf("config watcher error: %v", err)
		case <-timerCh:
			timerCh = nil
			if err := m.Reload(); err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					log.Printf("failed to reload config %q (keeping previous valid config): %v", m.path, err)
				}
			} else {
				log.Printf("reloaded config from %s", m.path)
			}
		}
	}
}

func BuildHTTPClient(proxyAddr string, timeout time.Duration) (*http.Client, error) {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	trimmed := strings.TrimSpace(proxyAddr)
	if trimmed != "" {
		u, err := url.Parse(trimmed)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL %q: %w", RedactURL(trimmed), err)
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https":
			tr.Proxy = http.ProxyURL(u)
		case "socks5", "socks5h":
			dialer, err := proxy.FromURL(u, proxy.Direct)
			if err != nil {
				return nil, fmt.Errorf("configure socks5 proxy %q: %w", RedactURL(trimmed), err)
			}
			tr.Proxy = nil
			tr.DialContext = wrapProxyDialContext(dialer)
		default:
			return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
		}
	}

	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
	}, nil
}

func BuildWSDialer(proxyAddr string) (*websocket.Dialer, error) {
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second
	dialer.Proxy = http.ProxyFromEnvironment

	trimmed := strings.TrimSpace(proxyAddr)
	if trimmed != "" {
		u, err := url.Parse(trimmed)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL %q: %w", RedactURL(trimmed), err)
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https":
			dialer.Proxy = http.ProxyURL(u)
		case "socks5", "socks5h":
			pDialer, err := proxy.FromURL(u, proxy.Direct)
			if err != nil {
				return nil, fmt.Errorf("configure socks5 proxy %q: %w", RedactURL(trimmed), err)
			}
			dialer.Proxy = nil
			dialer.NetDialContext = wrapProxyDialContext(pDialer)
		default:
			return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
		}
	}

	return &dialer, nil
}

func wrapProxyDialContext(d proxy.Dialer) func(context.Context, string, string) (net.Conn, error) {
	if cd, ok := d.(proxy.ContextDialer); ok {
		return cd.DialContext
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		type result struct {
			conn net.Conn
			err  error
		}
		ch := make(chan result, 1)
		go func() {
			c, err := d.Dial(network, addr)
			ch <- result{conn: c, err: err}
		}()
		select {
		case <-ctx.Done():
			go func() {
				res := <-ch
				if res.conn != nil {
					_ = res.conn.Close()
				}
			}()
			return nil, ctx.Err()
		case res := <-ch:
			return res.conn, res.err
		}
	}
}
