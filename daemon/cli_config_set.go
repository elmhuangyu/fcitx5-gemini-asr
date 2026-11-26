package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"strings"
)

func cmdConfigSet(args []string, deps cliDeps) int {
	fs := flag.NewFlagSet("config set", flag.ContinueOnError)
	fs.SetOutput(deps.stderr)
	configPath := fs.String("config", defaultConfigPath(), "Path to gemini-voice.conf")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 2 {
		_, _ = fmt.Fprintln(deps.stderr, "usage: gemini-voice config set [-config PATH] <key> <value>")
		return 2
	}

	cliMigrate(*configPath, deps)
	cfg, err := loadConfigOrDefault(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: load config %q: %v\n", *configPath, err)
		return 1
	}

	key := strings.TrimSpace(fs.Arg(0))
	imeDirty, err := setConfigKey(&cfg, key, fs.Arg(1))
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}
	if err := SaveConfigFile(*configPath, cfg); err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: save %q: %v\n", *configPath, err)
		return 1
	}

	// The daemon watches the conf and applies daemon-side keys by itself;
	// only keys the Fcitx5 addon consumes need a Fcitx5 reload.
	if imeDirty && ReloadFcitx5Config(context.Background(), deps) {
		_, _ = fmt.Fprintf(deps.stdout, "Updated %s in %s (reloaded Fcitx5)\n", key, *configPath)
	} else {
		_, _ = fmt.Fprintf(deps.stdout, "Updated %s in %s\n", key, *configPath)
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
