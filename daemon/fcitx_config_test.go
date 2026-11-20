package main

import (
	"bytes"
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

func TestMigrateOrphanedFcitxTranslateLanguages_SurgicalPreservation(t *testing.T) {
	t.Run("preserves comments, unknown keys, key order, and line endings", func(t *testing.T) {
		dir := t.TempDir()
		fcitxPath := filepath.Join(dir, "fcitx5", "conf", "gemini-voice.conf")
		markerPath := filepath.Join(dir, "gemini-voice", ".translate_languages_migrated")
		if err := os.MkdirAll(filepath.Dir(fcitxPath), 0o700); err != nil {
			t.Fatal(err)
		}

		orig := "# top hash comment\r\n" +
			"; top semicolon comment\r\n" +
			"PreeditMaxWidth=64\r\n" +
			"CustomUnknownKey=keep-me\r\n" +
			"ShowActionMenu=False\r\n" +
			"\r\n" +
			"[TriggerKey]\r\n" +
			"# trigger comment\r\n" +
			"0=Super+v\r\n" +
			"\r\n" +
			"[TranslateLanguages]\r\n" +
			"0=\"EN\"\r\n" +
			"\r\n" +
			"[CustomSection]\r\n" +
			"; section comment\r\n" +
			"Foo=Bar\r\n"

		want := "# top hash comment\r\n" +
			"; top semicolon comment\r\n" +
			"PreeditMaxWidth=64\r\n" +
			"CustomUnknownKey=keep-me\r\n" +
			"ShowActionMenu=False\r\n" +
			"\r\n" +
			"[TriggerKey]\r\n" +
			"# trigger comment\r\n" +
			"0=Super+v\r\n" +
			"\r\n" +
			"[CustomSection]\r\n" +
			"; section comment\r\n" +
			"Foo=Bar\r\n"

		if err := os.WriteFile(fcitxPath, []byte(orig), 0o600); err != nil {
			t.Fatal(err)
		}

		migrated, err := MigrateOrphanedFcitxTranslateLanguages(fcitxPath, markerPath)
		if err != nil {
			t.Fatalf("migrate failed: %v", err)
		}
		if !migrated {
			t.Fatal("expected migrated=true")
		}
		got, err := os.ReadFile(fcitxPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("surgical migration mismatch:\ngot:\n%q\nwant:\n%q", string(got), want)
		}
	})

	t.Run("removes trailing blank line when TranslateLanguages is at start of file", func(t *testing.T) {
		dir := t.TempDir()
		fcitxPath := filepath.Join(dir, "fcitx5", "conf", "gemini-voice.conf")
		markerPath := filepath.Join(dir, "gemini-voice", ".translate_languages_migrated")
		if err := os.MkdirAll(filepath.Dir(fcitxPath), 0o700); err != nil {
			t.Fatal(err)
		}

		orig := "[TranslateLanguages]\n0=en\n\n# keep comment\n[TriggerKey]\n0=Alt+space\n"
		want := "# keep comment\n[TriggerKey]\n0=Alt+space\n"
		if err := os.WriteFile(fcitxPath, []byte(orig), 0o600); err != nil {
			t.Fatal(err)
		}

		migrated, err := MigrateOrphanedFcitxTranslateLanguages(fcitxPath, markerPath)
		if err != nil {
			t.Fatalf("migrate failed: %v", err)
		}
		if !migrated {
			t.Fatal("expected migrated=true")
		}
		got, err := os.ReadFile(fcitxPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("mismatch:\ngot:\n%q\nwant:\n%q", string(got), want)
		}
	})

	t.Run("removes comments attached inside the orphaned section", func(t *testing.T) {
		dir := t.TempDir()
		fcitxPath := filepath.Join(dir, "fcitx5", "conf", "gemini-voice.conf")
		markerPath := filepath.Join(dir, "gemini-voice", ".translate_languages_migrated")
		if err := os.MkdirAll(filepath.Dir(fcitxPath), 0o700); err != nil {
			t.Fatal(err)
		}

		orig := "ShowActionMenu=True\n\n[TranslateLanguages]\n# english only\n0=en\n# trailing note\n[TriggerKey]\n0=Alt+space\n"
		want := "ShowActionMenu=True\n[TriggerKey]\n0=Alt+space\n"
		if err := os.WriteFile(fcitxPath, []byte(orig), 0o600); err != nil {
			t.Fatal(err)
		}

		migrated, err := MigrateOrphanedFcitxTranslateLanguages(fcitxPath, markerPath)
		if err != nil {
			t.Fatalf("migrate failed: %v", err)
		}
		if !migrated {
			t.Fatal("expected migrated=true")
		}
		got, err := os.ReadFile(fcitxPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("mismatch:\ngot:\n%q\nwant:\n%q", string(got), want)
		}
	})

	t.Run("keeps comment separated from orphaned section by a blank line", func(t *testing.T) {
		dir := t.TempDir()
		fcitxPath := filepath.Join(dir, "fcitx5", "conf", "gemini-voice.conf")
		markerPath := filepath.Join(dir, "gemini-voice", ".translate_languages_migrated")
		if err := os.MkdirAll(filepath.Dir(fcitxPath), 0o700); err != nil {
			t.Fatal(err)
		}

		orig := "[TranslateLanguages]\n0=en\n\n# documents the next section\n[TriggerKey]\n0=Alt+space\n"
		want := "# documents the next section\n[TriggerKey]\n0=Alt+space\n"
		if err := os.WriteFile(fcitxPath, []byte(orig), 0o600); err != nil {
			t.Fatal(err)
		}

		migrated, err := MigrateOrphanedFcitxTranslateLanguages(fcitxPath, markerPath)
		if err != nil {
			t.Fatalf("migrate failed: %v", err)
		}
		if !migrated {
			t.Fatal("expected migrated=true")
		}
		got, err := os.ReadFile(fcitxPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("mismatch:\ngot:\n%q\nwant:\n%q", string(got), want)
		}
	})

	t.Run("leaves custom TranslateLanguages 0=en + 1=ja untouched byte-for-byte", func(t *testing.T) {
		dir := t.TempDir()
		fcitxPath := filepath.Join(dir, "fcitx5", "conf", "gemini-voice.conf")
		markerPath := filepath.Join(dir, "gemini-voice", ".translate_languages_migrated")
		if err := os.MkdirAll(filepath.Dir(fcitxPath), 0o700); err != nil {
			t.Fatal(err)
		}

		orig := "# user comment\n" +
			"CustomKey=abc\n" +
			"ShowActionMenu=True\n\n" +
			"[TranslateLanguages]\n" +
			"0=en\n" +
			"1=ja\n"
		if err := os.WriteFile(fcitxPath, []byte(orig), 0o600); err != nil {
			t.Fatal(err)
		}

		migrated, err := MigrateOrphanedFcitxTranslateLanguages(fcitxPath, markerPath)
		if err != nil {
			t.Fatalf("migrate failed: %v", err)
		}
		if migrated {
			t.Fatal("expected migrated=false")
		}
		got, err := os.ReadFile(fcitxPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != orig {
			t.Fatalf("expected file untouched byte-for-byte:\ngot:\n%q\nwant:\n%q", string(got), orig)
		}
		if _, err := os.Stat(markerPath); err != nil {
			t.Fatalf("expected marker file created, stat err: %v", err)
		}
	})
}

func TestMergeFcitxAddonConfig(t *testing.T) {
	t.Run("rewrites managed keys while preserving comments, unknown keys, and CRLF", func(t *testing.T) {
		orig := "# managed by gemini-voice\r\n" +
			"ShowActionMenu=False\r\n" +
			"UnknownKey=keep\r\n" +
			"\r\n" +
			"[TriggerKey]\r\n" +
			"# my keys\r\n" +
			"0=Alt+space\r\n" +
			"1=Super+v\r\n" +
			"\r\n" +
			"[TranslateLanguages]\r\n" +
			"0=en\r\n" +
			"\r\n" +
			"[Custom]\r\n" +
			"; custom section\r\n" +
			"Foo=Bar\r\n"

		cfg := DefaultFcitxAddonConfig()
		cfg.TriggerKeys = []string{"Ctrl+F7"}
		cfg.TranslateLanguages = []string{"ja", "fr"}
		cfg.PreeditMaxWidth = 99

		want := "# managed by gemini-voice\r\n" +
			"ShowActionMenu=True\r\n" +
			"UnknownKey=keep\r\n" +
			"\r\n" +
			"SocketPath=" + DefaultFcitxSocketPath + "\r\n" +
			"Language=\r\n" +
			"RecordingPrefix=" + DefaultRecordingPrefix + "\r\n" +
			"PreeditMaxWidth=99\r\n" +
			"[TriggerKey]\r\n" +
			"0=Ctrl+F7\r\n" +
			"# my keys\r\n" +
			"\r\n" +
			"[TranslateLanguages]\r\n" +
			"0=ja\r\n" +
			"1=fr\r\n" +
			"\r\n" +
			"[Custom]\r\n" +
			"; custom section\r\n" +
			"Foo=Bar\r\n"

		got := mergeFcitxAddonConfig([]byte(orig), cfg)
		if string(got) != want {
			t.Fatalf("merge mismatch:\ngot:\n%q\nwant:\n%q", string(got), want)
		}
	})

	t.Run("drops TranslateLanguages section and attached comments when list empty", func(t *testing.T) {
		orig := "ShowActionMenu=True\n\n[TranslateLanguages]\n# english only\n0=en\n[TriggerKey]\n0=Alt+space\n"
		cfg := DefaultFcitxAddonConfig()
		cfg.TranslateLanguages = nil

		want := "ShowActionMenu=True\nSocketPath=" + DefaultFcitxSocketPath + "\nLanguage=\nRecordingPrefix=" + DefaultRecordingPrefix + "\nPreeditMaxWidth=80\n[TriggerKey]\n0=Alt+space\n"

		got := mergeFcitxAddonConfig([]byte(orig), cfg)
		if string(got) != want {
			t.Fatalf("merge mismatch:\ngot:\n%q\nwant:\n%q", string(got), want)
		}
		if strings.Contains(string(got), "[TranslateLanguages]") {
			t.Fatalf("expected [TranslateLanguages] dropped, got:\n%s", got)
		}
	})

	t.Run("appends missing sections and fixes missing trailing newline", func(t *testing.T) {
		orig := "# only a comment\nShowActionMenu=True"
		cfg := DefaultFcitxAddonConfig()
		cfg.TranslateLanguages = []string{"en"}

		want := "# only a comment\nShowActionMenu=True\nSocketPath=" + DefaultFcitxSocketPath + "\nLanguage=\nRecordingPrefix=" + DefaultRecordingPrefix + "\nPreeditMaxWidth=80\n\n[TriggerKey]\n0=Alt+space\n\n[TranslateLanguages]\n0=en\n"

		got := mergeFcitxAddonConfig([]byte(orig), cfg)
		if string(got) != want {
			t.Fatalf("merge mismatch:\ngot:\n%q\nwant:\n%q", string(got), want)
		}
	})

	t.Run("nil existing falls back to canonical render", func(t *testing.T) {
		cfg := DefaultFcitxAddonConfig()
		cfg.TranslateLanguages = []string{"en", "ja"}
		if got := mergeFcitxAddonConfig(nil, cfg); !bytes.Equal(got, renderFcitxAddonConfig(cfg)) {
			t.Fatalf("expected canonical render, got:\n%s", got)
		}
	})

	t.Run("merge with canonical content is idempotent", func(t *testing.T) {
		cfg := DefaultFcitxAddonConfig()
		cfg.TranslateLanguages = []string{"en"}
		canonical := renderFcitxAddonConfig(cfg)
		if got := mergeFcitxAddonConfig(canonical, cfg); !bytes.Equal(got, canonical) {
			t.Fatalf("expected idempotent merge:\ngot:\n%q\nwant:\n%q", string(got), string(canonical))
		}
	})
}
