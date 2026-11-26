package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// The golden files in testdata/ are produced by the real Fcitx5 serializer in
// the addon's tests (addon/tests); parsing them here proves both sides agree.

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func loadGolden(t *testing.T, name string) Config {
	t.Helper()
	cfg, err := LoadConfigFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return cfg
}

func TestGoldenFcitxConf_Parses(t *testing.T) {
	cfg := loadGolden(t, "gemini-voice.conf")

	wantIme := FcitxAddonConfig{
		TriggerKeys:        []string{"Alt+space", "Control+F12"},
		TranslateLanguages: []string{"en", "Pig Latin"},
		ShowActionMenu:     true,
		SocketPath:         DefaultFcitxSocketPath,
		Language:           "cmn-Hans-CN,en-US",
		RecordingPrefix:    DefaultRecordingPrefix,
		PreeditMaxWidth:    80,
	}
	if !reflect.DeepEqual(cfg.Ime, wantIme) {
		t.Fatalf("ime mismatch:\ngot:  %+v\nwant: %+v", cfg.Ime, wantIme)
	}
	if cfg.LiveTranscribe.Model != DefaultLiveModel || cfg.LiveTranscribe.Mode != "SMART" || cfg.LiveTranscribe.VADSilenceMs != 1500 {
		t.Fatalf("unexpected live config: %+v", cfg.LiveTranscribe)
	}
	if !reflect.DeepEqual(cfg.LiveTranscribe.CustomVocabulary, []string{"Fcitx5", "Pipe Wire"}) {
		t.Fatalf("vocabulary = %#v", cfg.LiveTranscribe.CustomVocabulary)
	}
	if cfg.PostProcess.Model != "gemini-3.5-flash/low" {
		t.Fatalf("post_process.model = %q", cfg.PostProcess.Model)
	}
	wantPrompt := "Line one \"quoted\" # not a comment\n  line two with \\ backslash: 中文"
	wantGroups := DefaultConfig().PostProcess.Groups
	wantGroups["refine_fix_typos"] = ActionGroup{Prefetch: false}
	wantGroups["refine_formal"] = ActionGroup{Model: "gemini-4-hydrogen/high", Prompt: wantPrompt, Prefetch: true}
	wantGroups[translateGroup] = ActionGroup{Model: "gemini-3.5-flash", Prefetch: true}
	if !reflect.DeepEqual(cfg.PostProcess.Groups, wantGroups) {
		t.Fatalf("groups = %#v, want %#v", cfg.PostProcess.Groups, wantGroups)
	}
	// translate_pig_latin is not in the configtool dropdown; it must load as-is.
	wantModels := map[string]string{"translate_ja": "gemini-3.5-flash/low"}
	if !reflect.DeepEqual(cfg.PostProcess.Models, wantModels) {
		t.Fatalf("models = %#v, want %#v", cfg.PostProcess.Models, wantModels)
	}
	wantPrompts := map[string]string{"translate_pig_latin": "Translate to Pig Latin."}
	if !reflect.DeepEqual(cfg.Prompts, wantPrompts) {
		t.Fatalf("prompts = %#v, want %#v", cfg.Prompts, wantPrompts)
	}
	if cfg.Connection.Proxy != "socks5://127.0.0.1:1080" || cfg.Connection.LiveWSURL != "wss://example.invalid/live" || cfg.Connection.APIBaseURL != "https://example.invalid/v1beta" {
		t.Fatalf("unexpected connection config: %+v", cfg.Connection)
	}

	// Precedence: Actions entry > fixed group > post_process.model.
	if got := cfg.ModelForAction("refine_formal"); got != "gemini-4-hydrogen/high" {
		t.Fatalf("refine_formal model = %q", got)
	}
	if got := cfg.ModelForAction("translate_ja"); got != "gemini-3.5-flash/low" {
		t.Fatalf("translate_ja model = %q (Actions entry must win over the Translate group)", got)
	}
	if got := cfg.ModelForAction("translate_fr"); got != "gemini-3.5-flash" {
		t.Fatalf("translate_fr model = %q (Translate group)", got)
	}
	if got := cfg.ModelForAction("ask_ai"); got != "gemini-3.5-flash/low" {
		t.Fatalf("ask_ai model = %q (post_process.model)", got)
	}
	if got, _ := cfg.PromptForAction("refine_formal"); got != wantPrompt {
		t.Fatalf("refine_formal prompt = %q", got)
	}
	if got, _ := cfg.PromptForAction("refine_casual"); got != defaultPrompts["refine_casual"] {
		t.Fatalf("built-in prompt lost: %q", got)
	}
	// Translate prefetch fans out to every configured target with its label.
	wantItems := []PrefetchItem{{Action: "refine_formal"}, {Action: "translate_en", Label: "English"}, {Action: "translate_pig_latin", Label: "Pig Latin"}}
	if got := cfg.PrefetchItems(); !reflect.DeepEqual(got, wantItems) {
		t.Fatalf("prefetch items = %#v, want %#v", got, wantItems)
	}
}

var commentedKeyRe = regexp.MustCompile(`^# (\[.*\]|[A-Za-z0-9]+=.*)$`)

// TestGoldenDefaultsMatchGoDefaults un-comments Fcitx5's "# Key=default" lines
// from a conf saved with every option at its default and checks that the Go
// defaults agree with the C++ schema defaults.
func TestGoldenDefaultsMatchGoDefaults(t *testing.T) {
	var lines []string
	for _, line := range strings.Split(string(readGolden(t, "gemini-voice.defaults.conf")), "\n") {
		if commentedKeyRe.MatchString(line) {
			line = strings.TrimPrefix(line, "# ")
		}
		lines = append(lines, line)
	}
	cfg, err := configFromINI(classifyINILines(splitINILines(strings.Join(lines, "\n"))))
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultConfig()
	// "Language=" and "TranslateLanguages=" are explicit empties.
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("Go defaults drifted from the addon schema defaults:\ngot:  %+v\nwant: %+v", cfg, want)
	}

	// And with the file as Fcitx5 wrote it (everything commented out), the
	// loader must yield the same defaults.
	plain := loadGolden(t, "gemini-voice.defaults.conf")
	if !reflect.DeepEqual(plain, want) {
		t.Fatalf("all-commented conf did not load as defaults: %+v", plain)
	}
}

func TestGoldenClearedPrefetchMeansNoPrefetch(t *testing.T) {
	cfg := loadGolden(t, "gemini-voice.cleared.conf")
	if len(cfg.PrefetchItems()) != 0 {
		t.Fatalf("expected prefetch switched off, got %#v", cfg.PrefetchItems())
	}
	if cfg.LiveTranscribe.VADSilenceMs != 0 {
		t.Fatalf("vad = %d, want 0", cfg.LiveTranscribe.VADSilenceMs)
	}
	if cfg.LiveTranscribe.Mode != "VERBATIM" {
		t.Fatalf("mode = %q", cfg.LiveTranscribe.Mode)
	}
}

// Merging the loaded values back into real Fcitx5 output must not touch it.
func TestMergeIsNoOpOnRealFcitxOutput(t *testing.T) {
	for _, name := range []string{"gemini-voice.conf", "gemini-voice.defaults.conf", "gemini-voice.cleared.conf"} {
		orig := readGolden(t, name)
		cfg := loadGolden(t, name)
		if got := mergeConfig(orig, cfg); !bytes.Equal(got, orig) {
			t.Fatalf("%s: merge changed unmodified Fcitx5 output:\n%s\n--- want\n%s", name, got, orig)
		}
	}
}

func TestQuoteUnquoteRoundTrip(t *testing.T) {
	for _, v := range []string{
		"", "plain", "a b", " lead", "trail ", "a\tb", "a#b", "a;b", "a=b", "[x]", `q"uote`,
		`back\slash`, "nl\nx", "cr\rx", `"startq`, `endq"`, "é中", `x\ny`, "#lead", `a\`, "🎙️",
	} {
		if got := unquoteFcitxValue(quoteFcitxValue(v)); got != v {
			t.Fatalf("round trip %q -> %q -> %q", v, quoteFcitxValue(v), got)
		}
	}
	// Exactly what Fcitx5 writes for these.
	for v, want := range map[string]string{
		"plain": "plain", "a b": `"a b"`, "a#b": "a#b", `q"uote`: `"q\"uote"`, "nl\nx": `"nl\nx"`, `a\`: `"a\\"`, "": "",
	} {
		if got := quoteFcitxValue(v); got != want {
			t.Fatalf("quote(%q) = %s, want %s", v, got, want)
		}
	}
}

func customConfig() Config {
	cfg := DefaultConfig()
	cfg.Ime = FcitxAddonConfig{
		TriggerKeys:        []string{"Alt+space", "Super+v"},
		TranslateLanguages: []string{"ja", "Pig Latin"},
		ShowActionMenu:     false,
		SocketPath:         "/tmp/path with spaces/gemini.sock",
		Language:           "en-US",
		RecordingPrefix:    "🔴",
		PreeditMaxWidth:    40,
	}
	cfg.Connection = ConnectionConfig{Proxy: "http://127.0.0.1:8080", LiveWSURL: "wss://x.invalid/live", APIBaseURL: "https://x.invalid"}
	cfg.LiveTranscribe = LiveTranscribeConfig{Model: "gemini-3.5-transcribe-live", Mode: "SMART", CustomVocabulary: []string{"Fcitx5", "Pipe Wire"}, VADSilenceMs: 0}
	cfg.PostProcess.Model = "gemini-3.5-flash/high"
	cfg.PostProcess.Groups["refine_fix_typos"] = ActionGroup{Prefetch: false}
	cfg.PostProcess.Groups["refine_casual"] = ActionGroup{Model: "gemini-3.5-flash"}
	cfg.PostProcess.Groups["refine_formal"] = ActionGroup{Prompt: "Be formal.\nSecond \"line\".", Prefetch: true}
	cfg.PostProcess.Groups[translateGroup] = ActionGroup{Model: "gemini-3.5-flash/low", Prefetch: true}
	cfg.PostProcess.Models = map[string]string{"translate_ja": "gemini-3.5-flash/high"}
	cfg.Prompts = map[string]string{"translate_pig_latin": "Translate to Pig Latin."}
	return cfg
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fcitx5", "conf", "gemini-voice.conf")

	def, err := LoadFcitxAddonConfig(path)
	if err != nil {
		t.Fatalf("load missing config: %v", err)
	}
	if !reflect.DeepEqual(def, DefaultFcitxAddonConfig()) {
		t.Fatalf("missing file must yield defaults, got %+v", def)
	}
	if _, err := LoadConfigFile(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadConfigFile on missing file: %v", err)
	}

	want := customConfig()
	if err := SaveConfigFile(path, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %04o, want 0600", info.Mode().Perm())
	}
	got, err := LoadConfigFile(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip mismatch:\ngot:  %+v\nwant: %+v", got, want)
	}

	// Saving the same config again is a byte-for-byte no-op.
	first, _ := os.ReadFile(path)
	if err := SaveConfigFile(path, got); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if !bytes.Equal(first, second) {
		t.Fatalf("second save changed the file:\n%s\n---\n%s", first, second)
	}
}

// testdata/gemini-voice.go-written.conf is what this program writes for
// customConfig(); the addon's tests (test_config_schema.cpp) load that very
// file through Fcitx5's own parser, so the daemon's writer is checked against
// the real reader. Regenerate with UPDATE_GOLDEN=1.
func TestGoWrittenGolden(t *testing.T) {
	got := mergeConfig(nil, customConfig())
	path := filepath.Join("testdata", "gemini-voice.go-written.conf")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("daemon output drifted from %s (rerun with UPDATE_GOLDEN=1):\n%s", path, got)
	}
}

func TestSaveFcitxAddonConfigKeepsDaemonKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gemini-voice.conf")
	full := customConfig()
	if err := SaveConfigFile(path, full); err != nil {
		t.Fatal(err)
	}
	ime := full.Ime
	ime.PreeditMaxWidth = 12
	if err := SaveFcitxAddonConfig(path, ime); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := full
	want.Ime.PreeditMaxWidth = 12
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("daemon keys changed:\ngot:  %+v\nwant: %+v", got, want)
	}
}

func TestFreshConfigIsCanonicalAndStable(t *testing.T) {
	def := DefaultConfig()
	fresh := mergeConfig(nil, def)
	want := "ShowActionMenu=True\n" +
		"SocketPath=" + DefaultFcitxSocketPath + "\n" +
		"Language=\n" +
		"RecordingPrefix=" + DefaultRecordingPrefix + "\n" +
		"PreeditMaxWidth=80\n" +
		"\n[TriggerKey]\n0=Alt+space\n" +
		"\n[LiveTranscription]\nModel=" + DefaultLiveModel + "\nMode=VERBATIM\nVadSilenceMs=1200\n" +
		"\n[PostProcess]\nModel=" + DefaultPostProcessModel + "\n" +
		"\n[Connection]\nProxy=\n"
	if string(fresh) != want {
		t.Fatalf("canonical render mismatch:\ngot:\n%s\nwant:\n%s", fresh, want)
	}
	if again := mergeConfig(fresh, def); !bytes.Equal(again, fresh) {
		t.Fatalf("merge not idempotent:\n%s", again)
	}
	got, err := configFromINI(classifyINILines(splitINILines(string(fresh))))
	if err != nil || !reflect.DeepEqual(got, def) {
		t.Fatalf("canonical file did not load back to defaults: %v %+v", err, got)
	}
	// The built-in prompts are never written unless the user edits them.
	if strings.Contains(string(fresh), "Prompt") || strings.Contains(string(fresh), "Actions") {
		t.Fatalf("default prompts leaked into the conf:\n%s", fresh)
	}
}

func TestMergePreservesHandEdits(t *testing.T) {
	t.Run("keeps comments, unknown keys and sections, CRLF; rewrites only changed keys", func(t *testing.T) {
		orig := "# managed by hand\r\n" +
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
			"[LiveTranscription]\r\n" +
			"# fast\r\n" +
			"VadSilenceMs = 900\r\n" +
			"\r\n" +
			"[Custom]\r\n" +
			"; custom section\r\n" +
			"Foo=Bar\r\n"

		cfg, err := configFromINI(classifyINILines(splitINILines(orig)))
		if err != nil {
			t.Fatal(err)
		}
		cfg.Ime.TriggerKeys = []string{"Ctrl+F7"}
		cfg.Ime.TranslateLanguages = []string{"ja", "fr"}
		cfg.Ime.PreeditMaxWidth = 99
		cfg.LiveTranscribe.Mode = "SMART"

		want := "# managed by hand\r\n" +
			"ShowActionMenu=False\r\n" +
			"UnknownKey=keep\r\n" +
			"PreeditMaxWidth=99\r\n" +
			"\r\n" +
			"[TriggerKey]\r\n" +
			"0=Ctrl+F7\r\n" +
			"# my keys\r\n" +
			"\r\n" +
			"[TranslateLanguages]\r\n" +
			"0=ja\r\n" +
			"1=fr\r\n" +
			"\r\n" +
			"[LiveTranscription]\r\n" +
			"# fast\r\n" +
			"VadSilenceMs = 900\r\n" +
			"Mode=SMART\r\n" +
			"\r\n" +
			"[Custom]\r\n" +
			"; custom section\r\n" +
			"Foo=Bar\r\n"
		if got := mergeConfig([]byte(orig), cfg); string(got) != want {
			t.Fatalf("merge mismatch:\ngot:\n%q\nwant:\n%q", got, want)
		}
	})

	t.Run("replaces a changed value in place and drops duplicates", func(t *testing.T) {
		orig := "PreeditMaxWidth=10\nLanguage=a\nPreeditMaxWidth=20\n"
		cfg := DefaultConfig()
		cfg.Ime.PreeditMaxWidth = 30
		cfg.Ime.Language = "a"
		got := mergeConfig([]byte(orig), cfg)
		want := "PreeditMaxWidth=30\nLanguage=a\n"
		if !strings.HasPrefix(string(got), want) {
			t.Fatalf("got %q, want prefix %q", got, want)
		}
		if strings.Count(string(got), "PreeditMaxWidth") != 1 {
			t.Fatalf("duplicate not removed: %q", got)
		}
	})

	t.Run("drops an emptied list section with its attached comments", func(t *testing.T) {
		orig := "ShowActionMenu=True\n\n[TranslateLanguages]\n# english only\n0=ja\n\n[TriggerKey]\n0=Alt+space\n"
		cfg := DefaultConfig()
		got := string(mergeConfig([]byte(orig), cfg))
		want := "ShowActionMenu=True\n\n[TriggerKey]\n0=Alt+space\n"
		if !strings.HasPrefix(got, want) {
			t.Fatalf("got %q want prefix %q", got, want)
		}
		if strings.Contains(got, "TranslateLanguages") || strings.Contains(got, "english only") {
			t.Fatalf("section not dropped: %q", got)
		}
	})

	t.Run("keeps a comment block that documents the next section", func(t *testing.T) {
		orig := "[TranslateLanguages]\n0=ja\n\n# about triggers\n[TriggerKey]\n0=Alt+space\n"
		got := string(mergeConfig([]byte(orig), DefaultConfig()))
		if strings.Contains(got, "TranslateLanguages") || !strings.Contains(got, "# about triggers\n[TriggerKey]") {
			t.Fatalf("unexpected result: %q", got)
		}
	})

	t.Run("fixes a missing trailing newline and appends missing sections", func(t *testing.T) {
		orig := "# only a comment\nShowActionMenu=True"
		cfg := DefaultConfig()
		cfg.Ime.TranslateLanguages = []string{"ja"}
		cfg.LiveTranscribe.Mode = "SMART"
		got := string(mergeConfig([]byte(orig), cfg))
		want := "# only a comment\nShowActionMenu=True\n" +
			"\n[TranslateLanguages]\n0=ja\n" +
			"\n[LiveTranscription]\nMode=SMART\n"
		if got != want {
			t.Fatalf("got:\n%q\nwant:\n%q", got, want)
		}
	})

	t.Run("an unknown key in a list section survives emptying the list", func(t *testing.T) {
		orig := "[TranslateLanguages]\n0=ja\nNote=keep\n"
		got := string(mergeConfig([]byte(orig), DefaultConfig()))
		if got != "[TranslateLanguages]\nNote=keep\n" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestSaveConfigFileUnreadableExisting(t *testing.T) {
	dir := t.TempDir()
	// A directory at the config path makes ReadFile fail with a
	// non-ErrNotExist error; the save must abort instead of overwriting.
	confDir := filepath.Join(dir, "gemini-voice.conf")
	if err := os.MkdirAll(confDir, 0o700); err != nil {
		t.Fatal(err)
	}
	err := SaveConfigFile(confDir, DefaultConfig())
	if err == nil {
		t.Fatal("expected error when existing config is unreadable")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected non-ErrNotExist read error, got %v", err)
	}
	if !strings.Contains(err.Error(), "read existing config") {
		t.Fatalf("unexpected error message: %v", err)
	}
}
