package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
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
	if legacy.PostProcess.Prefetch != nil && !hasList(pathLegacyPrefetch) {
		enabled := map[string]bool{}
		for _, name := range trimmedNonEmpty(*legacy.PostProcess.Prefetch) {
			if g, ok := groupForAction(name); ok {
				enabled[g] = true
			}
		}
		for _, g := range actionGroups {
			if has(groupPath(g, "Prefetch")) {
				continue
			}
			grp := cfg.PostProcess.Groups[g]
			grp.Prefetch = enabled[g]
			cfg.PostProcess.Groups[g] = grp
		}
	}
	if len(cfg.Ime.TranslateLanguages) == 0 {
		langs := trimmedNonEmpty(legacy.PostProcess.TranslateLanguages)
		// The old default ["en"] is also the built-in fallback; do not pin it.
		if len(langs) > 1 || (len(langs) == 1 && !strings.EqualFold(langs[0], "en")) {
			cfg.Ime.TranslateLanguages = langs
		}
	}
	// Models and prompts of the built-in actions go to their fixed group; the
	// rest (translate_<lang>, custom names) to the advanced Actions list.
	// Prompts equal to the built-in default (the old default template listed
	// them all) are not pinned.
	actionsInConf := len(iniStructIndices(lines, pathPostActions)) > 0
	for action, m := range legacy.PostProcess.Models {
		trimmed := strings.TrimSpace(m)
		if trimmed == "" {
			continue
		}
		if isGroupAction(action) {
			if !has(groupPath(action, "Model")) {
				grp := cfg.PostProcess.Groups[action]
				grp.Model = trimmed
				cfg.PostProcess.Groups[action] = grp
			}
		} else if !actionsInConf {
			if cfg.PostProcess.Models == nil {
				cfg.PostProcess.Models = make(map[string]string)
			}
			cfg.PostProcess.Models[action] = trimmed
		}
	}
	for action, prompt := range legacy.Prompts {
		if strings.TrimSpace(prompt) == "" || strings.TrimSpace(prompt) == strings.TrimSpace(defaultPrompts[action]) {
			continue
		}
		if isGroupAction(action) {
			if !has(groupPath(action, "Prompt")) {
				grp := cfg.PostProcess.Groups[action]
				grp.Prompt = prompt
				cfg.PostProcess.Groups[action] = grp
			}
		} else if !actionsInConf {
			if cfg.Prompts == nil {
				cfg.Prompts = make(map[string]string)
			}
			cfg.Prompts[action] = prompt
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
