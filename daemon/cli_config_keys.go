package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

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
	case "ime.recording_prefix":
		trimmed := strings.TrimSpace(val)
		if strings.EqualFold(trimmed, "none") || trimmed == "-" {
			fcitxCfg.RecordingPrefix = ""
		} else if strings.EqualFold(trimmed, "default") {
			fcitxCfg.RecordingPrefix = DefaultRecordingPrefix
		} else {
			fcitxCfg.RecordingPrefix = val
		}
		return false, true, nil
	case "ime.preedit_max_width":
		w, parseErr := strconv.Atoi(strings.TrimSpace(val))
		if parseErr != nil || w < 0 || w > 500 {
			return false, false, fmt.Errorf("invalid ime.preedit_max_width %q (expected integer 0..500)", val)
		}
		fcitxCfg.PreeditMaxWidth = w
		return false, true, nil
	case "ime.translate_languages":
		trimmed := strings.TrimSpace(val)
		if trimmed == "" || strings.EqualFold(trimmed, "default") || strings.EqualFold(trimmed, "none") || trimmed == "-" {
			fcitxCfg.TranslateLanguages = nil
			return false, true, nil
		}
		langs, listErr := parseCommaList(val)
		if listErr != nil || len(langs) == 0 {
			return false, false, errors.New("ime.translate_languages requires at least one language (or \"default\" to follow daemon)")
		}
		fcitxCfg.TranslateLanguages = langs
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
	case "post_process.translate_languages", "translate.languages":
		if strings.EqualFold(strings.TrimSpace(val), "default") {
			cfg.PostProcess.TranslateLanguages = []string{"en"}
			return true, false, nil
		}
		langs, listErr := parseCommaList(val)
		if listErr != nil {
			return false, false, fmt.Errorf("invalid translate_languages list: %w", listErr)
		}
		if len(langs) == 0 {
			return false, false, errors.New("post_process.translate_languages requires at least one language (e.g. en)")
		}
		cfg.PostProcess.TranslateLanguages = normalizeTranslateLanguages(langs)
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
