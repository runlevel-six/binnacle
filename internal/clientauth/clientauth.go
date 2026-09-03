// Package clientauth obtains the credential sextant presents to a binnacle
// server.
//
// The order below is the whole design, and it exists so that the common cases
// need no configuration and the awkward ones stay possible:
//
//  1. Ask the server whether it wants a credential at all. A binnacle with no
//     identity provider in front of it says no, and sextant sends nothing.
//     This is why --server works against a deployment that has not adopted
//     SSO, which is a deployment this project has to support.
//  2. An explicitly supplied token wins over everything. Someone who has
//     already solved this — a CI job, an operator with a token from their own
//     tooling — should not have their answer second-guessed.
//  3. A token command, run to produce one. This is the escape hatch that keeps
//     an identity provider we have never heard of from needing a fork.
//  4. A cached token from a previous sign-in, refreshed if it has aged out.
//  5. Failing all that, sign in interactively with the device authorization
//     grant.
//
// Nothing here verifies the token. The server does that, and a client that
// checked a signature it had no way to trust would only be pretending.
package clientauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/runlevel-six/binnacle/internal/auth"
)

// Options configures how a credential is obtained.
type Options struct {
	// Token, when set, is used as-is. Flag, environment and config file all
	// land here, and any of them means the caller has already decided.
	Token string
	// TokenCommand, when set, is run to produce a token on stdout. The first
	// element is the program; the rest are arguments. Modeled on kubectl's
	// credential plugins, and used the same way: whatever your provider
	// needs, wrapped in a script.
	TokenCommand []string
	// Prompt receives the sign-in instructions for an interactive flow. Nil
	// means non-interactive: device sign-in is skipped rather than blocking a
	// process nobody is watching.
	Prompt func(string)
	// Client is the HTTP client used against both binnacle and the identity
	// provider. Nil uses a client with a sensible timeout.
	Client *http.Client
	// Store persists tokens between runs. Nil disables caching, which is what
	// tests want and what a read-only home directory gets.
	Store *Cache
	// Now is the clock, for tests.
	Now func() time.Time
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) client() *http.Client {
	if o.Client != nil {
		return o.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Credential is what sextant sends, and what it knows about it.
type Credential struct {
	// Token is the bearer value. Empty means the server wants none.
	Token string
	// Source names where it came from, for a diagnostic line. Not for logic.
	Source string
}

// Fetch obtains a credential for the binnacle server at base.
func Fetch(ctx context.Context, base string, opts Options) (Credential, error) {
	base = strings.TrimRight(base, "/")

	info, err := Discover(ctx, base, opts.client())
	if err != nil {
		return Credential{}, err
	}
	if !info.Required {
		// Said plainly because the alternative — silently sending nothing and
		// failing later with a 401 — is the confusing version of this.
		return Credential{Source: "none required"}, nil
	}

	if opts.Token != "" {
		return Credential{Token: opts.Token, Source: "supplied"}, nil
	}

	if len(opts.TokenCommand) > 0 {
		tok, err := runTokenCommand(ctx, opts.TokenCommand)
		if err != nil {
			return Credential{}, err
		}
		return Credential{Token: tok, Source: "token command"}, nil
	}

	if info.Issuer == "" || info.ClientID == "" {
		return Credential{}, fmt.Errorf(
			"%s requires authentication but did not say which provider to use; "+
				"set a token with SEXTANT_SERVER_TOKEN or a token command", base)
	}

	return signIn(ctx, base, info, opts)
}

// Discover asks a binnacle server how, and whether, to authenticate.
//
// The endpoint is deliberately unauthenticated: a client calls it precisely
// because it holds no credential yet.
func Discover(ctx context.Context, base string, c *http.Client) (auth.ClientAuthInfo, error) {
	var info auth.ClientAuthInfo

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(base, "/")+"/api/v1/authinfo", nil)
	if err != nil {
		return info, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return info, fmt.Errorf("reaching %s: %w", base, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// An older binnacle has no such route. Treating 404 as "no
		// authentication" would be the dangerous reading — it would send
		// nothing to a server that wanted something and report the 401 as a
		// server fault — so say what happened instead.
		return info, fmt.Errorf("%s returned %s for /api/v1/authinfo; "+
			"a binnacle older than this sextant will not describe its authentication",
			base, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return info, fmt.Errorf("decoding authinfo from %s: %w", base, err)
	}
	return info, nil
}

// runTokenCommand executes a credential helper and returns its stdout.
func runTokenCommand(ctx context.Context, argv []string) (string, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stderr strings.Builder
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("token command %q: %w: %s", argv[0], err, msg)
		}
		return "", fmt.Errorf("token command %q: %w", argv[0], err)
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" {
		return "", fmt.Errorf("token command %q produced no token", argv[0])
	}
	return tok, nil
}

// expiry reads a JWT's exp claim without verifying anything.
//
// Only used to decide whether a cached token is worth presenting. A token this
// misreads is rejected by the server, which is the authority; the cost of
// getting it wrong is one wasted round trip, not a security hole. Hence no
// signature check and no dependency on a JWT library.
func expiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// fresh reports whether a token is worth presenting.
//
// The margin is what keeps a token that expires mid-request from being sent:
// a dashboard that had to sign in again because it saved a credential by two
// seconds is a worse trade than signing in slightly early.
func fresh(token string, now time.Time) bool {
	const margin = 60 * time.Second
	exp, ok := expiry(token)
	if !ok {
		// Unreadable expiry: let the server decide. Better than discarding a
		// token that may be perfectly good, since the failure is recoverable.
		return true
	}
	return now.Add(margin).Before(exp)
}
