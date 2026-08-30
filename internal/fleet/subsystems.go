package fleet

import (
	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/subsystem"
	"github.com/runlevel-six/sextant/pkg/subsystem/ceph"
	"github.com/runlevel-six/sextant/pkg/subsystem/cilium"
	"github.com/runlevel-six/sextant/pkg/subsystem/metallb"
	"github.com/runlevel-six/sextant/pkg/subsystem/openstack"
	"github.com/runlevel-six/sextant/pkg/subsystem/ovn"
)

// Subsystems is whatever optional subsystems this cluster runs.
//
// Every field is a pointer, and nil means the plugin published nothing — which
// on a cluster that does not run that subsystem is the correct and permanent
// answer. That is sextant's rule carried across the boundary: a subsystem the
// cluster does not have must be absent, not an empty pane.
type Subsystems struct {
	Cilium  *cilium.State
	OVN     *ovn.State
	MetalLB *metallb.State
	Ceph    *ceph.State

	OpenStack  *openstack.State
	Migrations *openstack.Migrations
	Inventory  *openstack.Inventory
}

// Any reports whether there is anything to render.
func (s Subsystems) Any() bool {
	return s.Cilium != nil || s.OVN != nil || s.MetalLB != nil || s.Ceph != nil ||
		s.OpenStack != nil || s.Migrations != nil || s.Inventory != nil
}

// Network reports whether the network pane has a source.
func (s Subsystems) Network() bool {
	return s.Cilium != nil || s.OVN != nil || s.MetalLB != nil
}

// Cloud reports whether the cloud pane has a source.
func (s Subsystems) Cloud() bool {
	return s.OpenStack != nil || s.Migrations != nil || s.Inventory != nil
}

// Degraded is a subsystem reporting less than it could, and why.
//
// A reduced tier is not a failure and must not render as one, but it must not
// render as absence either: a Cilium pane missing its IPAM figures because
// pods/exec was denied looks exactly like a Cilium with nothing to say. The
// reason is the difference.
type Degraded struct {
	Name   string
	Reason string
}

// Reduced lists every subsystem currently reporting below full detail.
func (s Subsystems) Reduced() []Degraded {
	var out []Degraded
	add := func(name string, tier subsystem.Tier, reason string) {
		if tier != subsystem.TierFull {
			if reason == "" {
				reason = "reporting what the API alone reveals"
			}
			out = append(out, Degraded{Name: name, Reason: reason})
		}
	}
	if s.Cilium != nil {
		add("Cilium", s.Cilium.Tier, s.Cilium.TierReason)
	}
	if s.OVN != nil {
		add("OVN", s.OVN.Tier, s.OVN.TierReason)
	}
	if s.Ceph != nil {
		add("Ceph", s.Ceph.Tier, s.Ceph.TierReason)
	}
	return out
}

// readSubsystems pulls whatever the plugins published for this cluster.
func readSubsystems(s *store.Store) Subsystems {
	var out Subsystems
	if v, ok := store.Get[cilium.State](s, cilium.KeyState); ok {
		out.Cilium = &v
	}
	if v, ok := store.Get[ovn.State](s, ovn.KeyState); ok {
		out.OVN = &v
	}
	if v, ok := store.Get[metallb.State](s, metallb.KeyState); ok {
		out.MetalLB = &v
	}
	if v, ok := store.Get[ceph.State](s, ceph.KeyState); ok {
		out.Ceph = &v
	}
	if v, ok := store.Get[openstack.State](s, openstack.KeyState); ok {
		out.OpenStack = &v
	}
	if v, ok := store.Get[openstack.Migrations](s, openstack.KeyMigrations); ok {
		out.Migrations = &v
	}
	if v, ok := store.Get[openstack.Inventory](s, openstack.KeyInventory); ok {
		out.Inventory = &v
	}
	return out
}
