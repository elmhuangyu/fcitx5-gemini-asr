package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func parseConf(t *testing.T, content string) Config {
	t.Helper()
	cfg, err := configFromINI(classifyINILines(splitINILines(content)))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestGroupsDefaultsAndFreshFile(t *testing.T) {
	def := DefaultConfig()
	if got := def.PrefetchActionNames(); !reflect.DeepEqual(got, []string{"refine_fix_typos", "refine_formal"}) {
		t.Fatalf("default prefetch = %v", got)
	}
	// A fresh file does not repeat group defaults.
	fresh := string(mergeConfig(nil, def))
	if strings.Contains(fresh, "Refine") || strings.Contains(fresh, "Prefetch") || strings.Contains(fresh, "Actions") {
		t.Fatalf("group defaults written to a fresh file:\n%s", fresh)
	}
}

func TestGroupWriteAndReadBack(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PostProcess.Groups["refine_casual"] = ActionGroup{Model: "gemini-3.5-flash/high", Prompt: "Be casual.\nPlease.", Prefetch: true}
	cfg.PostProcess.Groups["refine_formal"] = ActionGroup{Prefetch: false}
	orig := "# keep me\nShowActionMenu=True\n\n[Custom]\nFoo=Bar\n"
	got := string(mergeConfig([]byte(orig), cfg))
	for _, want := range []string{
		"# keep me\n", "[Custom]\nFoo=Bar\n",
		"[PostProcess/RefineCasual]\nModel=gemini-3.5-flash/high\nPrompt=\"Be casual.\\nPlease.\"\nPrefetch=True\n",
		"[PostProcess/RefineFormal]\nPrefetch=False\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if back := parseConf(t, got); !reflect.DeepEqual(back, cfg) {
		t.Fatalf("round trip mismatch:\ngot  %+v\nwant %+v", back, cfg)
	}
	// Writing the same config again leaves the file alone.
	if again := string(mergeConfig([]byte(got), cfg)); again != got {
		t.Fatalf("second merge changed the file:\n%s\n---\n%s", again, got)
	}
}

func TestInvalidGroupModelRejected(t *testing.T) {
	_, err := configFromINI(classifyINILines(splitINILines("[PostProcess/AskAi]\nModel=gemini-3.5-flash/turbo\n")))
	if err == nil || !strings.Contains(err.Error(), "PostProcess/AskAi/Model") {
		t.Fatalf("expected a validation error naming the key, got %v", err)
	}
}

func TestResolutionPrecedence(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PostProcess.Model = "gemini-3.5-flash-lite"
	cfg.PostProcess.Groups[translateGroup] = ActionGroup{Model: "gemini-3.5-flash", Prompt: "Group translate prompt."}
	cfg.PostProcess.Models = map[string]string{"translate_ja": "gemini-3.5-flash/high"}
	cfg.Prompts = map[string]string{"translate_ja": "Actions ja prompt."}

	cases := map[string]string{
		"refine_formal": "gemini-3.5-flash-lite", // default
		"translate_fr":  "gemini-3.5-flash",      // group
		"translate_ja":  "gemini-3.5-flash/high", // Actions entry beats group
	}
	for action, want := range cases {
		if got := cfg.ModelForAction(action); got != want {
			t.Errorf("ModelForAction(%s) = %q, want %q", action, got, want)
		}
	}
	prompts := map[string]string{
		"translate_ja":  "Actions ja prompt.",
		"translate_fr":  "Group translate prompt.",
		"refine_formal": defaultPrompts["refine_formal"],
	}
	for action, want := range prompts {
		if got, _ := cfg.PromptForAction(action); got != want {
			t.Errorf("PromptForAction(%s) = %q, want %q", action, got, want)
		}
	}
	// Without any override the translate prompt is synthesized per language.
	if got, _ := DefaultConfig().PromptForActionWithLabel("translate_ko", ""); !strings.Contains(got, "Korean") {
		t.Errorf("default translate prompt = %q", got)
	}
}

func TestSetActionModelRouting(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PostProcess.Models = map[string]string{"refine_formal": "gemini-3.5-flash"}
	if err := cfg.SetActionModel("refine_formal", "gemini-4-hydrogen/low"); err != nil {
		t.Fatal(err)
	}
	if cfg.PostProcess.Groups["refine_formal"].Model != "gemini-4-hydrogen/low" || len(cfg.PostProcess.Models) != 0 {
		t.Fatalf("built-in action not routed to its group: %+v", cfg.PostProcess)
	}
	if err := cfg.SetActionModel("translate", "gemini-3.5-flash"); err != nil {
		t.Fatal(err)
	}
	if cfg.PostProcess.Groups[translateGroup].Model != "gemini-3.5-flash" {
		t.Fatalf("translate alias not routed to the Translate group: %+v", cfg.PostProcess)
	}
	if err := cfg.SetActionModel("translate_ja", "gemini-3.5-flash/high"); err != nil {
		t.Fatal(err)
	}
	if cfg.PostProcess.Models["translate_ja"] != "gemini-3.5-flash/high" || cfg.ModelForAction("translate_ja") != "gemini-3.5-flash/high" {
		t.Fatalf("per-language model not in the Actions list: %+v", cfg.PostProcess)
	}
	if err := cfg.SetActionModel("translate_ja", "default"); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.PostProcess.Models["translate_ja"]; ok {
		t.Fatal("clearing did not remove the Actions entry")
	}
	if err := cfg.SetActionModel("ask_ai", "gemini-3.5-flash/turbo"); err == nil {
		t.Fatal("invalid effort suffix accepted")
	}
}

func TestSetPrefetchActions(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.SetPrefetchActions([]string{"refine_casual", "translate_ja"}); err != nil {
		t.Fatal(err)
	}
	if got := cfg.PrefetchActionNames(); !reflect.DeepEqual(got, []string{"refine_casual", "translate_en"}) {
		t.Fatalf("prefetch = %v", got)
	}
	if err := cfg.SetPrefetchActions([]string{"bogus"}); err == nil {
		t.Fatal("unknown action accepted")
	}
	if err := cfg.SetPrefetchActions(nil); err != nil || len(cfg.PrefetchItems()) != 0 {
		t.Fatalf("clear failed: %v %v", err, cfg.PrefetchItems())
	}
}

func TestMergeActionOverrides(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Ime.TranslateLanguages = []string{"ja", "Pig Latin"}
	cfg.Prompts = map[string]string{"translate_pig_latin": "Oink."}
	cfg.PostProcess.Models = map[string]string{"translate_ja": "gemini-3.5-flash", "ask_ai": "gemini-4-hydrogen/high"}

	orig := "# header\nShowActionMenu=True\n\n[Custom]\nFoo=Bar\n"
	merged := string(mergeConfig([]byte(orig), cfg))
	want := "# header\nShowActionMenu=True\n\n[Custom]\nFoo=Bar\n" +
		"\n[TranslateLanguages]\n0=ja\n1=\"Pig Latin\"\n" +
		"\n[PostProcess/Actions/0]\nName=translate_ja\nModel=gemini-3.5-flash\n" +
		"\n[PostProcess/Actions/1]\nName=translate_pig_latin\nPrompt=Oink.\n" +
		"\n[PostProcess/Actions/2]\nName=ask_ai\nModel=gemini-4-hydrogen/high\n"
	if !strings.HasPrefix(merged, want) {
		t.Fatalf("merged:\n%s\nwant prefix:\n%s", merged, want)
	}
	back := parseConf(t, merged)
	if !reflect.DeepEqual(back.PostProcess.Models, cfg.PostProcess.Models) || !reflect.DeepEqual(back.Prompts, cfg.Prompts) {
		t.Fatalf("overrides lost: %+v / %+v", back.PostProcess.Models, back.Prompts)
	}

	// Removing overrides removes their sections but keeps the rest in place.
	cfg.PostProcess.Models = nil
	cfg.Prompts = nil
	cleared := string(mergeConfig([]byte(merged), cfg))
	if strings.Contains(cleared, "Actions") || !strings.Contains(cleared, "[Custom]\nFoo=Bar\n") {
		t.Fatalf("Actions not removed:\n%s", cleared)
	}

	// Unchanged actions keep their hand-written layout and comments.
	handwritten := "[PostProcess/Actions/0]\n# why\nName = translate_ja\nModel=gemini-3.5-flash\n"
	cfg2 := DefaultConfig()
	cfg2.PostProcess.Models = map[string]string{"translate_ja": "gemini-3.5-flash"}
	if got := string(mergeConfig([]byte(handwritten), cfg2)); got != handwritten {
		t.Fatalf("unchanged actions rewritten: %q", got)
	}
}

// An Actions entry whose Name is not in the configtool dropdown (a custom
// translation language, a typo) is plain data to the daemon: it survives a
// load/merge cycle byte for byte.
func TestUnknownActionNameSurvives(t *testing.T) {
	orig := "[PostProcess/Actions/0]\nName=translate_klingon\nModel=gemini-3.5-flash\n\n[PostProcess/Actions/1]\nName=refine_typo_here\nPrompt=x\n"
	cfg := parseConf(t, orig)
	if cfg.PostProcess.Models["translate_klingon"] != "gemini-3.5-flash" || cfg.Prompts["refine_typo_here"] != "x" {
		t.Fatalf("entries not loaded: %+v %+v", cfg.PostProcess.Models, cfg.Prompts)
	}
	if got := string(mergeConfig([]byte(orig), cfg)); got != orig {
		t.Fatalf("unknown names rewritten:\n%s", got)
	}
}

func TestMigrateConfFileRealWorldFixture(t *testing.T) {
	orig := readGolden(t, "gemini-voice.legacy-prefetch.conf")
	if !strings.Contains(string(orig), "[PostProcess/Prefetch]") {
		t.Fatal("fixture lost its legacy section")
	}
	// The legacy list is honoured even before the file is rewritten.
	before, err := configFromINI(classifyINILines(splitINILines(string(orig))))
	if err != nil {
		t.Fatal(err)
	}
	wantPrefetch := []string{"refine_fix_typos", "refine_casual", "refine_formal"}
	if got := before.PrefetchActionNames(); !reflect.DeepEqual(got, wantPrefetch) {
		t.Fatalf("legacy list not honoured: %v", got)
	}

	path := filepath.Join(t.TempDir(), "gemini-voice.conf")
	if err := os.WriteFile(path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := MigrateConfFile(path)
	if err != nil || !changed {
		t.Fatalf("MigrateConfFile = %v, %v", changed, err)
	}
	got, _ := os.ReadFile(path)
	s := string(got)
	if strings.Contains(s, "[PostProcess/Prefetch]") || strings.Contains(s, "0=refine_fix_typos") {
		t.Fatalf("legacy list still present:\n%s", s)
	}
	for _, keep := range []string{
		"# Show LLM Action Menu\n# ShowActionMenu=True\n", "PreeditMaxWidth=120\n", "# [TriggerKey]\n# 0=Alt+space\n",
		"[TranslateLanguages]\n0=en\n1=japanese\n", "\nMode=SMART\n", "\nVadSilenceMs=2000\n",
	} {
		if !strings.Contains(s, keep) {
			t.Fatalf("migration lost %q:\n%s", keep, s)
		}
	}
	if !strings.Contains(s, "[PostProcess/RefineCasual]\nPrefetch=True\n") {
		t.Fatalf("refine_casual prefetch not converted:\n%s", s)
	}
	after, err := LoadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := after.PrefetchActionNames(); !reflect.DeepEqual(got, wantPrefetch) {
		t.Fatalf("effective prefetch changed by migration: %v", got)
	}
	if after.Ime.PreeditMaxWidth != 120 || after.LiveTranscribe.Mode != "SMART" || after.LiveTranscribe.VADSilenceMs != 2000 ||
		!reflect.DeepEqual(after.Ime.TranslateLanguages, []string{"en", "japanese"}) {
		t.Fatalf("other settings changed: %+v %+v", after.Ime, after.LiveTranscribe)
	}

	// Once only: the converted file is left untouched.
	again, err := MigrateConfFile(path)
	if err != nil || again {
		t.Fatalf("second migration = %v, %v", again, err)
	}
	if got2, _ := os.ReadFile(path); !bytes.Equal(got, got2) {
		t.Fatal("second migration changed the file")
	}
}

func TestMigrateConfFileEmptyAndDefaultLists(t *testing.T) {
	cases := []struct {
		name, in string
		want     []string
	}{
		{"empty marker means none", "[PostProcess]\nModel=gemini-3.5-flash-lite\nPrefetch=\n", nil},
		{"default list drops the section only", "[PostProcess/Prefetch]\n0=refine_fix_typos\n1=refine_formal\n", []string{"refine_fix_typos", "refine_formal"}},
		{"translate entry enables the group", "[PostProcess/Prefetch]\n0=ask_ai\n1=translate_en\n", []string{"translate_en", "ask_ai"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gemini-voice.conf")
			if err := os.WriteFile(path, []byte(tc.in), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := MigrateConfFile(path); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(path)
			if strings.Contains(string(got), "Prefetch/") || strings.Contains(string(got), "\nPrefetch=\n") || strings.HasPrefix(string(got), "Prefetch=") {
				t.Fatalf("legacy remnants:\n%s", got)
			}
			cfg, err := LoadConfigFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if names := cfg.PrefetchActionNames(); !reflect.DeepEqual(names, tc.want) {
				t.Fatalf("prefetch = %v, want %v\n%s", names, tc.want, got)
			}
		})
	}
}

func TestMigrateConfFileMissingOrCurrent(t *testing.T) {
	dir := t.TempDir()
	if changed, err := MigrateConfFile(filepath.Join(dir, "none.conf")); changed || err != nil {
		t.Fatalf("missing file: %v %v", changed, err)
	}
	path := filepath.Join(dir, "cur.conf")
	if err := os.WriteFile(path, []byte("[PostProcess/AskAi]\nPrefetch=True\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := MigrateConfFile(path); changed || err != nil {
		t.Fatalf("current layout: %v %v", changed, err)
	}
}

// engine.h shows the built-in prompts as tooltips; keep them in sync with the
// daemon's defaults.
func TestBuiltinPromptsMatchAddonTooltips(t *testing.T) {
	header, err := os.ReadFile(filepath.Join("..", "addon", "engine.h"))
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"refine_fix_typos", "refine_casual", "refine_formal", "as_prompt", "ask_ai"} {
		if !strings.Contains(string(header), `"`+defaultPrompts[action]+`"`) {
			t.Errorf("addon/engine.h tooltip for %s does not match defaultPrompts", action)
		}
	}
}

// The dropdown in addon/engine.h lists every built-in action and every known
// translate target.
func TestActionDropdownCoversKnownActions(t *testing.T) {
	header, err := os.ReadFile(filepath.Join("..", "addon", "engine.h"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, a := range KnownActions {
		want[a] = true
	}
	for _, target := range knownTranslateLanguages {
		want[target.Action] = true
	}
	for action := range want {
		if !strings.Contains(string(header), `{"`+action+`", N_(`) {
			t.Errorf("addon/engine.h dropdown lacks %s", action)
		}
	}
}
