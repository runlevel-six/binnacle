package clientauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// idp is a stub identity provider: a discovery document, a token endpoint that
// honors refresh_token, and a revocation endpoint. Every field a test needs to
// look at is recorded.
type idp struct {
	URL string
	// Refreshes counts token-endpoint calls with grant_type=refresh_token.
	Refreshes int
	// Revoked is the token last posted to the revocation endpoint.
	Revoked string
	// IDToken is what the next refresh returns; set it per test.
	IDToken string
	// Fail makes the token endpoint reject a refresh, as a provider does when
	// the session behind it has ended.
	Fail bool
	// NoRevocation drops the revocation endpoint from discovery.
	NoRevocation bool
}

func newIDP(t *testing.T) *idp {
	t.Helper()
	p := &idp{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		doc := map[string]any{
			"issuer":                                p.URL,
			"authorization_endpoint":                p.URL + "/auth",
			"token_endpoint":                        p.URL + "/token",
			"device_authorization_endpoint":         p.URL + "/device",
			"jwks_uri":                              p.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		if !p.NoRevocation {
			doc["revocation_endpoint"] = p.URL + "/revoke"
		}
		_ = json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") == "refresh_token" {
			p.Refreshes++
		}
		if p.Fail {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access",
			"refresh_token": "refresh-2",
			"id_token":      p.IDToken,
			"token_type":    "Bearer",
			"expires_in":    300,
		})
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		p.Revoked = r.Form.Get("token")
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p.URL = srv.URL
	return p
}

// session builds a Session against a stub binnacle and a stub provider, with a
// cache already holding one entry.
func session(t *testing.T, p *idp, e entry, opts Options) (*Session, *Cache, string) {
	t.Helper()
	base := binnacleServing(t,
		`{"auth_required":true,"issuer":"`+p.URL+`","client_id":"cli"}`, http.StatusOK)

	cache := NewCache(filepath.Join(t.TempDir(), "tokens.json"))
	e.Issuer, e.ClientID = p.URL, "cli"
	if err := cache.Put(base, e); err != nil {
		t.Fatal(err)
	}
	opts.Store = cache

	s, err := Start(context.Background(), base, opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return s, cache, base
}

// The defect this exists for: sextant resolved its credential once at startup
// and held the string for the life of the process, so a dashboard left open
// died when the ID token did — five minutes, in a stock Keycloak.
func TestSession_RenewsAnAgedToken(t *testing.T) {
	p := newIDP(t)
	p.IDToken = jwt(t, time.Now().Add(time.Hour))

	stale := jwt(t, time.Now().Add(-time.Minute))
	s, _, _ := session(t, p, entry{
		Token: stale, Refresh: "refresh-1", Saved: time.Now().Add(-10 * time.Minute),
	}, Options{})

	got := s.Token()
	if got == stale {
		t.Fatal("Token returned the expired credential without renewing it")
	}
	if got != p.IDToken {
		t.Errorf("Token returned %q, want the refreshed ID token", got)
	}
	if p.Refreshes == 0 {
		t.Error("no refresh reached the provider")
	}

	// A second call is served from memory: renewal happens when the credential
	// ages, not on every request the dashboard makes.
	before := p.Refreshes
	_ = s.Token()
	if p.Refreshes != before {
		t.Errorf("a fresh credential was refreshed again: %d then %d", before, p.Refreshes)
	}
}

// When renewal fails there is nothing useful to return but the credential we
// have: the server rejects it, and the fleet screen says the session expired
// and to restart — which is the context where signing in can be interactive.
func TestSession_KeepsTheOldTokenWhenRenewalFails(t *testing.T) {
	p := newIDP(t)
	p.Fail = true

	// Good when the session starts, aged out ten minutes later. This is the
	// shape of the real case: sextant is already running when the token dies.
	clock := time.Now()
	good := jwt(t, clock.Add(2*time.Minute))
	s, _, _ := session(t, p, entry{
		Token: good, Refresh: "refresh-1", Saved: clock,
	}, Options{Now: func() time.Time { return clock }})

	clock = clock.Add(10 * time.Minute)
	if got := s.Token(); got != good {
		t.Errorf("Token returned %q, want the credential it already had", got)
	}
}

// The ceiling is measured from the sign-in, so renewals cannot extend it. This
// is the bound that does not depend on the provider enforcing one — an
// offline_access token in a stock Keycloak realm is good for thirty days.
func TestSession_StopsRenewingPastTheCeiling(t *testing.T) {
	p := newIDP(t)
	p.IDToken = jwt(t, time.Now().Add(time.Hour))

	// Signed in this morning and still running: inside the ceiling when the
	// session starts, past it ten minutes later.
	clock := time.Now()
	good := jwt(t, clock.Add(2*time.Minute))
	s, _, _ := session(t, p, entry{
		Token:       good,
		Refresh:     "refresh-1",
		Saved:       clock,
		FirstSignIn: clock.Add(-11*time.Hour - 55*time.Minute),
	}, Options{MaxSession: 12 * time.Hour, Now: func() time.Time { return clock }})

	clock = clock.Add(10 * time.Minute)
	if got := s.Token(); got != good {
		t.Errorf("Token renewed a session past the ceiling: %q", got)
	}
	if p.Refreshes != 0 {
		t.Error("a session past the ceiling still asked the provider for a token")
	}
}

// And the next start clears it and asks for a sign-in, rather than leaving a
// credential on disk that this sextant will not use.
func TestSignIn_PastTheCeilingForgetsAndRequiresSignIn(t *testing.T) {
	p := newIDP(t)
	base := binnacleServing(t,
		`{"auth_required":true,"issuer":"`+p.URL+`","client_id":"cli"}`, http.StatusOK)

	cache := NewCache(filepath.Join(t.TempDir(), "tokens.json"))
	if err := cache.Put(base, entry{
		// Still valid as a token, but the session behind it is a day old.
		Token: jwt(t, time.Now().Add(time.Hour)), Refresh: "refresh-1",
		Issuer: p.URL, ClientID: "cli", FirstSignIn: time.Now().Add(-24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// No Prompt: non-interactive, so it reports rather than blocking.
	_, err := Fetch(context.Background(), base, Options{Store: cache, MaxSession: 12 * time.Hour})
	if err == nil {
		t.Fatal("expected a sign-in to be required past the ceiling")
	}
	if _, ok := cache.Lookup(base); ok {
		t.Error("the expired session was left on disk")
	}
}

// A refresh must carry the original sign-in forward. A ceiling that its own
// renewals reset is not a ceiling.
func TestRefresh_CarriesTheSignInTimeForward(t *testing.T) {
	p := newIDP(t)
	p.IDToken = jwt(t, time.Now().Add(time.Hour))

	signedIn := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	s, cache, base := session(t, p, entry{
		Token: jwt(t, time.Now().Add(-time.Minute)), Refresh: "refresh-1",
		Saved: time.Now().Add(-time.Hour), FirstSignIn: signedIn,
	}, Options{})

	_ = s.Token()

	e, ok := cache.Lookup(base)
	if !ok {
		t.Fatal("the renewed credential was not cached")
	}
	if !e.FirstSignIn.Equal(signedIn) {
		t.Errorf("FirstSignIn moved to %v; want it left at %v", e.FirstSignIn, signedIn)
	}
	if e.Refresh != "refresh-2" {
		t.Errorf("the new refresh token was not stored: %q", e.Refresh)
	}
}

// An entry from an older sextant carries no sign-in time. Discarding it would
// sign everyone out on upgrade for no security gain, since the token in it
// carries its own expiry.
func TestSignIn_EntryWithNoTimestampsIsAdopted(t *testing.T) {
	p := newIDP(t)
	base := binnacleServing(t,
		`{"auth_required":true,"issuer":"`+p.URL+`","client_id":"cli"}`, http.StatusOK)

	cache := NewCache(filepath.Join(t.TempDir(), "tokens.json"))
	good := jwt(t, time.Now().Add(time.Hour))
	if err := cache.Put(base, entry{Token: good, Issuer: p.URL, ClientID: "cli"}); err != nil {
		t.Fatal(err)
	}

	cred, err := Fetch(context.Background(), base, Options{Store: cache, MaxSession: 12 * time.Hour})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred.Token != good {
		t.Error("an entry with no timestamps was discarded rather than adopted")
	}
}

// Signing out has to end the session at the provider too. Deleting the local
// file alone leaves an offline session anyone with a copy can keep renewing.
func TestSignOut_RevokesAndForgets(t *testing.T) {
	p := newIDP(t)
	base := binnacleServing(t, `{"auth_required":true}`, http.StatusOK)

	cache := NewCache(filepath.Join(t.TempDir(), "tokens.json"))
	if err := cache.Put(base, entry{
		Token: jwt(t, time.Now().Add(time.Hour)), Refresh: "refresh-1",
		Issuer: p.URL, ClientID: "cli",
	}); err != nil {
		t.Fatal(err)
	}

	msg, err := SignOut(context.Background(), base, Options{Store: cache})
	if err != nil {
		t.Fatalf("SignOut: %v", err)
	}
	if p.Revoked != "refresh-1" {
		t.Errorf("revoked %q, want the refresh token", p.Revoked)
	}
	if _, ok := cache.Lookup(base); ok {
		t.Error("the credential is still on disk after signing out")
	}
	if !strings.Contains(msg, "revoked") {
		t.Errorf("the message does not report the revocation: %q", msg)
	}
}

// A provider with no revocation endpoint is not a failure — but the operator
// has to be told the session outlived the local credential.
func TestSignOut_SaysWhenTheProviderCannotRevoke(t *testing.T) {
	p := newIDP(t)
	p.NoRevocation = true
	base := binnacleServing(t, `{"auth_required":true}`, http.StatusOK)

	cache := NewCache(filepath.Join(t.TempDir(), "tokens.json"))
	if err := cache.Put(base, entry{
		Token: "t", Refresh: "refresh-1", Issuer: p.URL, ClientID: "cli",
	}); err != nil {
		t.Fatal(err)
	}

	msg, err := SignOut(context.Background(), base, Options{Store: cache})
	if err != nil {
		t.Fatalf("SignOut: %v", err)
	}
	if _, ok := cache.Lookup(base); ok {
		t.Error("the credential is still on disk after signing out")
	}
	if !strings.Contains(msg, "still exists") {
		t.Errorf("the message does not say the provider's session survives: %q", msg)
	}
}
