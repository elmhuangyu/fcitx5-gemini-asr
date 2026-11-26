package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
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
  ime.recording_prefix         Prefix shown during recording (e.g. 🎙️, [Recording...], or "none")
  ime.preedit_max_width        Wrap preedit after N display columns (0 disables wrapping)
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
	translateVal := strings.Join(cfg.PostProcess.TranslateLanguages, ", ")
	if translateVal == "" {
		translateVal = "en"
	}
	vocabVal := strings.Join(cfg.LiveTranscribe.CustomVocabulary, ", ")
	if vocabVal == "" {
		vocabVal = "(none)"
	}
	fcitxLang := fcitxCfg.Language
	if fcitxLang == "" {
		fcitxLang = "(use daemon)"
	}
	recPrefix := fcitxCfg.RecordingPrefix
	if recPrefix == "" {
		recPrefix = "(none)"
	}
	wrapWidth := fmt.Sprintf("%d columns", fcitxCfg.PreeditMaxWidth)
	if fcitxCfg.PreeditMaxWidth == 0 {
		wrapWidth = "0 (disabled)"
	}

	fcitxTranslate := strings.Join(fcitxCfg.TranslateLanguages, ", ")
	if fcitxTranslate == "" {
		fcitxTranslate = "(use daemon)"
	}

	_, _ = fmt.Fprintf(w, "Fcitx5 addon config (%s):\n", fcitxPath)
	_, _ = fmt.Fprintf(w, "  ime.mode:                 %s\n", mode)
	_, _ = fmt.Fprintf(w, "  ime.show_action_menu:     %t\n", fcitxCfg.ShowActionMenu)
	_, _ = fmt.Fprintf(w, "  ime.trigger_key:          %s\n", strings.Join(fcitxCfg.TriggerKeys, ", "))
	_, _ = fmt.Fprintf(w, "  ime.socket_path:          %s\n", fcitxCfg.SocketPath)
	_, _ = fmt.Fprintf(w, "  ime.language:             %s\n", fcitxLang)
	_, _ = fmt.Fprintf(w, "  ime.recording_prefix:     %s\n", recPrefix)
	_, _ = fmt.Fprintf(w, "  ime.preedit_max_width:    %s\n", wrapWidth)
	_, _ = fmt.Fprintf(w, "  ime.translate_languages:  %s\n", fcitxTranslate)

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
	_, _ = fmt.Fprintf(w, "  post_process.translate_languages: %s\n", translateVal)

	_, _ = fmt.Fprintln(w, "\nPer-action models:")
	printModelTable(w, cfg)

	_, _ = fmt.Fprintln(w, "\nPrompts:")
	for _, action := range cfg.EffectiveActionNames() {
		promptText, ok := cfg.PromptForAction(action)
		if !ok {
			continue
		}
		p := strings.ReplaceAll(promptText, "\n", " ")
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
