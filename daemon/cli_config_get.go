package main

import (
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"
)

func cmdConfigGet(args []string, deps cliDeps) int {
	fs := flag.NewFlagSet("config get", flag.ContinueOnError)
	fs.SetOutput(deps.stderr)
	configPath := fs.String("config", defaultConfigPath(), "Path to gemini-voice.conf")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		_, _ = fmt.Fprintln(deps.stderr, "usage: gemini-voice config get [-config PATH] <key>")
		return 2
	}

	cliMigrate(*configPath, deps)
	cfg, err := loadConfigOrDefault(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: load config %q: %v\n", *configPath, err)
		return 1
	}

	val, err := getConfigKey(cfg, fs.Arg(0))
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintln(deps.stdout, val)
	return 0
}

func getConfigKey(cfg Config, key string) (string, error) {
	fcitxCfg := cfg.Ime
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
	case "ime.recording_prefix":
		return fcitxCfg.RecordingPrefix, nil
	case "ime.preedit_max_width":
		return strconv.Itoa(fcitxCfg.PreeditMaxWidth), nil
	case "connection.proxy":
		return cfg.Connection.Proxy, nil
	case "live.model", "live_transcribe.model":
		return cfg.LiveTranscribe.Model, nil
	case "live.language", "live_transcribe.language":
		return cfg.LiveLanguage(), nil
	case "live.mode", "live_transcribe.mode":
		return cfg.LiveTranscribe.Mode, nil
	case "live.custom_vocabulary", "live_transcribe.custom_vocabulary":
		return strings.Join(cfg.LiveTranscribe.CustomVocabulary, ","), nil
	case "live.vad_silence_ms", "live_transcribe.vad_silence_ms":
		return strconv.Itoa(cfg.LiveTranscribe.VADSilenceMs), nil
	case "post_process.model":
		return cfg.PostProcess.Model, nil
	case "post_process.prefetch":
		return strings.Join(cfg.PostProcess.Prefetch, ","), nil
	case "ime.translate_languages", "post_process.translate_languages", "translate.languages":
		if len(fcitxCfg.TranslateLanguages) == 0 {
			return "en", nil
		}
		return strings.Join(fcitxCfg.TranslateLanguages, ","), nil
	}

	if action, ok := parseActionKey(k, "model.", "post_process.models."); ok {
		if !isKnownOrConfiguredAction(cfg, action) {
			return "", fmt.Errorf("unknown action %q", action)
		}
		return cfg.ModelForAction(action), nil
	}
	if action, ok := parseActionKey(k, "prompt.", "prompts."); ok {
		val, exists := cfg.PromptForAction(action)
		if !exists {
			return "", fmt.Errorf("unknown prompt action %q", action)
		}
		return val, nil
	}
	return "", fmt.Errorf("unknown config key %q", key)
}
