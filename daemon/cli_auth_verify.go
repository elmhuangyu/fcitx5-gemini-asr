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

func cmdAuthVerify(args []string, deps cliDeps) int {
	fs := flag.NewFlagSet("auth verify", flag.ContinueOnError)
	fs.SetOutput(deps.stderr)
	configPath := fs.String("config", defaultConfigPath(), "Path to config.toml")
	credPathFlag := fs.String("path", "", "Path to encrypted .cred file")
	credNameFlag := fs.String("name", "", "Credential name override")
	offline := fs.Bool("offline", false, "Verify local credential decryption only without calling Gemini API")
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
		if shouldPrintKeyScopeHint(res.stderr) {
			_, _ = fmt.Fprintln(deps.stderr, "hint: if this credential was encrypted with an older key scope (e.g. system --with-key=tpm2), re-run 'gemini-voice auth set' to re-encrypt it.")
		}
		return 1
	}

	plain := strings.TrimSpace(res.stdout)
	if plain == "" {
		_, _ = fmt.Fprintln(deps.stderr, "error: decrypted credential is empty")
		return 1
	}

	var model, preview string
	if !*offline {
		cfg, cfgErr := loadConfigOrDefault(*configPath)
		if cfgErr != nil {
			_, _ = fmt.Fprintf(deps.stderr, "error: load config %q: %v\n", *configPath, cfgErr)
			return 1
		}
		verifyFn := deps.verifyAPIKey
		if verifyFn == nil {
			verifyFn = defaultVerifyAPIKey
		}
		apiCtx, apiCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer apiCancel()
		var reply string
		var apiErr error
		model, reply, apiErr = verifyFn(apiCtx, cfg, plain)
		if apiErr != nil {
			_, _ = fmt.Fprintf(deps.stderr, "error: API verification failed: %v\n", apiErr)
			return 1
		}
		preview = strings.Join(strings.Fields(reply), " ")
		if len(preview) > 200 {
			preview = preview[:197] + "..."
		}
	}

	_, _ = fmt.Fprintf(deps.stdout, "OK: %s (name=%s, mode=%04o, key=%s, length=%d)\n",
		credPath, credName, info.Mode().Perm(), MaskSecret(plain), len(plain))
	if !*offline {
		_, _ = fmt.Fprintf(deps.stdout, "API OK (model=%s): %s\n", model, preview)
	}
	return 0
}

// shouldPrintKeyScopeHint reports whether systemd-creds decrypt stderr
// suggests a key, scope, TPM, or credential decryption mismatch rather than
// an unrelated filesystem or invocation error.
func shouldPrintKeyScopeHint(stderr string) bool {
	s := strings.ToLower(strings.TrimSpace(stderr))
	if s == "" {
		return false
	}
	// Check strong decryption/TPM indicators before filesystem error substrings
	// because TPM migration errors often wrap "No such file or directory".
	if containsAny(s, []string{
		"failed to decrypt",
		"tpm",
		"unseal",
	}) {
		return true
	}
	if containsAny(s, []string{
		"no such file",
		"does not exist",
		"permission denied",
		"operation not permitted",
		"is a directory",
	}) {
		return false
	}
	return containsAny(s, []string{
		"bad message",
		"key",
		"scope",
	})
}

func containsAny(s string, substrs []string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
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
