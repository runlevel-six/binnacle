package clientauth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/runlevel-six/binnacle/internal/auth"
)

// signIn produces a token from the cache, a refresh, or an interactive device
// sign-in, in that order, and remembers whatever it ends up with.
func signIn(ctx context.Context, base string, info auth.ClientAuthInfo, opts Options) (Credential, error) {
	now := opts.now()

	if e, ok := opts.Store.Get(base, info.Issuer, info.ClientID); ok {
		switch {
		case expired(e, opts, now):
			// Past the ceiling. Drop it rather than leave a credential on disk
			// that this sextant will not use and something else might, and
			// fall through to an interactive sign-in.
			_ = opts.Store.Forget(base)
		case fresh(e.Token, now):
			return Credential{Token: e.Token, Source: "cached"}, nil
		case e.Refresh != "":
			cred, err := refresh(ctx, base, info, opts, e)
			if err == nil {
				return cred, nil
			}
			// A refresh token can be revoked or simply too old. That is an
			// ordinary end to a session, not a failure worth stopping for, so
			// fall through to signing in again.
		}
	}

	if opts.Prompt == nil {
		return Credential{}, fmt.Errorf(
			"%s requires authentication and there is no usable saved token; "+
				"run sextant interactively to sign in, or set SEXTANT_SERVER_TOKEN", base)
	}
	return deviceSignIn(ctx, base, info, opts)
}

// oauthConfig builds the client-side flow from what the server published.
//
// Endpoints come from the provider's own discovery document rather than being
// derived from the issuer, because only the provider knows where they are —
// deriving them is how a client ends up working against one vendor.
func oauthConfig(ctx context.Context, info auth.ClientAuthInfo, opts Options) (oauth2.Config, error) {
	ctx = oidc.ClientContext(ctx, opts.client())
	provider, err := oidc.NewProvider(ctx, info.Issuer)
	if err != nil {
		return oauth2.Config{}, fmt.Errorf("discovering %s: %w", info.Issuer, err)
	}
	scopes := info.Scopes
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	return oauth2.Config{
		ClientID: info.ClientID,
		Endpoint: provider.Endpoint(),
		Scopes:   scopes,
	}, nil
}

// deviceSignIn runs the OAuth 2.0 device authorization grant.
//
// Chosen as the interactive default over a browser redirect because sextant is
// frequently run where no browser can be opened — over SSH, in a container, on
// a jump host. The device grant only needs a browser *somewhere*, which is a
// much weaker assumption than one on the same machine.
func deviceSignIn(ctx context.Context, base string, info auth.ClientAuthInfo, opts Options) (Credential, error) {
	cfg, err := oauthConfig(ctx, info, opts)
	if err != nil {
		return Credential{}, err
	}
	if cfg.Endpoint.DeviceAuthURL == "" {
		return Credential{}, fmt.Errorf(
			"%s does not offer the device authorization grant, so sextant cannot sign in on its own; "+
				"set SEXTANT_SERVER_TOKEN or configure a token command", info.Issuer)
	}

	resp, err := cfg.DeviceAuth(ctx)
	if err != nil {
		return Credential{}, fmt.Errorf("starting device sign-in with %s: %w", info.Issuer, err)
	}
	opts.Prompt(promptText(resp))

	// A provider that names no expiry would otherwise be polled until the
	// process is killed, which is a poor thing to leave on an unattended
	// terminal. The provider's own deadline wins whenever it gives one.
	if resp.Expiry.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, pollGrace)
		defer cancel()
	}

	tok, err := cfg.DeviceAccessToken(ctx, resp)
	if err != nil {
		return Credential{}, fmt.Errorf("waiting for sign-in: %w", err)
	}
	return store(base, info, opts, tok, "device sign-in", opts.now())
}

// maxSession is the ceiling in force, defaulted.
//
// A caller sets MaxSession explicitly to change it; only a negative value turns
// it off. Zero meaning "no ceiling" would make the safe configuration the one
// you have to remember to write.
func maxSession(opts Options) time.Duration {
	if opts.MaxSession == 0 {
		return DefaultMaxSession
	}
	return opts.MaxSession
}

// expired reports whether a cached session has outlived the ceiling.
//
// Measured from the sign-in, so refreshing does not extend it. This is the
// bound that does not depend on the provider being configured to enforce one:
// an offline_access token in a stock Keycloak realm is good for thirty days,
// and nothing but this stops sextant from using it for thirty days.
func expired(e entry, opts Options, now time.Time) bool {
	max := maxSession(opts)
	if max <= 0 {
		return false
	}
	started, ok := e.started()
	if !ok {
		// Nothing to measure from. Discarding it would sign the operator out
		// on upgrade for no security gain: the token in it carries its own
		// exp, and the write that follows stamps a start time, so the ceiling
		// applies from here rather than never.
		return false
	}
	return now.Sub(started) > max
}

// refresh exchanges a refresh token for a new one without user interaction.
func refresh(ctx context.Context, base string, info auth.ClientAuthInfo, opts Options, e entry) (Credential, error) {
	cfg, err := oauthConfig(ctx, info, opts)
	if err != nil {
		return Credential{}, err
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, opts.client())
	tok, err := cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: e.Refresh}).Token()
	if err != nil {
		return Credential{}, err
	}
	// The session began when the operator signed in, and carrying that forward
	// is what keeps the ceiling from being reset by its own renewals.
	started, ok := e.started()
	if !ok {
		started = opts.now()
	}
	return store(base, info, opts, tok, "refreshed", started)
}

// store pulls the ID token out of a token response and remembers it.
//
// The ID token, not the access token, is the credential: binnacle verifies it
// with the same checks it applies to a browser session. See internal/auth for
// why that is the choice that needs no provider configuration.
func store(base string, info auth.ClientAuthInfo, opts Options, tok *oauth2.Token,
	source string, firstSignIn time.Time,
) (Credential, error) {
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return Credential{}, errors.New(
			"the provider returned no id_token; the client may not have the openid scope")
	}
	if err := opts.Store.Put(base, entry{
		Token:       raw,
		Refresh:     tok.RefreshToken,
		Issuer:      info.Issuer,
		ClientID:    info.ClientID,
		Saved:       opts.now(),
		FirstSignIn: firstSignIn,
	}); err != nil && opts.Prompt != nil {
		// Failing to cache costs another sign-in next time; it does not cost
		// this one, so it is not worth refusing a token we already hold. The
		// nil check matters: a renewal runs with no Prompt, because the
		// dashboard owns the terminal by then.
		opts.Prompt(fmt.Sprintf("warning: could not save the token: %v", err))
	}
	return Credential{Token: raw, Source: source}, nil
}

// promptText is what an operator reads while signing in.
//
// The complete URL goes first when the provider offers one, because it is the
// version that needs no typing. The code is repeated on its own line anyway:
// a terminal that wrapped or truncated the URL still leaves something usable.
func promptText(r *oauth2.DeviceAuthResponse) string {
	if r.VerificationURIComplete != "" {
		return fmt.Sprintf("Sign in to continue:\n  %s\n\nIf that link does not open, visit %s and enter code %s\n",
			r.VerificationURIComplete, r.VerificationURI, r.UserCode)
	}
	return fmt.Sprintf("Sign in to continue:\n  visit %s and enter code %s\n",
		r.VerificationURI, r.UserCode)
}

// pollGrace bounds how long a sign-in waits when the provider names no
// expiry, so an unattended terminal does not sit forever.
const pollGrace = 10 * time.Minute
