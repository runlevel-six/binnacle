// Package auth gates the fleet page.
//
// Two schemes exist and they are not interchangeable. [Open] is for a developer
// running binnacle on their own machine against their own kubeconfig, where the
// operating system's user account already is the authentication. [OIDC] is for
// the deployed service, where the process holds cluster-wide read credentials
// that nobody browsing to it necessarily has.
//
// That difference is the reason this is an interface with two implementations
// rather than a boolean: choosing wrong should look like choosing wrong, not
// like forgetting a flag. See [RequireOIDCOffLoopback] for the guard that keeps
// the open scheme from reaching a network anyone else is on.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	sessionCookie = "binnacle_session"
	stateCookie   = "binnacle_state"
	sessionTTL    = 8 * time.Hour
)

// Open lets every request through.
type Open struct{}

// Middleware passes the request straight to h.
func (Open) Middleware(h http.Handler) http.Handler { return h }

// Routes registers nothing.
func (Open) Routes(*http.ServeMux) {}

// Describe names the scheme.
func (Open) Describe() string { return "no authentication" }

// RequireOIDCOffLoopback reports an error when an unauthenticated binnacle is
// about to listen somewhere other than loopback.
//
// Binnacle reads every cluster in the fleet with credentials of its own. On a
// laptop that is the operator's own access and no worse than the terminal
// dashboard; on a network address it is an open, unauthenticated window into
// every cluster the service account can see. The failure has to be at startup,
// because the version of this mistake that gets noticed later is the one
// somebody else finds first.
func RequireOIDCOffLoopback(addr string, authenticated bool) error {
	if authenticated {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parse listen address %q: %w", addr, err)
	}
	switch host {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return nil
	case "":
		// An empty host means every interface, which is the widest possible
		// binding and the one most likely to be a mistake.
		return fmt.Errorf("refusing to serve %s with no authentication: %s binds every interface, "+
			"which would expose every cluster in the fleet to anyone who can reach it. "+
			"Configure --oidc-issuer, or bind 127.0.0.1 for local use", addr, addr)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("refusing to serve %s with no authentication: binnacle reads every cluster "+
		"in the fleet with its own credentials, so an unauthenticated listener off loopback is an "+
		"open window into all of them. Configure --oidc-issuer, or bind 127.0.0.1 for local use", addr)
}

// OIDCConfig describes the identity provider.
type OIDCConfig struct {
	// Issuer is the provider's base URL, e.g. a Keycloak realm.
	Issuer string
	// ClientID and ClientSecret identify binnacle to the provider.
	ClientID     string
	ClientSecret string
	// RedirectURL is binnacle's own callback, as the browser reaches it. It
	// must match what is registered with the provider exactly.
	RedirectURL string
	// Scopes beyond "openid". Empty adds profile and email, which is what the
	// footer needs to name who is looking.
	Scopes []string
	// SessionKey signs the session cookie. Supply a stable value: a generated
	// one invalidates every session on restart, and with more than one replica
	// it produces a login loop as requests land on different pods.
	SessionKey []byte
	// Secure marks the cookies secure. Leave true unless testing over plain
	// HTTP on loopback.
	Secure bool
}

// OIDC authenticates against an OpenID Connect provider using the authorization
// code flow, and remembers the result in a signed cookie.
type OIDC struct {
	cfg      OIDCConfig
	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Config
}

// NewOIDC contacts the provider's discovery endpoint and builds the flow.
//
// Discovery happens here, at startup, rather than on the first login: a
// misconfigured issuer should stop the process, not greet the first person who
// tries to sign in.
func NewOIDC(ctx context.Context, cfg OIDCConfig) (*OIDC, error) {
	switch {
	case cfg.Issuer == "":
		return nil, fmt.Errorf("oidc: issuer is required")
	case cfg.ClientID == "":
		return nil, fmt.Errorf("oidc: client id is required")
	case cfg.RedirectURL == "":
		return nil, fmt.Errorf("oidc: redirect url is required")
	case len(cfg.SessionKey) == 0:
		return nil, fmt.Errorf("oidc: session key is required")
	}

	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: discover %s: %w", cfg.Issuer, err)
	}
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	return &OIDC{
		cfg:      cfg,
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  cfg.RedirectURL,
			Scopes:       scopes,
		},
	}, nil
}

// Describe names the scheme.
func (a *OIDC) Describe() string { return "signed in via " + a.cfg.Issuer }

// Routes registers the login, callback and logout endpoints.
func (a *OIDC) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", a.handleLogin)
	mux.HandleFunc("GET /auth/callback", a.handleCallback)
	mux.HandleFunc("GET /auth/logout", a.handleLogout)
}

// Middleware sends an unauthenticated browser to the provider.
//
// Only navigations are redirected. An expired session on the event stream gets
// 401, because redirecting an EventSource to a login page hands the browser a
// chunk of HTML it will try to parse as events — the page then looks live and
// updates never arrive, which is precisely the silent staleness this whole
// design is trying to avoid.
func (a *OIDC) Middleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.session(r); ok {
			h.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Accept") == "text/event-stream" {
			http.Error(w, "session expired", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/auth/login?next="+safeNext(r.URL.RequestURI()), http.StatusFound)
	})
}

func (a *OIDC) handleLogin(w http.ResponseWriter, r *http.Request) {
	nonce, err := randomString()
	if err != nil {
		http.Error(w, "could not start login", http.StatusInternalServerError)
		return
	}
	// The state cookie carries where to return to, so the redirect target
	// survives the round trip without being trusted from the query string on
	// the way back.
	state := nonce + ":" + safeNext(r.URL.Query().Get("next"))
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: state, Path: "/", MaxAge: 600,
		HttpOnly: true, Secure: a.cfg.Secure, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, a.oauth.AuthCodeURL(state), http.StatusFound)
}

func (a *OIDC) handleCallback(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(stateCookie)
	if err != nil {
		http.Error(w, "login expired, try again", http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(r.URL.Query().Get("state"))) != 1 {
		http.Error(w, "login state did not match", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Path: "/", MaxAge: -1})

	token, err := a.oauth.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "could not complete login", http.StatusBadGateway)
		return
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "provider returned no id token", http.StatusBadGateway)
		return
	}
	idToken, err := a.verifier.Verify(r.Context(), raw)
	if err != nil {
		http.Error(w, "could not verify identity", http.StatusForbidden)
		return
	}
	var claims struct {
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
	}
	_ = idToken.Claims(&claims)
	who := claims.Email
	if who == "" {
		who = claims.PreferredUsername
	}
	if who == "" {
		who = idToken.Subject
	}

	a.setSession(w, who)
	next := "/"
	if _, after, found := strings.Cut(cookie.Value, ":"); found && after != "" {
		next = after
	}
	http.Redirect(w, r, next, http.StatusFound)
}

func (a *OIDC) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/", http.StatusFound)
}

// setSession writes the signed cookie. The value is "who|expiry|signature";
// nothing secret is in it, so the signature is what makes it a session rather
// than a suggestion.
func (a *OIDC) setSession(w http.ResponseWriter, who string) {
	payload := who + "|" + strconv.FormatInt(time.Now().Add(sessionTTL).Unix(), 10)
	value := payload + "|" + a.sign(payload)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: encode(value),
		Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: a.cfg.Secure, SameSite: http.SameSiteLaxMode,
	})
}

// session returns who the request is, and whether the cookie is valid and
// unexpired.
func (a *OIDC) session(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil {
		return "", false
	}
	who, rest, ok := strings.Cut(string(decoded), "|")
	if !ok {
		return "", false
	}
	expiry, sig, ok := strings.Cut(rest, "|")
	if !ok {
		return "", false
	}
	if !hmac.Equal([]byte(sig), []byte(a.sign(who+"|"+expiry))) {
		return "", false
	}
	unix, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil || time.Now().After(time.Unix(unix, 0)) {
		return "", false
	}
	return who, true
}

func (a *OIDC) sign(payload string) string {
	mac := hmac.New(sha256.New, a.cfg.SessionKey)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// safeNext keeps a redirect target on this site.
//
// An open redirect through a login endpoint is the classic way a phishing link
// borrows a trusted hostname, so anything that is not a plain rooted path is
// discarded rather than sanitised.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}

// encode wraps a cookie value so that its separators survive transit.
func encode(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func randomString() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// NewSessionKey returns a random signing key, for a deployment that has not
// been given a stable one.
func NewSessionKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}
