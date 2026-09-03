package web

import (
	"net/http"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/runlevel-six/binnacle/internal/auth"
	"github.com/runlevel-six/binnacle/internal/fleet"
)

// Scope is the set of namespaces a user may see. A zero-value Scope (IsAll
// returns true) allows everything — the default when no scoping is configured
// or when the authenticator does not provide identity.
type Scope struct {
	namespaces map[string]bool
	all        bool
}

// AllScope returns a Scope that allows everything.
func AllScope() Scope { return Scope{all: true} }

// IsAll reports whether this scope allows everything.
func (s Scope) IsAll() bool { return s.all }

// Allows reports whether the namespace is within this scope.
func (s Scope) Allows(namespace string) bool {
	if s.all {
		return true
	}
	return s.namespaces[namespace]
}

// scopeFor extracts the identity from the request and resolves the scope.
// When no identity is present (Open auth, Unauthenticated), the scope is all.
func (s *Server) scopeFor(r *http.Request) Scope {
	id, ok := auth.IdentityFromContext(r.Context())
	if !ok {
		return AllScope()
	}
	return ResolveScope(id.Groups, s.groupScopes)
}

// ResolveScope determines which namespaces the user can see, based on their
// OIDC groups and the group-to-namespace mapping.
//
// A nil or empty groupScopes means no scoping is configured: everyone sees
// everything. A user in a group mapped to "*" sees everything. A user in no
// mapped groups sees nothing — which is a configuration error worth making
// visible, rather than silently widening to everything.
func ResolveScope(groups []string, groupScopes map[string][]string) Scope {
	if len(groupScopes) == 0 {
		return AllScope()
	}
	ns := map[string]bool{}
	for _, g := range groups {
		patterns, ok := groupScopes[g]
		if !ok {
			continue
		}
		for _, p := range patterns {
			if p == "*" {
				return AllScope()
			}
			ns[p] = true
		}
	}
	return Scope{namespaces: ns}
}

// filterViews returns only the clusters whose namespace is within scope.
func filterViews(views []fleet.ClusterView, scope Scope) []fleet.ClusterView {
	if scope.IsAll() {
		return views
	}
	out := make([]fleet.ClusterView, 0, len(views))
	for _, v := range views {
		if scope.Allows(v.Namespace) {
			out = append(out, v)
		}
	}
	return out
}

// filterStorage returns only the storage clusters that have at least one
// reporting cluster in scope. The hardware inventory (Hosts, ByRole) is
// datacenter-wide and cannot be narrowed per-namespace; a storage cluster is
// either visible or hidden in full.
func filterStorage(storage fleet.Storage, scope Scope) fleet.Storage {
	if scope.IsAll() {
		return storage
	}
	out := make([]fleet.StorageCluster, 0, len(storage.Clusters))
	for _, sc := range storage.Clusters {
		for _, ref := range sc.ReportedBy {
			if scope.Allows(ref.Namespace) {
				out = append(out, sc)
				break
			}
		}
	}
	storage.Clusters = out
	return storage
}

// scopeFile is the YAML structure of --scope-file.
type scopeFile struct {
	Groups map[string][]string `yaml:"groups"`
}

// LoadGroupScopes reads a --scope-file. An empty path returns nil (scoping
// off). The file format is:
//
//	groups:
//	  platform-admins:
//	    - "*"
//	  site-a-ops:
//	    - site-a
//	    - site-a-infra
func LoadGroupScopes(path string) (map[string][]string, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sf scopeFile
	if err := yaml.Unmarshal(raw, &sf); err != nil {
		return nil, err
	}
	return sf.Groups, nil
}
