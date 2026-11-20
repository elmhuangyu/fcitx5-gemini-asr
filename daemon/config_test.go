package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestConfigLoadSystemdTemplate(t *testing.T) {
	cfg, err := LoadConfigFile(filepath.Join("..", "systemd", "config.toml"))
	if err != nil {
		t.Fatalf("load systemd/config.toml: %v", err)
	}
	if cfg.LiveTranscribe.Model != "gemini-3.5-transcribe-live" {
		t.Fatalf("unexpected live model: %q", cfg.LiveTranscribe.Model)
	}
	if cfg.LiveTranscribe.Language != "auto" {
		t.Fatalf("unexpected language: %q", cfg.LiveTranscribe.Language)
	}
	if cfg.LiveTranscribe.Mode != "VERBATIM" {
		t.Fatalf("unexpected mode: %q", cfg.LiveTranscribe.Mode)
	}
	if cfg.LiveTranscribe.VADSilenceMs != 1200 {
		t.Fatalf("unexpected vad_silence_ms: %d", cfg.LiveTranscribe.VADSilenceMs)
	}
	if cfg.PostProcess.Model != "gemini-3.5-flash-lite" {
		t.Fatalf("unexpected post_process model: %q", cfg.PostProcess.Model)
	}
	if len(cfg.PostProcess.Prefetch) != 2 {
		t.Fatalf("unexpected prefetch: %#v", cfg.PostProcess.Prefetch)
	}
	for _, key := range []string{"refine_fix_typos", "refine_casual", "refine_formal", "as_prompt", "translate_en", "ask_ai"} {
		if cfg.Prompts[key] == "" {
			t.Fatalf("missing prompt %q", key)
		}
	}

	raw, err := os.ReadFile(filepath.Join("..", "systemd", "config.toml"))
	if err != nil {
		t.Fatalf("read systemd/config.toml: %v", err)
	}
	if string(raw) != DefaultConfigTOML() {
		t.Fatalf("DefaultConfigTOML() drifted from systemd/config.toml")
	}
}

func TestConfigManagerHotReload_WriteAndAtomicRename(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	initial := `
[live_transcribe]
model = "gemini-3.5-transcribe-live"
language = "zh-CN"
vad_silence_ms = 1200

[prompts]
refine_formal = "initial prompt"
`
	if err := os.WriteFile(cfgPath, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewConfigManager(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	reloaded := make(chan Config, 4)
	mgr.SetOnReload(func(c Config) {
		reloaded <- c
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- mgr.Watch(ctx)
	}()

	// Give fsnotify watcher a moment to attach.
	time.Sleep(50 * time.Millisecond)

	// 1) Atomic rename save (as done by editors like vim/nano).
	tmpPath := filepath.Join(dir, ".config.toml.tmp")
	updated1 := `
[live_transcribe]
model = "gemini-3.5-transcribe-live"
language = "en-US"
vad_silence_ms = 900

[prompts]
refine_formal = "atomic renamed prompt"
`
	if err := os.WriteFile(tmpPath, []byte(updated1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmpPath, cfgPath); err != nil {
		t.Fatal(err)
	}

	select {
	case c := <-reloaded:
		if c.Prompts["refine_formal"] != "atomic renamed prompt" || c.LiveTranscribe.Language != "en-US" {
			t.Fatalf("unexpected reloaded config: %+v", c)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for atomic rename hot-reload")
	}

	// 2) Broken TOML should preserve previous valid config.
	if err := os.WriteFile(cfgPath, []byte("this is = [not valid toml"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if got := mgr.Get().Prompts["refine_formal"]; got != "atomic renamed prompt" {
		t.Fatalf("expected previous valid config preserved, got %q", got)
	}

	// 3) Direct in-place write restores valid update.
	updated2 := `
[prompts]
refine_formal = "direct write prompt"
`
	if err := os.WriteFile(cfgPath, []byte(updated2), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-reloaded:
		if c.Prompts["refine_formal"] != "direct write prompt" {
			t.Fatalf("unexpected direct write config: %+v", c)
		}
		// Default prompts omitted from TOML should still exist.
		if c.Prompts["translate_en"] == "" {
			t.Fatal("expected omitted default prompts to remain intact")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for direct write hot-reload")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watcher returned error: %v", err)
	}
}

func TestConfigLoad_ModeAndVocabularyValidation(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	content := `
[live_transcribe]
language = "zh-CN"
mode = "smart"
custom_vocabulary = [" Fcitx5 ", "", "PipeWire", "Fcitx5"]
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfigFile(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if cfg.LiveTranscribe.Language != "zh-CN" {
		t.Fatalf("expected legacy language preserved, got %q", cfg.LiveTranscribe.Language)
	}
	if got := NormalizeLanguageCodes(cfg.LiveTranscribe.Language); !reflect.DeepEqual(got, []string{"cmn-Hans-CN"}) {
		t.Fatalf("NormalizeLanguageCodes(%q) = %#v", cfg.LiveTranscribe.Language, got)
	}
	if cfg.LiveTranscribe.Mode != "SMART" {
		t.Fatalf("mode = %q, want SMART", cfg.LiveTranscribe.Mode)
	}
	wantVocab := []string{"Fcitx5", "PipeWire"}
	if !reflect.DeepEqual(cfg.LiveTranscribe.CustomVocabulary, wantVocab) {
		t.Fatalf("custom_vocabulary = %#v, want %#v", cfg.LiveTranscribe.CustomVocabulary, wantVocab)
	}

	cloned := cfg.Clone()
	cloned.LiveTranscribe.CustomVocabulary[0] = "Mutated"
	if cfg.LiveTranscribe.CustomVocabulary[0] != "Fcitx5" {
		t.Fatalf("Clone() shared CustomVocabulary backing slice")
	}

	badPath := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(badPath, []byte("[live_transcribe]\nmode = \"bogus\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfigFile(badPath); err == nil {
		t.Fatal("expected error for invalid live_transcribe.mode")
	}
}

func TestTranslateTargetsAndPromptResolution(t *testing.T) {
	cfg := DefaultConfig()
	if got := cfg.TranslateTargets(); len(got) != 1 || got[0] != (TranslateTarget{Action: "translate_en", Label: "English"}) {
		t.Fatalf("default TranslateTargets = %#v", got)
	}

	cfg.PostProcess.TranslateLanguages = []string{"en", "Japanese", "zh-CN", "ja", "粤语"}
	targets := cfg.TranslateTargets()
	want := []TranslateTarget{
		{Action: "translate_en", Label: "English"},
		{Action: "translate_ja", Label: "Japanese"},
		{Action: "translate_zh_cn", Label: "Chinese (Simplified)"},
		{Action: "translate_u_828f92e6", Label: "粤语"},
	}
	// Check length and known actions (exact hash checked via PromptForAction).
	if len(targets) != len(want) {
		t.Fatalf("TranslateTargets len = %d, want %d: %#v", len(targets), len(want), targets)
	}
	for i := 0; i < 3; i++ {
		if targets[i] != want[i] {
			t.Fatalf("targets[%d] = %#v, want %#v", i, targets[i], want[i])
		}
	}
	if targets[3].Label != "粤语" || !strings.HasPrefix(targets[3].Action, "translate_") {
		t.Fatalf("unexpected custom target: %#v", targets[3])
	}

	promptJa, ok := cfg.PromptForAction("translate_ja")
	if !ok || !strings.Contains(promptJa, "Japanese") {
		t.Fatalf("PromptForAction(translate_ja) = %q, %v", promptJa, ok)
	}
	promptCustom, ok := cfg.PromptForAction(targets[3].Action)
	if !ok || !strings.Contains(promptCustom, "粤语") {
		t.Fatalf("PromptForAction(custom) = %q, %v", promptCustom, ok)
	}
}
