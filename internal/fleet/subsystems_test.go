package fleet

import (
	"testing"

	"github.com/runlevel-six/binnacle/pkg/store"
	"github.com/runlevel-six/binnacle/pkg/subsystem"
	"github.com/runlevel-six/binnacle/pkg/subsystem/cilium"
	"github.com/runlevel-six/binnacle/pkg/subsystem/metallb"
	"github.com/runlevel-six/binnacle/pkg/subsystem/openstack"
	"github.com/runlevel-six/binnacle/pkg/subsystem/ovn"
)

// A cluster that does not run a subsystem gets nothing, not an empty section.
// This is sextant's own rule — an absent subsystem must be absent — carried
// across the module boundary.
func TestSubsystems_AbsentStaysAbsent(t *testing.T) {
	s := readSubsystems(store.New())
	if s.Any() || s.Network() || s.Cloud() {
		t.Errorf("an empty store produced subsystems: %+v", s)
	}
	if s.Cilium != nil || s.OVN != nil || s.MetalLB != nil || s.OpenStack != nil {
		t.Error("absent subsystems came back non-nil")
	}
}

func TestSubsystems_ReadWhatWasPublished(t *testing.T) {
	st := store.New()
	st.Put(cilium.KeyState, cilium.State{Tier: subsystem.TierFull, AgentsReady: 5})
	st.Put(metallb.KeyState, metallb.State{Namespace: "kube-system"})
	st.Put(openstack.KeyMigrations, openstack.Migrations{})

	s := readSubsystems(st)
	if s.Cilium == nil || s.Cilium.AgentsReady != 5 {
		t.Errorf("cilium = %+v", s.Cilium)
	}
	if s.MetalLB == nil || s.MetalLB.Namespace != "kube-system" {
		t.Errorf("metallb = %+v", s.MetalLB)
	}
	if !s.Network() || !s.Cloud() {
		t.Error("network and cloud should both have a source")
	}
	if s.OVN != nil {
		t.Error("ovn published nothing and should be nil")
	}
}

// A subsystem reporting below full detail must say so and say why.
//
// This is the distinction the tier system exists for: a Cilium pane missing its
// IPAM figures because pods/exec was denied looks exactly like a Cilium with
// nothing to report. Only the reason separates them.
func TestSubsystems_ReducedTierIsNamedWithItsReason(t *testing.T) {
	st := store.New()
	st.Put(cilium.KeyState, cilium.State{
		Tier: subsystem.TierInformer, TierReason: "no permission to exec into cilium pods",
	})
	st.Put(ovn.KeyState, ovn.State{Tier: subsystem.TierFull})

	reduced := readSubsystems(st).Reduced()
	if len(reduced) != 1 {
		t.Fatalf("got %d reduced subsystems, want 1: %+v", len(reduced), reduced)
	}
	if reduced[0].Name != "Cilium" || reduced[0].Reason == "" {
		t.Errorf("got %+v", reduced[0])
	}
}

// A reduced tier with no reason recorded still reports something a reader can
// act on, rather than an empty cell.
func TestSubsystems_ReducedWithoutAReasonStillExplains(t *testing.T) {
	st := store.New()
	st.Put(cilium.KeyState, cilium.State{Tier: subsystem.TierInformer})
	if r := readSubsystems(st).Reduced(); len(r) != 1 || r[0].Reason == "" {
		t.Errorf("got %+v", r)
	}
}
