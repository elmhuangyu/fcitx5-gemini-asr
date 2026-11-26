package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultFcitxSocketPath = "$XDG_RUNTIME_DIR/gemini-ime.sock"
	DefaultRecordingPrefix = "🎙️"
	DefaultPreeditMaxWidth = 80
	defaultTriggerKey      = "Alt+space"
)

// Option paths inside gemini-voice.conf. They mirror the Fcitx5 schema in
// addon/engine.h; the top-level ones are consumed by the addon itself, the
// grouped ones by the daemon.
const (
	pathTriggerKey         = "TriggerKey"
	pathShowActionMenu     = "ShowActionMenu"
	pathSocketPath         = "SocketPath"
	pathLanguage           = "Language"
	pathRecordingPrefix    = "RecordingPrefix"
	pathPreeditMaxWidth    = "PreeditMaxWidth"
	pathTranslateLanguages = "TranslateLanguages"

	pathLiveModel      = "LiveTranscription/Model"
	pathLiveMode       = "LiveTranscription/Mode"
	pathLiveVocabulary = "LiveTranscription/CustomVocabulary"
	pathLiveVAD        = "LiveTranscription/VadSilenceMs"

	pathPostModel   = "PostProcess/Model"
	pathPostActions = "PostProcess/Actions"

	// pathLegacyPrefetch is the retired list of prefetched action names; it is
	// converted once into the per-group Prefetch switches (see
	// MigrateConfFile) and never written again.
	pathLegacyPrefetch = "PostProcess/Prefetch"

	pathConnProxy      = "Connection/Proxy"
	pathConnLiveWSURL  = "Connection/LiveWsUrl"
	pathConnAPIBaseURL = "Connection/ApiBaseUrl"
)

// FcitxAddonConfig holds the top-level keys of gemini-voice.conf, which the
// Fcitx5 addon itself consumes (and the daemon for Language and
// TranslateLanguages).
type FcitxAddonConfig struct {
	TriggerKeys        []string
	TranslateLanguages []string
	ShowActionMenu     bool
	SocketPath         string
	Language           string
	RecordingPrefix    string
	PreeditMaxWidth    int
}

func DefaultFcitxAddonConfig() FcitxAddonConfig {
	return FcitxAddonConfig{
		TriggerKeys:        []string{defaultTriggerKey},
		TranslateLanguages: nil,
		ShowActionMenu:     true,
		SocketPath:         DefaultFcitxSocketPath,
		Language:           "",
		RecordingPrefix:    DefaultRecordingPrefix,
		PreeditMaxWidth:    DefaultPreeditMaxWidth,
	}
}

func userConfigHome() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return xdg
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config")
	}
	return ""
}

// DefaultConfigPath is the Fcitx5 addon config file, the single source of
// truth for all non-secret settings.
func DefaultConfigPath() string {
	return filepath.Join(userConfigHome(), "fcitx5", "conf", "gemini-voice.conf")
}

// DefaultFcitxConfigPath is kept as an alias of DefaultConfigPath.
func DefaultFcitxConfigPath() string { return DefaultConfigPath() }

// legacyTOMLPath is the pre-conf daemon config that is migrated once.
func legacyTOMLPath() string {
	return filepath.Join(userConfigHome(), "gemini-voice", "config.toml")
}

// DefaultCredentialPath is where the encrypted API key credential lives. It
// stays next to the (retired) config.toml so the systemd unit keeps working.
func DefaultCredentialPath() string {
	return filepath.Join(userConfigHome(), "gemini-voice", DefaultCredentialName+".cred")
}

// groupPath is the conf path of a key of a fixed action group, e.g.
// "PostProcess/RefineFormal/Model".
func groupPath(group, key string) string {
	return "PostProcess/" + groupSections[group] + "/" + key
}

func parseBoolValue(v string) bool {
	return strings.EqualFold(v, "true") || v == "1"
}

// configFromINI builds the effective config from a parsed conf file: defaults
// for everything absent, validated like the daemon requires.
func configFromINI(lines []iniLine) (Config, error) {
	cfg := DefaultConfig()
	ime := &cfg.Ime

	if v, ok := iniScalar(lines, pathShowActionMenu); ok {
		ime.ShowActionMenu = parseBoolValue(v)
	}
	if v, ok := iniScalar(lines, pathSocketPath); ok && strings.TrimSpace(v) != "" {
		ime.SocketPath = v
	}
	if v, ok := iniScalar(lines, pathLanguage); ok {
		ime.Language = strings.TrimSpace(v)
	}
	if v, ok := iniScalar(lines, pathRecordingPrefix); ok {
		ime.RecordingPrefix = v
	}
	if v, ok := iniScalar(lines, pathPreeditMaxWidth); ok {
		if w, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && w >= 0 && w <= 500 {
			ime.PreeditMaxWidth = w
		}
	}
	if keys, ok := iniList(lines, pathTriggerKey); ok && len(keys) > 0 {
		ime.TriggerKeys = keys
	}
	if langs, ok := iniList(lines, pathTranslateLanguages); ok {
		ime.TranslateLanguages = langs
		if len(langs) == 0 {
			ime.TranslateLanguages = nil
		}
	}

	if v, ok := iniScalar(lines, pathLiveModel); ok && strings.TrimSpace(v) != "" {
		cfg.LiveTranscribe.Model = strings.TrimSpace(v)
	}
	if v, ok := iniScalar(lines, pathLiveMode); ok {
		mode, err := NormalizeLiveMode(v)
		if err != nil {
			return Config{}, err
		}
		cfg.LiveTranscribe.Mode = mode
	}
	if vocab, ok := iniList(lines, pathLiveVocabulary); ok {
		cfg.LiveTranscribe.CustomVocabulary = pruneCustomVocabulary(vocab)
	}
	if v, ok := iniScalar(lines, pathLiveVAD); ok {
		ms, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || ms < 0 {
			return Config{}, fmt.Errorf("invalid %s %q (must be an integer >= 0)", pathLiveVAD, v)
		}
		cfg.LiveTranscribe.VADSilenceMs = ms
	}

	if v, ok := iniScalar(lines, pathPostModel); ok && strings.TrimSpace(v) != "" {
		cfg.PostProcess.Model = strings.TrimSpace(v)
	}
	for _, g := range actionGroups {
		grp := cfg.PostProcess.Groups[g]
		if v, ok := iniScalar(lines, groupPath(g, "Model")); ok {
			grp.Model = strings.TrimSpace(v)
		}
		if v, ok := iniScalar(lines, groupPath(g, "Prompt")); ok && strings.TrimSpace(v) != "" {
			grp.Prompt = v
		}
		if v, ok := iniScalar(lines, groupPath(g, "Prefetch")); ok {
			grp.Prefetch = parseBoolValue(v)
		}
		cfg.PostProcess.Groups[g] = grp
	}
	applyLegacyPrefetch(&cfg, lines)
	for _, item := range readStructList(lines, pathPostActions, []string{"Name", "Prompt", "Model"}) {
		var name, prompt, model string
		for _, f := range item {
			switch f.key {
			case "Name":
				name = strings.TrimSpace(f.val)
			case "Prompt":
				prompt = f.val
			case "Model":
				model = strings.TrimSpace(f.val)
			}
		}
		if name == "" {
			continue
		}
		if cfg.Prompts == nil {
			cfg.Prompts = make(map[string]string)
		}
		if strings.TrimSpace(prompt) != "" {
			cfg.Prompts[name] = prompt
		}
		if model != "" {
			if cfg.PostProcess.Models == nil {
				cfg.PostProcess.Models = make(map[string]string)
			}
			cfg.PostProcess.Models[name] = model
		}
	}
	if len(cfg.Prompts) == 0 {
		cfg.Prompts = nil
	}

	if v, ok := iniScalar(lines, pathConnProxy); ok {
		cfg.Connection.Proxy = strings.TrimSpace(v)
	}
	if v, ok := iniScalar(lines, pathConnLiveWSURL); ok {
		cfg.Connection.LiveWSURL = strings.TrimSpace(v)
	}
	if v, ok := iniScalar(lines, pathConnAPIBaseURL); ok {
		cfg.Connection.APIBaseURL = strings.TrimSpace(v)
	}

	if err := validateConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validateConfig checks the values the daemon refuses to run with.
func validateConfig(cfg Config) error {
	if err := ValidateLiveModel(cfg.LiveTranscribe.Model); err != nil {
		return err
	}
	if len(cfg.LiveTranscribe.CustomVocabulary) > MaxCustomVocabulary {
		return fmt.Errorf("%s has %d entries (max %d)", pathLiveVocabulary, len(cfg.LiveTranscribe.CustomVocabulary), MaxCustomVocabulary)
	}
	if err := ValidatePostProcessModel(cfg.PostProcess.Model); err != nil {
		return fmt.Errorf("%s: %w", pathPostModel, err)
	}
	for _, g := range actionGroups {
		if m := cfg.PostProcess.Groups[g].Model; strings.TrimSpace(m) != "" {
			if err := ValidatePostProcessModel(m); err != nil {
				return fmt.Errorf("%s: %w", groupPath(g, "Model"), err)
			}
		}
	}
	for action, m := range cfg.PostProcess.Models {
		if strings.TrimSpace(m) == "" {
			continue
		}
		if err := ValidatePostProcessModel(m); err != nil {
			return fmt.Errorf("%s/%s model: %w", pathPostActions, action, err)
		}
	}
	return nil
}

// applyLegacyPrefetch converts the retired [PostProcess/Prefetch] list (action
// names, or "Prefetch=" for none) into the per-group Prefetch switches. A
// group that sets its own Prefetch key keeps it; any translate_* entry
// enables the Translate group.
func applyLegacyPrefetch(cfg *Config, lines []iniLine) {
	list, ok := iniList(lines, pathLegacyPrefetch)
	if !ok {
		return
	}
	enabled := map[string]bool{}
	for _, name := range list {
		if g, ok := groupForAction(strings.TrimSpace(name)); ok {
			enabled[g] = true
		}
	}
	for _, g := range actionGroups {
		if _, explicit := iniScalar(lines, groupPath(g, "Prefetch")); explicit {
			continue
		}
		grp := cfg.PostProcess.Groups[g]
		grp.Prefetch = enabled[g]
		cfg.PostProcess.Groups[g] = grp
	}
}

// removeLegacyPrefetch deletes the retired list section (or its explicit-empty
// "Prefetch=" scalar), keeping comments and unknown keys elsewhere.
func (d *iniDoc) removeLegacyPrefetch() {
	d.removeScalar(pathLegacyPrefetch)
	for {
		hdr := firstHeader(d.lines(), pathLegacyPrefetch)
		if hdr < 0 || !d.dropListSection(hdr) {
			return
		}
	}
}

// MigrateConfFile performs the one-time conversion of a conf that still has
// the retired [PostProcess/Prefetch] list: the list is folded into the
// per-group Prefetch switches and removed, leaving comments and all other
// keys untouched. It reports whether the file was rewritten; a missing file
// or one already in the new layout is left alone.
func MigrateConfFile(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read config %q: %w", path, err)
	}
	lines := classifyINILines(splitINILines(string(data)))
	if _, legacy := iniList(lines, pathLegacyPrefetch); !legacy {
		return false, nil
	}
	cfg, err := configFromINI(lines)
	if err != nil {
		return false, fmt.Errorf("load config %q: %w", path, err)
	}
	merged := mergeConfig(data, cfg)
	if bytes.Equal(merged, data) {
		return false, nil
	}
	if err := writeConfigFileAtomic(path, merged, "config"); err != nil {
		return false, err
	}
	return true, nil
}

// LoadConfigFile reads and validates gemini-voice.conf. A missing file is
// reported as an error wrapping os.ErrNotExist.
func LoadConfigFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return configFromINI(classifyINILines(splitINILines(string(data))))
}

// LoadFcitxAddonConfig returns the addon-consumed keys, defaults if absent.
func LoadFcitxAddonConfig(path string) (FcitxAddonConfig, error) {
	cfg, err := LoadConfigFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultFcitxAddonConfig(), nil
		}
		return FcitxAddonConfig{}, err
	}
	return cfg.Ime, nil
}

func formatBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

func trimmedNonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// existingActionNames lists the Names of the Actions entries already in the
// file, in file order, so rewriting keeps the user's ordering.
func existingActionNames(lines []iniLine) []string {
	var names []string
	for _, item := range readStructList(lines, pathPostActions, []string{"Name"}) {
		for _, f := range item {
			names = append(names, strings.TrimSpace(f.val))
		}
	}
	return names
}

// actionOverrideFields renders the advanced per-action overrides (prompt and
// model overrides) as Actions entries: entries already in the file keep their
// order, new ones follow in the order of EffectiveActionNames.
func actionOverrideFields(cfg Config, existing []string) [][]structField {
	var items [][]structField
	names := append(append([]string(nil), existing...), cfg.EffectiveActionNames()...)
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		prompt := strings.TrimSpace(cfg.Prompts[name])
		model := strings.TrimSpace(cfg.PostProcess.Models[name])
		if prompt == "" && model == "" {
			continue
		}
		fields := []structField{{"Name", name}}
		if prompt != "" {
			fields = append(fields, structField{"Prompt", prompt})
		}
		if model != "" {
			fields = append(fields, structField{"Model", model})
		}
		items = append(items, fields)
	}
	return items
}

type confScalar struct {
	path     string
	val      string
	advanced bool // omitted from a fresh file while at its default
}

func configScalars(cfg Config, group string) []confScalar {
	ime := cfg.Ime
	socket := strings.TrimSpace(ime.SocketPath)
	if socket == "" {
		socket = DefaultFcitxSocketPath
	}
	switch group {
	case "top":
		return []confScalar{
			{path: pathShowActionMenu, val: formatBool(ime.ShowActionMenu)},
			{path: pathSocketPath, val: socket},
			{path: pathLanguage, val: strings.TrimSpace(ime.Language)},
			{path: pathRecordingPrefix, val: ime.RecordingPrefix},
			{path: pathPreeditMaxWidth, val: strconv.Itoa(ime.PreeditMaxWidth)},
		}
	case "live":
		return []confScalar{
			{path: pathLiveModel, val: strings.TrimSpace(cfg.LiveTranscribe.Model)},
			{path: pathLiveMode, val: cfg.LiveTranscribe.Mode},
			{path: pathLiveVAD, val: strconv.Itoa(cfg.LiveTranscribe.VADSilenceMs)},
		}
	case "post":
		return []confScalar{
			{path: pathPostModel, val: strings.TrimSpace(cfg.PostProcess.Model)},
		}
	case "groups":
		var out []confScalar
		for _, g := range actionGroups {
			grp := cfg.PostProcess.Groups[g]
			out = append(out,
				confScalar{path: groupPath(g, "Model"), val: strings.TrimSpace(grp.Model), advanced: true},
				confScalar{path: groupPath(g, "Prompt"), val: grp.Prompt, advanced: true},
				confScalar{path: groupPath(g, "Prefetch"), val: formatBool(grp.Prefetch), advanced: true},
			)
		}
		return out
	default:
		return []confScalar{
			{path: pathConnProxy, val: strings.TrimSpace(cfg.Connection.Proxy)},
			{path: pathConnLiveWSURL, val: strings.TrimSpace(cfg.Connection.LiveWSURL), advanced: true},
			{path: pathConnAPIBaseURL, val: strings.TrimSpace(cfg.Connection.APIBaseURL), advanced: true},
		}
	}
}

// mergeConfig writes cfg into the gemini-voice.conf content existing, touching
// only the keys this program manages: comments, unknown keys, unknown
// sections, their order and the line endings are preserved, and a key whose
// value is unchanged is left byte-for-byte alone. A key absent from the file
// and equal to its default is not written (the default applies when missing).
// Empty existing content yields a complete canonical file.
func mergeConfig(existing []byte, cfg Config) []byte {
	fresh := len(existing) == 0
	d := newINIDoc(existing)
	def := DefaultConfig()

	scalars := func(group string) {
		defaults := map[string]string{}
		for _, s := range configScalars(def, group) {
			defaults[s.path] = s.val
		}
		for _, s := range configScalars(cfg, group) {
			switch {
			case d.has(s.path), s.val != defaults[s.path], fresh && !s.advanced:
				d.setScalar(s.path, s.val)
			}
		}
	}
	list := func(path string, values, defaults []string, defaultEmpty bool) {
		if d.listPresent(path) || !stringSlicesEqual(values, defaults) || (fresh && len(values) > 0) {
			d.setList(path, values, defaultEmpty)
		}
	}

	triggers := trimmedNonEmpty(cfg.Ime.TriggerKeys)
	if len(triggers) == 0 {
		triggers = []string{defaultTriggerKey}
	}

	scalars("top")
	list(pathTriggerKey, triggers, []string{defaultTriggerKey}, false)
	list(pathTranslateLanguages, trimmedNonEmpty(cfg.Ime.TranslateLanguages), nil, true)
	scalars("live")
	list(pathLiveVocabulary, pruneCustomVocabulary(cfg.LiveTranscribe.CustomVocabulary), nil, true)
	scalars("post")
	scalars("groups")
	d.removeLegacyPrefetch()
	d.setStructList(pathPostActions, actionOverrideFields(cfg, existingActionNames(d.lines())))
	scalars("conn")
	return d.bytes()
}

func (d *iniDoc) listPresent(path string) bool {
	_, ok := iniList(d.lines(), path)
	return ok
}

// SaveConfigFile atomically writes cfg into the conf file at path, merging
// into any existing content so hand edits survive. A file that exists but
// cannot be read is never overwritten; the read error is returned instead.
func SaveConfigFile(path string, cfg Config) error {
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read existing config %q: %w", path, err)
	}
	return writeConfigFileAtomic(path, mergeConfig(existing, cfg), "config")
}

// SaveFcitxAddonConfig updates only the addon-consumed keys of the conf file.
func SaveFcitxAddonConfig(path string, ime FcitxAddonConfig) error {
	cfg, err := LoadConfigFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err != nil {
		cfg = DefaultConfig()
	}
	cfg.Ime = ime
	return SaveConfigFile(path, cfg)
}

// ReloadFcitx5Config triggers a best-effort config reload in a running fcitx5 instance.
func ReloadFcitx5Config(ctx context.Context, deps cliDeps) bool {
	if _, err := deps.lookPath("fcitx5-remote"); err != nil {
		return false
	}
	rCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	res := deps.runCmd(rCtx, "", "fcitx5-remote", "--check", "-r")
	return res.err == nil
}
