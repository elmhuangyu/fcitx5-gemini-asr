package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/term"
)

const systemdServiceName = "gemini-voice.service"

type cmdResult struct {
	stdout string
	stderr string
	err    error
}

type cliDeps struct {
	stdin        io.Reader
	stdout       io.Writer
	stderr       io.Writer
	isTTY        func() bool
	readPassword func() (string, error)
	runCmd       func(ctx context.Context, stdin string, name string, args ...string) cmdResult
	lookPath     func(file string) (string, error)
	getenv       func(key string) string
	serveFn      func(configPath, socketPath string) int
	runSetupForm func(state *setupFormState, opts setupFormOptions) error
	verifyAPIKey func(ctx context.Context, cfg Config, apiKey string) (model, reply string, err error)
}

func defaultCLIDeps() cliDeps {
	return cliDeps{
		stdin:  os.Stdin,
		stdout: os.Stdout,
		stderr: os.Stderr,
		isTTY: func() bool {
			return term.IsTerminal(int(os.Stdin.Fd()))
		},
		readPassword: func() (string, error) {
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			if err != nil {
				return "", err
			}
			return string(b), nil
		},
		runCmd: func(ctx context.Context, stdin string, name string, args ...string) cmdResult {
			cmd := exec.CommandContext(ctx, name, args...)
			if stdin != "" {
				cmd.Stdin = strings.NewReader(stdin)
			}
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			return cmdResult{
				stdout: stdout.String(),
				stderr: stderr.String(),
				err:    err,
			}
		},
		lookPath:     exec.LookPath,
		getenv:       os.Getenv,
		serveFn:      runDaemon,
		verifyAPIKey: defaultVerifyAPIKey,
	}
}

func defaultVerifyAPIKey(ctx context.Context, cfg Config, apiKey string) (string, string, error) {
	model := strings.TrimPrefix(strings.TrimSpace(cfg.PostProcess.Model), "models/")
	if model == "" {
		model = DefaultPostProcessModel
	}
	reply, err := NewPostProcessor().GenerateRaw(ctx, cfg, apiKey, model, "Reply with your model name only.", 0.0)
	if err != nil {
		return model, "", err
	}
	return model, reply, nil
}

// MaskSecret returns a masked preview of a secret string without exposing its middle characters.
func MaskSecret(s string) string {
	trimmed := strings.TrimSpace(s)
	n := len(trimmed)
	if n == 0 {
		return "(empty)"
	}
	if n <= 8 {
		return strings.Repeat("*", n)
	}
	return trimmed[:4] + "..." + trimmed[n-4:]
}

func isDaemonServeFlag(arg string) bool {
	return arg == "-config" ||
		strings.HasPrefix(arg, "-config=") ||
		arg == "--config" ||
		strings.HasPrefix(arg, "--config=") ||
		arg == "-socket" ||
		strings.HasPrefix(arg, "-socket=") ||
		arg == "--socket" ||
		strings.HasPrefix(arg, "--socket=")
}

func runCLI(argv0 string, args []string, deps cliDeps) int {
	prog := filepath.Base(argv0)
	if strings.Contains(prog, "daemon") {
		if len(args) == 0 || isDaemonServeFlag(args[0]) {
			return cmdServe(args, deps)
		}
	}

	if len(args) == 0 {
		printMainUsage(deps.stdout)
		return 0
	}

	switch args[0] {
	case "serve":
		return cmdServe(args[1:], deps)
	case "init":
		return cmdInit(args[1:], deps)
	case "configure":
		return cmdConfigure(args[1:], deps)
	case "config":
		return cmdConfig(args[1:], deps)
	case "models":
		return cmdModels(args[1:], deps)
	case "auth":
		return cmdAuth(args[1:], deps)
	case "set-key":
		return cmdAuthSet(args[1:], deps)
	case "verify-key":
		return cmdAuthVerify(args[1:], deps)
	case "remove-key":
		return cmdAuthRemove(args[1:], deps)
	case "status":
		return cmdStatus(args[1:], deps)
	case "version", "--version", "-version":
		_, _ = fmt.Fprintf(deps.stdout, "gemini-voice %s\n", Version)
		return 0
	case "help", "-h", "--help":
		printMainUsage(deps.stdout)
		return 0
	default:
		_, _ = fmt.Fprintf(deps.stderr, "unknown command: %s\n\n", args[0])
		printMainUsage(deps.stderr)
		return 2
	}
}

func printMainUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `gemini-voice - CLI and background daemon for Fcitx5 Gemini Voice IME

Usage:
  gemini-voice <command> [options]

Commands:
  init                Interactive TUI setup wizard (config, input mode, models, and encrypted key)
  configure           Interactive TUI form for input mode, models, hotkeys, language, and proxy
  config              Show, get, or set daemon and Fcitx5 addon settings
  models              Inspect or update live and per-action post-process models
  auth set            Encrypt and store Gemini API key (alias: set-key)
  auth verify         Verify encrypted credential and test API connectivity (alias: verify-key)
  auth status         Show credential status
  auth remove         Delete encrypted credential file (alias: remove-key)
  status              Show full system diagnostics (config, credential, PipeWire, daemon)
  serve               Run the background Unix socket daemon
  version             Print version information
  help                Show this help message

Run "gemini-voice <command> --help" for command-specific flags.
`)
}

func printAuthUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage:
  gemini-voice auth <subcommand> [options]

Subcommands:
  set      Encrypt and store Gemini API key using systemd-creds
  verify   Decrypt-check the stored credential and test it against the Gemini API
  status   Show credential file and systemd-creds status
  remove   Delete the stored credential file
`)
}

func cmdServe(args []string, deps cliDeps) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(deps.stderr)
	configPath := fs.String("config", defaultConfigPath(), "Path to daemon config.toml")
	socketPath := fs.String("socket", defaultSocketPath(), "Path to Unix domain socket")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	return deps.serveFn(*configPath, *socketPath)
}
