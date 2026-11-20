package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recordedCmd struct {
	stdin string
	name  string
	args  []string
}

func TestMaskSecret(t *testing.T) {
	if got := MaskSecret(""); got != "(empty)" {
		t.Fatalf("got %q", got)
	}
	if got := MaskSecret("abcd1234"); got != "********" {
		t.Fatalf("got %q", got)
	}
	if got := MaskSecret("AIzaSyDummyKeyValue9876"); got != "AIza...9876" {
		t.Fatalf("got %q", got)
	}
}

func TestCLI_AuthSetVerifyRemove_StdinAndKeyFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gemini-voice", "config.toml")
	credPath := filepath.Join(dir, "gemini-voice", "gemini_api_key.cred")
	rawSecret := "AIzaSyTestSecretKey12345"

	var cmds []recordedCmd
	var stdout, stderr bytes.Buffer
	deps := cliDeps{
		stdin:  strings.NewReader("  " + rawSecret + "\n"),
		stdout: &stdout,
		stderr: &stderr,
		isTTY:  func() bool { return false },
		runCmd: func(ctx context.Context, stdin string, name string, args ...string) cmdResult {
			cmds = append(cmds, recordedCmd{stdin: stdin, name: name, args: args})
			if name == "systemd-creds" && len(args) > 0 && args[0] == "encrypt" {
				outFile := args[len(args)-1]
				if err := os.WriteFile(outFile, []byte("ENCRYPTED_BLOB"), 0o644); err != nil {
					return cmdResult{err: err}
				}
				return cmdResult{}
			}
			if name == "systemd-creds" && len(args) > 0 && args[0] == "decrypt" {
				return cmdResult{stdout: rawSecret + "\n"}
			}
			return cmdResult{}
		},
		lookPath: func(file string) (string, error) { return "/usr/bin/" + file, nil },
		getenv:   func(string) string { return "" },
		serveFn:  func(string, string) int { return 0 },
	}

	code := runCLI("gemini-voice", []string{"auth", "set", "--config", cfgPath, "--stdin"}, deps)
	if code != 0 {
		t.Fatalf("auth set exit=%d stderr=%s", code, stderr.String())
	}
	if len(cmds) != 2 {
		t.Fatalf("expected 2 systemd-creds calls (encrypt+decrypt), got %d", len(cmds))
	}
	if cmds[0].stdin != rawSecret {
		t.Fatalf("expected trimmed secret on stdin, got %q", cmds[0].stdin)
	}
	for _, a := range cmds[0].args {
		if strings.Contains(a, rawSecret) {
			t.Fatalf("secret leaked in CLI argument: %q", a)
		}
	}
	if strings.Contains(stdout.String(), rawSecret) {
		t.Fatalf("raw secret leaked in stdout: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "AIza...2345") {
		t.Fatalf("expected masked fingerprint in stdout: %s", stdout.String())
	}

	info, err := os.Stat(credPath)
	if err != nil {
		t.Fatalf("stat cred file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("cred file mode = %04o, want 0600", info.Mode().Perm())
	}

	// Verify command hits Gemini generateContent API with "Reply with your model name only."
	var gotPrompt string
	var gotAPIKey string
	var apiHits int
	apiStatus := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiHits++
		gotAPIKey = r.Header.Get("x-goog-api-key")
		body, _ := io.ReadAll(r.Body)
		var req generateContentRequest
		_ = json.Unmarshal(body, &req)
		if len(req.Contents) > 0 && len(req.Contents[0].Parts) > 0 {
			gotPrompt = req.Contents[0].Parts[0].Text
		}
		if apiStatus != http.StatusOK {
			w.WriteHeader(apiStatus)
			_, _ = w.Write([]byte(`{"error":{"code":401,"message":"API key invalid","status":"UNAUTHENTICATED"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"I am Gemini 3.5 Flash Lite."}]}}]}`))
	}))
	defer srv.Close()

	cfgTOML := "[connection]\napi_base_url = \"" + srv.URL + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfgTOML), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	code = runCLI("gemini-voice", []string{"verify-key", "--config", cfgPath}, deps)
	if code != 0 {
		t.Fatalf("verify-key exit=%d stderr=%s", code, stderr.String())
	}
	if gotAPIKey != rawSecret {
		t.Fatalf("expected x-goog-api-key=%q, got %q", rawSecret, gotAPIKey)
	}
	if gotPrompt != "Reply with your model name only." {
		t.Fatalf("expected prompt %q, got %q", "Reply with your model name only.", gotPrompt)
	}
	if strings.Contains(stdout.String(), rawSecret) ||
		!strings.Contains(stdout.String(), "AIza...2345") ||
		!strings.Contains(stdout.String(), "API OK") ||
		!strings.Contains(stdout.String(), "I am Gemini 3.5 Flash Lite.") {
		t.Fatalf("unexpected verify output: %s", stdout.String())
	}

	// Verify --offline skips API check and prints OK only
	apiHits = 0
	stdout.Reset()
	stderr.Reset()
	code = runCLI("gemini-voice", []string{"auth", "verify", "--offline", "--config", cfgPath}, deps)
	if code != 0 {
		t.Fatalf("auth verify --offline exit=%d stderr=%s", code, stderr.String())
	}
	if apiHits != 0 {
		t.Fatalf("expected 0 API hits with --offline, got %d", apiHits)
	}
	if !strings.Contains(stdout.String(), "OK:") || strings.Contains(stdout.String(), "API OK") {
		t.Fatalf("unexpected --offline stdout: %s", stdout.String())
	}

	// Verify fails when API returns an error and does not print OK before failing
	apiStatus = http.StatusUnauthorized
	stdout.Reset()
	stderr.Reset()
	code = runCLI("gemini-voice", []string{"auth", "verify", "--config", cfgPath}, deps)
	if code != 1 {
		t.Fatalf("expected verify failure on 401, got exit=%d", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected no stdout on failed verify, got: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "API verification failed") || strings.Contains(stderr.String(), rawSecret) {
		t.Fatalf("unexpected verify failure stderr: %s", stderr.String())
	}

	// Verify prints migration hint only when systemd-creds failure suggests a key/scope/decryption mismatch
	for _, tc := range []struct {
		name     string
		stderr   string
		wantHint bool
	}{
		{name: "failed to decrypt", stderr: "Failed to decrypt", wantHint: true},
		{name: "bad message", stderr: "Failed to decrypt: Bad message", wantHint: true},
		{name: "tpm unseal", stderr: "TPM2 unseal failed", wantHint: true},
		{name: "missing file", stderr: "No such file or directory", wantHint: false},
		{name: "permission denied", stderr: "Failed to open credential: Permission denied", wantHint: false},
		{name: "decrypt permission denied", stderr: "Failed to decrypt: Permission denied", wantHint: false},
		{name: "empty stderr", stderr: "", wantHint: false},
	} {
		failDeps := deps
		failDeps.runCmd = func(ctx context.Context, stdin string, name string, args ...string) cmdResult {
			return cmdResult{stderr: tc.stderr, err: errors.New("exit status 1")}
		}
		stdout.Reset()
		stderr.Reset()
		code = runCLI("gemini-voice", []string{"auth", "verify", "--config", cfgPath}, failDeps)
		if code != 1 {
			t.Fatalf("%s: expected verify failure on decrypt error, got exit=%d", tc.name, code)
		}
		hasHint := strings.Contains(stderr.String(), "gemini-voice auth set")
		if hasHint != tc.wantHint {
			t.Fatalf("%s: hint presence=%v, want %v; stderr=%s", tc.name, hasHint, tc.wantHint, stderr.String())
		}
	}

	// Key-file input
	keyFile := filepath.Join(dir, "raw.key")
	if err := os.WriteFile(keyFile, []byte("AIzaFromFileKey9999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmds = nil
	stdout.Reset()
	code = runCLI("gemini-voice", []string{"set-key", "--config", cfgPath, "--key-file", keyFile, "--with-key", "auto"}, deps)
	if code != 0 {
		t.Fatalf("set-key --key-file exit=%d stderr=%s", code, stderr.String())
	}
	if cmds[0].stdin != "AIzaFromFileKey9999" {
		t.Fatalf("unexpected key-file stdin: %q", cmds[0].stdin)
	}

	// Remove command
	stdout.Reset()
	code = runCLI("gemini-voice", []string{"auth", "remove", "--config", cfgPath}, deps)
	if code != 0 {
		t.Fatalf("auth remove exit=%d stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(credPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected cred file removed, stat err=%v", err)
	}
}

func TestCLI_AuthSetErrorsAndTempCleanup(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "gemini-voice")
	cfgPath := filepath.Join(cfgDir, "config.toml")

	// Empty key rejected
	var stdout, stderr bytes.Buffer
	deps := cliDeps{
		stdin:    strings.NewReader("   \n"),
		stdout:   &stdout,
		stderr:   &stderr,
		isTTY:    func() bool { return false },
		runCmd:   func(context.Context, string, string, ...string) cmdResult { return cmdResult{} },
		lookPath: func(string) (string, error) { return "", os.ErrNotExist },
		getenv:   func(string) string { return "" },
		serveFn:  func(string, string) int { return 0 },
	}
	if code := runCLI("gemini-voice", []string{"auth", "set", "--config", cfgPath, "--stdin"}, deps); code == 0 {
		t.Fatal("expected failure for empty API key")
	}

	// Encryption failure prints error detail and cleans up temp file
	stdout.Reset()
	stderr.Reset()
	deps.stdin = strings.NewReader("AIzaSyValidKey12345")
	deps.runCmd = func(context.Context, string, string, ...string) cmdResult {
		return cmdResult{stderr: "systemd-creds error", err: errors.New("exit status 1")}
	}
	if code := runCLI("gemini-voice", []string{"auth", "set", "--config", cfgPath, "--stdin"}, deps); code == 0 {
		t.Fatal("expected failure when systemd-creds fails")
	}
	if !strings.Contains(stderr.String(), "systemd-creds error") {
		t.Fatalf("expected systemd-creds error detail in stderr: %s", stderr.String())
	}
	entries, err := os.ReadDir(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected temp file cleaned up, found %d entries", len(entries))
	}
}

func TestCLI_InitStatusAndDaemonDispatch(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gemini-voice", "config.toml")
	sockPath := filepath.Join(dir, "gemini.sock")

	var stdout, stderr bytes.Buffer
	deps := cliDeps{
		stdin:  strings.NewReader("AIzaSyInitSecretKey7777\n"),
		stdout: &stdout,
		stderr: &stderr,
		isTTY:  func() bool { return false },
		runCmd: func(ctx context.Context, stdin string, name string, args ...string) cmdResult {
			if name == "systemd-creds" && len(args) > 0 && args[0] == "encrypt" {
				outFile := args[len(args)-1]
				if err := os.WriteFile(outFile, []byte("ENC"), 0o600); err != nil {
					return cmdResult{err: err}
				}
				return cmdResult{}
			}
			if name == "systemd-creds" && len(args) > 0 && args[0] == "decrypt" {
				return cmdResult{stdout: "AIzaSyInitSecretKey7777"}
			}
			if name == "systemctl" && len(args) >= 2 && args[1] == "is-active" {
				return cmdResult{stdout: "inactive\n"}
			}
			return cmdResult{}
		},
		lookPath: func(file string) (string, error) { return "/usr/bin/" + file, nil },
		getenv:   func(string) string { return "" },
		serveFn:  func(string, string) int { return 0 },
	}

	if code := runCLI("gemini-voice", []string{"init", "--config", cfgPath, "--stdin"}, deps); code != 0 {
		t.Fatalf("init exit=%d stderr=%s", code, stderr.String())
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != DefaultConfigTOML() {
		t.Fatal("init wrote unexpected config.toml content")
	}

	// Second init without --force must preserve custom config.toml
	if err := os.WriteFile(cfgPath, []byte("# custom\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deps.stdin = strings.NewReader("AIzaSyInitSecretKey8888\n")
	if code := runCLI("gemini-voice", []string{"init", "--config", cfgPath, "--stdin"}, deps); code != 0 {
		t.Fatalf("second init exit=%d stderr=%s", code, stderr.String())
	}
	data2, err := os.ReadFile(cfgPath)
	if err != nil || string(data2) != "# custom\n" {
		t.Fatalf("existing config.toml was overwritten: %v %q", err, string(data2))
	}

	// Status check
	stdout.Reset()
	if code := runCLI("gemini-voice", []string{"status", "--config", cfgPath, "--socket", sockPath}, deps); code != 0 {
		t.Fatalf("status exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "systemd-creds:    /usr/bin/systemd-creds") {
		t.Fatalf("unexpected status output:\n%s", stdout.String())
	}

	// Invoking as gemini-voice-daemon with no args or -socket dispatches to serveFn
	var servedCfg, servedSock string
	deps.serveFn = func(c, s string) int {
		servedCfg = c
		servedSock = s
		return 42
	}
	if code := runCLI("/usr/local/bin/gemini-voice-daemon", []string{"-config", cfgPath, "-socket", sockPath}, deps); code != 42 {
		t.Fatalf("daemon dispatch exit=%d, want 42", code)
	}
	if servedCfg != cfgPath || servedSock != sockPath {
		t.Fatalf("daemon got cfg=%q sock=%q", servedCfg, servedSock)
	}
}

func TestWriteFileAtomicAndSyncHelpers(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.toml")

	if err := writeFileAtomic(target, []byte("first\n"), 0o600); err != nil {
		t.Fatalf("initial writeFileAtomic: %v", err)
	}
	if err := writeFileAtomic(target, []byte("second\n"), 0o600); err != nil {
		t.Fatalf("overwrite writeFileAtomic: %v", err)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(data) != "second\n" {
		t.Fatalf("got %q, want %q", string(data), "second\n")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %04o, want 0600", info.Mode().Perm())
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.toml" {
		t.Fatalf("expected only config.toml in dir, got %d entries", len(entries))
	}

	if err := syncFilePath(target); err != nil {
		t.Fatalf("syncFilePath existing file: %v", err)
	}
	if err := syncFilePath(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("expected error from syncFilePath on missing file")
	}

	syncDir(dir)
	syncDir(filepath.Join(dir, "missing-dir"))
}

func TestCLI_AuthSetRejectsOversizedInput(t *testing.T) {
	cases := []struct {
		name     string
		makeArgs func(t *testing.T, dir, cfgPath string) ([]string, []byte)
	}{
		{
			name: "oversized_key_file",
			makeArgs: func(t *testing.T, dir, cfgPath string) ([]string, []byte) {
				t.Helper()
				keyFile := filepath.Join(dir, "oversized.key")
				if err := os.WriteFile(keyFile, bytes.Repeat([]byte("k"), maxCredentialBytes+1), 0o600); err != nil {
					t.Fatal(err)
				}
				return []string{"auth", "set", "--config", cfgPath, "--key-file", keyFile}, nil
			},
		},
		{
			name: "oversized_stdin",
			makeArgs: func(_ *testing.T, _, cfgPath string) ([]string, []byte) {
				return []string{"auth", "set", "--config", cfgPath, "--stdin"}, bytes.Repeat([]byte("s"), maxCredentialBytes+1)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "gemini-voice", "config.toml")
			args, stdinData := tc.makeArgs(t, dir, cfgPath)

			cmdCalled := false
			var stdout, stderr bytes.Buffer
			deps := cliDeps{
				stdin:  bytes.NewReader(stdinData),
				stdout: &stdout,
				stderr: &stderr,
				isTTY:  func() bool { return false },
				runCmd: func(context.Context, string, string, ...string) cmdResult {
					cmdCalled = true
					return cmdResult{}
				},
				lookPath: func(string) (string, error) { return "", os.ErrNotExist },
				getenv:   func(string) string { return "" },
				serveFn:  func(string, string) int { return 0 },
			}

			code := runCLI("gemini-voice", args, deps)
			if code == 0 {
				t.Fatalf("expected non-zero exit for %s", tc.name)
			}
			if cmdCalled {
				t.Fatalf("expected systemd-creds not to be invoked for %s", tc.name)
			}
			if !strings.Contains(stderr.String(), "exceeds maximum allowed size") {
				t.Fatalf("expected maximum size error in stderr, got: %s", stderr.String())
			}
		})
	}
}
