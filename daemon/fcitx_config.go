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

const DefaultFcitxSocketPath = "$XDG_RUNTIME_DIR/gemini-ime.sock"

type FcitxAddonConfig struct {
	TriggerKeys    []string
	ShowActionMenu bool
	SocketPath     string
	Language       string
}

func DefaultFcitxAddonConfig() FcitxAddonConfig {
	return FcitxAddonConfig{
		TriggerKeys:    []string{"Alt+space"},
		ShowActionMenu: true,
		SocketPath:     DefaultFcitxSocketPath,
		Language:       "",
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
	seenTriggerSection := false
	section := ""

	lines := strings.Split(string(data), "\n")
	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			if section == "TriggerKey" {
				seenTriggerSection = true
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
			}
		case "TriggerKey":
			if idx, convErr := strconv.Atoi(k); convErr == nil && v != "" {
				triggerEntries = append(triggerEntries, indexedKey{idx: idx, val: v})
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
	b.WriteByte('\n')
	b.WriteString("[TriggerKey]\n")
	for i, k := range triggerKeys {
		trimmed := strings.TrimSpace(k)
		if trimmed == "" {
			continue
		}
		fmt.Fprintf(&b, "%d=%s\n", i, quoteFcitxValue(trimmed))
	}
	return []byte(b.String())
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
