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

func TestEffectiveActionNamesIncludesModelOverrides(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Ime.TranslateLanguages = []string{"ja"}
	cfg.PostProcess.Models = map[string]string{
		"translate_ko": "gemini-2.5-flash",
		"translate_ja": "gemini-2.5-pro",
	}
	// translate_ko exists only as a prompt plus an override (not a known or
	// configured action); it must still appear exactly once.
	cfg.Prompts = map[string]string{"translate_ko": "Translate to Korean."}
	names := cfg.EffectiveActionNames()

	hasKo, hasJa := false, false
	for _, n := range names {
		switch n {
		case "translate_ko":
			hasKo = true
		case "translate_ja":
			hasJa = true
		}
	}
	if !hasKo {
		t.Fatalf("expected models-only override translate_ko in %v", names)
	}
	if !hasJa {
		t.Fatalf("expected configured target translate_ja in %v", names)
	}
	// No duplicates when an action appears in several of targets/prompts/overrides.
	seen := make(map[string]int, len(names))
	for _, n := range names {
		seen[n]++
	}
	for n, count := range seen {
		if count > 1 {
			t.Fatalf("action %q appears %d times in %v", n, count, names)
		}
	}
}

func TestConfigManagerHotReload_WriteAndAtomicRename(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "fcitx5", "conf", "gemini-voice.conf")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	initial := "Language=zh-CN\n\n[LiveTranscription]\nVadSilenceMs=1200\n\n[PostProcess/Actions/0]\nName=refine_formal\nPrompt=\"initial prompt\"\n"
	if err := os.WriteFile(cfgPath, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewConfigManager(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := mgr.Get().Prompts["refine_formal"]; got != "initial prompt" {
		t.Fatalf("initial prompt = %q", got)
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

	// Unrelated files in the (shared) fcitx5 conf directory must not trigger reloads.
	if err := os.WriteFile(filepath.Join(dir, "fcitx5", "conf", "other.conf"), []byte("x=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1) Atomic rename save, the way Fcitx5's safeSaveAsIni and editors do it.
	tmpPath := filepath.Join(filepath.Dir(cfgPath), ".gemini-voice.conf.tmp")
	updated1 := "Language=en-US\n\n[LiveTranscription]\nVadSilenceMs=900\n\n[PostProcess/Actions/0]\nName=refine_formal\nPrompt=\"atomic renamed prompt\"\n"
	if err := os.WriteFile(tmpPath, []byte(updated1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmpPath, cfgPath); err != nil {
		t.Fatal(err)
	}

	select {
	case c := <-reloaded:
		if c.Prompts["refine_formal"] != "atomic renamed prompt" || c.LiveLanguage() != "en-US" || c.LiveTranscribe.VADSilenceMs != 900 {
			t.Fatalf("unexpected reloaded config: %+v", c)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for atomic rename hot-reload")
	}

	// 2) An invalid value must preserve the previous valid config.
	if err := os.WriteFile(cfgPath, []byte("[LiveTranscription]\nMode=bogus\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if got := mgr.Get().Prompts["refine_formal"]; got != "atomic renamed prompt" {
		t.Fatalf("expected previous valid config preserved, got %q", got)
	}

	// 3) Direct in-place write restores a valid update.
	updated2 := "[PostProcess/Actions/0]\nName=refine_formal\nPrompt=direct\\ write\n"
	if err := os.WriteFile(cfgPath, []byte(updated2), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-reloaded:
		if c.Prompts["refine_formal"] != "direct\\ write" {
			t.Fatalf("unexpected direct write config: %+v", c)
		}
		// Built-in default prompts omitted from the conf still exist.
		if p, _ := c.PromptForAction("translate_en"); p == "" {
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
	cfgPath := filepath.Join(dir, "gemini-voice.conf")
	content := "Language=zh-CN\n\n[LiveTranscription]\nMode=smart\n\n[LiveTranscription/CustomVocabulary]\n0=\" Fcitx5 \"\n1=\n2=PipeWire\n3=Fcitx5\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfigFile(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if cfg.LiveLanguage() != "zh-CN" {
		t.Fatalf("expected language preserved, got %q", cfg.LiveLanguage())
	}
	if got := NormalizeLanguageCodes(cfg.LiveLanguage()); !reflect.DeepEqual(got, []string{"cmn-Hans-CN"}) {
		t.Fatalf("NormalizeLanguageCodes(%q) = %#v", cfg.LiveLanguage(), got)
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

	for name, bad := range map[string]string{
		"mode":       "[LiveTranscription]\nMode=bogus\n",
		"vad":        "[LiveTranscription]\nVadSilenceMs=-5\n",
		"live model": "[LiveTranscription]\nModel=gemini-3.5-flash-lite/low\n",
		"pp model":   "[PostProcess]\nModel=gemini-3.5-flash/turbo\n",
		"action":     "[PostProcess/Actions/0]\nName=refine_formal\nModel=gemini-3.5-flash/turbo\n",
	} {
		badPath := filepath.Join(dir, "bad.conf")
		if err := os.WriteFile(badPath, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfigFile(badPath); err == nil {
			t.Fatalf("expected error for invalid %s", name)
		}
	}
}

func TestTranslateTargetsAndPromptResolution(t *testing.T) {
	cfg := DefaultConfig()
	if got := cfg.TranslateTargets(); len(got) != 1 || got[0] != (TranslateTarget{Action: "translate_en", Label: "English"}) {
		t.Fatalf("default TranslateTargets = %#v", got)
	}

	cfg.Ime.TranslateLanguages = []string{"en", "Japanese", "zh-CN", "ja", "粤语"}
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
