package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLI_ModelEffortSuffix(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gemini-voice", "config.toml")
	if err := SaveConfigFile(cfgPath, DefaultConfig()); err != nil {
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
	run := func(args ...string) int {
		stdout.Reset()
		stderr.Reset()
		return runCLI("gemini-voice", args, deps)
	}

	if code := run("models", "--config", cfgPath, "--refine-formal", "gemini-3.5-flash/low", "--default", "gemini-2.5-flash/medium"); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	loaded, err := LoadConfigFile(cfgPath)
	if err != nil || loaded.ModelForAction("refine_formal") != "gemini-3.5-flash/low" || loaded.PostProcess.Model != "gemini-2.5-flash/medium" {
		t.Fatalf("not saved: %v %+v", err, loaded.PostProcess)
	}
	if code := run("models", "--config", cfgPath); code != 0 || !strings.Contains(stdout.String(), "gemini-3.5-flash (override)  effort: low") {
		t.Fatalf("models output missing effort:\n%s", stdout.String())
	}
	for _, args := range [][]string{
		{"models", "--config", cfgPath, "--refine-formal", "gemini-3.5-flash/bogus"},
		{"models", "--config", cfgPath, "--live", "gemini-3.5-transcribe-live/low"},
		{"models", "--config", cfgPath, "--translate", "ja=gemini-3.5-flash/xx"},
		{"config", "set", "--config", cfgPath, "model.ask_ai", "gemini-2.0-flash/low"},
		{"config", "set", "--config", cfgPath, "live.model", "x/low"},
	} {
		if code := run(args...); code == 0 {
			t.Fatalf("expected failure for %v", args)
		}
	}
	if code := run("config", "set", "--config", cfgPath, "model.ask_ai", "gemini-3.5-flash/high"); code != 0 {
		t.Fatalf("config set exit=%d stderr=%s", code, stderr.String())
	}
}
