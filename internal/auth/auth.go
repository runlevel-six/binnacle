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
	"slices"
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

// Identity is the signed-in user, available in request context when the
// authenticator is OIDC. The [Open] and [Unauthenticated] schemes do not
// provide one, which is how the server knows to skip scoping.
type Identity struct {
	Who    string
	Groups []string
}

type identityKey struct{}

// IdentityFromContext returns the signed-in user's identity, or false when
// the authenticator does not provide one.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// Open lets every request through.
type Open struct{}

// Middleware passes the request straight to h.
func (Open) Middleware(h http.Handler) http.Handler { return h }

// Routes registers nothing.
func (Open) Routes(*http.ServeMux) {}

// Describe names the scheme.
func (Open) Describe() string { return "no authentication" }

// Warning is empty: on loopback there is nobody else to warn.
func (Open) Warning() string { return "" }

// RequireOIDCOffLoopback reports an error when an unauthenticated binnacle is
// about to listen somewhere other than loopback.
//
// Binnacle reads every cluster in the fleet with credentials of its own. On a
// laptop that is the operator's own access and no worse than the terminal
// dashboard; on a network address it is an open, unauthenticated window into
// every cluster the service account can see. The failure has to be at startup,
// because the version of this mistake that gets noticed later is the one
// somebody else finds first.
//
// allowUnauthenticated is the deliberate override, for a network where the
// operator has decided reachability is itself the control. It is a separate
// parameter rather than a variant of authenticated because the two are not the
// same claim: one says who is looking, the other says nobody checked. Callers
// should make it awkward to set and impossible to set by accident — see
// [Unauthenticated], which is what the page then tells its readers.
func RequireOIDCOffLoopback(addr string, authenticated, allowUnauthenticated bool) error {
	if authenticated || allowUnauthenticated {
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
		"open window into all of them. Configure --oidc-issuer, bind 127.0.0.1 for local use, or "+
		"pass --allow-unauthenticated if reachability really is the only control you want", addr)
}

// Unauthenticated is [Open] serving somewhere other than loopback, having been
// told to.
//
// It exists to be visible. Open describes itself as "no authentication", which
// is honest on a laptop and easy to overlook on a shared address, so this says
// something a reader cannot mistake for a normal state — the footer carries it,
// and so does a banner across the top of every page.
type Unauthenticated struct{ Open }

// Describe names the scheme.
func (Unauthenticated) Describe() string {
	return "NO AUTHENTICATION — anyone who can reach this can see every cluster"
}

// Warning is the banner text, or empty for a scheme that needs no banner.
func (Unauthenticated) Warning() string {
	return "This binnacle has no authentication. Anyone who can reach this address " +
		"sees every cluster it can read."
}

// OIDCConfig describes the identity provider.
type OIDCConfig struct {
	// Issuer is the provider's base URL, e.g. a Keycloak realm.
	Issuer string
	// ClientID and ClientSecret identify binnacle to the provider.
	ClientID     string
	ClientSecret string
	// CLIClientID is a second client id whose tokens are also accepted on
	// Authorization: Bearer, for a terminal client registered separately.
	//
	// A CLI cannot hold a client secret, so it needs a public client of its
	// own, and a public client issues tokens with its own audience. Empty
	// accepts only ClientID, which is what a deployment with one client wants.
	CLIClientID string
	// RedirectURL is binnacle's own callback, as the browser reaches it. It
	// must match what is registered with the provider exactly.
	RedirectURL string
	// Scopes beyond "openid". Empty adds profile and email, which is what the
	// footer needs to name who is looking.
	//
	// These are the *browser's* scopes. A terminal client is told about
	// [OIDCConfig.CLIScopes] instead.
	Scopes []string
	// CLIScopes are the scopes a terminal client is told to request. Empty
	// means the same as [OIDCConfig.Scopes].
	//
	// Separate from Scopes because the two flows want different lifetimes. A
	// browser session should follow the provider's SSO session and end when it
	// does. A terminal client is started and stopped all day and has to survive
	// the gaps, which is what "offline_access" is for — and adding that scope to
	// the browser's flow would mint a long-lived credential for a session that
	// has no use for one.
	//
	// The list must include "openid": the credential a terminal presents is the
	// ID token, and without that scope the provider returns none.
	CLIScopes []string
	// SessionKey signs the session cookie. Supply a stable value: a generated
	// one invalidates every session on restart, and with more than one replica
	// it produces a login loop as requests land on different pods.
	SessionKey []byte
	// Secure marks the cookies secure. Leave true unless testing over plain
	// HTTP on loopback.
	Secure bool
	// GroupScopes maps OIDC group names to the namespaces a member of that
	// group may see. "*" means all namespaces. When nil, scoping is off and
	// every authenticated user sees the entire fleet — the historical
	// behavior, and what a single-team deployment wants.
	GroupScopes map[string][]string
}

// OIDC authenticates against an OpenID Connect provider using the authorization
// code flow, and remembers the result in a signed cookie.
type OIDC struct {
	cfg      OIDCConfig
	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Config
	// bearerVerifiers is one verifier per audience accepted on an
	// Authorization header — the browser's client and, when configured, the
	// terminal client's. See bearer.go.
	bearerVerifiers []*oidc.IDTokenVerifier
	// cliScopes is what a terminal client is told to request, already
	// defaulted.
	cliScopes []string
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
	cliScopes := cfg.CLIScopes
	if len(cliScopes) == 0 {
		cliScopes = scopes
	}
	// Checked here rather than left to fail at sign-in: without openid the
	// provider returns no ID token, and the ID token is the whole credential.
	// The failure would otherwise land on an operator at a device-code prompt,
	// a long way from the flag that caused it.
	if !slices.Contains(cliScopes, oidc.ScopeOpenID) {
		return nil, fmt.Errorf("oidc: cli scopes %v must include %q, "+
			"since the credential a terminal presents is the ID token", cliScopes, oidc.ScopeOpenID)
	}
	// The browser's audience first, so a single-client deployment verifies on
	// the first try and the error a misconfigured CLI sees names the client it
	// was actually meant to use.
	audiences := []string{cfg.ClientID}
	if cfg.CLIClientID != "" && cfg.CLIClientID != cfg.ClientID {
		audiences = append(audiences, cfg.CLIClientID)
	}
	bearerVerifiers := make([]*oidc.IDTokenVerifier, 0, len(audiences))
	for _, aud := range audiences {
		bearerVerifiers = append(bearerVerifiers, provider.Verifier(&oidc.Config{ClientID: aud}))
	}

	return &OIDC{
		cfg:             cfg,
		verifier:        provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		bearerVerifiers: bearerVerifiers,
		cliScopes:       cliScopes,
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

// Warning is empty: there is nothing unusual about being asked to sign in.
func (a *OIDC) Warning() string { return "" }

// Routes registers the login, callback and logout endpoints.
func (a *OIDC) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", a.handleLogin)
	mux.HandleFunc("GET /auth/callback", a.handleCallback)
	mux.HandleFunc("GET /auth/logout", a.handleLogout)
}

// Middleware sends an unauthenticated browser to the provider.
//
// Only navigations are redirected — see [isNavigation] for what that excludes
// and why. Everything else gets a 401, because a client that followed a 302
// would receive a login page with a 200 on it and have to guess that HTML was
// not the fleet.
func (a *OIDC) Middleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if raw, present := bearerToken(r); present {
			id, err := a.verifyBearer(r.Context(), raw)
			if err != nil {
				http.Error(w, "invalid bearer token: "+err.Error(), http.StatusUnauthorized)
				return
			}
			h.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), id)))
			return
		}
		id, ok := a.session(r)
		if ok {
			h.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), id)))
			return
		}
		if !isNavigation(r) {
			http.Error(w, "not authenticated", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/auth/login?next="+safeNext(r.URL.RequestURI()), http.StatusFound)
	})
}

// isNavigation reports whether a request is a browser following a link, which
// is the only kind of request a redirect to the provider can help.
//
// Two kinds are not, and they fail the same way if redirected — with a login
// page carrying a 200, which the caller has to guess is not what it asked for.
//
// An EventSource is one: it names its own content type, and HTML it parses as
// events leaves a page that looks live while no update ever arrives. That is
// the silent staleness this whole design exists to prevent.
//
// Anything under /api/ is the other. It is the same reason a request carrying a
// bearer token is never redirected, except that it must hold when the caller
// sends no credential at all — a script, a monitor, or a sextant pointed at a
// server it has not signed in to yet. Only /api/v1/authinfo is exempt, and it
// is exempt by living outside the protected mux entirely, so a client holding
// nothing can still ask how to authenticate.
func isNavigation(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		return false
	}
	// Matched per media type rather than against the whole header: a browser
	// sends the bare type, but a list or a q-value means the same thing and
	// nothing in the contract promises the short form.
	for _, accepted := range strings.Split(r.Header.Get("Accept"), ",") {
		mediaType, _, _ := strings.Cut(accepted, ";")
		if strings.TrimSpace(mediaType) == "text/event-stream" {
			return false
		}
	}
	return true
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
	id := identityFrom(idToken)

	a.setSession(w, id.Who, id.Groups)
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

// setSession writes the signed cookie. The value is "who|groups|expiry|signature";
// nothing secret is in it, so the signature is what makes it a session rather
// than a suggestion. Groups is a comma-separated list, empty when the provider
// does not issue a groups claim.
func (a *OIDC) setSession(w http.ResponseWriter, who string, groups []string) {
	groupsCSV := strings.Join(groups, ",")
	payload := who + "|" + groupsCSV + "|" + strconv.FormatInt(time.Now().Add(sessionTTL).Unix(), 10)
	value := payload + "|" + a.sign(payload)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: encode(value),
		Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: a.cfg.Secure, SameSite: http.SameSiteLaxMode,
	})
}

// session returns who the request is, and whether the cookie is valid and
// unexpired.
func (a *OIDC) session(r *http.Request) (Identity, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return Identity{}, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil {
		return Identity{}, false
	}
	parts := strings.SplitN(string(decoded), "|", 4)
	if len(parts) != 4 {
		return Identity{}, false
	}
	who, groupsCSV, expiry, sig := parts[0], parts[1], parts[2], parts[3]
	if !hmac.Equal([]byte(sig), []byte(a.sign(who+"|"+groupsCSV+"|"+expiry))) {
		return Identity{}, false
	}
	unix, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil || time.Now().After(time.Unix(unix, 0)) {
		return Identity{}, false
	}
	var groups []string
	if groupsCSV != "" {
		groups = strings.Split(groupsCSV, ",")
	}
	return Identity{Who: who, Groups: groups}, true
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
// discarded rather than sanitized.
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
