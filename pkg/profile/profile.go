// Package profile holds the site-specific knowledge that core code must not
// hardcode: which label keys carry a node's role, which namespaces are worth
// watching for events, which workloads are critical, and how to find a cluster's
// kubeconfig context.
//
// The rule this package exists to enforce is that no core file contains a
// site-specific string literal. A namespace name, a platform's own node-role
// label key, or the name of a database StatefulSet that must never be down is
// true for one deployment and false for the next, so it belongs in data.
//
// [Default] is a vanilla Cluster API plus Metal3 management cluster and assumes
// nothing else. It is what a user with no configuration gets, and it must stay
// useful on its own — the whole design depends on the zero-config path working.
//
// Loading profiles from YAML, including inheritance between them, comes later;
// for now these are Go values with accessors.
package profile

import (
	"regexp"
	"sort"
	"strings"
)

// WildcardSuffix marks a label key as a prefix match rather than an exact one.
//
// Two conventions exist for recording a node's role, and a profile has to
// express both. Upstream Kubernetes encodes the role in the *key*
// ("node-role.kubernetes.io/control-plane" with an empty value), while many
// platforms add a single key whose *value* is the role ("my-role: compute").
// A key ending in this suffix selects the former reading.
const WildcardSuffix = "/*"

// UpstreamRoleLabelPrefix is the conventional Kubernetes node-role label key
// prefix. Wildcarded, it is the default and works on any cluster.
const UpstreamRoleLabelPrefix = "node-role.kubernetes.io"

// Profile is a resolved set of site conventions.
type Profile struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	// Extends names a profile to inherit from. Unused until profiles are
	// loadable from YAML; declared here so the schema is stable.
	Extends string `yaml:"extends,omitempty"`

	Clusters          Clusters            `yaml:"clusters"`
	NodeRoles         NodeRoles           `yaml:"node_roles"`
	Events            Events              `yaml:"events"`
	CriticalWorkloads []CriticalWorkload  `yaml:"critical_workloads,omitempty"`
	Plugins           map[string]Settings `yaml:"plugins,omitempty"`
	Layout            Layout              `yaml:"layout,omitempty"`
}

// Settings is a plugin's opaque configuration block, interpreted by that plugin.
type Settings map[string]any

// Clusters describes how to find the clusters to watch.
type Clusters struct {
	Management ClusterRef `yaml:"management"`
	Workload   ClusterRef `yaml:"workload"`
}

// ClusterRef locates one cluster and scopes what is read from it.
type ClusterRef struct {
	// Context pins an exact kubeconfig context name.
	Context string `yaml:"context,omitempty"`
	// ContextPattern is a regular expression matched against a context's name,
	// cluster, and user. Used where context names follow a site convention;
	// unset on a stock cluster, where the current context is used instead.
	ContextPattern string `yaml:"context_pattern,omitempty"`
	// Namespaces restricts which namespaces are read on this cluster. Empty
	// means all namespaces, which is the correct default: upstream Cluster API
	// has no single conventional namespace for Cluster objects, and per-cluster
	// namespaces are common.
	Namespaces []string `yaml:"namespaces,omitempty"`

	// CAPIName is the name of the Cluster API Cluster object corresponding to
	// this cluster. Set it where one management cluster owns several clusters and
	// only one of them is being watched.
	//
	// This is not the same thing as the kubeconfig context: the context says how
	// to reach a cluster, while this says which Cluster object on the *management*
	// side describes it. Without it, a management cluster owning three clusters
	// shows all three clusters' Machines beside one cluster's Nodes.
	CAPIName string `yaml:"capi_name,omitempty"`
	// CAPINamePattern derives [CAPIName] from the resolved context name, for
	// sites whose context names encode the cluster. The pattern is matched
	// against the context name; the first capture group is used if there is one,
	// otherwise the whole match.
	//
	// A pattern is preferred over a literal wherever it works, because a literal
	// makes a profile single-cluster: the same file then cannot serve a second
	// datacentre whose context differs only in a substring.
	//
	// Either form is matched against a Cluster's name as an exact value *or* a
	// prefix of it, because the Cluster object usually carries a suffix the context
	// name does not: context site-a-tenant-01 identifies Cluster
	// tenant-01-cluster. Make the derived value specific enough not to
	// prefix-match a sibling — "tenant-0" would match 01 and 02 both.
	CAPINamePattern string `yaml:"capi_name_pattern,omitempty"`
}

// CAPINameFor resolves the Cluster API cluster name for a resolved context.
//
// An explicit [CAPIName] wins. Otherwise the pattern is applied to the context
// name. A pattern that does not match returns the empty string, which means no
// filtering rather than an empty result set — the same safe direction the
// namespace scoping takes, since showing more than was asked for is recoverable
// and silently hiding a cluster's Machines is not.
func (c ClusterRef) CAPINameFor(contextName string) string {
	if c.CAPIName != "" {
		return c.CAPIName
	}
	if c.CAPINamePattern == "" || contextName == "" {
		return ""
	}
	re, err := regexp.Compile(c.CAPINamePattern)
	if err != nil {
		// Validate rejects an uncompilable pattern before this is reached; a
		// profile built in Go rather than loaded could still carry one, and no
		// filtering is the safe reading.
		return ""
	}
	m := re.FindStringSubmatch(contextName)
	switch {
	case m == nil:
		return ""
	case len(m) > 1 && m[1] != "":
		return m[1]
	}
	return m[0]
}

// AllNamespaces reports whether this cluster should be read cluster-wide.
func (c ClusterRef) AllNamespaces() bool { return len(c.Namespaces) == 0 }

// NodeRoles describes how to derive and present a node's role.
type NodeRoles struct {
	// LabelKeys is consulted in order; the first key that yields a role wins.
	// A key ending in [WildcardSuffix] is a prefix match whose suffix is the
	// role. Ordering matters: put a platform-specific key first so it takes
	// precedence over the upstream fallback.
	LabelKeys []string `yaml:"label_keys"`
	// Display maps a raw role to a human label. A role with no entry is shown
	// as-is.
	Display map[string]string `yaml:"display,omitempty"`
	// MachineDeploymentMatch maps a role to substrings that identify it in a
	// MachineDeployment's name, for clusters where MD names encode the role.
	// Checked most-specific-first by the order roles appear in Roles().
	MachineDeploymentMatch map[string][]string `yaml:"machinedeployment_match,omitempty"`
	// CordonExpected names the roles whose nodes are cordoned on purpose, as a
	// steady state rather than mid-drain.
	//
	// This exists because a cordon means two opposite things depending on the
	// site. Ordinarily it is a drain in progress and worth attention. But a
	// hypervisor whose capacity is reserved for virtual machines rather than pods
	// is *permanently* cordoned by design, and reporting that as a warning paints
	// a healthy cluster amber forever — which trains the operator to ignore the
	// color that is supposed to mean something.
	//
	// Only a cordoned node that is also Ready is treated as expected. Cordoned
	// and NotReady is news whatever the role.
	CordonExpected []string `yaml:"cordon_expected,omitempty"`
}

// Events describes which namespaces' events are worth surfacing.
//
// The filter exists because volume, not relevance, is the problem: a busy
// cluster emits hundreds of events per second, and an unfiltered pane is
// unreadable.
type Events struct {
	// Namespaces are matched exactly.
	Namespaces []string `yaml:"namespaces,omitempty"`
	// NamespacePrefixes are matched as prefixes, for conventions like "team-*".
	NamespacePrefixes []string `yaml:"namespace_prefixes,omitempty"`
	// AllNamespaces disables filtering entirely. Set it deliberately; on a
	// large cluster the events pane becomes noise.
	AllNamespaces bool `yaml:"all_namespaces,omitempty"`
}

// CriticalWorkload pins a workload whose readiness is always shown, whether or
// not it is currently unhealthy — the things whose absence you want to notice
// immediately rather than discover by scrolling.
type CriticalWorkload struct {
	Kind      string `yaml:"kind"`
	Namespace string `yaml:"namespace"`
	Name      string `yaml:"name"`
}

// Layout describes pane placement.
type Layout struct {
	// TopRow lists pane IDs for the fixed row above the grid, left to right.
	TopRow []string `yaml:"top_row,omitempty"`
	// Grid lists pane IDs for the grid, in priority order.
	Grid []string `yaml:"grid,omitempty"`
	// Stack maps a pane ID to the host pane it sits beneath.
	Stack map[string]StackSpec `yaml:"stack,omitempty"`
}

// StackSpec places one pane beneath another in the same column.
type StackSpec struct {
	Under string  `yaml:"under"`
	Ratio float64 `yaml:"ratio"`
}

// Default returns the profile for a vanilla Cluster API plus Metal3 management
// cluster: upstream node-role labels, kube-system events only, all namespaces
// for Cluster API objects, and no assumptions about any other subsystem.
//
// Every optional plugin is left unconfigured. Detection decides what appears,
// so a cluster that happens to run Ceph or Cilium still gets those panes
// without naming them here.
func Default() Profile {
	return Profile{
		Name:        "metal3",
		Description: "Vanilla Cluster API + Metal3 management cluster",
		NodeRoles: NodeRoles{
			LabelKeys: []string{UpstreamRoleLabelPrefix + WildcardSuffix},
		},
		Events: Events{
			Namespaces: []string{"kube-system"},
		},
		Layout: Layout{
			TopRow: []string{"overview"},
			Grid:   []string{"machines", "nodes", "pods", "events"},
		},
	}
}

// RoleOf returns a node's primary role, or the empty string when no configured
// label key yields one.
//
// Keys are consulted in configured order. For a wildcard key, every matching
// label contributes a role and [PrimaryRole] picks between them.
func (n NodeRoles) RoleOf(labels map[string]string) string {
	roles := n.RolesOf(labels)
	if len(roles) == 0 {
		return ""
	}
	return PrimaryRole(roles)
}

// RolesOf returns every role a node carries, deduplicated and ordered: roles
// from earlier label keys first, alphabetically within a wildcard key.
//
// A node legitimately holds several roles at once — an upstream control-plane
// node carries both "control-plane" and the legacy "master".
func (n NodeRoles) RolesOf(labels map[string]string) []string {
	if len(labels) == 0 {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(role string) {
		if role != "" && !seen[role] {
			seen[role] = true
			out = append(out, role)
		}
	}

	for _, key := range n.LabelKeys {
		if prefix, wild := strings.CutSuffix(key, WildcardSuffix); wild {
			var matched []string
			for k, v := range labels {
				if role, ok := strings.CutPrefix(k, prefix+"/"); ok && role != "" {
					_ = v // an upstream node-role label's value is empty by convention
					matched = append(matched, role)
				}
			}
			sort.Strings(matched)
			for _, r := range matched {
				add(r)
			}
			continue
		}
		add(labels[key])
	}
	return out
}

// legacyControlPlaneRole is the deprecated alias upstream still applies
// alongside "control-plane" on control-plane nodes.
const legacyControlPlaneRole = "master"

// PrimaryRole picks a single role from a set already in precedence order.
//
// It takes the first entry, with one exception: it never returns the deprecated
// "master" alias while another role is present. Upstream labels control-plane
// nodes with both "control-plane" and "master", and reporting the alias would
// mislabel every control-plane node on such a cluster.
//
// Note what this deliberately does *not* do: promote "control-plane" from
// anywhere in the set. Doing so would let the upstream fallback outrank an
// earlier, site-specific label key, defeating the ordering in
// [NodeRoles.LabelKeys]. Precedence is [NodeRoles.RolesOf]'s job; this only
// resolves the alias.
func PrimaryRole(roles []string) string {
	if len(roles) == 0 {
		return ""
	}
	if roles[0] != legacyControlPlaneRole {
		return roles[0]
	}
	for _, r := range roles[1:] {
		if r != legacyControlPlaneRole {
			return r
		}
	}
	return roles[0]
}

// CordonIsNews reports whether a cordoned node deserves attention, or is simply
// sitting where the site intends it to sit.
//
// Callers check [CordonExpected] through this rather than reading the slice, so
// the "NotReady overrides the exemption" rule lives in one place. With no
// CordonExpected configured every cordon is news, which is the correct default:
// on a stock cluster a cordon really is a drain.
func (n NodeRoles) CordonIsNews(role string, ready bool) bool {
	if !ready {
		return true
	}
	for _, r := range n.CordonExpected {
		if r == role {
			return false
		}
	}
	return true
}

// DisplayName returns the human label for a role, falling back to the raw role.
func (n NodeRoles) DisplayName(role string) string {
	if role == "" {
		return "(unlabeled)"
	}
	if d, ok := n.Display[role]; ok && d != "" {
		return d
	}
	return role
}

// Roles returns the roles this profile knows about, in a stable order: those
// with display names first (in sorted order), then any appearing only in
// MachineDeploymentMatch.
func (n NodeRoles) Roles() []string {
	seen := map[string]bool{}
	var out []string
	for r := range n.Display {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	sort.Strings(out)
	extra := make([]string, 0, len(n.MachineDeploymentMatch))
	for r := range n.MachineDeploymentMatch {
		if !seen[r] {
			seen[r] = true
			extra = append(extra, r)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

// RoleFromMachineDeploymentName infers a role from a MachineDeployment's name
// using MachineDeploymentMatch, or returns "" when nothing matches.
//
// Roles are tried in [NodeRoles.Roles] order and, within a role, longest
// substring first. That matters when one role's substring contains another's:
// matching the longer pattern first stops an incidental hit from winning.
func (n NodeRoles) RoleFromMachineDeploymentName(name string) string {
	if name == "" || len(n.MachineDeploymentMatch) == 0 {
		return ""
	}
	type candidate struct {
		role    string
		pattern string
	}
	var candidates []candidate
	for _, role := range n.Roles() {
		for _, pat := range n.MachineDeploymentMatch[role] {
			if pat != "" {
				candidates = append(candidates, candidate{role, pat})
			}
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return len(candidates[i].pattern) > len(candidates[j].pattern)
	})
	for _, c := range candidates {
		if strings.Contains(name, c.pattern) {
			return c.role
		}
	}
	return ""
}

// Interesting reports whether events from a namespace should be surfaced.
//
// With no namespaces and no prefixes configured, nothing matches. That is
// deliberate: an unconfigured filter yields an empty pane rather than firehosing
// every event in the cluster. Set AllNamespaces to opt into that explicitly.
func (e Events) Interesting(namespace string) bool {
	if e.AllNamespaces {
		return true
	}
	for _, ns := range e.Namespaces {
		if ns == namespace {
			return true
		}
	}
	for _, p := range e.NamespacePrefixes {
		if p != "" && strings.HasPrefix(namespace, p) {
			return true
		}
	}
	return false
}

// Matches reports whether a pod belongs to this critical workload.
//
// Pods are matched by namespace and by a "name-" prefix, which covers both
// StatefulSet pods ("name-0") and Deployment pods ("name-<rs>-<pod>"). A
// sibling workload sharing the prefix would also match; that is acceptable for
// deliberately pinned names and keeps this independent of owner-reference
// walking.
func (c CriticalWorkload) Matches(namespace, podName string) bool {
	return c.Namespace == namespace && strings.HasPrefix(podName, c.Name+"-")
}
