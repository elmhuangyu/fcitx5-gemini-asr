package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

func cmdStatus(args []string, deps cliDeps) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(deps.stderr)
	configPath := fs.String("config", defaultConfigPath(), "Path to config.toml")
	socketPath := fs.String("socket", defaultSocketPath(), "Path to Unix domain socket")
	credPathFlag := fs.String("cred-path", "", "Path to encrypted .cred file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	cfgExists := "missing (using built-in defaults)"
	if info, err := os.Stat(*configPath); err == nil && !info.IsDir() {
		cfgExists = fmt.Sprintf("present (%04o)", info.Mode().Perm())
	}
	_, _ = fmt.Fprintf(deps.stdout, "Config file:      %s [%s]\n", *configPath, cfgExists)

	printAuthDiagnostics(*configPath, *credPathFlag, deps)

	if p, err := deps.lookPath("pw-record"); err == nil {
		_, _ = fmt.Fprintf(deps.stdout, "PipeWire capture: %s\n", p)
	} else {
		_, _ = fmt.Fprintln(deps.stdout, "PipeWire capture: missing (pw-record not found in PATH)")
	}

	sockStatus := "not listening"
	if info, err := os.Lstat(*socketPath); err == nil {
		if info.Mode()&os.ModeSocket != 0 {
			sockStatus = fmt.Sprintf("socket present (%04o)", info.Mode().Perm())
		} else {
			sockStatus = fmt.Sprintf("non-socket file present (%s)", info.Mode())
		}
	}
	_, _ = fmt.Fprintf(deps.stdout, "Daemon socket:    %s [%s]\n", *socketPath, sockStatus)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	svcRes := deps.runCmd(ctx, "", "systemctl", "--user", "is-active", systemdServiceName)
	svcState := strings.TrimSpace(svcRes.stdout)
	if svcState == "" {
		svcState = "unknown"
	}
	_, _ = fmt.Fprintf(deps.stdout, "Systemd service:  %s (%s)\n", systemdServiceName, svcState)

	return 0
}

func printAuthDiagnostics(configPath, credPathOverride string, deps cliDeps) {
	cfg, cfgErr := loadConfigOrDefault(configPath)
	credName, credPath, err := resolveCredNameAndPath(configPath, "", credPathOverride)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stdout, "Credential:       error (%v)\n", err)
		return
	}

	credStatus := "missing"
	if info, statErr := os.Stat(credPath); statErr == nil {
		credStatus = fmt.Sprintf("present (%04o, %d bytes)", info.Mode().Perm(), info.Size())
	}
	_, _ = fmt.Fprintf(deps.stdout, "Credential file:  %s [%s] (name=%s)\n", credPath, credStatus, credName)

	if p, lpErr := deps.lookPath("systemd-creds"); lpErr == nil {
		_, _ = fmt.Fprintf(deps.stdout, "systemd-creds:    %s\n", p)
	} else {
		_, _ = fmt.Fprintln(deps.stdout, "systemd-creds:    missing")
	}

	fallbackEnv := DefaultFallbackEnv
	if cfgErr == nil && strings.TrimSpace(cfg.Connection.FallbackEnv) != "" {
		fallbackEnv = cfg.Connection.FallbackEnv
	}
	envState := "unset"
	if strings.TrimSpace(deps.getenv(fallbackEnv)) != "" {
		envState = "set"
	}
	_, _ = fmt.Fprintf(deps.stdout, "Fallback env:     %s (%s)\n", fallbackEnv, envState)
}
