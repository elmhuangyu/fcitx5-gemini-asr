package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFcitxAddonConfig_DefaultsAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fcitx5", "conf", "gemini-voice.conf")

	// Missing file yields defaults.
	def, err := LoadFcitxAddonConfig(path)
	if err != nil {
		t.Fatalf("load missing config: %v", err)
	}
	if !def.ShowActionMenu || def.SocketPath != DefaultFcitxSocketPath || !reflect.DeepEqual(def.TriggerKeys, []string{"Alt+space"}) || def.RecordingPrefix != DefaultRecordingPrefix || def.PreeditMaxWidth != DefaultPreeditMaxWidth || len(def.TranslateLanguages) != 0 {
		t.Fatalf("unexpected defaults: %+v", def)
	}
	if strings.Contains(string(renderFcitxAddonConfig(def)), "[TranslateLanguages]") {
		t.Fatalf("default config should omit [TranslateLanguages], got:\n%s", renderFcitxAddonConfig(def))
	}

	want := FcitxAddonConfig{
		TriggerKeys:        []string{"Alt+space", "Super+v"},
		TranslateLanguages: []string{"en", "ja"},
		ShowActionMenu:     false,
		SocketPath:         "/tmp/path with spaces/gemini.sock",
		Language:           "en-US",
		RecordingPrefix:    "🔴",
		PreeditMaxWidth:    40,
	}
	if err := SaveFcitxAddonConfig(path, want); err != nil {
		t.Fatalf("save fcitx config: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %04o, want 0600", info.Mode().Perm())
	}

	got, err := LoadFcitxAddonConfig(path)
	if err != nil {
		t.Fatalf("reload fcitx config: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip mismatch:\ngot:  %+v\nwant: %+v", got, want)
	}
}

func TestFcitxAddonConfig_OmitEmptyTranslateLanguagesAndMigrate(t *testing.T) {
	dir := t.TempDir()
	fcitxPath := filepath.Join(dir, "fcitx5", "conf", "gemini-voice.conf")
	markerPath := filepath.Join(dir, "gemini-voice", ".translate_languages_migrated")

	// Saving empty/whitespace-only TranslateLanguages omits [TranslateLanguages].
	emptyCfg := DefaultFcitxAddonConfig()
	emptyCfg.TranslateLanguages = []string{"", "  "}
	if err := SaveFcitxAddonConfig(fcitxPath, emptyCfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(fcitxPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "[TranslateLanguages]") {
		t.Fatalf("expected [TranslateLanguages] omitted, got:\n%s", raw)
	}
	reloaded, err := LoadFcitxAddonConfig(fcitxPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.TranslateLanguages) != 0 {
		t.Fatalf("expected nil/empty TranslateLanguages, got %#v", reloaded.TranslateLanguages)
	}

	// Orphaned [TranslateLanguages] 0=en is cleared once by migration.
	legacyCfg := DefaultFcitxAddonConfig()
	legacyCfg.TranslateLanguages = []string{"en"}
	if err := SaveFcitxAddonConfig(fcitxPath, legacyCfg); err != nil {
		t.Fatal(err)
	}
	migrated, err := MigrateOrphanedFcitxTranslateLanguages(fcitxPath, markerPath)
	if err != nil {
		t.Fatalf("migrate failed: %v", err)
	}
	if !migrated {
		t.Fatal("expected migrated=true for orphaned 0=en")
	}
	afterMig, err := LoadFcitxAddonConfig(fcitxPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterMig.TranslateLanguages) != 0 {
		t.Fatalf("expected cleared TranslateLanguages after migration, got %#v", afterMig.TranslateLanguages)
	}

	// Deliberately re-setting TranslateLanguages=["en"] after migration is preserved.
	if err := SaveFcitxAddonConfig(fcitxPath, legacyCfg); err != nil {
		t.Fatal(err)
	}
	migrated2, err := MigrateOrphanedFcitxTranslateLanguages(fcitxPath, markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if migrated2 {
		t.Fatal("expected migrated=false on second run after marker exists")
	}
	preserved, err := LoadFcitxAddonConfig(fcitxPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(preserved.TranslateLanguages, []string{"en"}) {
		t.Fatalf("expected deliberate [en] preserved, got %#v", preserved.TranslateLanguages)
	}

	// Custom [TranslateLanguages] (e.g. en, ja) is not touched even on first migration.
	markerPath2 := filepath.Join(dir, "gemini-voice", ".translate_languages_migrated_2")
	customCfg := DefaultFcitxAddonConfig()
	customCfg.TranslateLanguages = []string{"en", "ja"}
	if err := SaveFcitxAddonConfig(fcitxPath, customCfg); err != nil {
		t.Fatal(err)
	}
	migrated3, err := MigrateOrphanedFcitxTranslateLanguages(fcitxPath, markerPath2)
	if err != nil {
		t.Fatal(err)
	}
	if migrated3 {
		t.Fatal("expected migrated=false for custom [en ja]")
	}
	customLoaded, err := LoadFcitxAddonConfig(fcitxPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(customLoaded.TranslateLanguages, []string{"en", "ja"}) {
		t.Fatalf("expected [en ja] preserved, got %#v", customLoaded.TranslateLanguages)
	}
}
