package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
)

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

	migratedFcitx, migErr := MigrateOrphanedFcitxTranslateLanguages(*fcitxPath, translateMigrationMarkerPath(*configPath))
	if migErr != nil {
		_, _ = fmt.Fprintf(deps.stderr, "warning: migrate fcitx translate_languages: %v\n", migErr)
	}
	if fcitxDirty && migratedFcitx && key != "ime.translate_languages" {
		fcitxCfg.TranslateLanguages = nil
	}

	// Render both configs before writing either file: with validation and
	// encoding done up front, only an I/O failure can leave the two files out
	// of sync, and writeConfigUpdates reports that split state clearly.
	var daemonData, fcitxData []byte
	if daemonDirty {
		if daemonData, err = renderConfigTOML(cfg); err != nil {
			_, _ = fmt.Fprintf(deps.stderr, "error: encode daemon config %q: %v\n", *configPath, err)
			return 1
		}
	}
	if fcitxDirty {
		existing, readErr := os.ReadFile(*fcitxPath)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			_, _ = fmt.Fprintf(deps.stderr, "error: read existing fcitx config %q: %v\n", *fcitxPath, readErr)
			return 1
		}
		fcitxData = mergeFcitxAddonConfig(existing, fcitxCfg)
	}
	if err := writeConfigUpdates(*fcitxPath, fcitxData, *configPath, daemonData); err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}

	if daemonDirty {
		_, _ = fmt.Fprintf(deps.stdout, "Updated %s in %s\n", key, *configPath)
	}
	if fcitxDirty {
		reloaded := ReloadFcitx5Config(context.Background(), deps)
		if reloaded {
			_, _ = fmt.Fprintf(deps.stdout, "Updated %s in %s (reloaded Fcitx5)\n", key, *fcitxPath)
		} else {
			_, _ = fmt.Fprintf(deps.stdout, "Updated %s in %s\n", key, *fcitxPath)
		}
	} else if migratedFcitx {
		ReloadFcitx5Config(context.Background(), deps)
	}
	return 0
}

// writeConfigUpdates persists pre-rendered config contents, writing the
// Fcitx5 addon config first and the daemon config second. A nil data slice
// means that file does not need updating. Saving the Fcitx5 config first puts
// the riskier write (its directory is owned by Fcitx5 tooling, while the
// daemon config directory is created by this CLI) before anything has been
// modified, so its failure leaves both files untouched. If the daemon write
// still fails after the Fcitx5 write succeeded, the returned error spells out
// the split state and how to reconcile it.
func writeConfigUpdates(fcitxPath string, fcitxData []byte, configPath string, daemonData []byte) error {
	if fcitxData != nil {
		if err := writeConfigFileAtomic(fcitxPath, fcitxData, "fcitx config"); err != nil {
			return fmt.Errorf("save %q: %w", fcitxPath, err)
		}
	}
	if daemonData != nil {
		if err := writeConfigFileAtomic(configPath, daemonData, "config"); err != nil {
			if fcitxData != nil {
				return fmt.Errorf("updated %q but failed to save daemon config %q: %v; the two config files are now out of sync, re-run the same command to reconcile them", fcitxPath, configPath, err)
			}
			return fmt.Errorf("save %q: %w", configPath, err)
		}
	}
	return nil
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

func resolveTranslateModelAction(cfg Config, raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "translate_") || strings.HasPrefix(lower, "translate-") {
		trimmed = strings.TrimSpace(trimmed[len("translate_"):])
		if trimmed == "" {
			return "", false
		}
	}
	alias := strings.ToLower(trimmed)
	alias = strings.ReplaceAll(alias, "_", "-")
	alias = strings.ReplaceAll(alias, " ", "-")
	if known, ok := knownTranslateLanguages[alias]; ok {
		return known.Action, true
	}
	target, ok := ResolveTranslateTarget(trimmed)
	if ok && isKnownOrConfiguredAction(cfg, target.Action) {
		return target.Action, true
	}
	return "", false
}

func isKnownOrConfiguredAction(cfg Config, action string) bool {
	for _, known := range KnownActions {
		if action == known {
			return true
		}
	}
	for _, target := range cfg.TranslateTargets() {
		if action == target.Action {
			return true
		}
	}
	if _, ok := cfg.PostProcess.Models[action]; ok {
		return true
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
