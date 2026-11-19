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
		"ime.mode":              "direct",
		"ime.show_action_menu":  "false",
		"ime.trigger_key":       "Alt+space,Super+v",
		"live.language":         "en-US",
		"live.vad_silence_ms":   "800",
		"model.refine_formal":   "gemini-2.5-pro",
		"model.refine_casual":   "gemini-2.5-flash",
		"post_process.prefetch": "refine_fix_typos,as_prompt",
		"connection.proxy":      "socks5://127.0.0.1:1080",
		"prompt.translate_en":   "Translate to formal UK English:",
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
	reloaded, err := LoadConfigFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.PostProcess.Prefetch) != 0 {
		t.Fatalf("expected empty prefetch after reload, got %#v", reloaded.PostProcess.Prefetch)
	}

	// config show displays both Fcitx5 and daemon sections.
	stdout.Reset()
	if code := runCLI("gemini-voice", []string{"config", "show", "-config", cfgPath, "-fcitx-config", fcitxPath}, deps); code != 0 {
		t.Fatalf("config show failed: %s", stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "ime.mode:") || !strings.Contains(out, "model.as_prompt:") {
		t.Fatalf("unexpected config show output:\n%s", out)
	}

	// Invalid key/value rejected.
	for _, bad := range [][2]string{
		{"ime.mode", "invalid"},
		{"connection.proxy", "ftp://bad"},
		{"model.unknown_action", "gemini-2.5-pro"},
		{"live.vad_silence_ms", "-5"},
	} {
		if code := runCLI("gemini-voice", []string{"config", "set", "-config", cfgPath, "-fcitx-config", fcitxPath, bad[0], bad[1]}, deps); code == 0 {
			t.Fatalf("expected error for %s=%s", bad[0], bad[1])
		}
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

	// Wizard scripted input:
	// 1) mode=1 (action menu)
	// 2) language=en-US
	// 3) vad=900
	// 4) live model=gemini-3.5-transcribe-live
	// 5) default pp model=gemini-2.5-flash
	// 6) per-action overrides? y
	//    refine_fix_typos -> gemini-2.5-flash-lite
	//    refine_casual -> (enter = keep)
	//    refine_formal -> gemini-2.5-pro
	//    translate_en -> (enter = keep)
	//    as_prompt -> gemini-2.5-pro
	// 7) prefetch -> refine_fix_typos,as_prompt
	// 8) trigger key -> Super+v
	// 9) proxy -> http://127.0.0.1:7890
	wizardInput := strings.Join([]string{
		"1",
		"en-US",
		"900",
		"gemini-3.5-transcribe-live",
		"gemini-2.5-flash",
		"y",
		"gemini-2.5-flash-lite",
		"",
		"gemini-2.5-pro",
		"",
		"gemini-2.5-pro",
		"refine_fix_typos,as_prompt",
		"Super+v",
		"http://127.0.0.1:7890",
		"",
	}, "\n")

	var stdout, stderr bytes.Buffer
	var encryptedSecret string
	deps := cliDeps{
		stdin:  strings.NewReader(wizardInput),
		stdout: &stdout,
		stderr: &stderr,
		isTTY:  func() bool { return true },
		readPassword: func() (string, error) {
			return "AIzaSyWizardKey5555", nil
		},
		runCmd: func(ctx context.Context, stdin string, name string, args ...string) cmdResult {
			if name == "systemd-creds" && len(args) > 0 && args[0] == "encrypt" {
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
	}

	if code := runCLI("gemini-voice", []string{"init", "-config", cfgPath, "-fcitx-config", fcitxPath}, deps); code != 0 {
		t.Fatalf("interactive init exit=%d stderr=%s", code, stderr.String())
	}

	cfg, err := LoadConfigFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LiveTranscribe.Language != "en-US" || cfg.LiveTranscribe.VADSilenceMs != 900 {
		t.Fatalf("unexpected live config: %+v", cfg.LiveTranscribe)
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
	if encryptedSecret != "AIzaSyWizardKey5555" {
		t.Fatalf("unexpected encrypted secret %q", encryptedSecret)
	}

	// Also verify standalone `configure` updates to direct commit mode.
	stdout.Reset()
	stderr.Reset()
	deps.stdin = strings.NewReader("2\n\n\n\n\nN\n\nnone\n")
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
}
