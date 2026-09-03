package clientauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// jwt builds an unsigned token with the given expiry. Nothing here verifies a
// signature — the server does that — so the tests only need the shape.
func jwt(t *testing.T, exp time.Time) string {
	t.Helper()
	b := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return b(map[string]any{"alg": "none"}) + "." +
		b(map[string]any{"exp": exp.Unix(), "sub": "someone"}) + ".sig"
}

// binnacleServing returns a stub binnacle publishing the given authinfo.
func binnacleServing(t *testing.T, body string, code int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/authinfo" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// The rule the user set: a binnacle with no identity provider must still be
// usable from the terminal. Nothing else in this package may run first.
func TestFetch_NoAuthRequiredSendsNothing(t *testing.T) {
	base := binnacleServing(t, `{"auth_required":false}`, http.StatusOK)

	got, err := Fetch(context.Background(), base, Options{
		// Deliberately hostile: a token command that would fail loudly, and no
		// prompt. Neither may be reached.
		TokenCommand: []string{"false"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Token != "" {
		t.Errorf("sent a credential to a server that wants none: %q", got.Token)
	}
}

// An explicit token beats everything, including a cache and a token command.
func TestFetch_SuppliedTokenWins(t *testing.T) {
	base := binnacleServing(t,
		`{"auth_required":true,"issuer":"https://idp.example","client_id":"cli"}`, http.StatusOK)

	got, err := Fetch(context.Background(), base, Options{
		Token:        "explicit",
		TokenCommand: []string{"false"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "explicit" {
		t.Errorf("got %q, want the supplied token", got.Token)
	}
}

func TestFetch_TokenCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	base := binnacleServing(t,
		`{"auth_required":true,"issuer":"https://idp.example","client_id":"cli"}`, http.StatusOK)

	got, err := Fetch(context.Background(), base, Options{
		TokenCommand: []string{"sh", "-c", "echo from-the-helper"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "from-the-helper" {
		t.Errorf("got %q, want from-the-helper", got.Token)
	}
}

// A failing helper must say why. An operator debugging their own script needs
// its stderr, not just a exit status.
func TestFetch_TokenCommandFailureCarriesStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	base := binnacleServing(t,
		`{"auth_required":true,"issuer":"https://idp.example","client_id":"cli"}`, http.StatusOK)

	_, err := Fetch(context.Background(), base, Options{
		TokenCommand: []string{"sh", "-c", "echo 'kubectl not found' >&2; exit 3"},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "kubectl not found") {
		t.Errorf("error lost the helper's stderr: %v", err)
	}
}

// A cached token that is still good is used without contacting the provider —
// the provider here does not exist, so reaching for it would fail the test.
func TestFetch_UsesFreshCachedToken(t *testing.T) {
	base := binnacleServing(t,
		`{"auth_required":true,"issuer":"https://idp.invalid","client_id":"cli"}`, http.StatusOK)

	cache := NewCache(filepath.Join(t.TempDir(), "tokens.json"))
	good := jwt(t, time.Now().Add(time.Hour))
	if err := cache.Put(base, entry{
		Token: good, Issuer: "https://idp.invalid", ClientID: "cli",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := Fetch(context.Background(), base, Options{Store: cache})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Token != good {
		t.Errorf("did not use the cached token")
	}
}

// A cache minted against a different provider must not be reused: a
// deployment repointed at a new issuer would otherwise present a credential
// the new one never issued, and the 401 would be baffling.
func TestCache_IgnoresEntryFromAnotherProvider(t *testing.T) {
	cache := NewCache(filepath.Join(t.TempDir(), "tokens.json"))
	if err := cache.Put("https://binnacle.example", entry{
		Token: "t", Issuer: "https://old.example", ClientID: "cli",
	}); err != nil {
		t.Fatal(err)
	}

	if _, ok := cache.Get("https://binnacle.example", "https://new.example", "cli"); ok {
		t.Error("reused a token minted by a different issuer")
	}
	if _, ok := cache.Get("https://binnacle.example", "https://old.example", "other"); ok {
		t.Error("reused a token minted for a different client")
	}
	if _, ok := cache.Get("https://binnacle.example", "https://old.example", "cli"); !ok {
		t.Error("did not return a matching entry")
	}
}

// The cache holds a bearer credential for every cluster in the fleet.
func TestCache_FileIsNotWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	path := filepath.Join(t.TempDir(), "tokens.json")
	if err := NewCache(path).Put("https://b.example", entry{Token: "secret"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("token cache is %o, want 600", perm)
	}
}

func TestCache_ForgetRemovesOneServer(t *testing.T) {
	cache := NewCache(filepath.Join(t.TempDir(), "tokens.json"))
	for _, s := range []string{"https://a.example", "https://b.example"} {
		if err := cache.Put(s, entry{Token: "t", Issuer: "i", ClientID: "c"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := cache.Forget("https://a.example"); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.Get("https://a.example", "i", "c"); ok {
		t.Error("forgotten server is still cached")
	}
	if _, ok := cache.Get("https://b.example", "i", "c"); !ok {
		t.Error("Forget removed the wrong server")
	}
}

// A nil cache is the no-persistence mode, and must not panic.
func TestCache_NilIsUsable(t *testing.T) {
	var c *Cache
	if _, ok := c.Get("s", "i", "cid"); ok {
		t.Error("nil cache returned an entry")
	}
	if err := c.Put("s", entry{}); err != nil {
		t.Errorf("nil cache Put: %v", err)
	}
	if err := c.Forget("s"); err != nil {
		t.Errorf("nil cache Forget: %v", err)
	}
}

// Without a prompt there is nobody to sign in, so say so rather than blocking
// a process that is not being watched.
func TestFetch_NonInteractiveSaysSoRatherThanHanging(t *testing.T) {
	base := binnacleServing(t,
		`{"auth_required":true,"issuer":"https://idp.invalid","client_id":"cli"}`, http.StatusOK)

	_, err := Fetch(context.Background(), base, Options{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "SEXTANT_SERVER_TOKEN") {
		t.Errorf("error should name a way forward, got: %v", err)
	}
}

// An older binnacle has no authinfo route. Reading 404 as "no authentication"
// would send nothing to a server that wanted something.
func TestDiscover_MissingRouteIsAnErrorNotAnAllow(t *testing.T) {
	base := binnacleServing(t, `nope`, http.StatusNotFound)

	_, err := Fetch(context.Background(), base, Options{})
	if err == nil {
		t.Fatal("a 404 was treated as no authentication required")
	}
	if !strings.Contains(err.Error(), "older") {
		t.Errorf("error should explain the version mismatch, got: %v", err)
	}
}

func TestFresh(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name  string
		token string
		want  bool
	}{
		{"valid for an hour", jwt(t, now.Add(time.Hour)), true},
		{"already expired", jwt(t, now.Add(-time.Minute)), false},
		{"expiring inside the margin", jwt(t, now.Add(30*time.Second)), false},
		{"unreadable, let the server decide", "not-a-jwt", true},
	} {
		if got := fresh(tc.token, now); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

// The prompt has to work when a terminal mangles the long URL, which is the
// case the separate code line exists for.
func TestPromptText_AlwaysCarriesTheCode(t *testing.T) {
	for _, complete := range []string{"", "https://idp.example/device?user_code=WDJB-MJHT"} {
		got := promptText(&oauth2.DeviceAuthResponse{
			VerificationURI:         "https://idp.example/device",
			VerificationURIComplete: complete,
			UserCode:                "WDJB-MJHT",
		})
		if !strings.Contains(got, "WDJB-MJHT") {
			t.Errorf("prompt omitted the user code (complete=%q): %s", complete, got)
		}
	}
}
