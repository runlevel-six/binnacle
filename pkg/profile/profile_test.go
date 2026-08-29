package profile

import (
	"strings"
	"testing"
)

// The zero-config path is load-bearing, so assert the default's shape directly.
func TestDefault_WorksOnAStockCluster(t *testing.T) {
	p := Default()

	if p.Name != "metal3" {
		t.Errorf("Name: got %q want metal3", p.Name)
	}
	// Upstream node-role labels must be understood with no configuration.
	if got := p.NodeRoles.RoleOf(map[string]string{
		"node-role.kubernetes.io/control-plane": "",
	}); got != "control-plane" {
		t.Errorf("stock control-plane node: got role %q want control-plane", got)
	}
	// All namespaces for Cluster API objects — upstream has no single
	// conventional namespace.
	if !p.Clusters.Management.AllNamespaces() {
		t.Error("default should read Cluster API objects from all namespaces")
	}
	// No plugin is named; detection decides.
	if len(p.Plugins) != 0 {
		t.Errorf("default should configure no plugins, got %v", p.Plugins)
	}
	if len(p.CriticalWorkloads) != 0 {
		t.Errorf("default should pin no workloads, got %v", p.CriticalWorkloads)
	}
	if len(p.Layout.Grid) == 0 {
		t.Error("default should place some grid panes")
	}
}

func TestAllNamespaces(t *testing.T) {
	if !(ClusterRef{}).AllNamespaces() {
		t.Error("empty Namespaces should mean all namespaces")
	}
	if (ClusterRef{Namespaces: []string{"capi"}}).AllNamespaces() {
		t.Error("a configured namespace should scope the read")
	}
}

// --- role derivation ------------------------------------------------------

func TestRolesOf_WildcardKey(t *testing.T) {
	n := NodeRoles{LabelKeys: []string{"node-role.kubernetes.io/*"}}
	got := n.RolesOf(map[string]string{
		"node-role.kubernetes.io/control-plane": "",
		"node-role.kubernetes.io/master":        "",
		"kubernetes.io/hostname":                "node-1",
	})
	// Sorted within a wildcard key, unrelated labels ignored.
	if strings.Join(got, ",") != "control-plane,master" {
		t.Errorf("got %v want [control-plane master]", got)
	}
}

func TestRolesOf_ExactKeyUsesValue(t *testing.T) {
	n := NodeRoles{LabelKeys: []string{"my-platform-role"}}
	got := n.RolesOf(map[string]string{"my-platform-role": "compute"})
	if len(got) != 1 || got[0] != "compute" {
		t.Errorf("got %v want [compute]", got)
	}
}

// Ordering of LabelKeys is the mechanism a site uses to take precedence over
// the upstream fallback.
func TestRolesOf_EarlierKeyWins(t *testing.T) {
	n := NodeRoles{LabelKeys: []string{"platform-role", "node-role.kubernetes.io/*"}}
	labels := map[string]string{
		"platform-role":                         "managed-services",
		"node-role.kubernetes.io/control-plane": "",
	}
	got := n.RolesOf(labels)
	if len(got) != 2 || got[0] != "managed-services" {
		t.Errorf("got %v want managed-services first", got)
	}
	if n.RoleOf(labels) != "managed-services" {
		t.Errorf("RoleOf: got %q want managed-services", n.RoleOf(labels))
	}
}

func TestRolesOf_Deduplicates(t *testing.T) {
	n := NodeRoles{LabelKeys: []string{"a", "b"}}
	got := n.RolesOf(map[string]string{"a": "compute", "b": "compute"})
	if len(got) != 1 {
		t.Errorf("got %v want a single entry", got)
	}
}

func TestRolesOf_EmptyAndMissing(t *testing.T) {
	n := NodeRoles{LabelKeys: []string{"role", "node-role.kubernetes.io/*"}}
	if got := n.RolesOf(nil); got != nil {
		t.Errorf("nil labels: got %v want nil", got)
	}
	if got := n.RolesOf(map[string]string{"other": "x"}); got != nil {
		t.Errorf("no matching key: got %v want nil", got)
	}
	// An empty value is not a role.
	if got := n.RolesOf(map[string]string{"role": ""}); got != nil {
		t.Errorf("empty value: got %v want nil", got)
	}
	// A bare prefix with no suffix is not a role either.
	if got := n.RolesOf(map[string]string{"node-role.kubernetes.io/": ""}); got != nil {
		t.Errorf("empty suffix: got %v want nil", got)
	}
}

func TestRoleOf_NoLabelKeysConfigured(t *testing.T) {
	if got := (NodeRoles{}).RoleOf(map[string]string{"anything": "x"}); got != "" {
		t.Errorf("got %q want empty", got)
	}
}

// An upstream control-plane node carries both control-plane and master; picking
// alphabetically would label it "master".
func TestPrimaryRole_PrefersControlPlaneOverMaster(t *testing.T) {
	if got := PrimaryRole([]string{"master", "control-plane"}); got != "control-plane" {
		t.Errorf("got %q want control-plane", got)
	}
	if got := PrimaryRole([]string{"control-plane"}); got != "control-plane" {
		t.Errorf("got %q want control-plane", got)
	}
}

func TestPrimaryRole_PrefersRealRoleOverLegacyMaster(t *testing.T) {
	if got := PrimaryRole([]string{"master", "etcd"}); got != "etcd" {
		t.Errorf("got %q want etcd", got)
	}
}

// PrimaryRole must not promote control-plane from anywhere in the set: doing so
// would let the upstream fallback label outrank an earlier site-specific key,
// which is precisely what LabelKeys ordering exists to control.
func TestPrimaryRole_DoesNotPromoteControlPlaneOverPrecedence(t *testing.T) {
	if got := PrimaryRole([]string{"managed-services", "control-plane"}); got != "managed-services" {
		t.Errorf("got %q want managed-services (first by precedence)", got)
	}
}

func TestPrimaryRole_EdgeCases(t *testing.T) {
	if got := PrimaryRole(nil); got != "" {
		t.Errorf("nil: got %q want empty", got)
	}
	if got := PrimaryRole([]string{"master"}); got != "master" {
		t.Errorf("master alone: got %q want master", got)
	}
	if got := PrimaryRole([]string{"compute", "gpu"}); got != "compute" {
		t.Errorf("got %q want the first entry", got)
	}
}

func TestDisplayName(t *testing.T) {
	n := NodeRoles{Display: map[string]string{"controller": "Control-Plane", "blank": ""}}
	if got := n.DisplayName("controller"); got != "Control-Plane" {
		t.Errorf("got %q want Control-Plane", got)
	}
	if got := n.DisplayName("compute"); got != "compute" {
		t.Errorf("unmapped role: got %q want compute", got)
	}
	if got := n.DisplayName("blank"); got != "blank" {
		t.Errorf("empty mapping should fall back: got %q want blank", got)
	}
	if got := n.DisplayName(""); got != "(unlabeled)" {
		t.Errorf("empty role: got %q want (unlabeled)", got)
	}
}

func TestRoles_StableOrder(t *testing.T) {
	n := NodeRoles{
		Display:                map[string]string{"compute": "Compute", "controller": "Control"},
		MachineDeploymentMatch: map[string][]string{"managed-services": {"msvc"}, "compute": {"compute"}},
	}
	got := n.Roles()
	// Display roles sorted first, then match-only roles sorted.
	if strings.Join(got, ",") != "compute,controller,managed-services" {
		t.Errorf("got %v want [compute controller managed-services]", got)
	}
	// Stable across calls, since map iteration order is not.
	for range 20 {
		if strings.Join(n.Roles(), ",") != strings.Join(got, ",") {
			t.Fatalf("Roles() is not deterministic: %v vs %v", n.Roles(), got)
		}
	}
}

// --- MachineDeployment name matching --------------------------------------

func TestRoleFromMachineDeploymentName(t *testing.T) {
	n := NodeRoles{
		Display: map[string]string{"compute": "Compute", "managed-services": "Managed"},
		MachineDeploymentMatch: map[string][]string{
			"managed-services": {"managed-services", "msvc"},
			"compute":          {"compute"},
		},
	}
	tests := []struct {
		name string
		want string
	}{
		{"tenant-01-compute", "compute"},
		{"tenant-01-msvc", "managed-services"},
		// "managed-services" contains no "compute", but this guards the
		// longest-pattern-first rule generally.
		{"tenant-01-managed-services", "managed-services"},
		{"tenant-01-something-else", ""},
		{"", ""},
	}
	for _, tc := range tests {
		if got := n.RoleFromMachineDeploymentName(tc.name); got != tc.want {
			t.Errorf("RoleFromMachineDeploymentName(%q): got %q want %q", tc.name, got, tc.want)
		}
	}
}

// A longer pattern must win over a shorter one it contains, regardless of which
// role happens to sort first.
func TestRoleFromMachineDeploymentName_LongestPatternWins(t *testing.T) {
	n := NodeRoles{
		MachineDeploymentMatch: map[string][]string{
			"aaa-short": {"gpu"},
			"zzz-long":  {"gpu-compute"},
		},
	}
	if got := n.RoleFromMachineDeploymentName("pool-gpu-compute-1"); got != "zzz-long" {
		t.Errorf("got %q want zzz-long (longest pattern)", got)
	}
}

func TestRoleFromMachineDeploymentName_NoMatchConfigured(t *testing.T) {
	if got := (NodeRoles{}).RoleFromMachineDeploymentName("tenant-01-compute"); got != "" {
		t.Errorf("got %q want empty", got)
	}
}

// --- event filtering ------------------------------------------------------

func TestEventsInteresting(t *testing.T) {
	e := Events{
		Namespaces:        []string{"openstack", "kube-system"},
		NamespacePrefixes: []string{"team-"},
	}
	tests := map[string]bool{
		"openstack":   true,
		"kube-system": true,
		"team-alpha":  true,
		"team-":       true,
		"default":     false,
		"openstack2":  false,
		"":            false,
	}
	for ns, want := range tests {
		if got := e.Interesting(ns); got != want {
			t.Errorf("Interesting(%q): got %v want %v", ns, got, want)
		}
	}
}

// An unconfigured filter yields an empty pane, not the whole cluster's events.
func TestEventsInteresting_UnconfiguredMatchesNothing(t *testing.T) {
	var e Events
	for _, ns := range []string{"default", "kube-system", ""} {
		if e.Interesting(ns) {
			t.Errorf("unconfigured filter matched %q", ns)
		}
	}
}

func TestEventsInteresting_AllNamespacesOptIn(t *testing.T) {
	e := Events{AllNamespaces: true}
	for _, ns := range []string{"anything", "", "kube-system"} {
		if !e.Interesting(ns) {
			t.Errorf("AllNamespaces should match %q", ns)
		}
	}
}

// An empty prefix would match every namespace; it must be ignored.
func TestEventsInteresting_EmptyPrefixIgnored(t *testing.T) {
	e := Events{NamespacePrefixes: []string{""}}
	if e.Interesting("anything") {
		t.Error("an empty prefix should not match everything")
	}
}

// --- critical workloads ---------------------------------------------------

func TestCriticalWorkloadMatches(t *testing.T) {
	c := CriticalWorkload{Kind: "StatefulSet", Namespace: "openstack", Name: "ovn-ovsdb-nb"}
	tests := []struct {
		ns, pod string
		want    bool
	}{
		{"openstack", "ovn-ovsdb-nb-0", true},
		{"openstack", "ovn-ovsdb-nb-abc123-xyz", true},
		{"openstack", "ovn-ovsdb-sb-0", false},
		{"other", "ovn-ovsdb-nb-0", false},
		// The bare name without a suffix is the workload, not a pod.
		{"openstack", "ovn-ovsdb-nb", false},
	}
	for _, tc := range tests {
		if got := c.Matches(tc.ns, tc.pod); got != tc.want {
			t.Errorf("Matches(%q, %q): got %v want %v", tc.ns, tc.pod, got, tc.want)
		}
	}
}

// A cordon means opposite things depending on the site: a drain in progress, or
// a hypervisor whose capacity is reserved for virtual machines and is therefore
// cordoned for its whole life.
func TestCordonIsNews(t *testing.T) {
	n := NodeRoles{CordonExpected: []string{"compute"}}

	if n.CordonIsNews("compute", true) {
		t.Error("an expected cordon on a Ready node should not be news")
	}
	// Not Ready overrides the exemption: that is a broken hypervisor, not a
	// reserved one.
	if !n.CordonIsNews("compute", false) {
		t.Error("a cordoned node that is also NotReady is always news")
	}
	if !n.CordonIsNews("controller", true) {
		t.Error("a role not listed should still be news")
	}
	if !n.CordonIsNews("", true) {
		t.Error("an unlabelled node should still be news")
	}
}

// The default has to be "every cordon is news" — on a stock cluster a cordon
// really is a drain, and silence there would hide it.
func TestCordonIsNews_UnconfiguredWarnsAsBefore(t *testing.T) {
	var n NodeRoles
	if !n.CordonIsNews("compute", true) {
		t.Error("with nothing configured, a cordon must stay news")
	}
}

func TestMerge_CordonExpectedReplaces(t *testing.T) {
	parent := Profile{NodeRoles: NodeRoles{CordonExpected: []string{"compute"}}}
	child := Profile{NodeRoles: NodeRoles{CordonExpected: []string{"storage"}}}

	if got := Merge(parent, child).NodeRoles.CordonExpected; len(got) != 1 || got[0] != "storage" {
		t.Errorf("child should replace the list, got %v", got)
	}
	// An unset child keeps the parent's, so extending a platform profile does
	// not silently re-enable the warning.
	if got := Merge(parent, Profile{}).NodeRoles.CordonExpected; len(got) != 1 || got[0] != "compute" {
		t.Errorf("unset child should inherit, got %v", got)
	}
}

// --- Cluster API cluster identity -----------------------------------------

func TestCAPINameFor(t *testing.T) {
	tests := []struct {
		name    string
		ref     ClusterRef
		context string
		want    string
	}{
		{"explicit name wins", ClusterRef{CAPIName: "prod", CAPINamePattern: `x\d+`}, "x1", "prod"},
		{"pattern over the context", ClusterRef{CAPINamePattern: `tenant-\d+`},
			"site-a-tenant-01", "tenant-01"},
		{"capture group wins over the whole match", ClusterRef{CAPINamePattern: `-(tenant-\d+)$`},
			"site-a-tenant-01", "tenant-01"},
		// No filtering rather than an empty result set: showing more than was asked
		// for is recoverable, and silently hiding a cluster's Machines is not.
		{"no match means no filter", ClusterRef{CAPINamePattern: `nothing-like-this`},
			"site-a-tenant-01", ""},
		{"nothing configured", ClusterRef{}, "site-a-tenant-01", ""},
		{"no context to match against", ClusterRef{CAPINamePattern: `x`}, "", ""},
		// An uncompilable pattern is rejected by Validate; reaching here it must
		// still not filter everything away.
		{"bad pattern", ClusterRef{CAPINamePattern: `([`}, "anything", ""},
	}
	for _, tc := range tests {
		if got := tc.ref.CAPINameFor(tc.context); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestMerge_CAPIClusterIdentity(t *testing.T) {
	parent := Profile{Clusters: Clusters{Workload: ClusterRef{CAPINamePattern: `parent\d+`}}}
	child := Profile{Clusters: Clusters{Workload: ClusterRef{CAPIName: "pinned"}}}

	got := Merge(parent, child).Clusters.Workload
	if got.CAPIName != "pinned" {
		t.Errorf("CAPIName: got %q", got.CAPIName)
	}
	// The parent's pattern survives, and the child's literal outranks it at
	// resolution time rather than by erasing it.
	if got.CAPINamePattern != `parent\d+` {
		t.Errorf("CAPINamePattern: got %q", got.CAPINamePattern)
	}
	if resolved := got.CAPINameFor("parent7"); resolved != "pinned" {
		t.Errorf("resolution should prefer the literal, got %q", resolved)
	}
}

func TestValidate_RejectsBadCAPIPattern(t *testing.T) {
	p := Default()
	p.Clusters.Workload.CAPINamePattern = `([`
	err := p.Validate()
	if err == nil {
		t.Fatal("an uncompilable pattern should not validate")
	}
	if !strings.Contains(err.Error(), "capi_name_pattern") {
		t.Errorf("error should name the field, got: %v", err)
	}
}
