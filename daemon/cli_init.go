package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/huh/v2"
)

func cmdInit(args []string, deps cliDeps) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(deps.stderr)
	configPath := fs.String("config", defaultConfigPath(), "Path to config.toml")
	fcitxPath := fs.String("fcitx-config", DefaultFcitxConfigPath(), "Path to Fcitx5 gemini-voice.conf")
	withKey := fs.String("with-key", "auto", "Encryption key type for systemd-creds --user: auto, host")
	force := fs.Bool("force", false, "Overwrite existing config.toml in non-interactive mode")
	enableSvc := fs.Bool("enable-service", false, "Enable and start gemini-voice.service after initialization")
	useStdin := fs.Bool("stdin", false, "Read API key from standard input")
	keyFile := fs.String("key-file", "", "Read API key from file")
	noVerify := fs.Bool("no-verify", false, "Skip decrypt verification after encryption")
	nonInteractive := fs.Bool("non-interactive", false, "Skip interactive wizard prompts and write default config if missing")
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
		_, credPath, resolveErr := resolveCredNameAndPath(*configPath, "", "")
		credExists := false
		if resolveErr == nil {
			if info, credErr := os.Stat(credPath); credErr == nil && !info.IsDir() {
				credExists = true
			}
		}

		res, err := runSetupTUI(*configPath, *fcitxPath, setupFormOptions{
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
				configPath: *configPath,
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
		cfgDir := filepath.Dir(*configPath)
		if err := os.MkdirAll(cfgDir, 0o700); err != nil {
			_, _ = fmt.Fprintf(deps.stderr, "error: create config directory %q: %v\n", cfgDir, err)
			return 1
		}

		_, statErr := os.Stat(*configPath)
		if errors.Is(statErr, os.ErrNotExist) || *force {
			if err := writeFileAtomic(*configPath, []byte(DefaultConfigTOML()), 0o600); err != nil {
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
			configPath: *configPath,
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
