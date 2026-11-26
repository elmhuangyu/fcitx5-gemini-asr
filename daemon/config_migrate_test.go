package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// legacyDefaultTOML is the config.toml template the previous release shipped.
const legacyDefaultTOML = `[connection]
credential_name = "gemini_api_key"
fallback_env = "GEMINI_API_KEY"
proxy = ""

[live_transcribe]
model = "gemini-3.5-transcribe-live"
language = "auto"
mode = "VERBATIM"
vad_silence_ms = 1200

[post_process]
model = "gemini-3.5-flash-lite"
prefetch = ["refine_fix_typos", "refine_formal"]
translate_languages = ["en"]

[prompts]
refine_fix_typos = "仅修正以下语音转录文本中的错别字、同音字错误和标点符号。严禁增删词汇或改变原本的口语语气，直接输出结果："
translate_en = "Translate the following text into natural, idiomatic English. Output ONLY the translation:"
`

const legacyCustomTOML = `[connection]
credential_name = "custom_cred"
fallback_env = "MY_KEY"
proxy = "socks5://127.0.0.1:1080"
live_ws_url = "wss://example.invalid/live"
api_base_url = "https://example.invalid"

[live_transcribe]
model = "gemini-3.5-transcribe-live"
language = "cmn-Hans-CN,en-US"
mode = "SMART"
custom_vocabulary = ["Fcitx5", "Pipe Wire"]
vad_silence_ms = 0

[post_process]
model = "gemini-3.5-flash/low"
prefetch = []
translate_languages = ["en", "ja"]

[post_process.models]
refine_casual = "gemini-4-hydrogen/high"
translate_ja = "gemini-3.5-flash"

[prompts]
refine_formal = "Be formal.\nSecond \"line\"."
translate_ja = "Translate to Japanese."
refine_fix_typos = "仅修正以下语音转录文本中的错别字、同音字错误和标点符号。严禁增删词汇或改变原本的口语语气，直接输出结果："
`

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateLegacyConfig_Defaults(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "gemini-voice", "config.toml")
	confPath := filepath.Join(dir, "fcitx5", "conf", "gemini-voice.conf")
	writeFile(t, tomlPath, legacyDefaultTOML)

	modified, err := MigrateLegacyConfig(tomlPath, confPath)
	if err != nil {
		t.Fatal(err)
	}
	// Everything in the old default file equals the new defaults, so the new
	// conf is just the canonical default file (no prompts, no overrides).
	if !modified {
		t.Fatal("a missing conf is created")
	}
	data, _ := os.ReadFile(confPath)
	if !bytes.Equal(data, mergeConfig(nil, DefaultConfig())) {
		t.Fatalf("default toml must yield the canonical default conf, got:\n%s", data)
	}
	if _, err := os.Stat(tomlPath); !os.IsNotExist(err) {
		t.Fatalf("config.toml must be renamed away, stat err=%v", err)
	}
	if _, err := os.Stat(tomlPath + ".migrated"); err != nil {
		t.Fatalf("expected config.toml.migrated: %v", err)
	}
	// Second run is a no-op: the toml is never read again.
	if modified, err := MigrateLegacyConfig(tomlPath, confPath); err != nil || modified {
		t.Fatalf("second migrate: modified=%v err=%v", modified, err)
	}
}

func TestMigrateLegacyConfig_ImportsEverything(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "gemini-voice", "config.toml")
	confPath := filepath.Join(dir, "fcitx5", "conf", "gemini-voice.conf")
	writeFile(t, tomlPath, legacyCustomTOML)
	// An existing addon conf (as saved by fcitx5-configtool): its addon keys
	// and comments survive, and its Language wins over live.language.
	writeFile(t, confPath, "# Show LLM Action Menu\n# ShowActionMenu=True\nRecordingPrefix=REC\nLanguage=ja-JP\n\n[TriggerKey]\n0=Super+v\n")

	modified, err := MigrateLegacyConfig(tomlPath, confPath)
	if err != nil || !modified {
		t.Fatalf("migrate: modified=%v err=%v", modified, err)
	}

	cfg, err := LoadConfigFile(confPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ime.RecordingPrefix != "REC" || !reflect.DeepEqual(cfg.Ime.TriggerKeys, []string{"Super+v"}) || cfg.Ime.Language != "ja-JP" {
		t.Fatalf("existing addon keys changed: %+v", cfg.Ime)
	}
	if !reflect.DeepEqual(cfg.Ime.TranslateLanguages, []string{"en", "ja"}) {
		t.Fatalf("translate languages = %#v", cfg.Ime.TranslateLanguages)
	}
	if cfg.Connection != (ConnectionConfig{Proxy: "socks5://127.0.0.1:1080", LiveWSURL: "wss://example.invalid/live", APIBaseURL: "https://example.invalid"}) {
		t.Fatalf("connection = %+v", cfg.Connection)
	}
	if cfg.LiveTranscribe.Mode != "SMART" || cfg.LiveTranscribe.VADSilenceMs != 0 || !reflect.DeepEqual(cfg.LiveTranscribe.CustomVocabulary, []string{"Fcitx5", "Pipe Wire"}) {
		t.Fatalf("live = %+v", cfg.LiveTranscribe)
	}
	if cfg.PostProcess.Model != "gemini-3.5-flash/low" || len(cfg.PrefetchActionNames()) != 0 {
		t.Fatalf("post process = %+v", cfg.PostProcess)
	}
	wantModels := map[string]string{"translate_ja": "gemini-3.5-flash"}
	if cfg.PostProcess.Groups["refine_casual"].Model != "gemini-4-hydrogen/high" {
		t.Fatalf("refine_casual group = %+v", cfg.PostProcess.Groups["refine_casual"])
	}
	if !reflect.DeepEqual(cfg.PostProcess.Models, wantModels) {
		t.Fatalf("models = %#v", cfg.PostProcess.Models)
	}
	if cfg.PostProcess.Groups["refine_formal"].Prompt != "Be formal.\nSecond \"line\"." || cfg.Prompts["translate_ja"] != "Translate to Japanese." {
		t.Fatalf("prompts = %#v", cfg.Prompts)
	}

	data, _ := os.ReadFile(confPath)
	if !strings.HasPrefix(string(data), "# Show LLM Action Menu\n# ShowActionMenu=True\nRecordingPrefix=REC\nLanguage=ja-JP\n") {
		t.Fatalf("hand-edited head of the conf was not preserved:\n%s", data)
	}
	// Prompts equal to the built-in default are not pinned.
	if strings.Contains(string(data), "refine_fix_typos") {
		t.Fatalf("default prompt was written to the conf:\n%s", data)
	}
	// The credential name/env are no longer configurable and never written.
	for _, forbidden := range []string{"custom_cred", "MY_KEY", "credential", "Credential"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("conf contains %q:\n%s", forbidden, data)
		}
	}
	if _, err := os.Stat(tomlPath + ".migrated"); err != nil {
		t.Fatalf("expected config.toml.migrated: %v", err)
	}
}

func TestMigrateLegacyConfig_LanguageAndExistingKeys(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	confPath := filepath.Join(dir, "gemini-voice.conf")
	writeFile(t, tomlPath, legacyCustomTOML)
	// The conf already configures some daemon keys: those win, the rest is imported.
	writeFile(t, confPath, "[LiveTranscription]\nMode=VERBATIM\n\n[PostProcess]\nModel=gemini-3.5-flash\n")

	if _, err := MigrateLegacyConfig(tomlPath, confPath); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigFile(confPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LiveTranscribe.Mode != "VERBATIM" || cfg.PostProcess.Model != "gemini-3.5-flash" {
		t.Fatalf("existing conf keys were overwritten: %+v %+v", cfg.LiveTranscribe, cfg.PostProcess)
	}
	// No Language in the conf: live.language is carried over.
	if cfg.Ime.Language != "cmn-Hans-CN,en-US" {
		t.Fatalf("Language = %q", cfg.Ime.Language)
	}
	if cfg.Connection.Proxy != "socks5://127.0.0.1:1080" {
		t.Fatalf("proxy not imported: %+v", cfg.Connection)
	}
}

func TestMigrateLegacyConfig_InvalidKeepsTOML(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	confPath := filepath.Join(dir, "gemini-voice.conf")

	writeFile(t, tomlPath, "this is = [not valid")
	if _, err := MigrateLegacyConfig(tomlPath, confPath); err == nil {
		t.Fatal("expected error for unparsable toml")
	}
	if _, err := os.Stat(tomlPath); err != nil {
		t.Fatalf("unparsable toml must stay in place: %v", err)
	}
	if _, err := os.Stat(confPath); !os.IsNotExist(err) {
		t.Fatalf("conf must not be created on failure, stat err=%v", err)
	}

	writeFile(t, tomlPath, "[post_process]\nmodel = \"gemini-3.5-flash/turbo\"\n")
	if _, err := MigrateLegacyConfig(tomlPath, confPath); err == nil {
		t.Fatal("expected validation error")
	}
	if _, err := os.Stat(tomlPath); err != nil {
		t.Fatalf("invalid toml must stay in place: %v", err)
	}
}

func TestMigrateLegacyConfig_MissingTOMLIsNoOp(t *testing.T) {
	dir := t.TempDir()
	modified, err := MigrateLegacyConfig(filepath.Join(dir, "config.toml"), filepath.Join(dir, "gemini-voice.conf"))
	if err != nil || modified {
		t.Fatalf("modified=%v err=%v", modified, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gemini-voice.conf")); !os.IsNotExist(err) {
		t.Fatal("conf must not be created when there is nothing to migrate")
	}
}

func TestMigrateLegacyConfig_KeepsExistingMigratedBackup(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	confPath := filepath.Join(dir, "gemini-voice.conf")
	writeFile(t, tomlPath+".migrated", "older backup")
	writeFile(t, tomlPath, legacyDefaultTOML)
	if _, err := MigrateLegacyConfig(tomlPath, confPath); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(tomlPath + ".migrated"); string(got) != "older backup" {
		t.Fatalf("existing backup overwritten: %q", got)
	}
}

func TestMigrateLegacyIfDefault_OnlyForDefaultPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	tomlPath := legacyTOMLPath()
	if tomlPath != filepath.Join(dir, "gemini-voice", "config.toml") {
		t.Fatalf("legacyTOMLPath = %q", tomlPath)
	}
	writeFile(t, tomlPath, legacyCustomTOML)

	var reloads int
	var logs bytes.Buffer
	deps := cliDeps{
		runCmd: func(_ context.Context, _ string, name string, _ ...string) cmdResult {
			if name == "fcitx5-remote" {
				reloads++
			}
			return cmdResult{}
		},
		lookPath: func(file string) (string, error) { return "/usr/bin/" + file, nil },
	}
	logf := func(format string, args ...any) { logs.WriteString(format) }

	// A custom path never touches the user's real legacy config.
	migrateLegacyIfDefault(filepath.Join(dir, "elsewhere.conf"), deps, logf)
	if _, err := os.Stat(tomlPath); err != nil {
		t.Fatalf("toml touched for a custom conf path: %v", err)
	}

	migrateLegacyIfDefault(DefaultConfigPath(), deps, logf)
	if _, err := os.Stat(tomlPath + ".migrated"); err != nil {
		t.Fatalf("toml not migrated for the default path: %v", err)
	}
	if reloads != 1 || logs.Len() == 0 {
		t.Fatalf("expected one Fcitx5 reload and a log line, got reloads=%d logs=%q", reloads, logs.String())
	}
	cfg, err := LoadConfigFile(DefaultConfigPath())
	if err != nil || cfg.Connection.Proxy != "socks5://127.0.0.1:1080" {
		t.Fatalf("unexpected migrated config: %v %+v", err, cfg.Connection)
	}
}
