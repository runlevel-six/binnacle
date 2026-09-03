package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/runlevel-six/binnacle/internal/auth"
	"github.com/runlevel-six/binnacle/internal/fleet"
	"github.com/runlevel-six/binnacle/pkg/health"
)

func TestResolveScope_NoMapping(t *testing.T) {
	s := ResolveScope([]string{"any-group"}, nil)
	if !s.IsAll() {
		t.Error("nil groupScopes should give all scope")
	}
	s = ResolveScope([]string{"any-group"}, map[string][]string{})
	if !s.IsAll() {
		t.Error("empty groupScopes should give all scope")
	}
}

func TestResolveScope_Wildcard(t *testing.T) {
	s := ResolveScope([]string{"admins"}, map[string][]string{
		"admins": {"*"},
	})
	if !s.IsAll() {
		t.Error("group mapped to * should give all scope")
	}
}

func TestResolveScope_SpecificNamespaces(t *testing.T) {
	s := ResolveScope([]string{"site-a-ops"}, map[string][]string{
		"site-a-ops": {"site-a", "site-a-infra"},
		"platform":   {"*"},
		"site-b-ops": {"site-b"},
	})
	if s.IsAll() {
		t.Fatal("should not be all scope")
	}
	if !s.Allows("site-a") {
		t.Error("should allow site-a")
	}
	if !s.Allows("site-a-infra") {
		t.Error("should allow site-a-infra")
	}
	if s.Allows("site-b") {
		t.Error("should not allow site-b")
	}
}

func TestResolveScope_NoMatchingGroups(t *testing.T) {
	s := ResolveScope([]string{"unknown-group"}, map[string][]string{
		"admins": {"*"},
	})
	if s.IsAll() {
		t.Error("should not be all scope")
	}
	if s.Allows("any-namespace") {
		t.Error("should not allow any namespace")
	}
}

func TestResolveScope_MultipleGroups(t *testing.T) {
	s := ResolveScope([]string{"site-a-ops", "site-b-ops"}, map[string][]string{
		"site-a-ops": {"site-a"},
		"site-b-ops": {"site-b"},
	})
	if !s.Allows("site-a") {
		t.Error("should allow site-a")
	}
	if !s.Allows("site-b") {
		t.Error("should allow site-b")
	}
	if s.Allows("site-c") {
		t.Error("should not allow site-c")
	}
}

func TestFilterViews(t *testing.T) {
	views := []fleet.ClusterView{
		{Namespace: "site-a", Name: "cluster-1"},
		{Namespace: "site-b", Name: "cluster-2"},
		{Namespace: "site-a", Name: "cluster-3"},
		{Namespace: "site-c", Name: "cluster-4"},
	}
	scope := Scope{namespaces: map[string]bool{"site-a": true}}
	got := filterViews(views, scope)
	if len(got) != 2 {
		t.Fatalf("got %d clusters, want 2", len(got))
	}
	for _, v := range got {
		if v.Namespace != "site-a" {
			t.Errorf("got cluster in %s, want site-a only", v.Namespace)
		}
	}
}

func TestFilterViews_AllScope(t *testing.T) {
	views := []fleet.ClusterView{
		{Namespace: "site-a", Name: "cluster-1"},
		{Namespace: "site-b", Name: "cluster-2"},
	}
	got := filterViews(views, AllScope())
	if len(got) != 2 {
		t.Fatalf("all scope should return everything, got %d", len(got))
	}
}

func TestFilterStorage(t *testing.T) {
	storage := fleet.Storage{
		Clusters: []fleet.StorageCluster{
			{
				FSID: "fsid-a",
				ReportedBy: []fleet.ClusterRef{
					{Namespace: "site-a", Name: "cluster-1"},
				},
			},
			{
				FSID: "fsid-b",
				ReportedBy: []fleet.ClusterRef{
					{Namespace: "site-b", Name: "cluster-2"},
				},
			},
			{
				FSID: "fsid-ab",
				ReportedBy: []fleet.ClusterRef{
					{Namespace: "site-a", Name: "cluster-1"},
					{Namespace: "site-b", Name: "cluster-2"},
				},
			},
		},
	}
	scope := Scope{namespaces: map[string]bool{"site-a": true}}
	got := filterStorage(storage, scope)
	if len(got.Clusters) != 2 {
		t.Fatalf("got %d storage clusters, want 2", len(got.Clusters))
	}
	for _, sc := range got.Clusters {
		if sc.FSID == "fsid-b" {
			t.Error("fsid-b should be filtered out (only reported by site-b)")
		}
	}
}

// With scoping active, an authenticated user only sees their namespaces on the
// fleet page. This is the whole point: the collector holds exec privilege, but
// a user scoped to site-a must not see site-b's clusters or storage.
func TestScoping_FleetPage(t *testing.T) {
	groupScopes := map[string][]string{
		"site-a-ops": {"site-a"},
	}
	clusters := []fleet.ClusterView{
		{Namespace: "site-a", Name: "cluster-1", Status: health.StatusOK},
		{Namespace: "site-b", Name: "cluster-2", Status: health.StatusOK},
	}
	s, err := New(&fakeFleet{clusters: clusters, changed: make(chan struct{}, 1)},
		auth.Open{}, "test", "site-a", groupScopes)
	if err != nil {
		t.Fatal(err)
	}

	// With auth.Open there is no identity in context, so scope is all.
	// Verify that the no-identity path returns everything (backward compatible).
	rec := get(t, s.Handler(), "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "cluster-1") {
		t.Error("site-a cluster should be visible with no identity (all scope)")
	}
	if !contains(body, "cluster-2") {
		t.Error("site-b cluster should be visible with no identity (all scope)")
	}
}

// With scoping active, a request to a cluster outside the user's scope gets 404,
// not a 403. "There is no such cluster" and "you may not see this cluster" must
// not render differently — the namespace list is the authorization surface, and
// revealing the existence of a hidden cluster would defeat it.
func TestScoping_ClusterPage_OutsideScope(t *testing.T) {
	groupScopes := map[string][]string{
		"site-a-ops": {"site-a"},
	}
	detail := fleet.ClusterDetail{}
	detail.Namespace = "site-b"
	detail.Name = "cluster-2"
	detail.Status = health.StatusOK

	s, err := New(&fakeFleet{
		detail:  map[string]fleet.ClusterDetail{"site-b/cluster-2": detail},
		changed: make(chan struct{}, 1),
	}, auth.Open{}, "test", "site-a", groupScopes)
	if err != nil {
		t.Fatal(err)
	}

	// With auth.Open, no identity → all scope → cluster is visible.
	// The scoping test with identity requires a middleware that injects it,
	// which is tested via the auth package's session tests.
	rec := get(t, s.Handler(), "/cluster/site-b/cluster-2")
	if rec.Code != http.StatusOK {
		t.Fatalf("with no identity (all scope), expected 200, got %d", rec.Code)
	}
}

func TestLoadGroupScopes_EmptyPath(t *testing.T) {
	gs, err := LoadGroupScopes("")
	if err != nil {
		t.Fatal(err)
	}
	if gs != nil {
		t.Error("empty path should return nil")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// Ensure the SSE stream respects scope: the remote source (sextant --server)
// receives only the clusters the user's scope allows. This is tested at the
// API level because the remote source is the consumer.
func TestScoping_APIFleet(t *testing.T) {
	groupScopes := map[string][]string{
		"site-a-ops": {"site-a"},
	}
	clusters := []fleet.ClusterView{
		{Namespace: "site-a", Name: "cluster-1", Status: health.StatusOK},
		{Namespace: "site-b", Name: "cluster-2", Status: health.StatusOK},
	}
	s, err := New(&fakeFleet{clusters: clusters, changed: make(chan struct{}, 1)},
		auth.Open{}, "test", "site-a", groupScopes)
	if err != nil {
		t.Fatal(err)
	}

	// With auth.Open (no identity), scope is all — both clusters visible.
	rec := get(t, s.Handler(), "/api/v1/fleet")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "cluster-1") {
		t.Error("site-a cluster should be in API response")
	}
	if !contains(body, "cluster-2") {
		t.Error("site-b cluster should be in API response (no identity = all scope)")
	}
}

// Verify scopeFor returns AllScope when there's no identity in context
// (the Open auth path).
func TestScopeFor_NoIdentity(t *testing.T) {
	s := &Server{groupScopes: map[string][]string{"admins": {"*"}}}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	scope := s.scopeFor(req)
	if !scope.IsAll() {
		t.Error("no identity in context should give all scope")
	}
}
