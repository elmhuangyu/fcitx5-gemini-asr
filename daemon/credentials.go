package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	DefaultCredentialName = "gemini_api_key"
	DefaultFallbackEnv    = "GEMINI_API_KEY"
)

var keyParamFallbackRe = regexp.MustCompile(`([?&](?:key|api_key|token)=)[^&\s"]+`)

// LoadAPIKey loads the Gemini API key from systemd's CREDENTIALS_DIRECTORY first,
// falling back to the configured environment variable only when CREDENTIALS_DIRECTORY is unset.
func LoadAPIKey(credName, fallbackEnv string) (string, error) {
	credName = strings.TrimSpace(credName)
	if credName == "" {
		credName = DefaultCredentialName
	}
	fallbackEnv = strings.TrimSpace(fallbackEnv)
	if fallbackEnv == "" {
		fallbackEnv = DefaultFallbackEnv
	}

	if err := validateCredentialName(credName); err != nil {
		return "", err
	}

	if credDir := strings.TrimSpace(os.Getenv("CREDENTIALS_DIRECTORY")); credDir != "" {
		credPath := filepath.Join(credDir, credName)
		data, err := os.ReadFile(credPath)
		if err != nil {
			return "", fmt.Errorf("failed to read credential %q from CREDENTIALS_DIRECTORY: %w", credName, err)
		}
		key := strings.TrimSpace(string(data))
		if key == "" {
			return "", fmt.Errorf("credential %q in CREDENTIALS_DIRECTORY is empty", credName)
		}
		return key, nil
	}

	key := strings.TrimSpace(os.Getenv(fallbackEnv))
	if key == "" {
		return "", fmt.Errorf("GEMINI API key not found: CREDENTIALS_DIRECTORY is unset and environment variable %s is empty", fallbackEnv)
	}
	return key, nil
}

func validateCredentialName(name string) error {
	if name == "" || name == "." || name == ".." {
		return errors.New("invalid credential name")
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return fmt.Errorf("invalid credential name %q: path traversal is not allowed", name)
	}
	if filepath.Base(name) != name {
		return fmt.Errorf("invalid credential name %q", name)
	}
	return nil
}

// RedactURL strips sensitive query parameters and userinfo from a URL before logging.
func RedactURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return keyParamFallbackRe.ReplaceAllString(rawURL, "${1}REDACTED")
	}
	if u.User != nil {
		u.User = url.User(u.User.Username())
	}
	q := u.Query()
	changed := false
	for _, sensitive := range []string{"key", "api_key", "token"} {
		if q.Has(sensitive) {
			q.Set(sensitive, "REDACTED")
			changed = true
		}
	}
	if changed {
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// SanitizeError removes any secret occurrences or ?key= query parameters from an error.
func SanitizeError(err error, secret string) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if secret != "" {
		msg = strings.ReplaceAll(msg, secret, "REDACTED")
	}
	msg = keyParamFallbackRe.ReplaceAllString(msg, "${1}REDACTED")
	return msg
}
