package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// This file adds the credential a terminal can carry.
//
// The browser flow signs in and gets a cookie. A CLI cannot: it has no cookie
// jar, no redirect to follow, and frequently no browser on the same machine.
// So it presents an OpenID Connect **ID token** as a bearer credential, which
// the server verifies with the same issuer, the same signature check and the
// same claims the cookie flow already uses.
//
// An ID token rather than an access token, deliberately. It is what kubectl
// does, so the audience for this tool already has the habit and the tooling.
// More practically it is what works with no identity-provider configuration:
// an ID token carries the client id as its audience and the groups claim the
// scoping already reads, where an access token generally needs a mapper for
// each before it carries either. A tool that demands two mappers before it
// runs is a tool that only works in the environment it was written in.
//
// The trade to know: an ID token is an authentication artifact, and purists
// reserve bearer credentials for access tokens. It is accepted here because it
// is short-lived, audience-restricted to this deployment, and verified in
// full — issuer, signature, audience and expiry. If a deployment would rather
// present access tokens, the audience list below is the seam to widen.

// ClientAuthInfo tells a client what it must do to authenticate, before it has.
//
// Required is the field that matters most: it is what lets `sextant --server`
// work against a binnacle nobody has put an identity provider in front of.
// A client sends no credential when nothing asks for one.
type ClientAuthInfo struct {
	// Required reports whether this deployment authenticates at all.
	Required bool `json:"auth_required"`
	// Issuer is the OpenID Connect provider to obtain a token from.
	Issuer string `json:"issuer,omitempty"`
	// ClientID is the client a terminal should identify as, which may differ
	// from the one the browser uses: a CLI cannot hold a client secret, so a
	// deployment usually registers a second, public client for it.
	ClientID string `json:"client_id,omitempty"`
	// Scopes the client should request. A hint, not a contract — a provider may
	// attach claims by client scope without being asked.
	Scopes []string `json:"scopes,omitempty"`
}

// ClientAuth reports that no credential is needed.
func (Open) ClientAuth() ClientAuthInfo { return ClientAuthInfo{} }

// ClientAuth describes how a terminal client should authenticate.
func (a *OIDC) ClientAuth() ClientAuthInfo {
	return ClientAuthInfo{
		Required: true,
		Issuer:   a.cfg.Issuer,
		ClientID: a.cliClientID(),
		Scopes:   a.cliScopes,
	}
}

// cliClientID is the client a terminal should use: the dedicated one when the
// deployment has registered it, otherwise the browser's. The fallback is what
// keeps a single-client deployment working without extra configuration.
func (a *OIDC) cliClientID() string {
	if a.cfg.CLIClientID != "" {
		return a.cfg.CLIClientID
	}
	return a.cfg.ClientID
}

// bearerToken pulls the credential out of an Authorization header.
//
// The scheme is compared case-insensitively because RFC 7235 says it is
// case-insensitive, and a client that sends "bearer" is not wrong.
func bearerToken(r *http.Request) (string, bool) {
	const scheme = "bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(scheme) || !strings.EqualFold(h[:len(scheme)], scheme) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(scheme):])
	return tok, tok != ""
}

// verifyBearer checks a token against every audience this deployment accepts
// and returns the identity it carries.
//
// One verifier per audience rather than one verifier with the audience check
// disabled and a manual comparison afterwards: skipping the library's check
// puts the security-relevant step in this file, where a later edit can drop it
// without failing a build. Two verifications of a short token cost nothing.
func (a *OIDC) verifyBearer(ctx context.Context, raw string) (Identity, error) {
	if len(a.bearerVerifiers) == 0 {
		return Identity{}, errors.New("no accepted audiences configured")
	}
	var lastErr error
	for _, v := range a.bearerVerifiers {
		idToken, err := v.Verify(ctx, raw)
		if err != nil {
			lastErr = err
			continue
		}
		return identityFrom(idToken), nil
	}
	return Identity{}, lastErr
}

// identityFrom reads the signed-in user out of a verified token.
//
// Shared with the browser callback on purpose. Two front ends deriving a user
// and their groups by separate code is how they come to disagree about who is
// looking, which is the same failure the shared health verdicts exist to
// prevent one layer down.
func identityFrom(idToken *oidc.IDToken) Identity {
	var claims struct {
		Email             string   `json:"email"`
		PreferredUsername string   `json:"preferred_username"`
		Groups            []string `json:"groups"`
	}
	_ = idToken.Claims(&claims)

	who := claims.Email
	if who == "" {
		who = claims.PreferredUsername
	}
	if who == "" {
		who = idToken.Subject
	}
	return Identity{Who: who, Groups: claims.Groups}
}

// withIdentity attaches an identity to a request context.
func withIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}
