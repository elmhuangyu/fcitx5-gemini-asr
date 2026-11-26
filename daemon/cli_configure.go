package main

import (
	"errors"
	"flag"
	"fmt"

	"charm.land/huh/v2"
)

func cmdConfigure(args []string, deps cliDeps) int {
	fs := flag.NewFlagSet("configure", flag.ContinueOnError)
	fs.SetOutput(deps.stderr)
	configPath := fs.String("config", defaultConfigPath(), "Path to gemini-voice.conf")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if deps.runSetupForm == nil && !deps.isTTY() {
		_, _ = fmt.Fprintln(deps.stderr, `error: configure requires an interactive terminal (use "gemini-voice config set <key> <value>" for non-interactive configuration)`)
		return 1
	}

	if _, err := runSetupTUI(*configPath, setupFormOptions{IncludeAPIKey: false}, deps); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			_, _ = fmt.Fprintln(deps.stderr, "Configuration cancelled; no changes were saved.")
			return 1
		}
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}
	return 0
}
