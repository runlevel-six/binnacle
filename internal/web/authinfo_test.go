package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/runlevel-six/binnacle/internal/auth"
)

// stubAuth is an authenticator that refuses everything, so a test can tell the
// difference between a route that is outside authentication and one that is
// merely being let through by auth.Open.
type stubAuth struct{ info auth.ClientAuthInfo }

func (stubAuth) Middleware(http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	})
}
func (stubAuth) Routes(*http.ServeMux)             {}
func (stubAuth) Describe() string                  { return "stub" }
func (stubAuth) Warning() string                   { return "" }
func (s stubAuth) ClientAuth() auth.ClientAuthInfo { return s.info }

// The endpoint has to be reachable by a client that holds no credential —
// asking how to authenticate is what it does before it can. If it ever slips
// behind the authenticator, a terminal client can never bootstrap.
func TestAuthInfo_IsReachableWithoutACredential(t *testing.T) {
	s, err := New(&fakeFleet{changed: make(chan struct{}, 1)}, stubAuth{
		info: auth.ClientAuthInfo{Required: true, Issuer: "https://idp.example/realms/x", ClientID: "binnacle-cli"},
	}, "test", "site-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()

	// Everything else this authenticator guards is refused...
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("the stub authenticator is not actually guarding: /api/v1/fleet gave %d", rec.Code)
	}

	// ...but this is answered.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/authinfo", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("authinfo gave %d, want 200", rec.Code)
	}

	var got auth.ClientAuthInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body, err)
	}
	if !got.Required || got.ClientID != "binnacle-cli" {
		t.Errorf("got %+v, want the CLI client and auth required", got)
	}
}

// With no provider configured the answer is "you need nothing", which is what
// keeps `sextant --server` working against a binnacle that has no identity
// provider in front of it.
func TestAuthInfo_SaysWhenNothingIsRequired(t *testing.T) {
	s, err := New(&fakeFleet{changed: make(chan struct{}, 1)}, auth.Open{}, "test", "site-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/authinfo", nil))

	var got auth.ClientAuthInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body, err)
	}
	if got.Required {
		t.Errorf("got %+v, want auth_required false", got)
	}
	if got.Issuer != "" || got.ClientID != "" {
		t.Errorf("an open deployment named a provider: %+v", got)
	}
}
