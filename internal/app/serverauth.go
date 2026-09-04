package app

import (
	"context"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"

	"github.com/runlevel-six/binnacle/internal/clientauth"
	"github.com/runlevel-six/binnacle/internal/config"
)

// ResolveServerSession obtains the credential to present to a binnacle server,
// and the session that keeps it current.
//
// The first sign-in runs before the UI starts, deliberately: it may need to say
// something to the operator and wait for them, and the alternate screen is no
// place to do either. By the time Bubble Tea takes the terminal, the question
// of who we are has already been settled — and from then on the session renews
// silently, because an ID token lasts minutes and a dashboard lasts a day.
//
// An empty credential is a valid answer, not a failure — a binnacle with no
// identity provider in front of it asks for nothing, and sextant sends
// nothing. See internal/clientauth for the full order of resolution.
func ResolveServerSession(ctx context.Context, serverURL string, cfg config.ServerConfig, prompt io.Writer) (*clientauth.Session, error) {
	opts := clientauth.Options{
		Token:        cfg.Token,
		TokenCommand: cfg.TokenCommand,
	}

	// A cache that cannot be opened is not worth failing over: the fallback is
	// signing in again next time, which is what no cache means anyway.
	if store, err := clientauth.UserCache(); err == nil {
		opts.Store = store
	}
	opts.MaxSession = cfg.MaxSession

	// Only offer to sign in when somebody is there to do it. The same test
	// the context picker uses: a redirected stderr or a detached stdin means
	// this is a script, and a script should get a clear error rather than a
	// process that sits waiting for a browser nobody will open.
	if interactive() {
		opts.Prompt = func(msg string) { fmt.Fprintln(prompt, msg) }
	}

	return clientauth.Start(ctx, serverURL, opts)
}

// interactive reports whether there is an operator at the terminal.
func interactive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

// SignOutServer discards the saved credential for a server and revokes the
// session behind it where the provider allows. It writes what happened to out.
//
// Separate from the sign-in path because it must work when the server does not:
// signing out is most wanted when something is wrong, and a sign-out that first
// asks the server for permission would be unavailable exactly then.
func SignOutServer(ctx context.Context, serverURL string, out io.Writer) error {
	store, err := clientauth.UserCache()
	if err != nil {
		return fmt.Errorf("opening the token cache: %w", err)
	}
	msg, err := clientauth.SignOut(ctx, serverURL, clientauth.Options{Store: store})
	if err != nil {
		return err
	}
	fmt.Fprintln(out, msg)
	return nil
}
