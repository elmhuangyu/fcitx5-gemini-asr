package main

import (
	"fmt"
	"strings"
)

// Thinking effort levels accepted as a "/<effort>" suffix on post-processing
// model strings, e.g. "gemini-3.5-flash/low".
var thinkingEfforts = []string{"minimal", "low", "medium", "high"}

// defaultEffortForAction returns the thinking effort applied when the model
// string carries no suffix. Simple text transforms think as little as
// possible; ask_ai and as_prompt keep the provider default ("").
func defaultEffortForAction(action string) string {
	if strings.HasPrefix(action, "refine_") || strings.HasPrefix(action, "translate_") {
		return "minimal"
	}
	return ""
}

// ModelSpec is a parsed model string: API model name plus optional effort.
type ModelSpec struct {
	Name   string
	Effort string
}

// ParseModelSpec splits "[models/]name[/effort]" and validates the effort.
func ParseModelSpec(spec string) (ModelSpec, error) {
	s := strings.TrimSpace(spec)
	s = strings.TrimPrefix(s, "models/")
	name, effort := s, ""
	if i := strings.LastIndex(s, "/"); i >= 0 {
		name, effort = strings.TrimSpace(s[:i]), strings.ToLower(strings.TrimSpace(s[i+1:]))
		if name == "" {
			return ModelSpec{}, fmt.Errorf("invalid model %q: empty model name", spec)
		}
		if strings.Contains(name, "/") {
			return ModelSpec{}, fmt.Errorf("invalid model %q: unexpected '/' in model name", spec)
		}
		valid := false
		for _, e := range thinkingEfforts {
			if e == effort {
				valid = true
			}
		}
		if !valid {
			return ModelSpec{}, fmt.Errorf("invalid thinking effort %q in model %q (valid: %s)", effort, spec, strings.Join(thinkingEfforts, ", "))
		}
		if !supportsThinkingLevel(name) {
			return ModelSpec{}, fmt.Errorf("thinking effort %q is not supported for model %q (only gemini-3 and newer models)", effort, name)
		}
	}
	return ModelSpec{Name: name, Effort: effort}, nil
}

// ValidatePostProcessModel checks a post-processing model string (with optional effort suffix).
func ValidatePostProcessModel(spec string) error {
	_, err := ParseModelSpec(spec)
	return err
}

// ValidateLiveModel rejects effort suffixes (or any '/') on the live ASR model.
func ValidateLiveModel(model string) error {
	m := strings.TrimPrefix(strings.TrimSpace(model), "models/")
	if strings.Contains(m, "/") {
		return fmt.Errorf("live.model %q: thinking-effort suffixes apply only to post-processing models", model)
	}
	return nil
}

// DescribeModelSpec returns the bare model name and a human-readable effort
// description (e.g. "minimal (default)", "low", "provider default").
func DescribeModelSpec(spec, action string) (name, effort string) {
	ms, err := ParseModelSpec(spec)
	if err != nil {
		return spec, "invalid"
	}
	if ms.Effort != "" {
		return ms.Name, ms.Effort
	}
	if d := defaultEffortForAction(action); d != "" && supportsThinkingLevel(ms.Name) {
		return ms.Name, d + " (default)"
	}
	return ms.Name, "provider default"
}

// supportsThinkingLevel reports whether the model takes a thinkingLevel
// (gemini-3 and newer major versions, e.g. gemini-3.5-flash, gemini-4-hydrogen).
func supportsThinkingLevel(name string) bool {
	n := strings.ToLower(strings.TrimPrefix(name, "models/"))
	rest, ok := strings.CutPrefix(n, "gemini-")
	if !ok {
		return false
	}
	major := 0
	digits := 0
	for _, r := range rest {
		if r < '0' || r > '9' {
			break
		}
		major = major*10 + int(r-'0')
		digits++
		if digits > 3 {
			return false
		}
	}
	return digits > 0 && major >= 3
}

type thinkingConfig struct {
	ThinkingLevel string `json:"thinkingLevel,omitempty"`
}

// buildThinkingConfig maps an effort to thinkingLevel. Returns nil when
// effort is empty or the model has no thinking control.
//
// Pro models and 3.8-flash lack "minimal", and 3-pro lacks "medium"; those
// are clamped to the nearest supported level.
func buildThinkingConfig(name, effort string) *thinkingConfig {
	if effort == "" || !supportsThinkingLevel(name) {
		return nil
	}
	n := strings.ToLower(strings.TrimPrefix(name, "models/"))
	level := effort
	if effort == "minimal" && (strings.Contains(n, "-pro") || strings.HasPrefix(n, "gemini-3.8-flash")) {
		level = "low"
	}
	if effort == "medium" && strings.HasPrefix(n, "gemini-3-pro") {
		level = "high"
	}
	return &thinkingConfig{ThinkingLevel: level}
}
