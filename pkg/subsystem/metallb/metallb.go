// Package metallb holds the state sextant's MetalLB plugin publishes.
//
// The types are here, separate from the plugin that fills them, so a consumer
// outside this module can read a cluster's MetalLB state without importing the
// machinery that produced it.
package metallb

import (
	"time"

	"github.com/runlevel-six/sextant/pkg/subsystem"
)

// KeyState holds a [State].
const KeyState = "metallb/state"

// Pool is one IPAddressPool.
//
// Addresses is the raw spec, because the syntax is heterogeneous — a CIDR, a
// dashed range, or a single address — and guessing at pool sizes from it would
// report confident nonsense. Showing the spec verbatim lets an operator check it
// against what they meant.
type Pool struct {
	Namespace  string
	Name       string
	Addresses  []string
	AutoAssign bool
	// Advertised names how this pool is advertised: "L2", "BGP", both, or empty
	// when nothing advertises it. An unadvertised pool hands out addresses that
	// nothing announces, which is a real and otherwise silent misconfiguration.
	Advertised []string
	// Assigned is how many of the pool's addresses are handed out, and Available
	// how many remain. Available is meaningful only when Usage is [UsageStatus];
	// nothing else can say how large a pool is without parsing its addresses.
	Assigned  int
	Available int
	// Usage records where those numbers came from, so the pane can tell an empty
	// pool from one it could not measure.
	Usage UsageSource
}

// UsageSource says how a pool's address usage was determined.
type UsageSource int

const (
	// UsageUnknown means nothing could attribute addresses to this pool. It is
	// not the same as a pool with nothing in it, and must not render as zero.
	UsageUnknown UsageSource = iota
	// UsageStatus means MetalLB published the counts on the IPAddressPool
	// itself. Both numbers are real, and they count addresses rather than
	// Services — which is the honest unit, since one Service can hold two
	// addresses on a dual-stack cluster and two can share one.
	UsageStatus
	// UsageAnnotations means the count was assembled from the pool each Service
	// records having been allocated from. Assigned is a count of Services and
	// Available is unknown.
	UsageAnnotations
)

// Total is the pool's size, when that is known.
func (p Pool) Total() int { return p.Assigned + p.Available }

// Exhausted reports whether a pool has published capacity and none of it left.
//
// The next LoadBalancer Service to ask this pool for an address will sit
// Pending forever, which is the failure this plugin exists to see coming.
func (p Pool) Exhausted() bool {
	return p.Usage == UsageStatus && p.Available == 0 && p.Assigned > 0
}

// Service is one LoadBalancer Service.
type Service struct {
	Namespace  string
	Name       string
	ExternalIP string
	Pool       string
}

// Pending reports whether the service is still waiting for an address.
func (s Service) Pending() bool { return s.ExternalIP == "" }

// State is everything the plugin publishes.
type State struct {
	Pools    []Pool
	Services []Service
	// Namespace is where MetalLB was found, derived from the pools unless pinned.
	Namespace string
	// SpeakerReady and SpeakerDesired describe the speaker DaemonSet. A pool
	// with addresses and no speaker announces nothing.
	SpeakerReady   int32
	SpeakerDesired int32
	// Rollout is the speaker DaemonSet's progress toward its own pod template,
	// which readiness alone does not report.
	Rollout   subsystem.Rollout
	UpdatedAt time.Time
	Err       error
}

// PendingServices counts the Services still without an address.
func (s State) PendingServices() int {
	n := 0
	for _, svc := range s.Services {
		if svc.Pending() {
			n++
		}
	}
	return n
}

// ExhaustedPools names pools that have handed out every address they have.
func (s State) ExhaustedPools() []string {
	var out []string
	for _, p := range s.Pools {
		if p.Exhausted() {
			out = append(out, p.Name)
		}
	}
	return out
}

// UnadvertisedPools names pools that nothing advertises.
func (s State) UnadvertisedPools() []string {
	var out []string
	for _, p := range s.Pools {
		if len(p.Advertised) == 0 {
			out = append(out, p.Name)
		}
	}
	return out
}
