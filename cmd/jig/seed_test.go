package main

import (
	"errors"
	"strings"
	"testing"
)

// Claude credentials are found the way the CLI finds them: the keychain on
// macOS (a credentials file there may be a stale leftover), the file
// elsewhere, and the file as macOS's fallback.
func TestClaudeCredentialsFollowTheCLIsOwnPrecedence(t *testing.T) {
	file := func() ([]byte, error) { return []byte("file"), nil }
	noFile := func() ([]byte, error) { return nil, errors.New("no such file") }
	keychain := func() ([]byte, error) { return []byte("keychain"), nil }
	noKeychain := func() ([]byte, error) { return nil, errors.New("item not found") }
	for _, tc := range []struct {
		name, goos           string
		readFile, keychainFn func() ([]byte, error)
		want, wantErr        string
	}{
		{"macOS prefers the keychain over a stale file", "darwin", file, keychain, "keychain", ""},
		{"macOS falls back to the file", "darwin", file, noKeychain, "file", ""},
		{"macOS with neither", "darwin", noFile, noKeychain, "", "no keychain item and no credentials file"},
		{"linux reads the file", "linux", file, keychain, "file", ""},
		{"linux without the file", "linux", noFile, keychain, "", "no credentials file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := claudeCredentials(tc.goos, tc.readFile, tc.keychainFn)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || string(got) != tc.want {
				t.Fatalf("credentials = %q err = %v, want %q", got, err, tc.want)
			}
		})
	}
}
