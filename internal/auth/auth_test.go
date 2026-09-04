package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// The guard is the one thing standing between a convenience default and an
// unauthenticated window into every cluster in the fleet, so it is tested for
// what it refuses, not only for what it allows.
func TestRequireOIDCOffLoopback(t *testing.T) {
	for name, tc := range map[string]struct {
		addr          string
		authenticated bool
		wantErr       bool
	}{
		"loopback by name":      {"localhost:8080", false, false},
		"loopback by v4":        {"127.0.0.1:8080", false, false},
		"loopback by v6":        {"[::1]:8080", false, false},
		"other loopback v4":     {"127.0.0.53:8080", false, false},
		"all interfaces":        {":8080", false, true},
		"routable address":      {"10.0.0.5:8080", false, true},
		"hostname":              {"binnacle.example:8080", false, true},
		"authenticated is fine": {"0.0.0.0:8080", true, false},
	} {
		err := RequireOIDCOffLoopback(tc.addr, tc.authenticated, false)
		if tc.wantErr && err == nil {
			t.Errorf("%s: expected a refusal for %s", name, tc.addr)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("%s: unexpected refusal for %s: %v", name, tc.addr, err)
		}
	}
}

// The refusal has to say what to do about it. An error a hurried operator
// cannot act on gets worked around rather than fixed.
func TestRequireOIDCOffLoopback_MessageIsActionable(t *testing.T) {
	err := RequireOIDCOffLoopback("0.0.0.0:8080", false, false)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"--oidc-issuer", "127.0.0.1", "--allow-unauthenticated"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message does not mention %q: %s", want, err)
		}
	}
}

// A login endpoint that will redirect anywhere is how a phishing link borrows a
// trusted hostname.
func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"":                      "/",
		"/":                     "/",
		"/cluster/tenant-01":    "/cluster/tenant-01",
		"//evil.example/":       "/",
		"https://evil.example/": "/",
		"http://evil.example/":  "/",
		"javascript:alert(1)":   "/",
		"/path?q=1#frag":        "/path?q=1#frag",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q want %q", in, got, want)
		}
	}
}

func testOIDC(t *testing.T) *OIDC {
	t.Helper()
	key, err := NewSessionKey()
	if err != nil {
		t.Fatal(err)
	}
	return &OIDC{cfg: OIDCConfig{SessionKey: key}}
}

// A session survives the round trip, and nothing else does.
func TestSession_RoundTrip(t *testing.T) {
	a := testOIDC(t)
	rec := httptest.NewRecorder()
	a.setSession(rec, "someone@example.com", nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	who, ok := a.session(req)
	if !ok {
		t.Fatal("valid session rejected")
	}
	if who.Who != "someone@example.com" {
		t.Errorf("got %q", who.Who)
	}
}

// The cookie is not secret, so the signature is the only thing making it a
// session rather than a suggestion. An edited one must not be honored.
func TestSession_TamperedCookieRejected(t *testing.T) {
	a := testOIDC(t)
	rec := httptest.NewRecorder()
	a.setSession(rec, "someone@example.com", nil)
	cookie := rec.Result().Cookies()[0]

	// Re-sign with a different key: the payload is well-formed, the signature
	// is not ours.
	other := testOIDC(t)
	forged := httptest.NewRecorder()
	other.setSession(forged, "admin@example.com", nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(forged.Result().Cookies()[0])
	if who, ok := a.session(req); ok {
		t.Errorf("accepted a cookie signed with another key, as %q", who.Who)
	}

	// A truncated value is not a panic either.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value[:len(cookie.Value)/2]})
	if _, ok := a.session(req); ok {
		t.Error("accepted a truncated cookie")
	}
}

func TestSession_ExpiredRejected(t *testing.T) {
	a := testOIDC(t)
	// Sign a payload that is already in the past, the way a cookie kept past
	// its TTL would look.
	payload := "someone@example.com||1000000000"
	value := payload + "|" + a.sign(payload)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: encode(value)})
	if _, ok := a.session(req); ok {
		t.Error("accepted an expired session")
	}
}

// An expired EventSource must get a status the browser can act on, not a login
// page it will try to parse as events — a page that looks live while receiving
// nothing is the failure this design exists to prevent.
func TestMiddleware_EventStreamGets401NotRedirect(t *testing.T) {
	a := testOIDC(t)
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("event stream: got %d want 401", rec.Code)
	}

	// A navigation still gets sent to the provider.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusFound {
		t.Errorf("navigation: got %d want 302", rec.Code)
	}
}

// Sanity: a fresh session is not already expired.
func TestSessionTTL(t *testing.T) {
	if sessionTTL < time.Hour {
		t.Errorf("sessionTTL = %s, suspiciously short", sessionTTL)
	}
}

// The override exists, and it is a separate claim from being authenticated:
// one says who is looking, the other says nobody checked.
func TestRequireOIDCOffLoopback_ExplicitOverride(t *testing.T) {
	if err := RequireOIDCOffLoopback("0.0.0.0:8080", false, true); err != nil {
		t.Errorf("the override was refused: %v", err)
	}
	// And it is genuinely required — the same address without it is still a
	// refusal, so the flag cannot be arrived at by accident.
	if err := RequireOIDCOffLoopback("0.0.0.0:8080", false, false); err == nil {
		t.Error("an unauthenticated listener off loopback was allowed without the flag")
	}
}

// An unauthenticated deployment says so on the page. "no authentication" in a
// footer is honest on a laptop and easy to miss on a shared address.
func TestUnauthenticated_WarnsVisibly(t *testing.T) {
	u := Unauthenticated{}
	if u.Warning() == "" {
		t.Error("no banner text")
	}
	if !strings.Contains(u.Describe(), "NO AUTHENTICATION") {
		t.Errorf("describe = %q; it should be hard to skim past", u.Describe())
	}
	// Open, which is only ever loopback, has nobody to warn.
	if (Open{}).Warning() != "" {
		t.Error("loopback should need no banner")
	}
}

// stubProvider is an OpenID Connect discovery document and nothing else, which
// is all NewOIDC reads at startup.
func stubProvider(t *testing.T) string {
	t.Helper()
	var url string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                url,
			"authorization_endpoint":                url + "/auth",
			"token_endpoint":                        url + "/token",
			"jwks_uri":                              url + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	}))
	t.Cleanup(srv.Close)
	url = srv.URL
	return url
}

// The CLI's scopes are published separately from the browser's, because
// offline_access belongs on one flow and not the other: a browser session
// should end with the SSO session, and a terminal has to survive the gaps.
func TestClientAuth_PublishesCLIScopesSeparately(t *testing.T) {
	issuer := stubProvider(t)
	a, err := NewOIDC(context.Background(), OIDCConfig{
		Issuer:      issuer,
		ClientID:    "web",
		CLIClientID: "cli",
		RedirectURL: "https://binnacle.example/auth/callback",
		Scopes:      []string{"openid", "profile", "email"},
		CLIScopes:   []string{"openid", "profile", "email", "offline_access"},
		SessionKey:  []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}

	if info := a.ClientAuth(); !slices.Contains(info.Scopes, "offline_access") {
		t.Errorf("the terminal was not told to request offline_access: %v", info.Scopes)
	}
	if slices.Contains(a.oauth.Scopes, "offline_access") {
		t.Error("offline_access leaked into the browser's flow")
	}
}

// Unset, the terminal is told what the browser uses — the behavior before this
// existed, and what a deployment that wants no offline tokens keeps.
func TestClientAuth_CLIScopesDefaultToTheBrowsers(t *testing.T) {
	issuer := stubProvider(t)
	a, err := NewOIDC(context.Background(), OIDCConfig{
		Issuer:      issuer,
		ClientID:    "web",
		RedirectURL: "https://binnacle.example/auth/callback",
		SessionKey:  []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	if got := a.ClientAuth().Scopes; !slices.Equal(got, a.oauth.Scopes) {
		t.Errorf("cli scopes %v, want the browser's %v", got, a.oauth.Scopes)
	}
}

// Without openid the provider returns no ID token, and the ID token is the
// whole credential. Better to fail at startup than at a device-code prompt.
func TestNewOIDC_RejectsCLIScopesWithoutOpenID(t *testing.T) {
	issuer := stubProvider(t)
	_, err := NewOIDC(context.Background(), OIDCConfig{
		Issuer:      issuer,
		ClientID:    "web",
		RedirectURL: "https://binnacle.example/auth/callback",
		CLIScopes:   []string{"profile", "offline_access"},
		SessionKey:  []byte("0123456789abcdef0123456789abcdef"),
	})
	if err == nil {
		t.Fatal("expected an error for cli scopes without openid")
	}
	if !strings.Contains(err.Error(), "openid") {
		t.Errorf("the error does not name the missing scope: %v", err)
	}
}
