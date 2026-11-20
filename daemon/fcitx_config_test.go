package main

import (
	"os"
	"path/filepath"
	"reflect"
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
	if !def.ShowActionMenu || def.SocketPath != DefaultFcitxSocketPath || !reflect.DeepEqual(def.TriggerKeys, []string{"Alt+space"}) || def.RecordingPrefix != DefaultRecordingPrefix || def.PreeditMaxWidth != DefaultPreeditMaxWidth {
		t.Fatalf("unexpected defaults: %+v", def)
	}

	want := FcitxAddonConfig{
		TriggerKeys:     []string{"Alt+space", "Super+v"},
		ShowActionMenu:  false,
		SocketPath:      "/tmp/path with spaces/gemini.sock",
		Language:        "en-US",
		RecordingPrefix: "🔴",
		PreeditMaxWidth: 40,
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
