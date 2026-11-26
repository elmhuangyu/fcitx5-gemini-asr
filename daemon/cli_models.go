package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"
)

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSliceFlag) Set(val string) error {
	*s = append(*s, val)
	return nil
}

func cmdModels(args []string, deps cliDeps) int {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	fs.SetOutput(deps.stderr)
	configPath := fs.String("config", defaultConfigPath(), "Path to gemini-voice.conf")
	remote := fs.Bool("remote", false, "Query Gemini API /v1beta/models using configured credentials")
	liveModel := fs.String("live", "", "Set live transcription model")
	defaultModel := fs.String("default", "", "Set default post-processing model; optional thinking effort suffix /low|medium|high, e.g. gemini-3.5-flash/low")
	fixTypos := fs.String("refine-fix-typos", "", "Set model override for refine_fix_typos (\"default\" clears)")
	casual := fs.String("refine-casual", "", "Set model override for refine_casual (\"default\" clears)")
	formal := fs.String("refine-formal", "", "Set model override for refine_formal (\"default\" clears)")
	translateEn := fs.String("translate-en", "", "Set model override for translate_en only, stored in the Actions list (\"default\" clears)")
	translateAll := fs.String("translate-all", "", "Set the model of the Translate group, used by every translate_<lang> without its own override (\"default\" clears)")
	var translateFlags stringSliceFlag
	fs.Var(&translateFlags, "translate", "Set model override for a translation target as <lang>=<model>[/effort] (e.g. ja=gemini-3.5-flash/low, \"<lang>=default\" clears)")
	asPrompt := fs.String("as-prompt", "", "Set model override for as_prompt (\"default\" clears)")
	askAI := fs.String("ask-ai", "", "Set model override for ask_ai (\"default\" clears)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	visited := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		visited[f.Name] = true
	})

	cliMigrate(*configPath, deps)
	cfg, err := loadConfigOrDefault(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: load config %q: %v\n", *configPath, err)
		return 1
	}

	dirty := false
	if visited["live"] {
		if strings.TrimSpace(*liveModel) == "" {
			_, _ = fmt.Fprintln(deps.stderr, "error: --live cannot be empty")
			return 1
		}
		if err := ValidateLiveModel(*liveModel); err != nil {
			_, _ = fmt.Fprintf(deps.stderr, "error: --live: %v\n", err)
			return 1
		}
		cfg.LiveTranscribe.Model = strings.TrimSpace(*liveModel)
		dirty = true
	}
	if visited["default"] {
		if strings.TrimSpace(*defaultModel) == "" {
			_, _ = fmt.Fprintln(deps.stderr, "error: --default cannot be empty")
			return 1
		}
		if err := ValidatePostProcessModel(*defaultModel); err != nil {
			_, _ = fmt.Fprintf(deps.stderr, "error: --default: %v\n", err)
			return 1
		}
		cfg.PostProcess.Model = strings.TrimSpace(*defaultModel)
		dirty = true
	}

	actionFlags := []struct {
		flagName string
		action   string
		val      string
	}{
		{"refine-fix-typos", "refine_fix_typos", *fixTypos},
		{"refine-casual", "refine_casual", *casual},
		{"refine-formal", "refine_formal", *formal},
		{"translate-en", "translate_en", *translateEn},
		{"translate-all", translateGroup, *translateAll},
		{"as-prompt", "as_prompt", *asPrompt},
		{"ask-ai", "ask_ai", *askAI},
	}
	for _, af := range actionFlags {
		if visited[af.flagName] {
			if err := cfg.SetActionModel(af.action, af.val); err != nil {
				_, _ = fmt.Fprintf(deps.stderr, "error: --%s: %v\n", af.flagName, err)
				return 1
			}
			dirty = true
		}
	}
	for _, spec := range translateFlags {
		lang, model, ok := strings.Cut(spec, "=")
		if !ok {
			_, _ = fmt.Fprintln(deps.stderr, "error: --translate must be in <lang>=<model> format (e.g. --translate ja=gemini-3.5-flash)")
			return 2
		}
		lang = strings.TrimSpace(lang)
		if lang == "" {
			_, _ = fmt.Fprintln(deps.stderr, "error: --translate language cannot be empty")
			return 2
		}
		action, ok := resolveTranslateModelAction(cfg, lang)
		if !ok {
			_, _ = fmt.Fprintf(deps.stderr, "error: unknown translation target %q (use a known language like en, ja, zh-cn or configure it in post_process.translate_languages)\n", lang)
			return 2
		}
		if err := cfg.SetActionModel(action, model); err != nil {
			_, _ = fmt.Fprintf(deps.stderr, "error: --translate %s: %v\n", lang, err)
			return 1
		}
		dirty = true
	}

	if dirty {
		if err := SaveConfigFile(*configPath, cfg); err != nil {
			_, _ = fmt.Fprintf(deps.stderr, "error: save %q: %v\n", *configPath, err)
			return 1
		}
		_, _ = fmt.Fprintf(deps.stdout, "Saved model configuration to %s\n\n", *configPath)
	}

	_, _ = fmt.Fprintf(deps.stdout, "Configured models (%s):\n", *configPath)
	printModelTable(deps.stdout, cfg)

	if *remote {
		apiKey, keyErr := LoadAPIKey(DefaultCredentialName, DefaultFallbackEnv)
		if keyErr != nil {
			_, _ = fmt.Fprintf(deps.stderr, "\nerror: load API key for --remote query: %v\n", keyErr)
			return 1
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		models, listErr := ListRemoteModels(ctx, cfg, apiKey)
		if listErr != nil {
			_, _ = fmt.Fprintf(deps.stderr, "\nerror: list remote models: %v\n", listErr)
			return 1
		}
		_, _ = fmt.Fprintf(deps.stdout, "\nAvailable Gemini models (%d):\n", len(models))
		for _, m := range models {
			var caps []string
			if m.SupportsLive {
				caps = append(caps, "live")
			}
			if m.SupportsGenerate {
				caps = append(caps, "generate")
			}
			_, _ = fmt.Fprintf(deps.stdout, "  %-34s [%s]\n", m.Name, strings.Join(caps, ", "))
		}
	}

	return 0
}
