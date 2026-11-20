package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultFcitxSocketPath = "$XDG_RUNTIME_DIR/gemini-ime.sock"
	DefaultRecordingPrefix = "🎙️"
	DefaultPreeditMaxWidth = 80
)

type FcitxAddonConfig struct {
	TriggerKeys        []string
	TranslateLanguages []string
	ShowActionMenu     bool
	SocketPath         string
	Language           string
	RecordingPrefix    string
	PreeditMaxWidth    int
}

func DefaultFcitxAddonConfig() FcitxAddonConfig {
	return FcitxAddonConfig{
		TriggerKeys:        []string{"Alt+space"},
		TranslateLanguages: nil,
		ShowActionMenu:     true,
		SocketPath:         DefaultFcitxSocketPath,
		Language:           "",
		RecordingPrefix:    DefaultRecordingPrefix,
		PreeditMaxWidth:    DefaultPreeditMaxWidth,
	}
}

func DefaultFcitxConfigPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "fcitx5", "conf", "gemini-voice.conf")
	}
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		return filepath.Join(home, ".config", "fcitx5", "conf", "gemini-voice.conf")
	}
	return filepath.Join("fcitx5", "conf", "gemini-voice.conf")
}

func unquoteFcitxValue(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		inner := v[1 : len(v)-1]
		inner = strings.ReplaceAll(inner, `\"`, `"`)
		inner = strings.ReplaceAll(inner, `\\`, `\`)
		return inner
	}
	return v
}

func quoteFcitxValue(v string) string {
	if v == "" {
		return ""
	}
	if strings.ContainsAny(v, " \t#\"\\") {
		escaped := strings.ReplaceAll(v, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		return `"` + escaped + `"`
	}
	return v
}

// LoadFcitxAddonConfig loads Fcitx5's gemini-voice.conf INI file, returning defaults if absent.
func LoadFcitxAddonConfig(path string) (FcitxAddonConfig, error) {
	cfg := DefaultFcitxAddonConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return FcitxAddonConfig{}, fmt.Errorf("read fcitx config %q: %w", path, err)
	}

	type indexedKey struct {
		idx int
		val string
	}
	var triggerEntries []indexedKey
	var translateEntries []indexedKey
	seenTriggerSection := false
	seenTranslateSection := false
	section := ""

	lines := strings.Split(string(data), "\n")
	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			switch section {
			case "TriggerKey":
				seenTriggerSection = true
			case "TranslateLanguages":
				seenTranslateSection = true
			}
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
		v := unquoteFcitxValue(line[eq+1:])

		switch section {
		case "":
			switch k {
			case "ShowActionMenu":
				cfg.ShowActionMenu = strings.EqualFold(v, "True") || strings.EqualFold(v, "true") || v == "1"
			case "SocketPath":
				cfg.SocketPath = v
			case "Language":
				cfg.Language = v
			case "RecordingPrefix":
				cfg.RecordingPrefix = v
			case "PreeditMaxWidth":
				if w, convErr := strconv.Atoi(v); convErr == nil && w >= 0 && w <= 500 {
					cfg.PreeditMaxWidth = w
				}
			}
		case "TriggerKey":
			if idx, convErr := strconv.Atoi(k); convErr == nil && v != "" {
				triggerEntries = append(triggerEntries, indexedKey{idx: idx, val: v})
			}
		case "TranslateLanguages":
			if idx, convErr := strconv.Atoi(k); convErr == nil && v != "" {
				translateEntries = append(translateEntries, indexedKey{idx: idx, val: v})
			}
		}
	}

	if seenTriggerSection && len(triggerEntries) > 0 {
		sort.Slice(triggerEntries, func(i, j int) bool {
			return triggerEntries[i].idx < triggerEntries[j].idx
		})
		keys := make([]string, 0, len(triggerEntries))
		for _, item := range triggerEntries {
			keys = append(keys, item.val)
		}
		cfg.TriggerKeys = keys
	}
	if seenTranslateSection && len(translateEntries) > 0 {
		sort.Slice(translateEntries, func(i, j int) bool {
			return translateEntries[i].idx < translateEntries[j].idx
		})
		langs := make([]string, 0, len(translateEntries))
		for _, item := range translateEntries {
			langs = append(langs, item.val)
		}
		cfg.TranslateLanguages = langs
	}

	return cfg, nil
}

// renderFcitxAddonConfig formats cfg as the gemini-voice.conf INI content.
func renderFcitxAddonConfig(cfg FcitxAddonConfig) []byte {
	showMenu := "False"
	if cfg.ShowActionMenu {
		showMenu = "True"
	}
	socketPath := strings.TrimSpace(cfg.SocketPath)
	if socketPath == "" {
		socketPath = DefaultFcitxSocketPath
	}
	triggerKeys := cfg.TriggerKeys
	if len(triggerKeys) == 0 {
		triggerKeys = []string{"Alt+space"}
	}

	var b strings.Builder
	b.WriteString("ShowActionMenu=")
	b.WriteString(showMenu)
	b.WriteByte('\n')
	b.WriteString("SocketPath=")
	b.WriteString(quoteFcitxValue(socketPath))
	b.WriteByte('\n')
	b.WriteString("Language=")
	b.WriteString(quoteFcitxValue(strings.TrimSpace(cfg.Language)))
	b.WriteByte('\n')
	b.WriteString("RecordingPrefix=")
	b.WriteString(quoteFcitxValue(cfg.RecordingPrefix))
	b.WriteByte('\n')
	b.WriteString("PreeditMaxWidth=")
	b.WriteString(strconv.Itoa(cfg.PreeditMaxWidth))
	b.WriteByte('\n')
	b.WriteByte('\n')
	b.WriteString("[TriggerKey]\n")
	for i, k := range triggerKeys {
		trimmed := strings.TrimSpace(k)
		if trimmed == "" {
			continue
		}
		fmt.Fprintf(&b, "%d=%s\n", i, quoteFcitxValue(trimmed))
	}
	var translateLangs []string
	for _, l := range cfg.TranslateLanguages {
		if trimmed := strings.TrimSpace(l); trimmed != "" {
			translateLangs = append(translateLangs, trimmed)
		}
	}
	if len(translateLangs) > 0 {
		b.WriteByte('\n')
		b.WriteString("[TranslateLanguages]\n")
		for i, l := range translateLangs {
			fmt.Fprintf(&b, "%d=%s\n", i, quoteFcitxValue(l))
		}
	}
	return []byte(b.String())
}

func translateMigrationMarkerPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), ".translate_languages_migrated")
}

type rawConfigLine struct {
	text string // line content without trailing \r\n or \n
	raw  string // exact line slice including trailing \r\n or \n
}

func splitRawConfigLines(s string) []rawConfigLine {
	if s == "" {
		return nil
	}
	var lines []rawConfigLine
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			raw := s[start : i+1]
			text := s[start:i]
			text = strings.TrimSuffix(text, "\r")
			lines = append(lines, rawConfigLine{text: text, raw: raw})
			start = i + 1
		}
	}
	if start < len(s) {
		raw := s[start:]
		text := strings.TrimSuffix(raw, "\r")
		lines = append(lines, rawConfigLine{text: text, raw: raw})
	}
	return lines
}

// stripOrphanedTranslateLanguagesSection surgically removes [TranslateLanguages]
// when it contains solely 0=en (case-insensitive, optionally quoted), leaving all
// comments, unknown keys, key ordering, and line endings untouched.
func stripOrphanedTranslateLanguagesSection(data []byte) ([]byte, bool) {
	lines := splitRawConfigLines(string(data))
	if len(lines) == 0 {
		return nil, false
	}

	section := ""
	var headerIndices []int
	enEntryIdx := -1
	enEntryCount := 0
	hasOtherEntry := false

	for i, ln := range lines {
		trimmed := strings.TrimSpace(ln.text)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			if section == "TranslateLanguages" {
				headerIndices = append(headerIndices, i)
			}
			continue
		}
		if section != "TranslateLanguages" {
			continue
		}
		eq := strings.IndexByte(trimmed, '=')
		if eq < 0 {
			hasOtherEntry = true
			break
		}
		k := strings.TrimSpace(trimmed[:eq])
		v := unquoteFcitxValue(trimmed[eq+1:])
		if k == "0" && strings.EqualFold(v, "en") {
			enEntryIdx = i
			enEntryCount++
		} else {
			hasOtherEntry = true
			break
		}
	}

	if len(headerIndices) == 0 || hasOtherEntry || enEntryCount != 1 {
		return nil, false
	}

	deleteLine := make([]bool, len(lines))
	for _, idx := range headerIndices {
		deleteLine[idx] = true
	}
	deleteLine[enEntryIdx] = true

	firstHeader := headerIndices[0]
	if firstHeader > 0 && strings.TrimSpace(lines[firstHeader-1].text) == "" {
		deleteLine[firstHeader-1] = true
	} else if enEntryIdx+1 < len(lines) && strings.TrimSpace(lines[enEntryIdx+1].text) == "" {
		deleteLine[enEntryIdx+1] = true
	}

	var b strings.Builder
	b.Grow(len(data))
	for i, ln := range lines {
		if !deleteLine[i] {
			b.WriteString(ln.raw)
		}
	}
	return []byte(b.String()), true
}

// MigrateOrphanedFcitxTranslateLanguages performs a one-time migration for
// existing users whose gemini-voice.conf has the old default [TranslateLanguages] 0=en.
// Clearing it allows Fcitx5 to fall back to the daemon's translate_targets.
func MigrateOrphanedFcitxTranslateLanguages(fcitxPath, markerPath string) (bool, error) {
	if markerPath != "" {
		if _, err := os.Stat(markerPath); err == nil {
			return false, nil
		}
	}
	data, err := os.ReadFile(fcitxPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if markerPath != "" {
				_ = writeConfigFileAtomic(markerPath, []byte("1\n"), "translate migration marker")
			}
			return false, nil
		}
		return false, err
	}

	migrated := false
	if updated, ok := stripOrphanedTranslateLanguagesSection(data); ok {
		if err := writeConfigFileAtomic(fcitxPath, updated, "fcitx config"); err != nil {
			return false, err
		}
		migrated = true
	}

	if markerPath != "" {
		_ = writeConfigFileAtomic(markerPath, []byte("1\n"), "translate migration marker")
	}
	return migrated, nil
}

// SaveFcitxAddonConfig writes Fcitx5's gemini-voice.conf INI file atomically.
func SaveFcitxAddonConfig(path string, cfg FcitxAddonConfig) error {
	return writeConfigFileAtomic(path, renderFcitxAddonConfig(cfg), "fcitx config")
}

// ReloadFcitx5Config triggers a best-effort config reload in a running fcitx5 instance.
func ReloadFcitx5Config(ctx context.Context, deps cliDeps) bool {
	if _, err := deps.lookPath("fcitx5-remote"); err != nil {
		return false
	}
	rCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	res := deps.runCmd(rCtx, "", "fcitx5-remote", "--check", "-r")
	return res.err == nil
}
