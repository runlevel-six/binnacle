package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The tests below run against a real OpenID Connect provider — a small one
// served by httptest, with a real RSA key and a real JWKS. Verification is the
// whole security value of the bearer path, so a fake that returns "yes" would
// test nothing: the audience check in particular is one line away from
// accepting any token the issuer ever minted for anything.
//
// The tokens are signed by hand rather than with a JWT library, to avoid
// promoting one of go-oidc's indirect dependencies into a direct one for the
// sake of test code. RS256 is a SHA-256 hash and a PKCS#1 v1.5 signature over
// two base64url segments, which is less code than the import would be.

type idp struct {
	issuer string
	key    *rsa.PrivateKey
}

func newIDP(t *testing.T) *idp {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &idp{key: key}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, map[string]any{
			"issuer":                                p.issuer,
			"authorization_endpoint":                p.issuer + "/auth",
			"token_endpoint":                        p.issuer + "/token",
			"jwks_uri":                              p.issuer + "/jwks",
			"device_authorization_endpoint":         p.issuer + "/auth/device",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, map[string]any{"keys": []any{map[string]any{
			"kty": "RSA",
			"kid": "test",
			"alg": "RS256",
			"use": "sig",
			"n":   b64(p.key.N.Bytes()),
			"e":   b64(big.NewInt(int64(p.key.E)).Bytes()),
		}}})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p.issuer = srv.URL
	return p
}

// mint signs an ID token for one audience.
func (p *idp) mint(t *testing.T, audience string, claims map[string]any) string {
	t.Helper()
	now := time.Now()
	payload := map[string]any{
		"iss": p.issuer,
		"aud": audience,
		"sub": "subject-1",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
	for k, v := range claims {
		payload[k] = v
	}

	head := b64(mustJSON(t, map[string]any{"alg": "RS256", "typ": "JWT", "kid": "test"}))
	body := b64(mustJSON(t, payload))
	signing := head + "." + body

	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + b64(sig)
}

func (p *idp) oidc(t *testing.T, clientID, cliClientID string) *OIDC {
	t.Helper()
	key, err := NewSessionKey()
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewOIDC(context.Background(), OIDCConfig{
		Issuer:      p.issuer,
		ClientID:    clientID,
		CLIClientID: cliClientID,
		RedirectURL: "https://binnacle.example/auth/callback",
		SessionKey:  key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// A valid token is accepted, and the identity it carries is the one the
// scoping layer will see.
func TestBearer_ValidTokenAuthenticates(t *testing.T) {
	p := newIDP(t)
	a := p.oidc(t, "binnacle", "binnacle-cli")

	var got Identity
	h := a.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, _ = IdentityFromContext(r.Context())
	}))

	tok := p.mint(t, "binnacle-cli", map[string]any{
		"email":  "operator@example.com",
		"groups": []string{"platform-admins"},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, body %q", rec.Code, rec.Body)
	}
	if got.Who != "operator@example.com" {
		t.Errorf("who = %q, want operator@example.com", got.Who)
	}
	if len(got.Groups) != 1 || got.Groups[0] != "platform-admins" {
		t.Errorf("groups = %v, want [platform-admins]", got.Groups)
	}
}

// The load-bearing one: a CLI must never be redirected. A 302 to a login page
// hands it HTML with a 200 on the end of it.
func TestBearer_BadTokenGets401NotRedirect(t *testing.T) {
	p := newIDP(t)
	a := p.oidc(t, "binnacle", "binnacle-cli")
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, tc := range []struct{ name, token string }{
		{"garbage", "not-a-token"},
		{"wrong audience", p.mint(t, "some-other-app", nil)},
		{"expired", p.mintExpired(t, "binnacle")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("got %d want 401 (a redirect would be a login page with a 200)", rec.Code)
			}
			if loc := rec.Header().Get("Location"); loc != "" {
				t.Errorf("redirected to %q", loc)
			}
		})
	}
}

// The second audience is real: a CLI token is rejected until the deployment
// says that client is expected.
func TestBearer_CLIAudienceMustBeConfigured(t *testing.T) {
	p := newIDP(t)
	tok := p.mint(t, "binnacle-cli", nil)

	serve := func(a *OIDC) int {
		h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := serve(p.oidc(t, "binnacle", "")); code != http.StatusUnauthorized {
		t.Errorf("unconfigured CLI audience: got %d want 401", code)
	}
	if code := serve(p.oidc(t, "binnacle", "binnacle-cli")); code != http.StatusOK {
		t.Errorf("configured CLI audience: got %d want 200", code)
	}
}

// A browser with no Authorization header behaves exactly as it did before.
func TestBearer_AbsentHeaderLeavesTheBrowserFlowAlone(t *testing.T) {
	p := newIDP(t)
	a := p.oidc(t, "binnacle", "binnacle-cli")
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusFound {
		t.Errorf("navigation: got %d want 302 to the provider", rec.Code)
	}
}

// ClientAuth is what makes the client conditional: no provider, no credential.
func TestClientAuth_OpenRequiresNothing(t *testing.T) {
	if got := (Open{}).ClientAuth(); got.Required {
		t.Errorf("Open reported auth required: %+v", got)
	}
	// Unauthenticated embeds Open, so it answers the same way — a deployment
	// that turned auth off should not make a terminal client hunt for a token.
	if got := (Unauthenticated{}).ClientAuth(); got.Required {
		t.Errorf("Unauthenticated reported auth required: %+v", got)
	}
}

func TestClientAuth_OIDCNamesTheCLIClient(t *testing.T) {
	p := newIDP(t)

	got := p.oidc(t, "binnacle", "binnacle-cli").ClientAuth()
	if !got.Required {
		t.Error("OIDC did not report auth required")
	}
	if got.Issuer != p.issuer {
		t.Errorf("issuer = %q, want %q", got.Issuer, p.issuer)
	}
	if got.ClientID != "binnacle-cli" {
		t.Errorf("client id = %q, want binnacle-cli", got.ClientID)
	}

	// Falling back keeps a one-client deployment working with no extra flag.
	if got := p.oidc(t, "binnacle", "").ClientAuth(); got.ClientID != "binnacle" {
		t.Errorf("fallback client id = %q, want binnacle", got.ClientID)
	}
}

func TestBearerToken_Parsing(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   string
		ok     bool
	}{
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true}, // RFC 7235: the scheme is case-insensitive
		{"BEARER abc", "abc", true},
		{"Bearer   abc  ", "abc", true},
		{"Basic abc", "", false},
		{"Bearer", "", false},
		{"Bearer ", "", false},
		{"", "", false},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if tc.header != "" {
			r.Header.Set("Authorization", tc.header)
		}
		got, ok := bearerToken(r)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%q: got (%q, %v) want (%q, %v)", tc.header, got, ok, tc.want, tc.ok)
		}
	}
}

// mintExpired signs a token that was already stale when it was issued.
func (p *idp) mintExpired(t *testing.T, audience string) string {
	t.Helper()
	past := time.Now().Add(-2 * time.Hour)
	return p.mint(t, audience, map[string]any{
		"iat": past.Unix(),
		"exp": past.Add(time.Minute).Unix(),
	})
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
