package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// setConfigKey applies one "config set" key to cfg. imeDirty reports that a
// key consumed by the Fcitx5 addon itself changed (so Fcitx5 must reload the
// conf); every other key is picked up by the running daemon's file watcher.
func setConfigKey(cfg *Config, key, val string) (imeDirty bool, err error) {
	fcitxCfg := &cfg.Ime
	switch key {
	case "ime.mode":
		switch strings.ToLower(strings.TrimSpace(val)) {
		case "menu":
			fcitxCfg.ShowActionMenu = true
		case "direct":
			fcitxCfg.ShowActionMenu = false
		default:
			return false, fmt.Errorf("invalid ime.mode %q (expected menu or direct)", val)
		}
		return true, nil
	case "ime.show_action_menu":
		b, parseErr := strconv.ParseBool(strings.TrimSpace(val))
		if parseErr != nil {
			return false, fmt.Errorf("invalid boolean %q for %s", val, key)
		}
		fcitxCfg.ShowActionMenu = b
		return true, nil
	case "ime.trigger_key":
		keys, listErr := parseCommaList(val)
		if listErr != nil || len(keys) == 0 {
			return false, errors.New("ime.trigger_key requires at least one key (e.g. Alt+space)")
		}
		fcitxCfg.TriggerKeys = keys
		return true, nil
	case "ime.socket_path":
		trimmed := strings.TrimSpace(val)
		if trimmed == "" || strings.EqualFold(trimmed, "default") {
			trimmed = DefaultFcitxSocketPath
		}
		fcitxCfg.SocketPath = trimmed
		return true, nil
	case "ime.language":
		trimmed := strings.TrimSpace(val)
		if strings.EqualFold(trimmed, "none") || trimmed == "-" {
			trimmed = ""
		}
		fcitxCfg.Language = trimmed
		return true, nil
	case "ime.recording_prefix":
		trimmed := strings.TrimSpace(val)
		if strings.EqualFold(trimmed, "none") || trimmed == "-" {
			fcitxCfg.RecordingPrefix = ""
		} else if strings.EqualFold(trimmed, "default") {
			fcitxCfg.RecordingPrefix = DefaultRecordingPrefix
		} else {
			fcitxCfg.RecordingPrefix = val
		}
		return true, nil
	case "ime.preedit_max_width":
		w, parseErr := strconv.Atoi(strings.TrimSpace(val))
		if parseErr != nil || w < 0 || w > 500 {
			return false, fmt.Errorf("invalid ime.preedit_max_width %q (expected integer 0..500)", val)
		}
		fcitxCfg.PreeditMaxWidth = w
		return true, nil
	case "connection.proxy":
		p, pErr := validateProxyURL(val)
		if pErr != nil {
			return false, pErr
		}
		cfg.Connection.Proxy = p
		return false, nil
	case "live.model", "live_transcribe.model":
		trimmed := strings.TrimSpace(val)
		if trimmed == "" {
			return false, errors.New("live.model cannot be empty")
		}
		if err := ValidateLiveModel(trimmed); err != nil {
			return false, err
		}
		cfg.LiveTranscribe.Model = trimmed
		return false, nil
	case "live.language", "live_transcribe.language":
		trimmed := strings.TrimSpace(val)
		if trimmed == "" {
			return false, errors.New("live.language cannot be empty")
		}
		if strings.EqualFold(trimmed, DefaultLiveLanguage) {
			trimmed = "" // "auto" is the default; keep the conf free of it
		}
		cfg.Ime.Language = trimmed
		return true, nil
	case "live.mode", "live_transcribe.mode":
		if strings.TrimSpace(val) == "" {
			return false, errors.New("live.mode cannot be empty (expected VERBATIM or SMART)")
		}
		mode, mErr := NormalizeLiveMode(val)
		if mErr != nil {
			return false, mErr
		}
		cfg.LiveTranscribe.Mode = mode
		return false, nil
	case "live.custom_vocabulary", "live_transcribe.custom_vocabulary":
		terms, listErr := parseCommaList(val)
		if listErr != nil {
			return false, fmt.Errorf("invalid custom_vocabulary list: %w", listErr)
		}
		if len(terms) > MaxCustomVocabulary {
			return false, fmt.Errorf("custom_vocabulary has %d entries (max %d)", len(terms), MaxCustomVocabulary)
		}
		if len(terms) == 0 {
			cfg.LiveTranscribe.CustomVocabulary = nil
		} else {
			cfg.LiveTranscribe.CustomVocabulary = terms
		}
		return false, nil
	case "live.vad_silence_ms", "live_transcribe.vad_silence_ms":
		ms, convErr := strconv.Atoi(strings.TrimSpace(val))
		if convErr != nil || ms < 0 {
			return false, fmt.Errorf("invalid vad_silence_ms %q (must be integer >= 0)", val)
		}
		cfg.LiveTranscribe.VADSilenceMs = ms
		return false, nil
	case "post_process.model":
		trimmed := strings.TrimSpace(val)
		if trimmed == "" {
			return false, errors.New("post_process.model cannot be empty")
		}
		if err := ValidatePostProcessModel(trimmed); err != nil {
			return false, err
		}
		cfg.PostProcess.Model = trimmed
		return false, nil
	case "post_process.prefetch":
		actions, listErr := parseCommaList(val)
		if listErr != nil {
			return false, fmt.Errorf("invalid prefetch list: %w", listErr)
		}
		for _, a := range actions {
			if !isKnownOrConfiguredAction(*cfg, a) {
				return false, fmt.Errorf("unknown action %q in prefetch list", a)
			}
		}
		cfg.PostProcess.Prefetch = actions
		return false, nil
	case "ime.translate_languages", "post_process.translate_languages", "translate.languages":
		trimmed := strings.TrimSpace(val)
		if trimmed == "" || strings.EqualFold(trimmed, "default") || strings.EqualFold(trimmed, "none") || trimmed == "-" {
			fcitxCfg.TranslateLanguages = nil
			return true, nil
		}
		langs, listErr := parseCommaList(val)
		if listErr != nil {
			return false, fmt.Errorf("invalid translate_languages list: %w", listErr)
		}
		langs = normalizeTranslateLanguages(langs)
		if len(langs) == 1 && strings.EqualFold(langs[0], "en") {
			langs = nil // English only is the built-in default
		}
		fcitxCfg.TranslateLanguages = langs
		return true, nil
	}

	if action, ok := parseActionKey(key, "model.", "post_process.models."); ok {
		if !isKnownOrConfiguredAction(*cfg, action) {
			return false, fmt.Errorf("unknown action %q", action)
		}
		if err := setActionModelOverride(cfg, action, val); err != nil {
			return false, err
		}
		return false, nil
	}
	if action, ok := parseActionKey(key, "prompt.", "prompts."); ok {
		if strings.ContainsAny(action, " \t\r\n") {
			return false, fmt.Errorf("invalid action name %q", action)
		}
		trimmed := strings.TrimSpace(val)
		if trimmed == "" {
			return false, fmt.Errorf("prompt for %q cannot be empty", action)
		}
		if cfg.Prompts == nil {
			cfg.Prompts = make(map[string]string)
		}
		cfg.Prompts[action] = trimmed
		return false, nil
	}

	return false, fmt.Errorf("unknown config key %q", key)
}

func setActionModelOverride(cfg *Config, action, val string) error {
	trimmed := strings.TrimSpace(val)
	if trimmed == "" || strings.EqualFold(trimmed, "default") || trimmed == "-" {
		delete(cfg.PostProcess.Models, action)
		return nil
	}
	if err := ValidatePostProcessModel(trimmed); err != nil {
		return err
	}
	if cfg.PostProcess.Models == nil {
		cfg.PostProcess.Models = make(map[string]string)
	}
	cfg.PostProcess.Models[action] = trimmed
	return nil
}
