package main

import (
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

	"github.com/fsnotify/fsnotify"
	"github.com/gorilla/websocket"
	"golang.org/x/net/proxy"
)

const (
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

// Config is the effective daemon configuration, read from the Fcitx5 addon
// config file (gemini-voice.conf). Ime holds the addon-consumed keys; they live
// in the same file and the daemon reads the language and translation targets
// from them. The API key is never part of it (see credentials.go).
type Config struct {
	Ime            FcitxAddonConfig
	Connection     ConnectionConfig
	LiveTranscribe LiveTranscribeConfig
	PostProcess    PostProcessConfig
	// Prompts holds the per-action prompt overrides of the advanced Actions
	// list (empty by default). They take precedence over the group prompts in
	// PostProcess.Groups, which take precedence over the built-in defaults.
	Prompts map[string]string
}

type ConnectionConfig struct {
	Proxy      string
	LiveWSURL  string
	APIBaseURL string
}

type LiveTranscribeConfig struct {
	Model            string
	Mode             string
	CustomVocabulary []string
	VADSilenceMs     int
}

type TranslateTarget struct {
	Action string `json:"action"`
	Label  string `json:"label"`
}

// ActionGroup is the settings of one fixed fcitx sub-group ([PostProcess/
// RefineFormal] etc.): a model override, a prompt override (both empty inherit)
// and whether the action runs when recording stops.
type ActionGroup struct {
	Model    string
	Prompt   string
	Prefetch bool
}

// translateGroup is the group id covering every translate_<lang> action.
const translateGroup = "translate"

// actionGroups lists the fixed group ids in display order; every id except
// translateGroup is also the name of its single action.
var actionGroups = []string{
	"refine_fix_typos",
	"refine_casual",
	"refine_formal",
	"as_prompt",
	translateGroup,
	"ask_ai",
}

// groupSections maps a group id to its section name under [PostProcess] in
// gemini-voice.conf (mirroring addon/engine.h).
var groupSections = map[string]string{
	"refine_fix_typos": "RefineFixTypos",
	"refine_casual":    "RefineCasual",
	"refine_formal":    "RefineFormal",
	"as_prompt":        "AsPrompt",
	translateGroup:     "Translate",
	"ask_ai":           "AskAi",
}

// defaultPrefetchGroups are the groups whose Prefetch defaults to true.
var defaultPrefetchGroups = map[string]bool{"refine_fix_typos": true, "refine_formal": true}

// groupForAction returns the fixed group id an action belongs to, if any.
func groupForAction(action string) (string, bool) {
	if action == translateGroup || strings.HasPrefix(action, "translate_") {
		return translateGroup, true
	}
	for _, g := range actionGroups {
		if g == action {
			return g, true
		}
	}
	return "", false
}

type PostProcessConfig struct {
	Model string
	// Groups holds the fixed per-action groups, keyed by group id (always
	// populated for every id in actionGroups).
	Groups map[string]ActionGroup
	// Models holds the per-action model overrides of the advanced Actions
	// list, for actions without a fixed group (e.g. translate_ja). They win
	// over Groups and Model.
	Models map[string]string
}

// PrefetchItem is one post-processing request started when recording stops.
type PrefetchItem struct {
	Action string
	Label  string
}

// PrefetchItems resolves the groups with Prefetch enabled into the requests to
// start: one per built-in action, and one per configured translation target
// for the Translate group.
func (c Config) PrefetchItems() []PrefetchItem {
	var items []PrefetchItem
	for _, g := range actionGroups {
		if !c.PostProcess.Groups[g].Prefetch {
			continue
		}
		if g == translateGroup {
			for _, t := range c.TranslateTargets() {
				items = append(items, PrefetchItem(t))
			}
			continue
		}
		items = append(items, PrefetchItem{Action: g})
	}
	return items
}

// isGroupAction reports whether action is exactly a fixed group's id (a
// built-in action, or "translate" for all target languages).
func isGroupAction(action string) bool {
	for _, g := range actionGroups {
		if g == action {
			return true
		}
	}
	return false
}

// SetActionModel sets (or, for "", "default" and "-", clears) the model of an
// action. The built-in actions and "translate" (all target languages) are
// stored in their fixed group; any other action (e.g. translate_ja) goes to
// the advanced Actions list. Setting a group model also drops a conflicting
// Actions-list entry so the new value is the effective one.
func (c *Config) SetActionModel(action, val string) error {
	trimmed := strings.TrimSpace(val)
	cleared := trimmed == "" || strings.EqualFold(trimmed, "default") || trimmed == "-"
	if cleared {
		trimmed = ""
	} else if err := ValidatePostProcessModel(trimmed); err != nil {
		return err
	}
	if isGroupAction(action) {
		grp := c.PostProcess.Groups[action]
		grp.Model = trimmed
		c.PostProcess.Groups[action] = grp
		delete(c.PostProcess.Models, action)
		return nil
	}
	if cleared {
		delete(c.PostProcess.Models, action)
		return nil
	}
	if c.PostProcess.Models == nil {
		c.PostProcess.Models = make(map[string]string)
	}
	c.PostProcess.Models[action] = trimmed
	return nil
}

// ActionModelOverride returns the explicitly configured model of an action
// (Actions list first, then its fixed group), or "" when it inherits
// post_process.model.
func (c Config) ActionModelOverride(action string) string {
	if m := strings.TrimSpace(c.PostProcess.Models[action]); m != "" {
		return m
	}
	if g, ok := groupForAction(action); ok {
		return strings.TrimSpace(c.PostProcess.Groups[g].Model)
	}
	return ""
}

// PrefetchActionNames lists the actions that run when recording stops, one
// entry per built-in action or translate target.
func (c Config) PrefetchActionNames() []string {
	var out []string
	for _, item := range c.PrefetchItems() {
		out = append(out, item.Action)
	}
	return out
}

// SetPrefetchActions enables Prefetch for exactly the groups of the named
// actions (any translate_* name, or "translate", enables the Translate
// group, which prefetches every configured target).
func (c *Config) SetPrefetchActions(names []string) error {
	enabled := map[string]bool{}
	for _, n := range names {
		g, ok := groupForAction(strings.TrimSpace(n))
		if !ok {
			return fmt.Errorf("unknown action %q in prefetch list", n)
		}
		enabled[g] = true
	}
	for _, g := range actionGroups {
		grp := c.PostProcess.Groups[g]
		grp.Prefetch = enabled[g]
		c.PostProcess.Groups[g] = grp
	}
	return nil
}

// SetActionPrompt sets the prompt of an action, routed like SetActionModel.
func (c *Config) SetActionPrompt(action, prompt string) {
	if isGroupAction(action) {
		grp := c.PostProcess.Groups[action]
		grp.Prompt = prompt
		c.PostProcess.Groups[action] = grp
		delete(c.Prompts, action)
		return
	}
	if c.Prompts == nil {
		c.Prompts = make(map[string]string)
	}
	c.Prompts[action] = prompt
}

// defaultPrompts are the built-in prompts; they live in code and are only
// written to the config file when the user overrides them.
var defaultPrompts = map[string]string{
	"refine_fix_typos": "仅修正以下语音转录文本中的错别字、同音字错误和标点符号。严禁增删词汇或改变原本的口语语气，直接输出结果：",
	"refine_casual":    "将以下文本润色为自然、流畅、轻松的日常对话语气（适合即时通讯聊天），保留原意，直接输出结果：",
	"refine_formal":    "将以下文本润色为清晰、专业、得体的正式书面语（适合工作邮件或技术文档），直接输出结果：",
	"as_prompt":        "将以下口述需求整理为结构清晰、条件明确的 AI Prompt，直接输出提示词内容：",
	"translate_en":     "Translate the following text into natural, idiomatic English. Output ONLY the translation:",
	"ask_ai":           "请直接将以下语音内容作为提示词或指令执行，并直接输出生成的最终内容，不要复述指令或添加多余解释：",
}

const DefaultVADSilenceMs = 1200

func DefaultConfig() Config {
	groups := make(map[string]ActionGroup, len(actionGroups))
	for _, g := range actionGroups {
		groups[g] = ActionGroup{Prefetch: defaultPrefetchGroups[g]}
	}
	return Config{
		Ime: DefaultFcitxAddonConfig(),
		LiveTranscribe: LiveTranscribeConfig{
			Model:        DefaultLiveModel,
			Mode:         DefaultLiveMode,
			VADSilenceMs: DefaultVADSilenceMs,
		},
		PostProcess: PostProcessConfig{
			Model:  DefaultPostProcessModel,
			Groups: groups,
		},
	}
}

// LiveLanguage returns the transcription language, defaulting to "auto".
func (c Config) LiveLanguage() string {
	if l := strings.TrimSpace(c.Ime.Language); l != "" {
		return l
	}
	return DefaultLiveLanguage
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
	out.Ime.TriggerKeys = append([]string(nil), c.Ime.TriggerKeys...)
	if c.Ime.TranslateLanguages != nil {
		out.Ime.TranslateLanguages = append([]string(nil), c.Ime.TranslateLanguages...)
	}
	if c.LiveTranscribe.CustomVocabulary != nil {
		out.LiveTranscribe.CustomVocabulary = append([]string(nil), c.LiveTranscribe.CustomVocabulary...)
	}
	if c.PostProcess.Groups != nil {
		out.PostProcess.Groups = make(map[string]ActionGroup, len(c.PostProcess.Groups))
		for k, v := range c.PostProcess.Groups {
			out.PostProcess.Groups[k] = v
		}
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

// ModelForAction resolves the model of an action: an Actions-list entry wins
// over the action's fixed group, which wins over post_process.model.
func (c Config) ModelForAction(action string) string {
	if m := strings.TrimSpace(c.PostProcess.Models[action]); m != "" {
		return m
	}
	if g, ok := groupForAction(action); ok {
		if m := strings.TrimSpace(c.PostProcess.Groups[g].Model); m != "" {
			return m
		}
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
	out := make([]TranslateTarget, 0, len(c.Ime.TranslateLanguages))
	seen := make(map[string]struct{}, len(c.Ime.TranslateLanguages))
	for _, raw := range c.Ime.TranslateLanguages {
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
	if p := c.Prompts[action]; strings.TrimSpace(p) != "" {
		return p, true
	}
	if g, ok := groupForAction(action); ok {
		if p := c.PostProcess.Groups[g].Prompt; strings.TrimSpace(p) != "" {
			return p, true
		}
	}
	if p, ok := defaultPrompts[action]; ok {
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

// EffectiveActionNames returns KnownActions (with translate_en expanded to configured TranslateTargets) followed by sorted extra keys from Prompts and model overrides, so actions that exist only as model overrides (e.g. translate_ko written by "models --translate ko=...") stay visible.
func (c Config) EffectiveActionNames() []string {
	targets := c.TranslateTargets()
	out := make([]string, 0, len(KnownActions)+len(targets)+len(c.Prompts)+len(c.PostProcess.Models))
	seen := make(map[string]struct{}, len(KnownActions)+len(targets)+len(c.Prompts)+len(c.PostProcess.Models))
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
			seen[k] = struct{}{}
		}
	}
	for k := range c.PostProcess.Models {
		if _, ok := seen[k]; !ok {
			extra = append(extra, k)
			seen[k] = struct{}{}
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
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
