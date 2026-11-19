package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAPIKey_FromCredentialsDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gemini_api_key"), []byte("  cred-secret-123\n\t "), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	t.Setenv("GEMINI_API_KEY", "env-secret-should-not-be-used")

	got, err := LoadAPIKey("gemini_api_key", "GEMINI_API_KEY")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "cred-secret-123" {
		t.Fatalf("got %q, want %q", got, "cred-secret-123")
	}
}

func TestLoadAPIKey_FailClosedWhenCredentialsDirMissingFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	t.Setenv("GEMINI_API_KEY", "env-fallback")

	_, err := LoadAPIKey("gemini_api_key", "GEMINI_API_KEY")
	if err == nil {
		t.Fatal("expected error when CREDENTIALS_DIRECTORY is set but credential file is missing")
	}
}

func TestLoadAPIKey_FallbackEnvWhenCredentialsDirUnset(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	t.Setenv("GEMINI_API_KEY", "  dev-env-key-456 \n")

	got, err := LoadAPIKey("gemini_api_key", "GEMINI_API_KEY")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "dev-env-key-456" {
		t.Fatalf("got %q, want %q", got, "dev-env-key-456")
	}
}

func TestLoadAPIKey_RejectsPathTraversal(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", t.TempDir())
	for _, bad := range []string{"../secret", "..\\secret", "a/b", "..", "."} {
		if _, err := LoadAPIKey(bad, "GEMINI_API_KEY"); err == nil {
			t.Fatalf("expected path traversal rejection for %q", bad)
		}
	}
}

func TestRedactURLAndSanitizeError(t *testing.T) {
	raw := "wss://generativelanguage.googleapis.com/ws/foo?alt=json&key=SUPER_SECRET_KEY_123"
	redacted := RedactURL(raw)
	if strings.Contains(redacted, "SUPER_SECRET_KEY_123") {
		t.Fatalf("RedactURL leaked secret: %q", redacted)
	}
	if !strings.Contains(redacted, "REDACTED") {
		t.Fatalf("expected REDACTED marker in %q", redacted)
	}
}

func TestLoadAPIKey_RejectsOversizedCredential(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CREDENTIALS_DIRECTORY", dir)

	maxPath := filepath.Join(dir, "exact_limit")
	exact := bytes.Repeat([]byte("a"), maxCredentialBytes)
	if err := os.WriteFile(maxPath, exact, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadAPIKey("exact_limit", "GEMINI_API_KEY")
	if err != nil {
		t.Fatalf("expected %d-byte credential to succeed, got error: %v", maxCredentialBytes, err)
	}
	if len(got) != maxCredentialBytes {
		t.Fatalf("got key length %d, want %d", len(got), maxCredentialBytes)
	}

	oversizedPath := filepath.Join(dir, "gemini_api_key")
	oversized := bytes.Repeat([]byte("a"), maxCredentialBytes+1)
	if err := os.WriteFile(oversizedPath, oversized, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadAPIKey("gemini_api_key", "GEMINI_API_KEY")
	if err == nil {
		t.Fatal("expected error for oversized credential file")
	}
	if !strings.Contains(err.Error(), "exceeds maximum allowed size") {
		t.Fatalf("expected maximum size error, got: %v", err)
	}
}
