// Package cilium holds the state sextant's Cilium plugin publishes.
//
// The types are here, separate from the plugin that fills them, so a consumer
// outside this module can read a cluster's Cilium state without importing the
// machinery that produced it.
package cilium

import (
	"strings"
	"time"

	"github.com/runlevel-six/sextant/pkg/subsystem"
)

// KeyState holds a [State].
const KeyState = "cilium/state"

// IPAM is one agent's view of pod-address allocation.
//
// These numbers are per-node, not cluster-wide: a pane must say which agent they
// came from or a reader will take one node's exhaustion for the cluster's.
type IPAM struct {
	Used      int
	Available int
}

// Total is the implied pool size. Cilium reports no total because a pool can
// fragment across CIDRs, but used plus available is the figure that answers "are
// we about to run out".
func (i IPAM) Total() int { return i.Used + i.Available }

// ExhaustionKnown reports whether Available is meaningful.
//
// The oldest status shape lists allocated addresses with no remaining count, so
// Available is zero because it is unknown, not because the pool is full.
// Presenting that as 100% used would be a false alarm.
func (i IPAM) ExhaustionKnown() bool { return i.Available > 0 || i.Used == 0 }

// Percent returns used as a percentage of the pool, or -1 when unknown.
func (i IPAM) Percent() int {
	if !i.ExhaustionKnown() || i.Total() == 0 {
		return -1
	}
	return i.Used * 100 / i.Total()
}

// Hubble is the observability layer's state.
type Hubble struct {
	Enabled   bool
	State     string
	SeenFlows int64
	// FlowsPerSecond is derived between polls, since SeenFlows is a lifetime
	// counter. A lifetime average would understate a current spike by orders of
	// magnitude on a long-running agent.
	FlowsPerSecond float64
}

// Controllers counts Cilium's internal controllers on the agent polled.
type Controllers struct {
	Total   int
	Failing int
}

// MeshPeer is one cluster-mesh peer.
type MeshPeer struct {
	Name  string
	Ready bool
}

// ClusterMesh is the cross-cluster view. No peers means mesh is not enabled.
type ClusterMesh struct {
	Peers          []MeshPeer
	GlobalServices int
}

// Status is what one agent reports.
type Status struct {
	Version string
	State   string
	// KubeProxyReplacement is Cilium's own mode string, and it reads as its own
	// opposite: "true" means Cilium has *replaced* kube-proxy, not that
	// kube-proxy is running. Render it through [Status.KubeProxyText] rather
	// than showing the field.
	KubeProxyReplacement string
	EncryptionMode       string
	IPAM                 IPAM
	Hubble               Hubble
	Controllers          Controllers
	ClusterMesh          ClusterMesh
	// Unreadable names the status sections that could not be decoded, so a
	// missing cell is reported as unreadable rather than as absent or healthy.
	Unreadable []string
}

// State is everything the plugin publishes.
type State struct {
	// Tier is how much of the below is populated.
	Tier subsystem.Tier
	// AgentsReady and AgentsDesired come from the DaemonSet and are available at
	// every tier.
	AgentsReady   int32
	AgentsDesired int32
	// Rollout is the agent DaemonSet's progress toward its own pod template.
	// Distinct from readiness: every agent can be Ready while half of them still
	// run the version before the one the chart now asks for.
	Rollout subsystem.Rollout
	// Status is populated only at the full tier.
	Status Status
	// Pod names the agent Status came from, so per-node figures can be labeled.
	Pod string
	// TierReason explains a reduced tier, so a thin pane says why it is thin.
	TierReason string
	UpdatedAt  time.Time
	Err        error
}

// KubeProxyText says what the replacement mode means, in words.
//
// The raw value is a trap: Cilium reports "true" when it has *replaced*
// kube-proxy, so a display showing the field reads as the opposite of the
// truth. The mapping lives on the type rather than in a pane so that every
// front end says the same thing — this exact inversion was already found once
// against a live cluster.
func (s Status) KubeProxyText() string {
	switch strings.ToLower(s.KubeProxyReplacement) {
	case "":
		return "unknown"
	case "true", "strict":
		return "replaced by Cilium"
	case "false", "disabled":
		// Deliberately says nothing about whether kube-proxy is running:
		// Cilium reports only that it is not replacing it, and claiming more
		// than that would be inventing an observation.
		return "not replaced"
	case "partial":
		return "partially replaced"
	}
	return s.KubeProxyReplacement
}
