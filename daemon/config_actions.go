package main

import "strings"

// prefetchSummary names the groups with Prefetch enabled for display
// ("translate" stands for every configured target language).
func prefetchSummary(c Config) []string {
	var out []string
	for _, g := range actionGroups {
		if c.PostProcess.Groups[g].Prefetch {
			out = append(out, g)
		}
	}
	return out
}

// hasModelOverrides reports whether any action has an explicit model.
func hasModelOverrides(c Config) bool {
	if len(c.PostProcess.Models) > 0 {
		return true
	}
	for _, g := range actionGroups {
		if strings.TrimSpace(c.PostProcess.Groups[g].Model) != "" {
			return true
		}
	}
	return false
}
