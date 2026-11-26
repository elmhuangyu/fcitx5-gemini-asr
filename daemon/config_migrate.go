package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// legacyConfig mirrors the retired ~/.config/gemini-voice/config.toml. It is
// only read once, by MigrateLegacyConfig.
type legacyConfig struct {
	Connection struct {
		Proxy      string `toml:"proxy"`
		LiveWSURL  string `toml:"live_ws_url"`
		APIBaseURL string `toml:"api_base_url"`
	} `toml:"connection"`
	LiveTranscribe struct {
		Model            string   `toml:"model"`
		Language         string   `toml:"language"`
		Mode             string   `toml:"mode"`
		CustomVocabulary []string `toml:"custom_vocabulary"`
		VADSilenceMs     *int     `toml:"vad_silence_ms"`
	} `toml:"live_transcribe"`
	PostProcess struct {
		Model              string            `toml:"model"`
		Models             map[string]string `toml:"models"`
		Prefetch           *[]string         `toml:"prefetch"`
		TranslateLanguages []string          `toml:"translate_languages"`
	} `toml:"post_process"`
	Prompts map[string]string `toml:"prompts"`
}

// applyLegacyConfig overlays the values of a legacy config.toml onto cfg, one
// setting at a time and only where the conf (lines) does not already set it,
// so anything configured in the new format wins. For settings that existed in
// both places (language, translation targets) the addon's conf value won
// before as well.
func applyLegacyConfig(cfg *Config, lines []iniLine, legacy legacyConfig) {
	has := func(path string) bool {
		_, ok := iniScalar(lines, path)
		return ok
	}
	hasList := func(path string) bool {
		_, ok := iniList(lines, path)
		return ok
	}

	if !has(pathConnProxy) {
		cfg.Connection.Proxy = strings.TrimSpace(legacy.Connection.Proxy)
	}
	if !has(pathConnLiveWSURL) {
		cfg.Connection.LiveWSURL = strings.TrimSpace(legacy.Connection.LiveWSURL)
	}
	if !has(pathConnAPIBaseURL) {
		cfg.Connection.APIBaseURL = strings.TrimSpace(legacy.Connection.APIBaseURL)
	}

	if m := strings.TrimSpace(legacy.LiveTranscribe.Model); m != "" && !has(pathLiveModel) {
		cfg.LiveTranscribe.Model = m
	}
	if mode, err := NormalizeLiveMode(legacy.LiveTranscribe.Mode); err == nil && !has(pathLiveMode) {
		cfg.LiveTranscribe.Mode = mode
	}
	if !hasList(pathLiveVocabulary) {
		cfg.LiveTranscribe.CustomVocabulary = pruneCustomVocabulary(legacy.LiveTranscribe.CustomVocabulary)
	}
	if legacy.LiveTranscribe.VADSilenceMs != nil && *legacy.LiveTranscribe.VADSilenceMs >= 0 && !has(pathLiveVAD) {
		cfg.LiveTranscribe.VADSilenceMs = *legacy.LiveTranscribe.VADSilenceMs
	}
	if lang := strings.TrimSpace(legacy.LiveTranscribe.Language); cfg.Ime.Language == "" && lang != "" && !strings.EqualFold(lang, DefaultLiveLanguage) {
		cfg.Ime.Language = lang
	}

	if m := strings.TrimSpace(legacy.PostProcess.Model); m != "" && !has(pathPostModel) {
		cfg.PostProcess.Model = m
	}
	if legacy.PostProcess.Prefetch != nil && !hasList(pathPostPrefetch) {
		cfg.PostProcess.Prefetch = trimmedNonEmpty(*legacy.PostProcess.Prefetch)
	}
	if len(cfg.Ime.TranslateLanguages) == 0 {
		langs := trimmedNonEmpty(legacy.PostProcess.TranslateLanguages)
		// The old default ["en"] is also the built-in fallback; do not pin it.
		if len(langs) > 1 || (len(langs) == 1 && !strings.EqualFold(langs[0], "en")) {
			cfg.Ime.TranslateLanguages = langs
		}
	}
	if len(iniStructIndices(lines, pathPostActions)) == 0 {
		for action, m := range legacy.PostProcess.Models {
			if trimmed := strings.TrimSpace(m); trimmed != "" {
				if cfg.PostProcess.Models == nil {
					cfg.PostProcess.Models = make(map[string]string)
				}
				cfg.PostProcess.Models[action] = trimmed
			}
		}
		for action, prompt := range legacy.Prompts {
			if strings.TrimSpace(prompt) != "" {
				cfg.Prompts[action] = prompt
			}
		}
	}
}

// MigrateLegacyConfig performs the one-time import of the retired config.toml
// into gemini-voice.conf. When tomlPath exists it is renamed to
// <tomlPath>.migrated afterwards and never read again. Values are imported
// per setting, only where the conf does not already set them. It reports whether the conf
// was modified. A toml that cannot be parsed or yields an invalid config is
// left in place and an error is returned.
func MigrateLegacyConfig(tomlPath, confPath string) (bool, error) {
	if _, err := os.Stat(tomlPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat legacy config %q: %w", tomlPath, err)
	}

	existing, err := os.ReadFile(confPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("read config %q: %w", confPath, err)
	}
	lines := classifyINILines(splitINILines(string(existing)))

	var legacy legacyConfig
	if _, err := toml.DecodeFile(tomlPath, &legacy); err != nil {
		return false, fmt.Errorf("parse legacy config %q: %w", tomlPath, err)
	}
	cfg, err := configFromINI(lines)
	if err != nil {
		return false, fmt.Errorf("load config %q: %w", confPath, err)
	}
	applyLegacyConfig(&cfg, lines, legacy)
	if err := validateConfig(cfg); err != nil {
		return false, fmt.Errorf("legacy config %q: %w", tomlPath, err)
	}
	merged := mergeConfig(existing, cfg)
	modified := !bytes.Equal(merged, existing)
	if modified {
		if err := writeConfigFileAtomic(confPath, merged, "config"); err != nil {
			return false, fmt.Errorf("write config %q: %w", confPath, err)
		}
	}

	dest := tomlPath + ".migrated"
	if _, err := os.Stat(dest); err == nil {
		dest = fmt.Sprintf("%s.%d", dest, time.Now().Unix())
	}
	if err := os.Rename(tomlPath, dest); err != nil {
		return modified, fmt.Errorf("rename legacy config to %q: %w", dest, err)
	}
	return modified, nil
}

// migrateLegacyIfDefault runs MigrateLegacyConfig when confPath is the default
// conf location (custom paths never touch the user's real legacy config) and
// reloads Fcitx5 when the addon-consumed keys may have changed.
func migrateLegacyIfDefault(confPath string, deps cliDeps, logf func(format string, args ...any)) {
	if filepath.Clean(confPath) != filepath.Clean(DefaultConfigPath()) {
		return
	}
	modified, err := MigrateLegacyConfig(legacyTOMLPath(), confPath)
	if err != nil {
		logf("migrate legacy config %s: %v", legacyTOMLPath(), err)
		return
	}
	if modified {
		logf("migrated settings from %s into %s (old file renamed to config.toml.migrated)", legacyTOMLPath(), confPath)
		ReloadFcitx5Config(context.Background(), deps)
	}
}
