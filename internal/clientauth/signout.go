package clientauth

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// SignOut discards the saved credential for a server and, where the provider
// supports it, revokes the refresh token behind it.
//
// Both halves matter, and the second is the one that is easy to skip. Deleting
// the local file ends the session on *this* machine; the provider still holds
// an active session that anyone with a copy of the token can keep renewing. For
// an ID token good for five minutes that distinction is academic. For an
// offline token — which is the point of [Options] carrying a scope that asks
// for one — it is the difference between signing out and tidying up.
//
// It returns what actually happened, for a caller to print. A provider with no
// revocation endpoint is not an error: the local credential is still gone, and
// the operator is told where the remaining session lives.
func SignOut(ctx context.Context, base string, opts Options) (string, error) {
	base = strings.TrimRight(base, "/")

	e, ok := opts.Store.Lookup(base)
	if !ok {
		return fmt.Sprintf("no saved credential for %s", base), nil
	}

	// Forget first. If revocation fails, the operator has still signed out
	// here, which is what they asked for; leaving the credential on disk
	// because a network call failed would be the wrong way round.
	if err := opts.Store.Forget(base); err != nil {
		return "", fmt.Errorf("removing the saved credential: %w", err)
	}
	local := fmt.Sprintf("signed out of %s; the saved credential is gone from %s",
		base, opts.Store.Path())

	switch {
	case e.Refresh == "":
		return local + ".\nThere was no refresh token to revoke.", nil
	case e.Issuer == "":
		return local + ".\nThe entry named no provider, so nothing was revoked.", nil
	}

	endpoint, err := revocationEndpoint(ctx, e.Issuer, opts)
	switch {
	case err != nil:
		return local + fmt.Sprintf(".\nCould not ask %s how to revoke the session: %v\n"+
			"The provider still holds it; end it in the provider's console.", e.Issuer, err), nil
	case endpoint == "":
		return local + fmt.Sprintf(".\n%s publishes no revocation endpoint, so the session still exists there.\n"+
			"End it in the provider's console if it needs to be gone everywhere.", e.Issuer), nil
	}

	if err := revoke(ctx, endpoint, e.Refresh, e.ClientID, opts); err != nil {
		return local + fmt.Sprintf(".\nRevoking the session at %s failed: %v\n"+
			"The provider still holds it; end it in the provider's console.", e.Issuer, err), nil
	}
	return local + ",\nand the session was revoked at " + e.Issuer + ".", nil
}

// revocationEndpoint reads the provider's discovery document for RFC 7009's
// endpoint, which go-oidc does not model directly.
func revocationEndpoint(ctx context.Context, issuer string, opts Options) (string, error) {
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, opts.client()), issuer)
	if err != nil {
		return "", err
	}
	var doc struct {
		RevocationEndpoint string `json:"revocation_endpoint"`
	}
	if err := provider.Claims(&doc); err != nil {
		return "", err
	}
	return doc.RevocationEndpoint, nil
}

// revoke posts a refresh token to the provider's revocation endpoint.
//
// No client secret: the terminal client is public, which is what makes it
// usable from a laptop in the first place. RFC 7009 has the client identify
// itself with client_id alone in that case.
func revoke(ctx context.Context, endpoint, token, clientID string, opts Options) error {
	form := url.Values{
		"token":           {token},
		"token_type_hint": {"refresh_token"},
		"client_id":       {clientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := opts.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	// RFC 7009: a successful revocation, and an unknown token, are both 200.
	// A client cannot tell them apart and should not try.
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the provider returned %s", resp.Status)
	}
	return nil
}
