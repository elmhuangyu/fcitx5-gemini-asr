package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/huh/v2"
)

type setupFormOptions struct {
	IncludeAPIKey bool
	WithKey       string
	CredPath      string
	CredExists    bool
}

type setupFormState struct {
	showActionMenu     bool
	language           string
	liveMode           string
	vadSilenceMs       string
	liveModel          string
	postProcessModel   string
	configureOverrides bool
	actionOrder        []string
	actionOverrides    []string
	prefetch           []string
	translateLanguages string
	customVocabulary   string
	triggerKeys        string
	proxy              string
	overwriteKey       bool
	apiKey             string
}

type setupResult struct {
	OverwriteKey bool
	APIKey       string
}

func newSetupFormState(cfg Config, opts setupFormOptions) *setupFormState {
	fcitxCfg := cfg.Ime
	actionOrder := cfg.EffectiveActionNames()
	overrides := make([]string, len(actionOrder))
	for i, action := range actionOrder {
		overrides[i] = strings.TrimSpace(cfg.PostProcess.Models[action])
	}

	liveMode, err := NormalizeLiveMode(cfg.LiveTranscribe.Mode)
	if err != nil {
		liveMode = DefaultLiveMode
	}

	lang := cfg.LiveLanguage()

	liveModel := strings.TrimSpace(cfg.LiveTranscribe.Model)
	if liveModel == "" {
		liveModel = DefaultLiveModel
	}

	ppModel := strings.TrimSpace(cfg.PostProcess.Model)
	if ppModel == "" {
		ppModel = DefaultPostProcessModel
	}

	translateLangs := strings.Join(fcitxCfg.TranslateLanguages, ", ")
	if strings.TrimSpace(translateLangs) == "" {
		translateLangs = "en"
	}

	return &setupFormState{
		showActionMenu:     fcitxCfg.ShowActionMenu,
		language:           lang,
		liveMode:           liveMode,
		vadSilenceMs:       strconv.Itoa(cfg.LiveTranscribe.VADSilenceMs),
		liveModel:          liveModel,
		postProcessModel:   ppModel,
		configureOverrides: len(cfg.PostProcess.Models) > 0,
		actionOrder:        actionOrder,
		actionOverrides:    overrides,
		prefetch:           append([]string(nil), cfg.PostProcess.Prefetch...),
		translateLanguages: translateLangs,
		customVocabulary:   strings.Join(cfg.LiveTranscribe.CustomVocabulary, ", "),
		triggerKeys:        strings.Join(fcitxCfg.TriggerKeys, ", "),
		proxy:              cfg.Connection.Proxy,
		overwriteKey:       opts.IncludeAPIKey && !opts.CredExists,
	}
}

func setupKeyMap() *huh.KeyMap {
	km := huh.NewDefaultKeyMap()
	km.Input.Prev = key.NewBinding(key.WithKeys("shift+tab", "up"), key.WithHelp("↑/shift+tab", "prev"))
	km.Input.Next = key.NewBinding(key.WithKeys("enter", "tab", "down"), key.WithHelp("↓/enter/tab", "next"))
	km.Confirm.Prev = key.NewBinding(key.WithKeys("shift+tab", "up"), key.WithHelp("↑/shift+tab", "prev"))
	km.Confirm.Next = key.NewBinding(key.WithKeys("enter", "tab", "down"), key.WithHelp("↓/enter/tab", "next"))
	return km
}

func validateNonEmpty(label string) func(string) error {
	return func(s string) error {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("%s cannot be empty", label)
		}
		return nil
	}
}

func buildSetupForm(state *setupFormState, opts setupFormOptions) *huh.Form {
	withKey := strings.TrimSpace(opts.WithKey)
	if withKey == "" {
		withKey = "auto"
	}

	groups := []*huh.Group{
		huh.NewGroup(
			huh.NewSelect[bool]().
				Title("Input mode after dictation").
				Description("Choose whether dictation opens the Rime-style action menu or commits directly.").
				Options(
					huh.NewOption("Action menu (Refine / Translate / Ask AI)", true),
					huh.NewOption("Direct commit (insert raw transcript immediately)", false),
				).
				Value(&state.showActionMenu),

			huh.NewInput().
				Title("Live transcription language").
				Description("Use \"auto\" (85+ languages & code-switching) or comma-separated BCP-47 tags (e.g. cmn-Hans-CN,en-US).").
				Suggestions([]string{"auto", "cmn-Hans-CN", "cmn-Hans-CN,en-US", "en-US", "ja-JP"}).
				Value(&state.language).
				Validate(validateNonEmpty("live.language")),

			huh.NewSelect[string]().
				Title("Live transcription mode").
				Description("VERBATIM preserves literal speech; SMART cleans disfluencies and formats text.").
				Options(
					huh.NewOption("VERBATIM — literal speech-to-text", "VERBATIM"),
					huh.NewOption("SMART — disfluency cleanup & formatting", "SMART"),
				).
				Value(&state.liveMode),

			huh.NewInput().
				Title("VAD silence auto-stop (ms)").
				Description("Auto-stop after silence in milliseconds (0 disables auto-stop).").
				Value(&state.vadSilenceMs).
				Validate(func(s string) error {
					ms, err := strconv.Atoi(strings.TrimSpace(s))
					if err != nil || ms < 0 {
						return fmt.Errorf("invalid vad_silence_ms %q (must be integer >= 0)", s)
					}
					return nil
				}),
		).Title("1. Input & Recognition"),

		huh.NewGroup(
			huh.NewInput().
				Title("Live transcription model").
				Description("Gemini Live API model used for streaming speech recognition.").
				Suggestions([]string{DefaultLiveModel}).
				Value(&state.liveModel).
				Validate(func(s string) error {
					if err := validateNonEmpty("live.model")(s); err != nil {
						return err
					}
					return ValidateLiveModel(s)
				}),

			huh.NewInput().
				Title("Default post-processing model").
				Description("Used for Refine / Translate / Ask AI unless overridden per action.").
				Suggestions([]string{DefaultPostProcessModel, "gemini-3.5-flash", "gemini-3.5-flash-lite", "gemini-4-hydrogen"}).
				Value(&state.postProcessModel).
				Validate(func(s string) error {
					if err := validateNonEmpty("post_process.model")(s); err != nil {
						return err
					}
					return ValidatePostProcessModel(s)
				}),

			huh.NewConfirm().
				Title("Configure per-action model overrides?").
				Description("Yes: set per-action models. No: inherit the default model.").
				Affirmative("Yes").
				Negative("No").
				Value(&state.configureOverrides),
		).Title("2. Models"),
	}

	overrideFields := make([]huh.Field, 0, len(state.actionOrder))
	for i, action := range state.actionOrder {
		idx := i
		overrideFields = append(overrideFields,
			huh.NewInput().
				Title(action).
				Placeholder("default (inherit "+state.postProcessModel+")").
				Suggestions([]string{DefaultPostProcessModel, "gemini-3.5-flash", "gemini-3.5-flash-lite", "gemini-4-hydrogen"}).
				Value(&state.actionOverrides[idx]).
				Validate(func(s string) error {
					t := strings.TrimSpace(s)
					if t == "" || strings.EqualFold(t, "default") || t == "-" {
						return nil
					}
					return ValidatePostProcessModel(t)
				}),
		)
	}
	groups = append(groups,
		huh.NewGroup(overrideFields...).
			Title("3. Per-Action Model Overrides").
			Description("Leave blank or enter \"default\" to inherit the default post-processing model. Optional thinking effort suffix: model/low|medium|high.").
			WithHideFunc(func() bool { return !state.configureOverrides }),
	)

	prefetchOptions := huh.NewOptions(state.actionOrder...)
	groups = append(groups,
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Prefetch actions when recording stops").
				Description("Use ↑/↓ to move, Space/x to toggle, Enter to confirm.").
				Options(prefetchOptions...).
				Value(&state.prefetch),
		).
			Title("4. Action Menu Prefetch").
			WithHideFunc(func() bool { return !state.showActionMenu }),
	)

	groups = append(groups,
		huh.NewGroup(
			huh.NewInput().
				Title("Custom vocabulary (optional)").
				Description("Comma-separated technical terms or proper nouns (up to 1000 terms; leave empty for none).").
				Placeholder("e.g. Fcitx5, PipeWire, Gemini").
				Value(&state.customVocabulary).
				Validate(func(s string) error {
					if strings.TrimSpace(s) == "" {
						return nil
					}
					terms, err := parseCommaList(s)
					if err != nil {
						return fmt.Errorf("invalid custom_vocabulary list: %w", err)
					}
					if len(terms) > MaxCustomVocabulary {
						return fmt.Errorf("custom_vocabulary has %d entries (max %d)", len(terms), MaxCustomVocabulary)
					}
					return nil
				}),

			huh.NewInput().
				Title("Fcitx5 trigger key(s)").
				Description("Comma-separated Fcitx5 shortcut keys.").
				Value(&state.triggerKeys).
				Validate(func(s string) error {
					keys, err := parseCommaList(s)
					if err != nil || len(keys) == 0 {
						return errors.New("ime.trigger_key requires at least one key (e.g. Alt+space)")
					}
					return nil
				}),

			huh.NewInput().
				Title("HTTP / SOCKS5 proxy URL (optional)").
				Description("Leave blank or \"none\" for a direct connection.").
				Placeholder("e.g. socks5://127.0.0.1:1080").
				Value(&state.proxy).
				Validate(func(s string) error {
					_, err := validateProxyURL(s)
					return err
				}),

			huh.NewInput().
				Title("Translation target languages").
				Description("Comma-separated target languages (e.g. en, ja, zh-CN). Multiple languages open a submenu.").
				Placeholder("e.g. en, ja").
				Value(&state.translateLanguages).
				Validate(func(s string) error {
					langs, err := parseCommaList(s)
					if err != nil || len(langs) == 0 {
						return errors.New("post_process.translate_languages requires at least one language (e.g. en)")
					}
					return nil
				}),
		).Title("5. Translation, Vocabulary, Hotkey & Network"),
	)

	if opts.IncludeAPIKey {
		if opts.CredExists {
			groups = append(groups,
				huh.NewGroup(
					huh.NewConfirm().
						Title(fmt.Sprintf("Encrypted credential exists at %s", opts.CredPath)).
						Description("Use ←/→ to choose whether to keep the existing credential or overwrite it.").
						Affirmative("Overwrite").
						Negative("Keep existing").
						Value(&state.overwriteKey),
				).Title("6. Gemini API Credential"),

				huh.NewGroup(
					huh.NewInput().
						Title("Gemini API key").
						Description(fmt.Sprintf("Input is hidden and encrypted via systemd-creds (--with-key=%s).", withKey)).
						EchoMode(huh.EchoModePassword).
						Value(&state.apiKey).
						Validate(validateNonEmpty("API key")),
				).
					Title("6. Gemini API Key").
					WithHideFunc(func() bool { return !state.overwriteKey }),
			)
		} else {
			groups = append(groups,
				huh.NewGroup(
					huh.NewInput().
						Title("Gemini API key").
						Description(fmt.Sprintf("Input is hidden and encrypted via systemd-creds (--with-key=%s).", withKey)).
						EchoMode(huh.EchoModePassword).
						Value(&state.apiKey).
						Validate(validateNonEmpty("API key")),
				).Title("6. Gemini API Key"),
			)
		}
	}

	return huh.NewForm(groups...).
		WithTheme(huh.ThemeFunc(huh.ThemeCharm)).
		WithKeyMap(setupKeyMap()).
		WithShowHelp(true).
		WithShowErrors(true)
}

func applySetupFormState(cfg *Config, state *setupFormState) error {
	imeMode := "direct"
	if state.showActionMenu {
		imeMode = "menu"
	}
	if _, err := setConfigKey(cfg, "ime.mode", imeMode); err != nil {
		return err
	}
	if _, err := setConfigKey(cfg, "live.language", state.language); err != nil {
		return err
	}
	if _, err := setConfigKey(cfg, "live.mode", state.liveMode); err != nil {
		return err
	}
	if _, err := setConfigKey(cfg, "live.vad_silence_ms", state.vadSilenceMs); err != nil {
		return err
	}
	if _, err := setConfigKey(cfg, "live.model", state.liveModel); err != nil {
		return err
	}
	if _, err := setConfigKey(cfg, "post_process.model", state.postProcessModel); err != nil {
		return err
	}
	if state.configureOverrides {
		for i, action := range state.actionOrder {
			if i < len(state.actionOverrides) {
				if err := setActionModelOverride(cfg, action, state.actionOverrides[i]); err != nil {
					return fmt.Errorf("model.%s: %w", action, err)
				}
			}
		}
	} else {
		cfg.PostProcess.Models = nil
	}
	if state.showActionMenu {
		if len(state.prefetch) == 0 {
			cfg.PostProcess.Prefetch = []string{}
		} else {
			cfg.PostProcess.Prefetch = append([]string(nil), state.prefetch...)
		}
	}
	if _, err := setConfigKey(cfg, "live.custom_vocabulary", state.customVocabulary); err != nil {
		return err
	}
	if _, err := setConfigKey(cfg, "ime.trigger_key", state.triggerKeys); err != nil {
		return err
	}
	if _, err := setConfigKey(cfg, "connection.proxy", state.proxy); err != nil {
		return err
	}
	if _, err := setConfigKey(cfg, "post_process.translate_languages", state.translateLanguages); err != nil {
		return err
	}
	return nil
}

func runHuhSetupForm(state *setupFormState, opts setupFormOptions, deps cliDeps) error {
	form := buildSetupForm(state, opts)
	if deps.stdin != nil {
		form = form.WithInput(deps.stdin)
	}
	if deps.stderr != nil {
		form = form.WithOutput(deps.stderr)
	} else {
		form = form.WithOutput(os.Stderr)
	}
	return form.Run()
}

func runSetupTUI(configPath string, opts setupFormOptions, deps cliDeps) (setupResult, error) {
	cliMigrate(configPath, deps)
	cfg, err := loadConfigOrDefault(configPath)
	if err != nil {
		return setupResult{}, fmt.Errorf("load config %q: %w", configPath, err)
	}

	state := newSetupFormState(cfg, opts)
	if deps.runSetupForm != nil {
		err = deps.runSetupForm(state, opts)
	} else {
		err = runHuhSetupForm(state, opts, deps)
	}
	if err != nil {
		return setupResult{}, err
	}

	if opts.IncludeAPIKey && state.overwriteKey && strings.TrimSpace(state.apiKey) == "" {
		return setupResult{}, errors.New("API key cannot be empty")
	}

	if err := applySetupFormState(&cfg, state); err != nil {
		return setupResult{}, err
	}
	if err := SaveConfigFile(configPath, cfg); err != nil {
		return setupResult{}, fmt.Errorf("save config %q: %w", configPath, err)
	}
	reloaded := ReloadFcitx5Config(context.Background(), deps)

	if reloaded {
		_, _ = fmt.Fprintf(deps.stdout, "\nSaved config: %s (reloaded Fcitx5; the daemon applies changes automatically)\n", configPath)
	} else {
		_, _ = fmt.Fprintf(deps.stdout, "\nSaved config: %s (the daemon applies changes automatically)\n", configPath)
	}

	return setupResult{
		OverwriteKey: state.overwriteKey,
		APIKey:       strings.TrimSpace(state.apiKey),
	}, nil
}
