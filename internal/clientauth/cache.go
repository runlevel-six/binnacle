package clientauth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Cache stores tokens between runs, so an operator signs in once a day rather
// than once a session.
//
// It is a file because that is what the credential outlives: sextant is a
// foreground process an operator starts and stops all day, and a token held
// only in memory would mean a device sign-in every time. Permissions are
// 0600 on the file and 0700 on the directory — the contents are a bearer
// credential for every cluster in the fleet.
type Cache struct {
	path string
}

// entry is one server's tokens.
type entry struct {
	// Token is the ID token presented to binnacle.
	Token string `json:"token"`
	// Refresh, when present, buys a new token without another sign-in.
	Refresh string `json:"refresh,omitempty"`
	// Issuer and ClientID are recorded so a deployment that repoints at a
	// different provider does not silently reuse a credential minted for the
	// old one.
	Issuer   string `json:"issuer,omitempty"`
	ClientID string `json:"client_id,omitempty"`
	// Saved is when this was written, for diagnostics only. Expiry is read
	// from the token itself, which is the authority.
	Saved time.Time `json:"saved"`
}

// UserCache returns the cache in the user's cache directory, creating it.
//
// An error here is not fatal to signing in — the caller carries on without a
// cache — so it is reported rather than swallowed, and callers treat a nil
// Cache as "do not persist".
func UserCache() (*Cache, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	dir = filepath.Join(dir, "sextant")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Cache{path: filepath.Join(dir, "tokens.json")}, nil
}

// NewCache returns a cache backed by an explicit path, for tests.
func NewCache(path string) *Cache { return &Cache{path: path} }

// Path reports where the cache lives, for a diagnostic line.
func (c *Cache) Path() string {
	if c == nil {
		return ""
	}
	return c.path
}

func (c *Cache) load() map[string]entry {
	if c == nil {
		return nil
	}
	b, err := os.ReadFile(c.path)
	if err != nil {
		return nil
	}
	var m map[string]entry
	if err := json.Unmarshal(b, &m); err != nil {
		// A corrupt cache is not worth an error: the fallback is to sign in
		// again, which is exactly what an empty cache does.
		return nil
	}
	return m
}

// Get returns the stored entry for a server, if it was minted by the same
// provider and client the server currently names.
func (c *Cache) Get(server, issuer, clientID string) (entry, bool) {
	e, ok := c.load()[server]
	if !ok {
		return entry{}, false
	}
	if e.Issuer != issuer || e.ClientID != clientID {
		return entry{}, false
	}
	return e, true
}

// Put stores an entry, replacing any previous one for that server.
func (c *Cache) Put(server string, e entry) error {
	if c == nil {
		return nil
	}
	m := c.load()
	if m == nil {
		m = map[string]entry{}
	}
	m[server] = e

	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// Written through a temporary file so an interrupted write cannot leave a
	// half-file that the next run reads as corrupt and discards.
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("saving token cache: %w", err)
	}
	return nil
}

// Forget drops a server's entry, for a sign-out.
func (c *Cache) Forget(server string) error {
	if c == nil {
		return nil
	}
	m := c.load()
	if _, ok := m[server]; !ok {
		return nil
	}
	delete(m, server)
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, b, 0o600)
}
