package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func cmdConfig(args []string, deps cliDeps) int {
	if len(args) == 0 {
		printConfigUsage(deps.stdout)
		return 0
	}
	switch args[0] {
	case "show":
		return cmdConfigShow(args[1:], deps)
	case "get":
		return cmdConfigGet(args[1:], deps)
	case "set":
		return cmdConfigSet(args[1:], deps)
	case "models":
		return cmdModels(args[1:], deps)
	case "wizard", "configure":
		return cmdConfigure(args[1:], deps)
	case "help", "-h", "--help":
		printConfigUsage(deps.stdout)
		return 0
	default:
		_, _ = fmt.Fprintf(deps.stderr, "unknown config subcommand: %s\n\n", args[0])
		printConfigUsage(deps.stderr)
		return 2
	}
}

func printConfigUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage:
  gemini-voice config <subcommand> [options]

Subcommands:
  show                Display effective daemon and Fcitx5 addon configuration
  get <key>           Get a single configuration value
  set <key> <value>   Update a configuration key and save atomically
  models              Inspect or update live/post-process models (alias: gemini-voice models)
  wizard              Run the interactive setup wizard (alias: gemini-voice configure)

Common keys for "config get" / "config set":
  ime.mode                     menu | direct (show action menu vs commit raw text directly)
  ime.show_action_menu         true | false
  ime.trigger_key              Trigger shortcut list, comma-separated (e.g. Alt+space)
  ime.socket_path              Unix socket path for addon <-> daemon
  ime.language                 Optional frontend language override (empty = use daemon)
  live.model                   Gemini Live transcription model
  live.language                Transcription language (e.g. auto, cmn-Hans-CN, en-US, cmn-Hans-CN,en-US)
  live.mode                    Live transcription mode (VERBATIM | SMART)
  live.custom_vocabulary       Comma-separated terms to bias recognition (or "none")
  live.vad_silence_ms          Silence timeout in ms (0 disables auto-stop)
  live.vad_threshold           RMS energy threshold for speech detection
  post_process.model           Default post-processing model
  post_process.prefetch        Comma-separated actions to prefetch (or "none")
  model.<action>               Per-action model override ("default" clears override)
  prompt.<action>              Prompt template for <action>
  connection.proxy             HTTP/HTTPS/SOCKS5 proxy URL (empty or "none" clears)
  connection.credential_name   Encrypted credential name in CREDENTIALS_DIRECTORY
  connection.fallback_env      Fallback env var name when CREDENTIALS_DIRECTORY is unset
`)
}

func cmdConfigShow(args []string, deps cliDeps) int {
	fs := flag.NewFlagSet("config show", flag.ContinueOnError)
	fs.SetOutput(deps.stderr)
	configPath := fs.String("config", defaultConfigPath(), "Path to config.toml")
	fcitxPath := fs.String("fcitx-config", DefaultFcitxConfigPath(), "Path to Fcitx5 gemini-voice.conf")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	cfg, err := loadConfigOrDefault(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: load config %q: %v\n", *configPath, err)
		return 1
	}
	fcitxCfg, err := LoadFcitxAddonConfig(*fcitxPath)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}

	printConfigSummary(deps.stdout, *configPath, cfg, *fcitxPath, fcitxCfg)
	return 0
}

func printConfigSummary(w io.Writer, configPath string, cfg Config, fcitxPath string, fcitxCfg FcitxAddonConfig) {
	mode := "menu (action menu after transcription)"
	if !fcitxCfg.ShowActionMenu {
		mode = "direct (commit raw transcript immediately)"
	}
	proxyVal := cfg.Connection.Proxy
	if strings.TrimSpace(proxyVal) == "" {
		proxyVal = "(none)"
	}
	prefetchVal := strings.Join(cfg.PostProcess.Prefetch, ", ")
	if prefetchVal == "" {
		prefetchVal = "(none)"
	}
	vocabVal := strings.Join(cfg.LiveTranscribe.CustomVocabulary, ", ")
	if vocabVal == "" {
		vocabVal = "(none)"
	}
	fcitxLang := fcitxCfg.Language
	if fcitxLang == "" {
		fcitxLang = "(use daemon)"
	}

	_, _ = fmt.Fprintf(w, "Fcitx5 addon config (%s):\n", fcitxPath)
	_, _ = fmt.Fprintf(w, "  ime.mode:                 %s\n", mode)
	_, _ = fmt.Fprintf(w, "  ime.show_action_menu:     %t\n", fcitxCfg.ShowActionMenu)
	_, _ = fmt.Fprintf(w, "  ime.trigger_key:          %s\n", strings.Join(fcitxCfg.TriggerKeys, ", "))
	_, _ = fmt.Fprintf(w, "  ime.socket_path:          %s\n", fcitxCfg.SocketPath)
	_, _ = fmt.Fprintf(w, "  ime.language:             %s\n", fcitxLang)

	_, _ = fmt.Fprintf(w, "\nDaemon config (%s):\n", configPath)
	_, _ = fmt.Fprintf(w, "  connection.proxy:         %s\n", proxyVal)
	_, _ = fmt.Fprintf(w, "  connection.credential:    %s (fallback env: %s)\n", cfg.Connection.CredentialName, cfg.Connection.FallbackEnv)
	_, _ = fmt.Fprintf(w, "  live.model:               %s\n", cfg.LiveTranscribe.Model)
	_, _ = fmt.Fprintf(w, "  live.language:            %s\n", cfg.LiveTranscribe.Language)
	_, _ = fmt.Fprintf(w, "  live.mode:                %s\n", cfg.LiveTranscribe.Mode)
	_, _ = fmt.Fprintf(w, "  live.custom_vocabulary:   %s\n", vocabVal)
	_, _ = fmt.Fprintf(w, "  live.vad_silence_ms:      %d\n", cfg.LiveTranscribe.VADSilenceMs)
	_, _ = fmt.Fprintf(w, "  live.vad_threshold:       %.0f\n", cfg.LiveTranscribe.VADThreshold)
	_, _ = fmt.Fprintf(w, "  post_process.model:       %s\n", cfg.PostProcess.Model)
	_, _ = fmt.Fprintf(w, "  post_process.prefetch:    %s\n", prefetchVal)

	_, _ = fmt.Fprintln(w, "\nPer-action models:")
	printModelTable(w, cfg)

	_, _ = fmt.Fprintln(w, "\nPrompts:")
	for _, action := range cfg.EffectiveActionNames() {
		p := strings.ReplaceAll(cfg.Prompts[action], "\n", " ")
		_, _ = fmt.Fprintf(w, "  %-20s %s\n", action+":", truncatePreview(p, 64))
	}
}

func printModelTable(w io.Writer, cfg Config) {
	_, _ = fmt.Fprintf(w, "  %-20s %s\n", "live_transcribe:", cfg.LiveTranscribe.Model)
	_, _ = fmt.Fprintf(w, "  %-20s %s (default)\n", "post_process.model:", cfg.PostProcess.Model)
	for _, action := range cfg.EffectiveActionNames() {
		override := strings.TrimSpace(cfg.PostProcess.Models[action])
		if override != "" {
			_, _ = fmt.Fprintf(w, "  %-20s %s (override)\n", "model."+action+":", override)
		} else {
			_, _ = fmt.Fprintf(w, "  %-20s %s (inherited)\n", "model."+action+":", cfg.ModelForAction(action))
		}
	}
}

func truncatePreview(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}

func cmdConfigGet(args []string, deps cliDeps) int {
	fs := flag.NewFlagSet("config get", flag.ContinueOnError)
	fs.SetOutput(deps.stderr)
	configPath := fs.String("config", defaultConfigPath(), "Path to config.toml")
	fcitxPath := fs.String("fcitx-config", DefaultFcitxConfigPath(), "Path to Fcitx5 gemini-voice.conf")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		_, _ = fmt.Fprintln(deps.stderr, "usage: gemini-voice config get [-config PATH] [-fcitx-config PATH] <key>")
		return 2
	}

	cfg, err := loadConfigOrDefault(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: load config %q: %v\n", *configPath, err)
		return 1
	}
	fcitxCfg, err := LoadFcitxAddonConfig(*fcitxPath)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}

	val, err := getConfigKey(cfg, fcitxCfg, fs.Arg(0))
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintln(deps.stdout, val)
	return 0
}

func getConfigKey(cfg Config, fcitxCfg FcitxAddonConfig, key string) (string, error) {
	k := strings.TrimSpace(key)
	switch k {
	case "ime.mode":
		if fcitxCfg.ShowActionMenu {
			return "menu", nil
		}
		return "direct", nil
	case "ime.show_action_menu":
		return strconv.FormatBool(fcitxCfg.ShowActionMenu), nil
	case "ime.trigger_key":
		return strings.Join(fcitxCfg.TriggerKeys, ","), nil
	case "ime.socket_path":
		return fcitxCfg.SocketPath, nil
	case "ime.language":
		return fcitxCfg.Language, nil
	case "connection.proxy":
		return cfg.Connection.Proxy, nil
	case "connection.credential_name":
		return cfg.Connection.CredentialName, nil
	case "connection.fallback_env":
		return cfg.Connection.FallbackEnv, nil
	case "live.model", "live_transcribe.model":
		return cfg.LiveTranscribe.Model, nil
	case "live.language", "live_transcribe.language":
		return cfg.LiveTranscribe.Language, nil
	case "live.mode", "live_transcribe.mode":
		return cfg.LiveTranscribe.Mode, nil
	case "live.custom_vocabulary", "live_transcribe.custom_vocabulary":
		return strings.Join(cfg.LiveTranscribe.CustomVocabulary, ","), nil
	case "live.vad_silence_ms", "live_transcribe.vad_silence_ms":
		return strconv.Itoa(cfg.LiveTranscribe.VADSilenceMs), nil
	case "live.vad_threshold", "live_transcribe.vad_threshold":
		return strconv.FormatFloat(cfg.LiveTranscribe.VADThreshold, 'f', -1, 64), nil
	case "post_process.model":
		return cfg.PostProcess.Model, nil
	case "post_process.prefetch":
		return strings.Join(cfg.PostProcess.Prefetch, ","), nil
	}

	if action, ok := parseActionKey(k, "model.", "post_process.models."); ok {
		if !isKnownOrConfiguredAction(cfg, action) {
			return "", fmt.Errorf("unknown action %q", action)
		}
		return cfg.ModelForAction(action), nil
	}
	if action, ok := parseActionKey(k, "prompt.", "prompts."); ok {
		val, exists := cfg.Prompts[action]
		if !exists {
			return "", fmt.Errorf("unknown prompt action %q", action)
		}
		return val, nil
	}
	return "", fmt.Errorf("unknown config key %q", key)
}

func cmdConfigSet(args []string, deps cliDeps) int {
	fs := flag.NewFlagSet("config set", flag.ContinueOnError)
	fs.SetOutput(deps.stderr)
	configPath := fs.String("config", defaultConfigPath(), "Path to config.toml")
	fcitxPath := fs.String("fcitx-config", DefaultFcitxConfigPath(), "Path to Fcitx5 gemini-voice.conf")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 2 {
		_, _ = fmt.Fprintln(deps.stderr, "usage: gemini-voice config set [-config PATH] [-fcitx-config PATH] <key> <value>")
		return 2
	}

	cfg, err := loadConfigOrDefault(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: load config %q: %v\n", *configPath, err)
		return 1
	}
	fcitxCfg, err := LoadFcitxAddonConfig(*fcitxPath)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}

	key := strings.TrimSpace(fs.Arg(0))
	val := fs.Arg(1)
	daemonDirty, fcitxDirty, err := setConfigKey(&cfg, &fcitxCfg, key, val)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}

	if daemonDirty {
		if err := SaveConfigFile(*configPath, cfg); err != nil {
			_, _ = fmt.Fprintf(deps.stderr, "error: save %q: %v\n", *configPath, err)
			return 1
		}
		_, _ = fmt.Fprintf(deps.stdout, "Updated %s in %s\n", key, *configPath)
	}
	if fcitxDirty {
		if err := SaveFcitxAddonConfig(*fcitxPath, fcitxCfg); err != nil {
			_, _ = fmt.Fprintf(deps.stderr, "error: save %q: %v\n", *fcitxPath, err)
			return 1
		}
		reloaded := ReloadFcitx5Config(context.Background(), deps)
		if reloaded {
			_, _ = fmt.Fprintf(deps.stdout, "Updated %s in %s (reloaded Fcitx5)\n", key, *fcitxPath)
		} else {
			_, _ = fmt.Fprintf(deps.stdout, "Updated %s in %s\n", key, *fcitxPath)
		}
	}
	return 0
}

func parseActionKey(k string, prefixes ...string) (string, bool) {
	for _, p := range prefixes {
		if strings.HasPrefix(k, p) {
			action := strings.TrimSpace(strings.TrimPrefix(k, p))
			if action != "" {
				return action, true
			}
		}
	}
	return "", false
}

func isKnownOrConfiguredAction(cfg Config, action string) bool {
	for _, known := range KnownActions {
		if action == known {
			return true
		}
	}
	_, ok := cfg.Prompts[action]
	return ok
}

func validateProxyURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.EqualFold(trimmed, "none") || trimmed == "-" {
		return "", nil
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("invalid proxy URL %q: %w", RedactURL(trimmed), err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", fmt.Errorf("unsupported proxy scheme %q (expected http, https, socks5, socks5h)", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid proxy URL %q: missing host", RedactURL(trimmed))
	}
	return trimmed, nil
}

func parseCommaList(raw string) ([]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.EqualFold(trimmed, "none") || trimmed == "-" {
		return []string{}, nil
	}
	parts := strings.Split(trimmed, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		item := strings.TrimSpace(p)
		if item == "" {
			return nil, errors.New("list contains an empty entry")
		}
		if _, dup := seen[item]; dup {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out, nil
}

func setConfigKey(cfg *Config, fcitxCfg *FcitxAddonConfig, key, val string) (daemonDirty bool, fcitxDirty bool, err error) {
	switch key {
	case "ime.mode":
		switch strings.ToLower(strings.TrimSpace(val)) {
		case "menu":
			fcitxCfg.ShowActionMenu = true
		case "direct":
			fcitxCfg.ShowActionMenu = false
		default:
			return false, false, fmt.Errorf("invalid ime.mode %q (expected menu or direct)", val)
		}
		return false, true, nil
	case "ime.show_action_menu":
		b, parseErr := strconv.ParseBool(strings.TrimSpace(val))
		if parseErr != nil {
			return false, false, fmt.Errorf("invalid boolean %q for %s", val, key)
		}
		fcitxCfg.ShowActionMenu = b
		return false, true, nil
	case "ime.trigger_key":
		keys, listErr := parseCommaList(val)
		if listErr != nil || len(keys) == 0 {
			return false, false, errors.New("ime.trigger_key requires at least one key (e.g. Alt+space)")
		}
		fcitxCfg.TriggerKeys = keys
		return false, true, nil
	case "ime.socket_path":
		trimmed := strings.TrimSpace(val)
		if trimmed == "" || strings.EqualFold(trimmed, "default") {
			trimmed = DefaultFcitxSocketPath
		}
		fcitxCfg.SocketPath = trimmed
		return false, true, nil
	case "ime.language":
		trimmed := strings.TrimSpace(val)
		if strings.EqualFold(trimmed, "none") || trimmed == "-" {
			trimmed = ""
		}
		fcitxCfg.Language = trimmed
		return false, true, nil
	case "connection.proxy":
		p, pErr := validateProxyURL(val)
		if pErr != nil {
			return false, false, pErr
		}
		cfg.Connection.Proxy = p
		return true, false, nil
	case "connection.credential_name":
		trimmed := strings.TrimSpace(val)
		if vErr := validateCredentialName(trimmed); vErr != nil {
			return false, false, vErr
		}
		cfg.Connection.CredentialName = trimmed
		return true, false, nil
	case "connection.fallback_env":
		trimmed := strings.TrimSpace(val)
		if trimmed == "" || strings.ContainsAny(trimmed, " \t=") {
			return false, false, fmt.Errorf("invalid environment variable name %q", val)
		}
		cfg.Connection.FallbackEnv = trimmed
		return true, false, nil
	case "live.model", "live_transcribe.model":
		trimmed := strings.TrimSpace(val)
		if trimmed == "" {
			return false, false, errors.New("live.model cannot be empty")
		}
		cfg.LiveTranscribe.Model = trimmed
		return true, false, nil
	case "live.language", "live_transcribe.language":
		trimmed := strings.TrimSpace(val)
		if trimmed == "" {
			return false, false, errors.New("live.language cannot be empty")
		}
		cfg.LiveTranscribe.Language = trimmed
		return true, false, nil
	case "live.mode", "live_transcribe.mode":
		if strings.TrimSpace(val) == "" {
			return false, false, errors.New("live.mode cannot be empty (expected VERBATIM or SMART)")
		}
		mode, mErr := NormalizeLiveMode(val)
		if mErr != nil {
			return false, false, mErr
		}
		cfg.LiveTranscribe.Mode = mode
		return true, false, nil
	case "live.custom_vocabulary", "live_transcribe.custom_vocabulary":
		terms, listErr := parseCommaList(val)
		if listErr != nil {
			return false, false, fmt.Errorf("invalid custom_vocabulary list: %w", listErr)
		}
		if len(terms) > MaxCustomVocabulary {
			return false, false, fmt.Errorf("custom_vocabulary has %d entries (max %d)", len(terms), MaxCustomVocabulary)
		}
		if len(terms) == 0 {
			cfg.LiveTranscribe.CustomVocabulary = nil
		} else {
			cfg.LiveTranscribe.CustomVocabulary = terms
		}
		return true, false, nil
	case "live.vad_silence_ms", "live_transcribe.vad_silence_ms":
		ms, convErr := strconv.Atoi(strings.TrimSpace(val))
		if convErr != nil || ms < 0 {
			return false, false, fmt.Errorf("invalid vad_silence_ms %q (must be integer >= 0)", val)
		}
		cfg.LiveTranscribe.VADSilenceMs = ms
		return true, false, nil
	case "live.vad_threshold", "live_transcribe.vad_threshold":
		th, convErr := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if convErr != nil || th <= 0 {
			return false, false, fmt.Errorf("invalid vad_threshold %q (must be > 0)", val)
		}
		cfg.LiveTranscribe.VADThreshold = th
		return true, false, nil
	case "post_process.model":
		trimmed := strings.TrimSpace(val)
		if trimmed == "" {
			return false, false, errors.New("post_process.model cannot be empty")
		}
		cfg.PostProcess.Model = trimmed
		return true, false, nil
	case "post_process.prefetch":
		actions, listErr := parseCommaList(val)
		if listErr != nil {
			return false, false, fmt.Errorf("invalid prefetch list: %w", listErr)
		}
		for _, a := range actions {
			if !isKnownOrConfiguredAction(*cfg, a) {
				return false, false, fmt.Errorf("unknown action %q in prefetch list", a)
			}
		}
		cfg.PostProcess.Prefetch = actions
		return true, false, nil
	}

	if action, ok := parseActionKey(key, "model.", "post_process.models."); ok {
		if !isKnownOrConfiguredAction(*cfg, action) {
			return false, false, fmt.Errorf("unknown action %q", action)
		}
		setActionModelOverride(cfg, action, val)
		return true, false, nil
	}
	if action, ok := parseActionKey(key, "prompt.", "prompts."); ok {
		if strings.ContainsAny(action, " \t\r\n") {
			return false, false, fmt.Errorf("invalid action name %q", action)
		}
		trimmed := strings.TrimSpace(val)
		if trimmed == "" {
			return false, false, fmt.Errorf("prompt for %q cannot be empty", action)
		}
		if cfg.Prompts == nil {
			cfg.Prompts = make(map[string]string)
		}
		cfg.Prompts[action] = trimmed
		return true, false, nil
	}

	return false, false, fmt.Errorf("unknown config key %q", key)
}

func setActionModelOverride(cfg *Config, action, val string) {
	trimmed := strings.TrimSpace(val)
	if trimmed == "" || strings.EqualFold(trimmed, "default") || trimmed == "-" {
		delete(cfg.PostProcess.Models, action)
		return
	}
	if cfg.PostProcess.Models == nil {
		cfg.PostProcess.Models = make(map[string]string)
	}
	cfg.PostProcess.Models[action] = trimmed
}

func cmdModels(args []string, deps cliDeps) int {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	fs.SetOutput(deps.stderr)
	configPath := fs.String("config", defaultConfigPath(), "Path to config.toml")
	remote := fs.Bool("remote", false, "Query Gemini API /v1beta/models using configured credentials")
	liveModel := fs.String("live", "", "Set live transcription model")
	defaultModel := fs.String("default", "", "Set default post-processing model")
	fixTypos := fs.String("refine-fix-typos", "", "Set model override for refine_fix_typos (\"default\" clears)")
	casual := fs.String("refine-casual", "", "Set model override for refine_casual (\"default\" clears)")
	formal := fs.String("refine-formal", "", "Set model override for refine_formal (\"default\" clears)")
	translateEn := fs.String("translate-en", "", "Set model override for translate_en (\"default\" clears)")
	asPrompt := fs.String("as-prompt", "", "Set model override for as_prompt (\"default\" clears)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	visited := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		visited[f.Name] = true
	})

	cfg, err := loadConfigOrDefault(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: load config %q: %v\n", *configPath, err)
		return 1
	}

	dirty := false
	if visited["live"] {
		if strings.TrimSpace(*liveModel) == "" {
			_, _ = fmt.Fprintln(deps.stderr, "error: --live cannot be empty")
			return 1
		}
		cfg.LiveTranscribe.Model = strings.TrimSpace(*liveModel)
		dirty = true
	}
	if visited["default"] {
		if strings.TrimSpace(*defaultModel) == "" {
			_, _ = fmt.Fprintln(deps.stderr, "error: --default cannot be empty")
			return 1
		}
		cfg.PostProcess.Model = strings.TrimSpace(*defaultModel)
		dirty = true
	}

	actionFlags := []struct {
		flagName string
		action   string
		val      string
	}{
		{"refine-fix-typos", "refine_fix_typos", *fixTypos},
		{"refine-casual", "refine_casual", *casual},
		{"refine-formal", "refine_formal", *formal},
		{"translate-en", "translate_en", *translateEn},
		{"as-prompt", "as_prompt", *asPrompt},
	}
	for _, af := range actionFlags {
		if visited[af.flagName] {
			setActionModelOverride(&cfg, af.action, af.val)
			dirty = true
		}
	}

	if dirty {
		if err := SaveConfigFile(*configPath, cfg); err != nil {
			_, _ = fmt.Fprintf(deps.stderr, "error: save %q: %v\n", *configPath, err)
			return 1
		}
		_, _ = fmt.Fprintf(deps.stdout, "Saved model configuration to %s\n\n", *configPath)
	}

	_, _ = fmt.Fprintf(deps.stdout, "Configured models (%s):\n", *configPath)
	printModelTable(deps.stdout, cfg)

	if *remote {
		apiKey, keyErr := LoadAPIKey(cfg.Connection.CredentialName, cfg.Connection.FallbackEnv)
		if keyErr != nil {
			_, _ = fmt.Fprintf(deps.stderr, "\nerror: load API key for --remote query: %v\n", keyErr)
			return 1
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		models, listErr := ListRemoteModels(ctx, cfg, apiKey)
		if listErr != nil {
			_, _ = fmt.Fprintf(deps.stderr, "\nerror: list remote models: %v\n", listErr)
			return 1
		}
		_, _ = fmt.Fprintf(deps.stdout, "\nAvailable Gemini models (%d):\n", len(models))
		for _, m := range models {
			var caps []string
			if m.SupportsLive {
				caps = append(caps, "live")
			}
			if m.SupportsGenerate {
				caps = append(caps, "generate")
			}
			_, _ = fmt.Fprintf(deps.stdout, "  %-34s [%s]\n", m.Name, strings.Join(caps, ", "))
		}
	}

	return 0
}

func cmdConfigure(args []string, deps cliDeps) int {
	fs := flag.NewFlagSet("configure", flag.ContinueOnError)
	fs.SetOutput(deps.stderr)
	configPath := fs.String("config", defaultConfigPath(), "Path to config.toml")
	fcitxPath := fs.String("fcitx-config", DefaultFcitxConfigPath(), "Path to Fcitx5 gemini-voice.conf")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if err := runConfigureWizard(*configPath, *fcitxPath, deps); err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

// readPromptLine reads one line from r byte-by-byte so unbuffered subsequent reads
// (such as hidden password entry or stdin secrets) never lose buffered bytes.
func readPromptLine(r io.Reader) (string, error) {
	var buf [1]byte
	var line strings.Builder
	for {
		n, err := r.Read(buf[:])
		if n > 0 {
			if buf[0] == '\n' {
				return strings.TrimRight(line.String(), "\r"), nil
			}
			line.WriteByte(buf[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) && line.Len() > 0 {
				return strings.TrimRight(line.String(), "\r"), nil
			}
			return strings.TrimRight(line.String(), "\r"), err
		}
	}
}

func promptValue(r io.Reader, w io.Writer, label, defaultVal string) (string, error) {
	if defaultVal != "" {
		_, _ = fmt.Fprintf(w, "%s [%s]: ", label, defaultVal)
	} else {
		_, _ = fmt.Fprintf(w, "%s: ", label)
	}
	line, err := readPromptLine(r)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return defaultVal, nil
	}
	return trimmed, nil
}

func runConfigureWizard(configPath, fcitxConfigPath string, deps cliDeps) error {
	cfg, err := loadConfigOrDefault(configPath)
	if err != nil {
		return fmt.Errorf("load config %q: %w", configPath, err)
	}
	fcitxCfg, err := LoadFcitxAddonConfig(fcitxConfigPath)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintln(deps.stderr, "=== Fcitx5 Gemini Voice Configuration ===")

	defaultMode := "1"
	if !fcitxCfg.ShowActionMenu {
		defaultMode = "2"
	}
	_, _ = fmt.Fprintln(deps.stderr, "Select input behavior after speech transcription:")
	_, _ = fmt.Fprintln(deps.stderr, "  1) Action menu (choose Raw, Refine, Translate, or As Prompt)")
	_, _ = fmt.Fprintln(deps.stderr, "  2) Direct commit (insert raw transcription immediately)")
	for {
		ans, pErr := promptValue(deps.stdin, deps.stderr, "Input mode (1 or 2)", defaultMode)
		if pErr != nil {
			return pErr
		}
		switch strings.ToLower(ans) {
		case "1", "menu":
			fcitxCfg.ShowActionMenu = true
		case "2", "direct":
			fcitxCfg.ShowActionMenu = false
		default:
			_, _ = fmt.Fprintln(deps.stderr, "Please enter 1 or 2.")
			continue
		}
		break
	}

	lang, err := promptValue(deps.stdin, deps.stderr, "Transcription language (auto, cmn-Hans-CN, en-US, cmn-Hans-CN,en-US)", cfg.LiveTranscribe.Language)
	if err != nil {
		return err
	}
	if strings.TrimSpace(lang) != "" {
		cfg.LiveTranscribe.Language = strings.TrimSpace(lang)
	}

	for {
		vadStr, pErr := promptValue(deps.stdin, deps.stderr, "VAD silence auto-stop ms (0 disables)", strconv.Itoa(cfg.LiveTranscribe.VADSilenceMs))
		if pErr != nil {
			return pErr
		}
		vadMs, convErr := strconv.Atoi(strings.TrimSpace(vadStr))
		if convErr != nil || vadMs < 0 {
			_, _ = fmt.Fprintln(deps.stderr, "Please enter a non-negative integer.")
			continue
		}
		cfg.LiveTranscribe.VADSilenceMs = vadMs
		break
	}

	liveModel, err := promptValue(deps.stdin, deps.stderr, "Live transcription model", cfg.LiveTranscribe.Model)
	if err != nil {
		return err
	}
	if strings.TrimSpace(liveModel) != "" {
		cfg.LiveTranscribe.Model = strings.TrimSpace(liveModel)
	}

	defaultPPModel, err := promptValue(deps.stdin, deps.stderr, "Default post-processing model (refine/translate/prompt)", cfg.PostProcess.Model)
	if err != nil {
		return err
	}
	if strings.TrimSpace(defaultPPModel) != "" {
		cfg.PostProcess.Model = strings.TrimSpace(defaultPPModel)
	}

	overridePromptDefault := "N"
	if len(cfg.PostProcess.Models) > 0 {
		overridePromptDefault = "y"
	}
	overrideAns, err := promptValue(deps.stdin, deps.stderr, "Configure per-action model overrides? (y/N)", overridePromptDefault)
	if err != nil {
		return err
	}
	if strings.HasPrefix(strings.ToLower(overrideAns), "y") {
		_, _ = fmt.Fprintln(deps.stderr, "Enter model name for each action (or 'default'/'-' to inherit default):")
		for _, action := range cfg.EffectiveActionNames() {
			cur := cfg.PostProcess.Models[action]
			if cur == "" {
				cur = "default (" + cfg.PostProcess.Model + ")"
			}
			val, pErr := promptValue(deps.stdin, deps.stderr, "  "+action, cur)
			if pErr != nil {
				return pErr
			}
			if val != cur {
				setActionModelOverride(&cfg, action, val)
			}
		}
	}

	if fcitxCfg.ShowActionMenu {
		curPrefetch := strings.Join(cfg.PostProcess.Prefetch, ",")
		if curPrefetch == "" {
			curPrefetch = "none"
		}
		for {
			pfAns, pErr := promptValue(deps.stdin, deps.stderr, "Prefetch actions on stop (comma-separated, or 'none')", curPrefetch)
			if pErr != nil {
				return pErr
			}
			if _, _, setErr := setConfigKey(&cfg, &fcitxCfg, "post_process.prefetch", pfAns); setErr != nil {
				_, _ = fmt.Fprintf(deps.stderr, "Invalid prefetch list: %v\n", setErr)
				continue
			}
			break
		}
	}

	for {
		curTrig := strings.Join(fcitxCfg.TriggerKeys, ",")
		trigAns, pErr := promptValue(deps.stdin, deps.stderr, "Fcitx5 trigger key(s) (comma-separated)", curTrig)
		if pErr != nil {
			return pErr
		}
		if _, _, setErr := setConfigKey(&cfg, &fcitxCfg, "ime.trigger_key", trigAns); setErr != nil {
			_, _ = fmt.Fprintf(deps.stderr, "Invalid trigger key: %v\n", setErr)
			continue
		}
		break
	}

	for {
		curProxy := cfg.Connection.Proxy
		if curProxy == "" {
			curProxy = "none"
		}
		proxyAns, pErr := promptValue(deps.stdin, deps.stderr, "HTTP/SOCKS5 proxy URL (or 'none')", curProxy)
		if pErr != nil {
			return pErr
		}
		if proxyAns == "none" && cfg.Connection.Proxy == "" {
			break
		}
		if _, _, setErr := setConfigKey(&cfg, &fcitxCfg, "connection.proxy", proxyAns); setErr != nil {
			_, _ = fmt.Fprintf(deps.stderr, "Invalid proxy URL: %v\n", setErr)
			continue
		}
		break
	}

	if err := SaveConfigFile(configPath, cfg); err != nil {
		return fmt.Errorf("save daemon config %q: %w", configPath, err)
	}
	if err := SaveFcitxAddonConfig(fcitxConfigPath, fcitxCfg); err != nil {
		return fmt.Errorf("save fcitx5 config %q: %w", fcitxConfigPath, err)
	}
	reloaded := ReloadFcitx5Config(context.Background(), deps)

	_, _ = fmt.Fprintf(deps.stdout, "\nSaved daemon config: %s\n", configPath)
	if reloaded {
		_, _ = fmt.Fprintf(deps.stdout, "Saved Fcitx5 config: %s (reloaded Fcitx5)\n", fcitxConfigPath)
	} else {
		_, _ = fmt.Fprintf(deps.stdout, "Saved Fcitx5 config: %s\n", fcitxConfigPath)
	}
	return nil
}

// Keep unused import check clean if sort is used via EffectiveActionNames.
var _ = sort.Strings
var _ = filepath.Clean
var _ = os.ErrNotExist
