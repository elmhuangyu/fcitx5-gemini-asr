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
	"time"

	"charm.land/huh/v2"
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
		lookPath: exec.LookPath,
		getenv:   os.Getenv,
		serveFn:  runDaemon,
	}
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
  auth verify         Verify encrypted credential can be decrypted (alias: verify-key)
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
  verify   Decrypt-check the stored credential file without printing the secret
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

func cmdAuth(args []string, deps cliDeps) int {
	if len(args) == 0 {
		printAuthUsage(deps.stdout)
		return 0
	}
	switch args[0] {
	case "set":
		return cmdAuthSet(args[1:], deps)
	case "verify":
		return cmdAuthVerify(args[1:], deps)
	case "status":
		return cmdAuthStatus(args[1:], deps)
	case "remove":
		return cmdAuthRemove(args[1:], deps)
	case "help", "-h", "--help":
		printAuthUsage(deps.stdout)
		return 0
	default:
		_, _ = fmt.Fprintf(deps.stderr, "unknown auth subcommand: %s\n\n", args[0])
		printAuthUsage(deps.stderr)
		return 2
	}
}

func loadConfigOrDefault(configPath string) (Config, error) {
	cfg, err := LoadConfigFile(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultConfig(), nil
		}
		return Config{}, err
	}
	return cfg, nil
}

func validateWithKeyOption(withKey string) error {
	switch withKey {
	case "auto", "host":
		return nil
	default:
		return fmt.Errorf("unsupported --with-key %q in --user mode (must be one of: auto, host)", withKey)
	}
}

func resolveCredNameAndPath(configPath, nameOverride, pathOverride string) (string, string, error) {
	cfg, err := loadConfigOrDefault(configPath)
	if err != nil {
		return "", "", fmt.Errorf("load config %q: %w", configPath, err)
	}
	credName := strings.TrimSpace(nameOverride)
	if credName == "" {
		credName = cfg.Connection.CredentialName
	}
	if credName == "" {
		credName = DefaultCredentialName
	}
	if err := validateCredentialName(credName); err != nil {
		return "", "", err
	}

	credPath := strings.TrimSpace(pathOverride)
	if credPath == "" {
		credPath = filepath.Join(filepath.Dir(configPath), credName+".cred")
	}
	return credName, filepath.Clean(credPath), nil
}

func readSecretInput(deps cliDeps, keyFile string, useStdin bool) (string, error) {
	if strings.TrimSpace(keyFile) != "" {
		info, err := os.Stat(keyFile)
		if err != nil {
			return "", fmt.Errorf("stat key file %q: %w", keyFile, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("key file %q is a directory", keyFile)
		}
		data, err := readBoundedFile(keyFile, maxCredentialBytes)
		if err != nil {
			return "", fmt.Errorf("read key file %q: %w", keyFile, err)
		}
		key := strings.TrimSpace(string(data))
		if key == "" {
			return "", errors.New("API key in key file is empty")
		}
		return key, nil
	}

	if useStdin || !deps.isTTY() {
		data, err := io.ReadAll(io.LimitReader(deps.stdin, maxCredentialBytes+1))
		if err != nil {
			return "", fmt.Errorf("read API key from stdin: %w", err)
		}
		if int64(len(data)) > maxCredentialBytes {
			return "", fmt.Errorf("API key from stdin exceeds maximum allowed size of %d bytes", maxCredentialBytes)
		}
		key := strings.TrimSpace(string(data))
		if key == "" {
			return "", errors.New("API key from stdin is empty")
		}
		return key, nil
	}

	_, _ = fmt.Fprint(deps.stderr, "Enter Gemini API key (input hidden): ")
	secret, err := deps.readPassword()
	_, _ = fmt.Fprintln(deps.stderr)
	if err != nil {
		return "", fmt.Errorf("read API key from terminal: %w", err)
	}
	key := strings.TrimSpace(secret)
	if key == "" {
		return "", errors.New("API key cannot be empty")
	}
	return key, nil
}

type authSetOptions struct {
	configPath string
	outputPath string
	credName   string
	withKey    string
	useStdin   bool
	keyFile    string
	secret     string
	noVerify   bool
	restart    bool
}

func encryptAndSaveCredential(opts authSetOptions, deps cliDeps) error {
	if err := validateWithKeyOption(opts.withKey); err != nil {
		return err
	}
	credName, outPath, err := resolveCredNameAndPath(opts.configPath, opts.credName, opts.outputPath)
	if err != nil {
		return err
	}

	apiKey := strings.TrimSpace(opts.secret)
	if apiKey == "" {
		apiKey, err = readSecretInput(deps, opts.keyFile, opts.useStdin)
		if err != nil {
			return err
		}
	}

	dir := filepath.Dir(outPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create credential directory %q: %w", dir, err)
	}
	if filepath.Base(dir) == "gemini-voice" {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("chmod directory %q: %w", dir, err)
		}
	}

	tmpFile, err := os.CreateTemp(dir, ".cred.tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary credential file: %w", err)
	}
	tmpPath := tmpFile.Name()
	_ = tmpFile.Close()
	_ = os.Chmod(tmpPath, 0o600)

	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res := deps.runCmd(
		ctx,
		apiKey,
		"systemd-creds",
		"encrypt",
		"--user",
		"--with-key="+opts.withKey,
		"--name="+credName,
		"--no-ask-password",
		"-",
		tmpPath,
	)
	if res.err != nil {
		errDetail := strings.TrimSpace(res.stderr)
		if errDetail != "" {
			errDetail = ": " + errDetail
		}
		return fmt.Errorf("systemd-creds encrypt failed (%w)%s", res.err, errDetail)
	}

	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("chmod temporary credential file: %w", err)
	}
	if err := syncFilePath(tmpPath); err != nil {
		return fmt.Errorf("sync temporary credential file: %w", err)
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		return fmt.Errorf("install credential file %q: %w", outPath, err)
	}
	renamed = true
	syncDir(dir)

	if !opts.noVerify {
		decRes := deps.runCmd(
			ctx,
			"",
			"systemd-creds",
			"decrypt",
			"--user",
			"--name="+credName,
			"--no-ask-password",
			outPath,
			"-",
		)
		if decRes.err != nil {
			detail := strings.TrimSpace(decRes.stderr)
			if detail != "" {
				detail = ": " + detail
			}
			return fmt.Errorf("verification decrypt failed (%w)%s", decRes.err, detail)
		}
		plain := strings.TrimSpace(decRes.stdout)
		if plain == "" {
			return errors.New("verification decrypt returned empty credential")
		}
		_, _ = fmt.Fprintf(deps.stdout, "Saved and verified encrypted credential: %s (name=%s, with-key=%s, key=%s, length=%d)\n",
			outPath, credName, opts.withKey, MaskSecret(plain), len(plain))
	} else {
		_, _ = fmt.Fprintf(deps.stdout, "Saved encrypted credential: %s (name=%s, with-key=%s)\n",
			outPath, credName, opts.withKey)
	}

	if opts.restart {
		rRes := deps.runCmd(ctx, "", "systemctl", "--user", "restart", systemdServiceName)
		if rRes.err != nil {
			detail := strings.TrimSpace(rRes.stderr)
			if detail != "" {
				detail = ": " + detail
			}
			return fmt.Errorf("failed to restart %s (%w)%s", systemdServiceName, rRes.err, detail)
		}
		_, _ = fmt.Fprintf(deps.stdout, "Restarted %s\n", systemdServiceName)
	}

	return nil
}

func cmdAuthSet(args []string, deps cliDeps) int {
	fs := flag.NewFlagSet("auth set", flag.ContinueOnError)
	fs.SetOutput(deps.stderr)
	configPath := fs.String("config", defaultConfigPath(), "Path to config.toml")
	outputPath := fs.String("output", "", "Path to output .cred file (defaults to <config_dir>/<credential_name>.cred)")
	credName := fs.String("name", "", "Credential name override (defaults to config credential_name)")
	withKey := fs.String("with-key", "auto", "Encryption key type for systemd-creds --user: auto, host")
	useStdin := fs.Bool("stdin", false, "Read API key from standard input")
	keyFile := fs.String("key-file", "", "Read API key from file")
	noVerify := fs.Bool("no-verify", false, "Skip decrypt verification after encryption")
	restart := fs.Bool("restart", false, "Restart gemini-voice.service after saving credential")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	err := encryptAndSaveCredential(authSetOptions{
		configPath: *configPath,
		outputPath: *outputPath,
		credName:   *credName,
		withKey:    *withKey,
		useStdin:   *useStdin,
		keyFile:    *keyFile,
		noVerify:   *noVerify,
		restart:    *restart,
	}, deps)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func cmdAuthVerify(args []string, deps cliDeps) int {
	fs := flag.NewFlagSet("auth verify", flag.ContinueOnError)
	fs.SetOutput(deps.stderr)
	configPath := fs.String("config", defaultConfigPath(), "Path to config.toml")
	credPathFlag := fs.String("path", "", "Path to encrypted .cred file")
	credNameFlag := fs.String("name", "", "Credential name override")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	credName, credPath, err := resolveCredNameAndPath(*configPath, *credNameFlag, *credPathFlag)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}

	info, err := os.Stat(credPath)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: credential file %q not accessible: %v\n", credPath, err)
		return 1
	}
	if info.Mode().Perm()&0o077 != 0 {
		_, _ = fmt.Fprintf(deps.stderr, "warning: credential file %q has permissive mode %04o (recommend 0600)\n",
			credPath, info.Mode().Perm())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res := deps.runCmd(
		ctx,
		"",
		"systemd-creds",
		"decrypt",
		"--user",
		"--name="+credName,
		"--no-ask-password",
		credPath,
		"-",
	)
	if res.err != nil {
		detail := strings.TrimSpace(res.stderr)
		if detail != "" {
			detail = ": " + detail
		}
		_, _ = fmt.Fprintf(deps.stderr, "error: failed to decrypt %q (%v)%s\n", credPath, res.err, detail)
		return 1
	}

	plain := strings.TrimSpace(res.stdout)
	if plain == "" {
		_, _ = fmt.Fprintln(deps.stderr, "error: decrypted credential is empty")
		return 1
	}

	_, _ = fmt.Fprintf(deps.stdout, "OK: %s (name=%s, mode=%04o, key=%s, length=%d)\n",
		credPath, credName, info.Mode().Perm(), MaskSecret(plain), len(plain))
	return 0
}

func cmdAuthRemove(args []string, deps cliDeps) int {
	fs := flag.NewFlagSet("auth remove", flag.ContinueOnError)
	fs.SetOutput(deps.stderr)
	configPath := fs.String("config", defaultConfigPath(), "Path to config.toml")
	credPathFlag := fs.String("path", "", "Path to encrypted .cred file")
	credNameFlag := fs.String("name", "", "Credential name override")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	_, credPath, err := resolveCredNameAndPath(*configPath, *credNameFlag, *credPathFlag)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}

	if err := os.Remove(credPath); err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: remove %q: %v\n", credPath, err)
		return 1
	}
	_, _ = fmt.Fprintf(deps.stdout, "Removed encrypted credential: %s\n", credPath)
	return 0
}

func cmdAuthStatus(args []string, deps cliDeps) int {
	fs := flag.NewFlagSet("auth status", flag.ContinueOnError)
	fs.SetOutput(deps.stderr)
	configPath := fs.String("config", defaultConfigPath(), "Path to config.toml")
	credPathFlag := fs.String("path", "", "Path to encrypted .cred file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	printAuthDiagnostics(*configPath, *credPathFlag, deps)
	return 0
}

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

func syncFilePath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	syncDir(dir)
	return nil
}
