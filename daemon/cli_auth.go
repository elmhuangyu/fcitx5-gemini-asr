package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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
