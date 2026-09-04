package clientauth

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/runlevel-six/binnacle/internal/auth"
)

// Session holds the credential for one server and renews it as it ages.
//
// It exists because a sextant window outlives its token by orders of
// magnitude. An operator leaves the fleet up for a working day; the ID token it
// signs in with is good for five minutes in a stock Keycloak. Resolving the
// credential once at startup — which is what sextant used to do — means the
// screen stops updating a few minutes in and the only remedy is a restart.
//
// Renewal is deliberately non-interactive. A device sign-in prints a URL and a
// code, and there is nowhere to print them once the alt-screen UI owns the
// terminal. So when renewal fails, [Session.Token] returns the credential it
// already has: the server rejects it, and the fleet screen says the session has
// expired and to restart — which is exactly right, because a restart is the
// context where signing in *can* be interactive.
type Session struct {
	base string
	info auth.ClientAuthInfo
	opts Options

	// mu guards cred. Token is called from the UI goroutine and from the SSE
	// subscriber, and a renewal must happen once rather than once per caller.
	mu   sync.Mutex
	cred Credential
}

// Start obtains the initial credential, signing in interactively if the caller
// allowed it, and returns a Session that keeps it current.
func Start(ctx context.Context, base string, opts Options) (*Session, error) {
	base = strings.TrimRight(base, "/")

	info, err := Discover(ctx, base, opts.client())
	if err != nil {
		return nil, err
	}
	cred, err := fetchWith(ctx, base, info, opts)
	if err != nil {
		return nil, err
	}
	return &Session{base: base, info: info, opts: opts, cred: cred}, nil
}

// Credential returns the credential obtained at startup, for a diagnostic line
// naming where it came from.
func (s *Session) Credential() Credential {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cred
}

// Token returns the credential to present now, renewing it first if it has
// aged out. It never blocks on a human and never returns an error: the empty
// string means this server wants no credential.
func (s *Session) Token() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cred.Token == "" || fresh(s.cred.Token, s.opts.now()) {
		return s.cred.Token
	}
	if cred, ok := s.renew(); ok {
		s.cred = cred
	}
	return s.cred.Token
}

// renew tries to obtain a newer credential without asking anybody anything.
//
// The bound is its own, not the caller's: renewal happens on the path of a
// request the UI is waiting for, and inheriting a canceled or long-lived
// context from one of them would either cancel the renewal or let it hang the
// frame.
func (s *Session) renew() (Credential, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), renewTimeout)
	defer cancel()

	// Prompt is dropped for the duration: nothing here may print to a terminal
	// the dashboard is drawing on, and nothing here may wait for a browser.
	opts := s.opts
	opts.Prompt = nil

	switch {
	case opts.Token != "":
		// Supplied on the command line or in the environment. There is nothing
		// to renew it with, and second-guessing an explicit credential is not
		// this package's job.
		return Credential{}, false

	case len(opts.TokenCommand) > 0:
		// Re-run it, the way kubectl re-runs a credential plugin. A command
		// that produced a token an hour ago is the thing that knows how to
		// produce the next one.
		tok, err := runTokenCommand(ctx, opts.TokenCommand)
		if err != nil {
			return Credential{}, false
		}
		return Credential{Token: tok, Source: "token command"}, true
	}

	e, ok := opts.Store.Get(s.base, s.info.Issuer, s.info.ClientID)
	switch {
	case !ok, e.Refresh == "":
		return Credential{}, false
	case expired(e, opts, opts.now()):
		// The ceiling has been reached. Leave the entry for the next start to
		// clear, which is where an operator is present to sign in again.
		return Credential{}, false
	}

	cred, err := refresh(ctx, s.base, s.info, opts, e)
	if err != nil {
		return Credential{}, false
	}
	return cred, true
}

// renewTimeout bounds one renewal attempt. Generous enough for a provider
// having a slow moment, short enough that a frame does not hang on it.
const renewTimeout = 20 * time.Second
