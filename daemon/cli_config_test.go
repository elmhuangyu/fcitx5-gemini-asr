package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"charm.land/huh/v2"
)

func TestCLI_ConfigSetGetShow(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gemini-voice", "config.toml")
	fcitxPath := filepath.Join(dir, "fcitx5", "conf", "gemini-voice.conf")

	var stdout, stderr bytes.Buffer
	var ranFcitxReload bool
	deps := cliDeps{
		stdin:  strings.NewReader(""),
		stdout: &stdout,
		stderr: &stderr,
		isTTY:  func() bool { return false },
		runCmd: func(ctx context.Context, stdin string, name string, args ...string) cmdResult {
			if name == "fcitx5-remote" {
				ranFcitxReload = true
			}
			return cmdResult{}
		},
		lookPath: func(file string) (string, error) { return "/usr/bin/" + file, nil },
		getenv:   func(string) string { return "" },
		serveFn:  func(string, string) int { return 0 },
	}

	cases := []struct {
		key string
		val string
	}{
		{"ime.mode", "direct"},
		{"ime.trigger_key", "Alt+space,Super+v"},
		{"live.model", "gemini-3.5-transcribe-live"},
		{"live.language", "en-US"},
		{"live.mode", "smart"},
		{"live.custom_vocabulary", "Fcitx5, PipeWire, Gemini"},
		{"live.vad_silence_ms", "800"},
		{"post_process.model", "gemini-2.5-flash"},
		{"model.refine_formal", "gemini-2.5-pro"},
		{"model.as_prompt", "gemini-2.5-pro"},
		{"post_process.prefetch", "refine_fix_typos,as_prompt"},
		{"connection.proxy", "socks5://127.0.0.1:1080"},
		{"prompt.translate_en", "Translate to formal UK English:"},
	}

	for _, tc := range cases {
		stdout.Reset()
		stderr.Reset()
		if code := runCLI("gemini-voice", []string{"config", "set", "-config", cfgPath, "-fcitx-config", fcitxPath, tc.key, tc.val}, deps); code != 0 {
			t.Fatalf("config set %s=%s exit=%d stderr=%s", tc.key, tc.val, code, stderr.String())
		}
	}
	if !ranFcitxReload {
		t.Fatal("expected best-effort fcitx5-remote reload call")
	}

	// Verify config get reads back expected values.
	wantGet := map[string]string{
		"ime.mode":               "direct",
		"ime.show_action_menu":   "false",
		"ime.trigger_key":        "Alt+space,Super+v",
		"live.language":          "en-US",
		"live.mode":              "SMART",
		"live.custom_vocabulary": "Fcitx5,PipeWire,Gemini",
		"live.vad_silence_ms":    "800",
		"model.refine_formal":    "gemini-2.5-pro",
		"model.refine_casual":    "gemini-2.5-flash",
		"post_process.prefetch":  "refine_fix_typos,as_prompt",
		"connection.proxy":       "socks5://127.0.0.1:1080",
		"prompt.translate_en":    "Translate to formal UK English:",
	}
	for k, want := range wantGet {
		stdout.Reset()
		stderr.Reset()
		if code := runCLI("gemini-voice", []string{"config", "get", "-config", cfgPath, "-fcitx-config", fcitxPath, k}, deps); code != 0 {
			t.Fatalf("config get %s exit=%d stderr=%s", k, code, stderr.String())
		}
		if got := strings.TrimSpace(stdout.String()); got != want {
			t.Fatalf("config get %s = %q, want %q", k, got, want)
		}
	}

	// Clearing per-action model override reverts to post_process.model.
	if code := runCLI("gemini-voice", []string{"config", "set", "-config", cfgPath, "-fcitx-config", fcitxPath, "model.refine_formal", "default"}, deps); code != 0 {
		t.Fatalf("clear override exit=%d stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	if code := runCLI("gemini-voice", []string{"config", "get", "-config", cfgPath, "-fcitx-config", fcitxPath, "model.refine_formal"}, deps); code != 0 {
		t.Fatal("get after clear failed")
	}
	if got := strings.TrimSpace(stdout.String()); got != "gemini-2.5-flash" {
		t.Fatalf("expected fallback model gemini-2.5-flash, got %q", got)
	}

	// Clearing prefetch with "none" persists empty slice even after reloading TOML.
	if code := runCLI("gemini-voice", []string{"config", "set", "-config", cfgPath, "-fcitx-config", fcitxPath, "post_process.prefetch", "none"}, deps); code != 0 {
		t.Fatalf("clear prefetch failed: %s", stderr.String())
	}
	// Clearing custom_vocabulary with "none" persists empty slice after reloading TOML.
	if code := runCLI("gemini-voice", []string{"config", "set", "-config", cfgPath, "-fcitx-config", fcitxPath, "live.custom_vocabulary", "none"}, deps); code != 0 {
		t.Fatalf("clear custom_vocabulary failed: %s", stderr.String())
	}
	reloaded, err := LoadConfigFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.PostProcess.Prefetch) != 0 {
		t.Fatalf("expected empty prefetch after reload, got %#v", reloaded.PostProcess.Prefetch)
	}
	if len(reloaded.LiveTranscribe.CustomVocabulary) != 0 {
		t.Fatalf("expected empty custom_vocabulary after reload, got %#v", reloaded.LiveTranscribe.CustomVocabulary)
	}

	// config show displays both Fcitx5 and daemon sections.
	stdout.Reset()
	if code := runCLI("gemini-voice", []string{"config", "show", "-config", cfgPath, "-fcitx-config", fcitxPath}, deps); code != 0 {
		t.Fatalf("config show failed: %s", stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "ime.mode:") || !strings.Contains(out, "live.mode:") || !strings.Contains(out, "live.custom_vocabulary:") || !strings.Contains(out, "post_process.translate_languages:") || !strings.Contains(out, "model.as_prompt:") {
		t.Fatalf("unexpected config show output:\n%s", out)
	}

	// Setting post_process.translate_languages updates EffectiveActionNames and prompt lookup.
	if code := runCLI("gemini-voice", []string{"config", "set", "-config", cfgPath, "-fcitx-config", fcitxPath, "post_process.translate_languages", "en,ja"}, deps); code != 0 {
		t.Fatalf("config set translate_languages failed: %s", stderr.String())
	}
	stdout.Reset()
	if code := runCLI("gemini-voice", []string{"config", "get", "-config", cfgPath, "-fcitx-config", fcitxPath, "post_process.translate_languages"}, deps); code != 0 {
		t.Fatalf("config get translate_languages failed: %s", stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "en,ja" {
		t.Fatalf("translate_languages = %q, want %q", stdout.String(), "en,ja")
	}

	// Invalid key/value rejected.
	for _, bad := range [][2]string{
		{"ime.mode", "invalid"},
		{"connection.proxy", "ftp://bad"},
		{"model.unknown_action", "gemini-2.5-pro"},
		{"live.vad_silence_ms", "-5"},
		{"live.mode", "invalid"},
		{"live.mode", ""},
	} {
		if code := runCLI("gemini-voice", []string{"config", "set", "-config", cfgPath, "-fcitx-config", fcitxPath, bad[0], bad[1]}, deps); code == 0 {
			t.Fatalf("expected error for %s=%s", bad[0], bad[1])
		}
	}
}

func TestWriteConfigUpdatesReportsSplitState(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only directories do not block writes for root")
	}

	fcitxDir := t.TempDir()
	fcitxPath := filepath.Join(fcitxDir, "gemini-voice.conf")

	daemonDir := t.TempDir()
	daemonPath := filepath.Join(daemonDir, "config.toml")
	if err := os.Chmod(daemonDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(daemonDir, 0o700) })

	err := writeConfigUpdates(fcitxPath, []byte("ShowActionMenu=False\n"), daemonPath, []byte("live = {}\n"))
	if err == nil {
		t.Fatal("expected daemon save failure after successful fcitx save")
	}
	msg := err.Error()
	if !strings.Contains(msg, daemonPath) {
		t.Fatalf("error should name the failed daemon config path: %q", msg)
	}
	if !strings.Contains(msg, fcitxPath) {
		t.Fatalf("error should mention the updated fcitx config path: %q", msg)
	}
	if !strings.Contains(msg, "re-run") {
		t.Fatalf("error should explain how to reconcile the split state: %q", msg)
	}
	if _, statErr := os.Stat(fcitxPath); statErr != nil {
		t.Fatalf("fcitx config should have been written before the failure: %v", statErr)
	}
	if _, statErr := os.Stat(daemonPath); statErr == nil {
		t.Fatal("daemon config should not have been written")
	}
}

func TestWriteConfigUpdatesSavesBoth(t *testing.T) {
	dir := t.TempDir()
	fcitxPath := filepath.Join(dir, "fcitx5", "conf", "gemini-voice.conf")
	daemonPath := filepath.Join(dir, "gemini-voice", "config.toml")

	if err := writeConfigUpdates(fcitxPath, []byte("ShowActionMenu=False\n"), daemonPath, []byte("live = {}\n")); err != nil {
		t.Fatalf("writeConfigUpdates: %v", err)
	}
	if data, err := os.ReadFile(fcitxPath); err != nil || !strings.Contains(string(data), "ShowActionMenu=False") {
		t.Fatalf("unexpected fcitx config content: %q err=%v", data, err)
	}
	if data, err := os.ReadFile(daemonPath); err != nil || !strings.Contains(string(data), "live") {
		t.Fatalf("unexpected daemon config content: %q err=%v", data, err)
	}

	// Nil sides are skipped without touching the other file.
	before, err := os.ReadFile(fcitxPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeConfigUpdates(fcitxPath, nil, daemonPath, nil); err != nil {
		t.Fatalf("no-op writeConfigUpdates: %v", err)
	}
	after, err := os.ReadFile(fcitxPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("expected fcitx config untouched when both sides are nil")
	}
}

func TestCLI_ModelsCommandAndRemote(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{
					"name":                       "models/gemini-2.5-pro",
					"supportedGenerationMethods": []string{"generateContent"},
				},
				{
					"name":                       "models/gemini-3.5-transcribe-live",
					"supportedGenerationMethods": []string{"bidiGenerateContent"},
				},
			},
		})
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cfg := DefaultConfig()
	cfg.Connection.APIBaseURL = srv.URL
	if err := SaveConfigFile(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CREDENTIALS_DIRECTORY", "")
	t.Setenv("GEMINI_API_KEY", "test-models-key")

	var stdout, stderr bytes.Buffer
	deps := cliDeps{
		stdin:    strings.NewReader(""),
		stdout:   &stdout,
		stderr:   &stderr,
		isTTY:    func() bool { return false },
		runCmd:   func(context.Context, string, string, ...string) cmdResult { return cmdResult{} },
		lookPath: func(string) (string, error) { return "", os.ErrNotExist },
		getenv:   os.Getenv,
		serveFn:  func(string, string) int { return 0 },
	}

	code := runCLI("gemini-voice", []string{
		"models",
		"-config", cfgPath,
		"--default", "gemini-2.5-flash-lite",
		"--as-prompt", "gemini-2.5-pro",
		"--remote",
	}, deps)
	if code != 0 {
		t.Fatalf("models exit=%d stderr=%s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "gemini-2.5-flash-lite (default)") ||
		!strings.Contains(out, "gemini-2.5-pro (override)") ||
		!strings.Contains(out, "Available Gemini models (2):") ||
		!strings.Contains(out, "gemini-3.5-transcribe-live") {
		t.Fatalf("unexpected models output:\n%s", out)
	}
}

func TestCLI_ConfigureWizardAndInteractiveInit(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gemini-voice", "config.toml")
	fcitxPath := filepath.Join(dir, "fcitx5", "conf", "gemini-voice.conf")

	var stdout, stderr bytes.Buffer
	var encryptedSecret string
	var encryptCalls int
	deps := cliDeps{
		stdin:  strings.NewReader(""),
		stdout: &stdout,
		stderr: &stderr,
		isTTY:  func() bool { return true },
		runCmd: func(ctx context.Context, stdin string, name string, args ...string) cmdResult {
			if name == "systemd-creds" && len(args) > 0 && args[0] == "encrypt" {
				encryptCalls++
				encryptedSecret = stdin
				outFile := args[len(args)-1]
				if err := os.WriteFile(outFile, []byte("ENC"), 0o600); err != nil {
					return cmdResult{err: err}
				}
				return cmdResult{}
			}
			if name == "systemd-creds" && len(args) > 0 && args[0] == "decrypt" {
				return cmdResult{stdout: encryptedSecret}
			}
			return cmdResult{}
		},
		lookPath: func(file string) (string, error) { return "/usr/bin/" + file, nil },
		getenv:   func(string) string { return "" },
		serveFn:  func(string, string) int { return 0 },
		runSetupForm: func(state *setupFormState, opts setupFormOptions) error {
			if !opts.IncludeAPIKey || opts.CredExists {
				t.Fatalf("unexpected init setup opts: %+v", opts)
			}
			state.showActionMenu = true
			state.language = "en-US"
			state.liveMode = "SMART"
			state.vadSilenceMs = "900"
			state.liveModel = "gemini-3.5-transcribe-live"
			state.postProcessModel = "gemini-2.5-flash"
			state.configureOverrides = true
			for i, a := range state.actionOrder {
				switch a {
				case "refine_fix_typos":
					state.actionOverrides[i] = "gemini-2.5-flash-lite"
				case "refine_formal", "as_prompt":
					state.actionOverrides[i] = "gemini-2.5-pro"
				}
			}
			state.prefetch = []string{"refine_fix_typos", "as_prompt"}
			state.customVocabulary = "Fcitx5, PipeWire"
			state.triggerKeys = "Super+v"
			state.proxy = "http://127.0.0.1:7890"
			state.overwriteKey = true
			state.apiKey = "AIzaSyWizardKey5555"
			return nil
		},
	}

	if code := runCLI("gemini-voice", []string{"init", "-config", cfgPath, "-fcitx-config", fcitxPath}, deps); code != 0 {
		t.Fatalf("interactive init exit=%d stderr=%s", code, stderr.String())
	}

	cfg, err := LoadConfigFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LiveTranscribe.Language != "en-US" || cfg.LiveTranscribe.Mode != "SMART" || cfg.LiveTranscribe.VADSilenceMs != 900 {
		t.Fatalf("unexpected live config: %+v", cfg.LiveTranscribe)
	}
	if !reflect.DeepEqual(cfg.LiveTranscribe.CustomVocabulary, []string{"Fcitx5", "PipeWire"}) {
		t.Fatalf("unexpected custom_vocabulary: %+v", cfg.LiveTranscribe.CustomVocabulary)
	}
	if cfg.ModelForAction("refine_fix_typos") != "gemini-2.5-flash-lite" ||
		cfg.ModelForAction("refine_formal") != "gemini-2.5-pro" ||
		cfg.ModelForAction("as_prompt") != "gemini-2.5-pro" ||
		cfg.ModelForAction("translate_en") != "gemini-2.5-flash" {
		t.Fatalf("unexpected model routing: %+v", cfg.PostProcess)
	}
	if !reflect.DeepEqual(cfg.PostProcess.Prefetch, []string{"refine_fix_typos", "as_prompt"}) {
		t.Fatalf("unexpected prefetch: %+v", cfg.PostProcess.Prefetch)
	}
	if cfg.Connection.Proxy != "http://127.0.0.1:7890" {
		t.Fatalf("unexpected proxy: %q", cfg.Connection.Proxy)
	}

	fcitxCfg, err := LoadFcitxAddonConfig(fcitxPath)
	if err != nil {
		t.Fatal(err)
	}
	if !fcitxCfg.ShowActionMenu || !reflect.DeepEqual(fcitxCfg.TriggerKeys, []string{"Super+v"}) {
		t.Fatalf("unexpected fcitx config: %+v", fcitxCfg)
	}
	if encryptedSecret != "AIzaSyWizardKey5555" || encryptCalls != 1 {
		t.Fatalf("unexpected encrypted secret %q calls=%d", encryptedSecret, encryptCalls)
	}

	// Second init when credential exists and user chooses to keep existing credential.
	stdout.Reset()
	stderr.Reset()
	deps.runSetupForm = func(state *setupFormState, opts setupFormOptions) error {
		if !opts.CredExists {
			t.Fatal("expected CredExists=true on second init")
		}
		if state.overwriteKey {
			t.Fatal("expected overwriteKey default false when credential exists")
		}
		return nil
	}
	if code := runCLI("gemini-voice", []string{"init", "-config", cfgPath, "-fcitx-config", fcitxPath}, deps); code != 0 {
		t.Fatalf("second interactive init exit=%d stderr=%s", code, stderr.String())
	}
	if encryptCalls != 1 || !strings.Contains(stdout.String(), "Keeping existing credential") {
		t.Fatalf("expected existing credential preserved without re-encrypting; calls=%d stdout=%s", encryptCalls, stdout.String())
	}

	// Aborting TUI init must exit non-zero and leave existing config intact.
	beforeAbort, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	deps.runSetupForm = func(state *setupFormState, opts setupFormOptions) error {
		state.language = "fr-FR"
		return huh.ErrUserAborted
	}
	if code := runCLI("gemini-voice", []string{"init", "-config", cfgPath, "-fcitx-config", fcitxPath}, deps); code != 1 {
		t.Fatalf("expected exit 1 on abort, got %d", code)
	}
	if !strings.Contains(stderr.String(), "Setup cancelled") {
		t.Fatalf("expected cancellation message, got %q", stderr.String())
	}
	afterAbort, err := os.ReadFile(cfgPath)
	if err != nil || string(afterAbort) != string(beforeAbort) {
		t.Fatalf("aborted init modified config.toml")
	}

	// Standalone `configure` updates to direct commit mode without touching API key.
	stdout.Reset()
	stderr.Reset()
	deps.runSetupForm = func(state *setupFormState, opts setupFormOptions) error {
		if opts.IncludeAPIKey {
			t.Fatal("configure should not prompt for API key")
		}
		state.showActionMenu = false
		state.proxy = "none"
		return nil
	}
	if code := runCLI("gemini-voice", []string{"configure", "-config", cfgPath, "-fcitx-config", fcitxPath}, deps); code != 0 {
		t.Fatalf("configure exit=%d stderr=%s", code, stderr.String())
	}
	fcitxCfg2, err := LoadFcitxAddonConfig(fcitxPath)
	if err != nil {
		t.Fatal(err)
	}
	if fcitxCfg2.ShowActionMenu {
		t.Fatal("expected ShowActionMenu=false after selecting direct mode in configure")
	}

	// Non-TTY configure fails clearly.
	stdout.Reset()
	stderr.Reset()
	deps.runSetupForm = nil
	deps.isTTY = func() bool { return false }
	if code := runCLI("gemini-voice", []string{"configure", "-config", cfgPath, "-fcitx-config", fcitxPath}, deps); code != 1 {
		t.Fatalf("expected exit 1 for non-TTY configure, got %d", code)
	}
	if !strings.Contains(stderr.String(), "gemini-voice config set") {
		t.Fatalf("expected config set hint in non-TTY stderr: %q", stderr.String())
	}
}

func TestCLI_ModelsTranslateFlag(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gemini-voice", "config.toml")
	cfg := DefaultConfig()
	cfg.PostProcess.TranslateLanguages = []string{"ja", "zh-cn"}
	if err := SaveConfigFile(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	deps := cliDeps{
		stdin:    strings.NewReader(""),
		stdout:   &stdout,
		stderr:   &stderr,
		isTTY:    func() bool { return false },
		runCmd:   func(context.Context, string, string, ...string) cmdResult { return cmdResult{} },
		lookPath: func(string) (string, error) { return "", os.ErrNotExist },
		getenv:   func(string) string { return "" },
		serveFn:  func(string, string) int { return 0 },
	}

	// Set per-language translation overrides via --translate and keep --translate-en working.
	code := runCLI("gemini-voice", []string{
		"models", "--config", cfgPath,
		"--translate", "ja=gemini-2.5-flash",
		"--translate", "zh-cn=gemini-2.5-pro",
		"--translate-en", "gemini-2.5-flash-lite",
	}, deps)
	if code != 0 {
		t.Fatalf("models --translate exit=%d stderr=%s", code, stderr.String())
	}

	loaded, err := LoadConfigFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ModelForAction("translate_ja") != "gemini-2.5-flash" {
		t.Fatalf("expected translate_ja=gemini-2.5-flash, got %q", loaded.ModelForAction("translate_ja"))
	}
	if loaded.ModelForAction("translate_zh_cn") != "gemini-2.5-pro" {
		t.Fatalf("expected translate_zh_cn=gemini-2.5-pro, got %q", loaded.ModelForAction("translate_zh_cn"))
	}
	if loaded.ModelForAction("translate_en") != "gemini-2.5-flash-lite" {
		t.Fatalf("expected translate_en=gemini-2.5-flash-lite, got %q", loaded.ModelForAction("translate_en"))
	}

	// Clear override with <lang>=default.
	stdout.Reset()
	stderr.Reset()
	code = runCLI("gemini-voice", []string{"models", "--config", cfgPath, "--translate", "ja=default"}, deps)
	if code != 0 {
		t.Fatalf("models --translate ja=default exit=%d stderr=%s", code, stderr.String())
	}
	loaded, err = LoadConfigFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.PostProcess.Models["translate_ja"]; ok {
		t.Fatalf("expected translate_ja override cleared, got %+v", loaded.PostProcess.Models)
	}

	// Invalid format without '=' returns exit 2.
	stdout.Reset()
	stderr.Reset()
	code = runCLI("gemini-voice", []string{"models", "--config", cfgPath, "--translate", "ja"}, deps)
	if code != 2 {
		t.Fatalf("expected exit 2 for invalid --translate format, got %d", code)
	}

	// Hyphenated/underscored translate-jp / translate_jp resolves to translate_ja without double-prefixing.
	stdout.Reset()
	stderr.Reset()
	code = runCLI("gemini-voice", []string{
		"models", "--config", cfgPath,
		"--translate", "translate-jp=gemini-2.5-flash",
		"--translate", "fr=gemini-2.5-pro",
	}, deps)
	if code != 0 {
		t.Fatalf("models --translate translate-jp exit=%d stderr=%s", code, stderr.String())
	}
	loaded, err = LoadConfigFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ModelForAction("translate_ja") != "gemini-2.5-flash" {
		t.Fatalf("expected translate_ja=gemini-2.5-flash, got %q", loaded.ModelForAction("translate_ja"))
	}
	if _, doublePrefixed := loaded.PostProcess.Models["translate_translate_jp"]; doublePrefixed {
		t.Fatalf("unexpected double-prefixed key translate_translate_jp in %+v", loaded.PostProcess.Models)
	}
	if loaded.ModelForAction("translate_fr") != "gemini-2.5-pro" {
		t.Fatalf("expected translate_fr=gemini-2.5-pro, got %q", loaded.ModelForAction("translate_fr"))
	}

	// Unknown/unsupported or malformed targets return exit 2.
	for _, badSpec := range []string{
		"klingon=gemini-2.5-flash",
		"translate-klingon=gemini-2.5-flash",
		"translate_klingon=gemini-2.5-flash",
		"translate-=gemini-2.5-flash",
		"translate_=gemini-2.5-flash",
	} {
		stdout.Reset()
		stderr.Reset()
		if code := runCLI("gemini-voice", []string{"models", "--config", cfgPath, "--translate", badSpec}, deps); code != 2 {
			t.Fatalf("expected exit 2 for --translate %q, got %d (stderr=%s)", badSpec, code, stderr.String())
		}
	}

	// Configured custom target language is accepted.
	cfg.PostProcess.TranslateLanguages = []string{"ja", "zh-cn", "粤语"}
	if err := SaveConfigFile(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runCLI("gemini-voice", []string{"models", "--config", cfgPath, "--translate", "粤语=gemini-2.5-flash-lite"}, deps); code != 0 {
		t.Fatalf("expected configured custom language accepted, exit=%d stderr=%s", code, stderr.String())
	}
}

func TestCLI_ImeTranslateLanguagesResetAndMigration(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gemini-voice", "config.toml")
	fcitxPath := filepath.Join(dir, "fcitx5", "conf", "gemini-voice.conf")

	var stdout, stderr bytes.Buffer
	reloadCalls := 0
	deps := cliDeps{
		stdin:  strings.NewReader(""),
		stdout: &stdout,
		stderr: &stderr,
		isTTY:  func() bool { return false },
		runCmd: func(ctx context.Context, stdin string, name string, args ...string) cmdResult {
			if name == "fcitx5-remote" {
				reloadCalls++
			}
			return cmdResult{}
		},
		lookPath: func(file string) (string, error) { return "/usr/bin/" + file, nil },
		getenv:   func(string) string { return "" },
		serveFn:  func(string, string) int { return 0 },
	}

	// Write legacy gemini-voice.conf with orphaned [TranslateLanguages] 0=en.
	legacy := DefaultFcitxAddonConfig()
	legacy.TranslateLanguages = []string{"en"}
	if err := SaveFcitxAddonConfig(fcitxPath, legacy); err != nil {
		t.Fatal(err)
	}

	// Setting post_process.translate_languages triggers one-time migration of orphaned 0=en.
	if code := runCLI("gemini-voice", []string{"config", "set", "-config", cfgPath, "-fcitx-config", fcitxPath, "post_process.translate_languages", "en,ja"}, deps); code != 0 {
		t.Fatalf("config set post_process.translate_languages exit=%d stderr=%s", code, stderr.String())
	}
	if reloadCalls != 1 {
		t.Fatalf("expected fcitx5-remote reload after migration, got %d", reloadCalls)
	}
	raw, err := os.ReadFile(fcitxPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "[TranslateLanguages]") {
		t.Fatalf("expected [TranslateLanguages] omitted after migration, got:\n%s", raw)
	}

	// Explicitly setting ime.translate_languages=en after migration is preserved.
	if code := runCLI("gemini-voice", []string{"config", "set", "-config", cfgPath, "-fcitx-config", fcitxPath, "ime.translate_languages", "en"}, deps); code != 0 {
		t.Fatalf("config set ime.translate_languages=en exit=%d stderr=%s", code, stderr.String())
	}
	fcitxCfg, err := LoadFcitxAddonConfig(fcitxPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fcitxCfg.TranslateLanguages, []string{"en"}) {
		t.Fatalf("expected [en], got %#v", fcitxCfg.TranslateLanguages)
	}

	// Resetting with "default" or "" clears TranslateLanguages and omits [TranslateLanguages].
	for _, resetVal := range []string{"default", ""} {
		if code := runCLI("gemini-voice", []string{"config", "set", "-config", cfgPath, "-fcitx-config", fcitxPath, "ime.translate_languages", "ja,fr"}, deps); code != 0 {
			t.Fatalf("config set ime.translate_languages=ja,fr exit=%d stderr=%s", code, stderr.String())
		}
		if code := runCLI("gemini-voice", []string{"config", "set", "-config", cfgPath, "-fcitx-config", fcitxPath, "ime.translate_languages", resetVal}, deps); code != 0 {
			t.Fatalf("config set ime.translate_languages=%q exit=%d stderr=%s", resetVal, code, stderr.String())
		}
		raw, err = os.ReadFile(fcitxPath)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "[TranslateLanguages]") {
			t.Fatalf("expected [TranslateLanguages] omitted after reset %q, got:\n%s", resetVal, raw)
		}
		stdout.Reset()
		if code := runCLI("gemini-voice", []string{"config", "get", "-config", cfgPath, "-fcitx-config", fcitxPath, "ime.translate_languages"}, deps); code != 0 {
			t.Fatalf("config get ime.translate_languages exit=%d stderr=%s", code, stderr.String())
		}
		if got := strings.TrimSpace(stdout.String()); got != "" {
			t.Fatalf("expected empty ime.translate_languages after reset %q, got %q", resetVal, got)
		}
	}
}

func TestCLI_ConfigSetInvalidKeyDoesNotMigrate(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gemini-voice", "config.toml")
	fcitxPath := filepath.Join(dir, "fcitx5", "conf", "gemini-voice.conf")
	markerPath := translateMigrationMarkerPath(cfgPath)

	if err := os.MkdirAll(filepath.Dir(fcitxPath), 0o700); err != nil {
		t.Fatal(err)
	}
	orig := "# keep comment\nShowActionMenu=True\n\n[TranslateLanguages]\n0=en\n"
	if err := os.WriteFile(fcitxPath, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	deps := cliDeps{
		stdin:    strings.NewReader(""),
		stdout:   &stdout,
		stderr:   &stderr,
		isTTY:    func() bool { return false },
		runCmd:   func(context.Context, string, string, ...string) cmdResult { return cmdResult{} },
		lookPath: func(string) (string, error) { return "", os.ErrNotExist },
		getenv:   func(string) string { return "" },
		serveFn:  func(string, string) int { return 0 },
	}

	code := runCLI("gemini-voice", []string{"config", "set", "-config", cfgPath, "-fcitx-config", fcitxPath, "bogus.key", "x"}, deps)
	if code != 1 {
		t.Fatalf("expected exit 1 for bogus.key, got %d (stderr=%s)", code, stderr.String())
	}

	got, err := os.ReadFile(fcitxPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != orig {
		t.Fatalf("expected gemini-voice.conf untouched on failed config set:\ngot:\n%s\nwant:\n%s", string(got), orig)
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("expected no migration marker on failed config set, stat err=%v", err)
	}
}

func TestCLI_ConfigSetFcitxDirtyWithAndWithoutTranslateLanguages(t *testing.T) {
	t.Run("ime.trigger_key clears orphaned 0=en after migration", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "gemini-voice", "config.toml")
		fcitxPath := filepath.Join(dir, "fcitx5", "conf", "gemini-voice.conf")

		legacy := DefaultFcitxAddonConfig()
		legacy.TranslateLanguages = []string{"en"}
		if err := SaveFcitxAddonConfig(fcitxPath, legacy); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		deps := cliDeps{
			stdin:    strings.NewReader(""),
			stdout:   &stdout,
			stderr:   &stderr,
			isTTY:    func() bool { return false },
			runCmd:   func(context.Context, string, string, ...string) cmdResult { return cmdResult{} },
			lookPath: func(string) (string, error) { return "", os.ErrNotExist },
			getenv:   func(string) string { return "" },
			serveFn:  func(string, string) int { return 0 },
		}

		if code := runCLI("gemini-voice", []string{"config", "set", "-config", cfgPath, "-fcitx-config", fcitxPath, "ime.trigger_key", "Alt+a"}, deps); code != 0 {
			t.Fatalf("config set ime.trigger_key exit=%d stderr=%s", code, stderr.String())
		}
		fcitxCfg, err := LoadFcitxAddonConfig(fcitxPath)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(fcitxCfg.TriggerKeys, []string{"Alt+a"}) {
			t.Fatalf("expected TriggerKeys=[Alt+a], got %#v", fcitxCfg.TriggerKeys)
		}
		if len(fcitxCfg.TranslateLanguages) != 0 {
			t.Fatalf("expected migrated TranslateLanguages cleared, got %#v", fcitxCfg.TranslateLanguages)
		}
	})

	t.Run("ime.translate_languages=en preserves explicit en even on first run", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "gemini-voice", "config.toml")
		fcitxPath := filepath.Join(dir, "fcitx5", "conf", "gemini-voice.conf")

		legacy := DefaultFcitxAddonConfig()
		legacy.TranslateLanguages = []string{"en"}
		if err := SaveFcitxAddonConfig(fcitxPath, legacy); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		deps := cliDeps{
			stdin:    strings.NewReader(""),
			stdout:   &stdout,
			stderr:   &stderr,
			isTTY:    func() bool { return false },
			runCmd:   func(context.Context, string, string, ...string) cmdResult { return cmdResult{} },
			lookPath: func(string) (string, error) { return "", os.ErrNotExist },
			getenv:   func(string) string { return "" },
			serveFn:  func(string, string) int { return 0 },
		}

		if code := runCLI("gemini-voice", []string{"config", "set", "-config", cfgPath, "-fcitx-config", fcitxPath, "ime.translate_languages", "en"}, deps); code != 0 {
			t.Fatalf("config set ime.translate_languages=en exit=%d stderr=%s", code, stderr.String())
		}
		fcitxCfg, err := LoadFcitxAddonConfig(fcitxPath)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(fcitxCfg.TranslateLanguages, []string{"en"}) {
			t.Fatalf("expected explicit TranslateLanguages=[en] preserved, got %#v", fcitxCfg.TranslateLanguages)
		}
		if _, err := os.Stat(translateMigrationMarkerPath(cfgPath)); err != nil {
			t.Fatalf("expected migration marker created, stat err=%v", err)
		}
	})
}
