package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

// A signing key is opaque bytes. Base64 is what the documented one-liner
// produces, but a random secret from anywhere else is just as good a key, and
// refusing it produced an error naming a byte offset and no remedy.
func TestSessionKey_AcceptsBase64OrRaw(t *testing.T) {
	t.Run("base64", func(t *testing.T) {
		want := strings.Repeat("k", 32)
		t.Setenv("BINNACLE_SESSION_KEY", base64.StdEncoding.EncodeToString([]byte(want)))
		got, err := sessionKey()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("base64 was not decoded: %q", got)
		}
	})

	t.Run("raw", func(t *testing.T) {
		// The shape that actually turned up: base64 alphabet, but leading
		// padding, so not decodable. It is still 45 characters of secret.
		raw := "=+aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789abcDeF="
		t.Setenv("BINNACLE_SESSION_KEY", raw)
		got, err := sessionKey()
		if err != nil {
			t.Fatalf("a 45-character secret was rejected: %v", err)
		}
		if string(got) != raw {
			t.Errorf("got %q, want the value itself", got)
		}
	})

	t.Run("surrounding whitespace", func(t *testing.T) {
		raw := strings.Repeat("x", 40)
		t.Setenv("BINNACLE_SESSION_KEY", "\n  "+raw+"\n")
		got, err := sessionKey()
		if err != nil || string(got) != raw {
			t.Errorf("got %q, %v", got, err)
		}
	})
}

// Short is short either way, and the error says what to do rather than naming a
// byte offset.
func TestSessionKey_TooShortIsExplained(t *testing.T) {
	t.Setenv("BINNACLE_SESSION_KEY", "hunter2")
	_, err := sessionKey()
	if err == nil {
		t.Fatal("a seven-character key was accepted")
	}
	for _, want := range []string{"7 characters", "at least 32"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// Valid base64 that decodes to too few bytes is treated as the raw key, which
// is the stronger reading: 32 characters beats the 24 bytes they decode to.
func TestSessionKey_ShortDecodePrefersTheRawValue(t *testing.T) {
	raw := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("s", 24))) // 32 chars
	t.Setenv("BINNACLE_SESSION_KEY", raw)
	got, err := sessionKey()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != raw {
		t.Errorf("got %d bytes; the 32-character raw form is the better key", len(got))
	}
}

// Unset generates one, so a single-replica deployment starts without ceremony.
func TestSessionKey_UnsetGenerates(t *testing.T) {
	t.Setenv("BINNACLE_SESSION_KEY", "")
	got, err := sessionKey()
	if err != nil || len(got) < minSessionKey {
		t.Errorf("got %d bytes, %v", len(got), err)
	}
}

// A downloaded binary has to be able to say which one it is.
//
// The version reaches the page footer and the startup line, but both of those
// require the server to have found a kubeconfig and an identity provider
// first. Somebody checking what they just extracted from a release archive
// has neither, and that is exactly when the question is asked.
func TestVersionFlag(t *testing.T) {
	var sb strings.Builder
	if err := run([]string{"--version"}, &sb); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.TrimSpace(sb.String()); got != version {
		t.Errorf("got %q, want %q", got, version)
	}
}

// And it must not need a cluster to answer: --version returning before any
// credential is resolved is the whole point.
func TestVersionFlag_NeedsNoCluster(t *testing.T) {
	t.Setenv("KUBECONFIG", "/nonexistent/kubeconfig")
	var sb strings.Builder
	if err := run([]string{"--version"}, &sb); err != nil {
		t.Errorf("--version wanted a cluster: %v", err)
	}
}
