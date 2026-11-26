package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/huh/v2"
)

func cmdInit(args []string, deps cliDeps) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(deps.stderr)
	configPath := fs.String("config", defaultConfigPath(), "Path to gemini-voice.conf")
	credPathFlag := fs.String("cred-path", "", "Path to the encrypted .cred file (default ~/.config/gemini-voice/gemini_api_key.cred)")
	withKey := fs.String("with-key", "auto", "Encryption key type for systemd-creds --user: auto, host")
	force := fs.Bool("force", false, "Reset all settings in gemini-voice.conf to defaults in non-interactive mode")
	enableSvc := fs.Bool("enable-service", false, "Enable and start gemini-voice.service after initialization")
	useStdin := fs.Bool("stdin", false, "Read API key from standard input")
	keyFile := fs.String("key-file", "", "Read API key from file")
	noVerify := fs.Bool("no-verify", false, "Skip decrypt verification after encryption")
	nonInteractive := fs.Bool("non-interactive", false, "Skip interactive wizard prompts and create gemini-voice.conf with defaults if missing")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if err := validateWithKeyOption(*withKey); err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}

	interactive := !*nonInteractive && !*useStdin && strings.TrimSpace(*keyFile) == "" && deps.isTTY()
	if interactive {
		_, credPath := resolveCredPath(*credPathFlag)
		credExists := false
		if info, credErr := os.Stat(credPath); credErr == nil && !info.IsDir() {
			credExists = true
		}

		res, err := runSetupTUI(*configPath, setupFormOptions{
			IncludeAPIKey: true,
			WithKey:       *withKey,
			CredPath:      credPath,
			CredExists:    credExists,
		}, deps)
		if err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				_, _ = fmt.Fprintln(deps.stderr, "Setup cancelled; no changes were saved.")
				return 1
			}
			_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
			return 1
		}

		if res.OverwriteKey {
			if err := encryptAndSaveCredential(authSetOptions{
				outputPath: *credPathFlag,
				withKey:    *withKey,
				secret:     res.APIKey,
				noVerify:   *noVerify,
			}, deps); err != nil {
				_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
				return 1
			}
		} else if credPath != "" {
			_, _ = fmt.Fprintf(deps.stdout, "Keeping existing credential: %s\n", credPath)
		}
	} else {
		cliMigrate(*configPath, deps)
		_, statErr := os.Stat(*configPath)
		if errors.Is(statErr, os.ErrNotExist) || *force {
			// Reset every managed key to its default while keeping comments
			// and unknown keys of an existing file.
			existing, _ := os.ReadFile(*configPath)
			if err := writeConfigFileAtomic(*configPath, mergeConfig(existing, DefaultConfig()), "config"); err != nil {
				_, _ = fmt.Fprintf(deps.stderr, "error: write default config %q: %v\n", *configPath, err)
				return 1
			}
			_, _ = fmt.Fprintf(deps.stdout, "Wrote default config: %s\n", *configPath)
		} else if statErr != nil {
			_, _ = fmt.Fprintf(deps.stderr, "error: stat config %q: %v\n", *configPath, statErr)
			return 1
		} else {
			_, _ = fmt.Fprintf(deps.stdout, "Keeping existing config: %s\n", *configPath)
		}

		if err := encryptAndSaveCredential(authSetOptions{
			outputPath: *credPathFlag,
			withKey:    *withKey,
			useStdin:   *useStdin,
			keyFile:    *keyFile,
			noVerify:   *noVerify,
		}, deps); err != nil {
			_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
			return 1
		}
	}

	if *enableSvc {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = deps.runCmd(ctx, "", "systemctl", "--user", "daemon-reload")
		res := deps.runCmd(ctx, "", "systemctl", "--user", "enable", "--now", systemdServiceName)
		if res.err != nil {
			detail := strings.TrimSpace(res.stderr)
			if detail != "" {
				detail = ": " + detail
			}
			_, _ = fmt.Fprintf(deps.stderr, "error: enable %s (%v)%s\n", systemdServiceName, res.err, detail)
			return 1
		}
		_, _ = fmt.Fprintf(deps.stdout, "Enabled and started %s\n", systemdServiceName)
	} else {
		_, _ = fmt.Fprintln(deps.stdout, "\nNext steps:")
		_, _ = fmt.Fprintf(deps.stdout, "  systemctl --user enable --now %s\n", systemdServiceName)
		_, _ = fmt.Fprintln(deps.stdout, "  Add \"Gemini Voice\" in fcitx5-configtool")
	}

	return 0
}
