package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"
)

func drainFormCmds(m huh.Model, cmd tea.Cmd, depth int) *huh.Form {
	f := m.(*huh.Form)
	if cmd == nil || f.State != huh.StateNormal || depth >= 16 {
		return f
	}
	ch := make(chan tea.Msg, 1)
	go func() {
		ch <- cmd()
	}()
	var msg tea.Msg
	select {
	case msg = <-ch:
	case <-time.After(3 * time.Millisecond):
		// Skip blocking timer/IO cmds such as cursor blink ticks or terminal size queries.
		return f
	}
	if msg == nil {
		return f
	}

	rv := reflect.ValueOf(msg)
	if rv.Kind() == reflect.Slice && rv.Type().Elem() == reflect.TypeOf((tea.Cmd)(nil)) {
		for i := 0; i < rv.Len(); i++ {
			subCmd, _ := rv.Index(i).Interface().(tea.Cmd)
			if f.State != huh.StateNormal {
				break
			}
			f = drainFormCmds(f, subCmd, depth+1)
		}
		return f
	}
	if _, isQuit := msg.(tea.QuitMsg); isQuit {
		return f
	}
	nextModel, nextCmd := f.Update(msg)
	return drainFormCmds(nextModel, nextCmd, depth+1)
}

func initTestForm(form *huh.Form) *huh.Form {
	return drainFormCmds(form, form.Init(), 0)
}

func pressKey(f *huh.Form, k tea.Key) *huh.Form {
	m, cmd := f.Update(tea.KeyPressMsg(k))
	return drainFormCmds(m, cmd, 0)
}

func pressSpecial(f *huh.Form, code rune) *huh.Form {
	return pressKey(f, tea.Key{Code: code})
}

func typeRunes(f *huh.Form, s string) *huh.Form {
	for _, r := range s {
		f = pressKey(f, tea.Key{Text: string(r), Code: r, ShiftedCode: r})
	}
	return f
}

func clearChars(f *huh.Form, n int) *huh.Form {
	for i := 0; i < n; i++ {
		f = pressSpecial(f, tea.KeyBackspace)
	}
	return f
}

func TestBuildSetupForm_ArrowKeysValidationAndPasswordMasking(t *testing.T) {
	cfg := DefaultConfig()
	opts := setupFormOptions{
		IncludeAPIKey: true,
		WithKey:       "auto",
		CredExists:    false,
	}
	state := newSetupFormState(cfg, opts)
	form := initTestForm(buildSetupForm(state, opts))

	// Group 1, Field 1: Select Input mode (move down -> up -> confirm Action menu).
	form = pressSpecial(form, tea.KeyDown)
	form = pressSpecial(form, tea.KeyUp)
	form = pressSpecial(form, tea.KeyEnter)

	// Group 1, Field 2: Input Language (clear "auto", type "en-XXS", move left before 'S', delete "XX" and insert 'U' -> "en-US", move down).
	form = clearChars(form, 4)
	form = typeRunes(form, "en-XXS")
	form = pressSpecial(form, tea.KeyLeft)
	form = clearChars(form, 2)
	form = typeRunes(form, "U")
	form = pressSpecial(form, tea.KeyRight)
	form = pressSpecial(form, tea.KeyDown)

	// Group 1, Field 3: Select Live mode (VERBATIM -> move down to SMART -> Enter).
	form = pressSpecial(form, tea.KeyDown)
	form = pressSpecial(form, tea.KeyEnter)

	// Group 1, Field 4: Input VAD silence ms (invalid "-5" blocks advancing, then correct to "900").
	form = clearChars(form, 4)
	form = typeRunes(form, "-5")
	form = pressSpecial(form, tea.KeyDown)
	if !strings.Contains(ansi.Strip(form.View()), "invalid vad_silence_ms") {
		t.Fatalf("expected VAD validation error in view:\n%s", ansi.Strip(form.View()))
	}
	form = clearChars(form, 2)
	form = typeRunes(form, "900")
	form = pressSpecial(form, tea.KeyDown)

	// Group 2: Models (keep liveModel via Down, keep postProcessModel via Enter, toggle Confirm overrides to Yes via Left).
	form = pressSpecial(form, tea.KeyDown)
	form = pressSpecial(form, tea.KeyEnter)
	form = pressSpecial(form, tea.KeyLeft)
	form = pressSpecial(form, tea.KeyEnter)

	// Group 3: Per-Action Model Overrides (6 KnownActions).
	if !strings.Contains(ansi.Strip(form.View()), "Per-Action Model Overrides") {
		t.Fatalf("expected Group 3 Per-Action Model Overrides in view:\n%s", ansi.Strip(form.View()))
	}
	form = typeRunes(form, "gemini-2.5-flash-lite") // refine_fix_typos
	form = pressSpecial(form, tea.KeyDown)
	form = pressSpecial(form, tea.KeyDown)   // refine_casual (inherit)
	form = typeRunes(form, "gemini-2.5-pro") // refine_formal
	form = pressSpecial(form, tea.KeyDown)
	form = typeRunes(form, "gemini-2.5-pro") // as_prompt
	form = pressSpecial(form, tea.KeyDown)
	form = pressSpecial(form, tea.KeyDown)  // translate_en (inherit)
	form = pressSpecial(form, tea.KeyEnter) // ask_ai (inherit)

	// Group 4: MultiSelect Prefetch (uncheck refine_formal, check as_prompt using Space).
	if !strings.Contains(ansi.Strip(form.View()), "Prefetch actions") {
		t.Fatalf("expected Group 4 Action Menu Prefetch in view:\n%s", ansi.Strip(form.View()))
	}
	form = pressSpecial(form, tea.KeyDown)
	form = pressSpecial(form, tea.KeyDown)
	form = pressSpecial(form, tea.KeySpace) // uncheck refine_formal
	form = pressSpecial(form, tea.KeyDown)
	form = pressSpecial(form, tea.KeySpace) // check as_prompt
	form = pressSpecial(form, tea.KeyEnter)

	// Group 5: Custom vocabulary, trigger keys, proxy (test invalid proxy rejection then valid socks5).
	form = typeRunes(form, "Fcitx5, PipeWire")
	form = pressSpecial(form, tea.KeyDown)
	form = pressSpecial(form, tea.KeyDown) // keep "Alt+space"
	form = typeRunes(form, "ftp://bad")
	form = pressSpecial(form, tea.KeyEnter)
	if !strings.Contains(ansi.Strip(form.View()), "unsupported proxy scheme") {
		t.Fatalf("expected proxy validation error in view:\n%s", ansi.Strip(form.View()))
	}
	form = clearChars(form, len("ftp://bad"))
	form = typeRunes(form, "socks5://127.0.0.1:1080")
	form = pressSpecial(form, tea.KeyDown)

	// Translation target languages: append ", ja" to default "en".
	form = typeRunes(form, ", ja")
	form = pressSpecial(form, tea.KeyEnter)

	// Group 6: API key (empty rejected, masked input hides raw key from View()).
	form = pressSpecial(form, tea.KeyEnter)
	if !strings.Contains(ansi.Strip(form.View()), "API key cannot be empty") {
		t.Fatalf("expected empty API key error in view:\n%s", ansi.Strip(form.View()))
	}
	secret := "AIzaSuperSecretKey9999"
	form = typeRunes(form, secret)
	renderedKeyView := ansi.Strip(form.View())
	if strings.Contains(renderedKeyView, secret) {
		t.Fatalf("raw API key leaked into TUI view:\n%s", renderedKeyView)
	}
	form = pressSpecial(form, tea.KeyEnter)

	if form.State != huh.StateCompleted {
		t.Fatalf("form.State = %v, want StateCompleted (%v)\nview:\n%s", form.State, huh.StateCompleted, ansi.Strip(form.View()))
	}

	if err := applySetupFormState(&cfg, state); err != nil {
		t.Fatalf("applySetupFormState: %v", err)
	}
	if !cfg.Ime.ShowActionMenu {
		t.Fatal("expected ShowActionMenu=true")
	}
	if cfg.LiveLanguage() != "en-US" {
		t.Fatalf("language = %q, want en-US", cfg.LiveLanguage())
	}
	if cfg.LiveTranscribe.Mode != "SMART" {
		t.Fatalf("mode = %q, want SMART", cfg.LiveTranscribe.Mode)
	}
	if cfg.LiveTranscribe.VADSilenceMs != 900 {
		t.Fatalf("vad_silence_ms = %d, want 900", cfg.LiveTranscribe.VADSilenceMs)
	}
	if cfg.ModelForAction("refine_fix_typos") != "gemini-2.5-flash-lite" ||
		cfg.ModelForAction("refine_formal") != "gemini-2.5-pro" ||
		cfg.ModelForAction("as_prompt") != "gemini-2.5-pro" {
		t.Fatalf("unexpected model overrides: %+v", cfg.PostProcess.Models)
	}
	wantPrefetch := []string{"refine_fix_typos", "as_prompt"}
	if !reflect.DeepEqual(cfg.PostProcess.Prefetch, wantPrefetch) {
		t.Fatalf("prefetch = %#v, want %#v", cfg.PostProcess.Prefetch, wantPrefetch)
	}
	wantVocab := []string{"Fcitx5", "PipeWire"}
	if !reflect.DeepEqual(cfg.LiveTranscribe.CustomVocabulary, wantVocab) {
		t.Fatalf("custom_vocabulary = %#v, want %#v", cfg.LiveTranscribe.CustomVocabulary, wantVocab)
	}
	if !reflect.DeepEqual(cfg.Ime.TranslateLanguages, []string{"en", "ja"}) {
		t.Fatalf("TranslateLanguages = %#v, want [en ja]", cfg.Ime.TranslateLanguages)
	}
	if cfg.Connection.Proxy != "socks5://127.0.0.1:1080" {
		t.Fatalf("proxy = %q", cfg.Connection.Proxy)
	}
	if state.apiKey != secret || !state.overwriteKey {
		t.Fatalf("unexpected API key state: overwrite=%v len=%d", state.overwriteKey, len(state.apiKey))
	}
}

func TestBuildSetupForm_ConditionalHiddenGroupsAndAbort(t *testing.T) {
	t.Run("hidden_groups_skipped", func(t *testing.T) {
		cfg := DefaultConfig()
		opts := setupFormOptions{
			IncludeAPIKey: true,
			WithKey:       "auto",
			CredPath:      "/tmp/test.cred",
			CredExists:    true,
		}
		state := newSetupFormState(cfg, opts)
		form := initTestForm(buildSetupForm(state, opts))

		// Group 1: choose Direct commit (Down + Enter), keep language (Enter), keep mode (Enter), keep VAD (Enter).
		form = pressSpecial(form, tea.KeyDown)
		form = pressSpecial(form, tea.KeyEnter)
		form = pressSpecial(form, tea.KeyEnter)
		form = pressSpecial(form, tea.KeyEnter)
		form = pressSpecial(form, tea.KeyEnter)

		// Group 2: keep liveModel (Enter), keep postProcessModel (Enter), keep configureOverrides=false (Enter).
		form = pressSpecial(form, tea.KeyEnter)
		form = pressSpecial(form, tea.KeyEnter)
		form = pressSpecial(form, tea.KeyEnter)

		// Groups 3 (overrides) and 4 (prefetch) are hidden -> lands directly on Group 5!
		if !strings.Contains(ansi.Strip(form.View()), "Custom vocabulary") {
			t.Fatalf("expected hidden Groups 3 & 4 skipped directly to Group 5, got:\n%s", ansi.Strip(form.View()))
		}
		form = pressSpecial(form, tea.KeyEnter) // vocab
		form = pressSpecial(form, tea.KeyEnter) // triggerKeys
		form = pressSpecial(form, tea.KeyEnter) // proxy
		form = pressSpecial(form, tea.KeyEnter) // translateLanguages

		// Group 6a: Confirm keep existing credential (default false -> Enter), skipping Group 6b API key input.
		if !strings.Contains(ansi.Strip(form.View()), "Encrypted credential exists") {
			t.Fatalf("expected Group 6a existing credential confirm, got:\n%s", ansi.Strip(form.View()))
		}
		form = pressSpecial(form, tea.KeyEnter)

		if form.State != huh.StateCompleted {
			t.Fatalf("form.State = %v, want StateCompleted", form.State)
		}
		if state.showActionMenu {
			t.Fatal("expected showActionMenu=false")
		}
		if state.overwriteKey {
			t.Fatal("expected overwriteKey=false")
		}
	})

	t.Run("ctrl_c_aborts", func(t *testing.T) {
		cfg := DefaultConfig()
		opts := setupFormOptions{IncludeAPIKey: false}
		state := newSetupFormState(cfg, opts)
		form := initTestForm(buildSetupForm(state, opts))

		form = pressKey(form, tea.Key{Code: 'c', Mod: tea.ModCtrl})
		if form.State != huh.StateAborted {
			t.Fatalf("form.State = %v, want StateAborted (%v)", form.State, huh.StateAborted)
		}
	})
}

func TestApplySetupFormState_DisableOverridesClearsStaleModels(t *testing.T) {
	newInitialConfigs := func() Config {
		cfg := DefaultConfig()
		cfg.PostProcess.Models = map[string]string{
			"refine_formal": "gemini-2.5-pro",
			"as_prompt":     "gemini-2.5-pro",
		}
		return cfg
	}

	assertCleared := func(t *testing.T, cfg Config) {
		t.Helper()
		if cfg.PostProcess.Models != nil {
			t.Fatalf("expected PostProcess.Models cleared, got %#v", cfg.PostProcess.Models)
		}
		for _, action := range []string{"refine_formal", "as_prompt"} {
			if got := cfg.ModelForAction(action); got != cfg.PostProcess.Model {
				t.Fatalf("ModelForAction(%q) = %q, want %q", action, got, cfg.PostProcess.Model)
			}
		}
	}

	t.Run("direct_state_toggle", func(t *testing.T) {
		cfg := newInitialConfigs()
		state := newSetupFormState(cfg, setupFormOptions{})
		if !state.configureOverrides {
			t.Fatal("expected initial configureOverrides=true when PostProcess.Models is non-empty")
		}
		state.configureOverrides = false

		if err := applySetupFormState(&cfg, state); err != nil {
			t.Fatalf("applySetupFormState: %v", err)
		}
		assertCleared(t, cfg)
	})

	t.Run("interactive_form_toggle", func(t *testing.T) {
		cfg := newInitialConfigs()
		opts := setupFormOptions{IncludeAPIKey: false}
		state := newSetupFormState(cfg, opts)
		if !state.configureOverrides {
			t.Fatal("expected initial configureOverrides=true when PostProcess.Models is non-empty")
		}
		form := initTestForm(buildSetupForm(state, opts))

		// Group 1: keep all defaults (4 fields).
		for i := 0; i < 4; i++ {
			form = pressSpecial(form, tea.KeyEnter)
		}

		// Group 2: keep liveModel and postProcessModel, then toggle Confirm overrides from Yes to No via Right.
		form = pressSpecial(form, tea.KeyEnter)
		form = pressSpecial(form, tea.KeyEnter)
		// Assert a short, stable fragment of the Confirm description so the
		// check does not depend on huh's rendering width (long lines truncate).
		normalizedView := strings.Join(strings.Fields(strings.ReplaceAll(ansi.Strip(form.View()), "┃", " ")), " ")
		if !strings.Contains(normalizedView, "set per-action models") {
			t.Fatalf("expected updated Confirm description in view:\n%s", ansi.Strip(form.View()))
		}
		form = pressSpecial(form, tea.KeyRight)
		form = pressSpecial(form, tea.KeyEnter)

		// Group 3 (Per-Action Model Overrides) must be skipped, landing directly on Group 4 (Prefetch).
		if !strings.Contains(ansi.Strip(form.View()), "Prefetch actions") {
			t.Fatalf("expected Group 3 skipped to Group 4 Prefetch actions, got:\n%s", ansi.Strip(form.View()))
		}
		form = pressSpecial(form, tea.KeyEnter)

		// Group 5: keep customVocabulary, triggerKeys, proxy, translateLanguages (4 fields).
		for i := 0; i < 4; i++ {
			form = pressSpecial(form, tea.KeyEnter)
		}

		if form.State != huh.StateCompleted {
			t.Fatalf("form.State = %v, want StateCompleted (%v)\nview:\n%s", form.State, huh.StateCompleted, ansi.Strip(form.View()))
		}
		if state.configureOverrides {
			t.Fatal("expected state.configureOverrides=false after toggling Confirm to No")
		}
		if err := applySetupFormState(&cfg, state); err != nil {
			t.Fatalf("applySetupFormState: %v", err)
		}
		assertCleared(t, cfg)
	})
}
