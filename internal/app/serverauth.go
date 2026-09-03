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

// ResolveServerToken obtains the credential to present to a binnacle server.
//
// It runs before the UI starts, deliberately: signing in may need to say
// something to the operator and wait for them, and the alternate screen is no
// place to do either. By the time Bubble Tea takes the terminal, the question
// of who we are has already been settled.
//
// An empty token is a valid answer, not a failure — a binnacle with no
// identity provider in front of it asks for nothing, and sextant sends
// nothing. See internal/clientauth for the full order of resolution.
func ResolveServerToken(ctx context.Context, serverURL string, cfg config.ServerConfig, prompt io.Writer) (string, error) {
	opts := clientauth.Options{
		Token:        cfg.Token,
		TokenCommand: cfg.TokenCommand,
	}

	// A cache that cannot be opened is not worth failing over: the fallback is
	// signing in again next time, which is what no cache means anyway.
	if store, err := clientauth.UserCache(); err == nil {
		opts.Store = store
	}

	// Only offer to sign in when somebody is there to do it. The same test
	// the context picker uses: a redirected stderr or a detached stdin means
	// this is a script, and a script should get a clear error rather than a
	// process that sits waiting for a browser nobody will open.
	if interactive() {
		opts.Prompt = func(msg string) { fmt.Fprintln(prompt, msg) }
	}

	cred, err := clientauth.Fetch(ctx, serverURL, opts)
	if err != nil {
		return "", err
	}
	return cred.Token, nil
}

// interactive reports whether there is an operator at the terminal.
func interactive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}
