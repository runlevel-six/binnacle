package capi

import (
	"sort"

	"github.com/runlevel-six/sextant/internal/core/model"
)

// Metal3MachineKind is the infrastructure kind a Machine references when it is
// backed by Metal3.
const Metal3MachineKind = "Metal3Machine"

// controlPlaneOwnerKind is the ownerReference kind on a control-plane Machine.
const controlPlaneOwnerKind = "KubeadmControlPlane"

// ControlPlaneRole is the role reported for control-plane machines.
const ControlPlaneRole = "control-plane"

// HostRow is one Machine joined to the provider machine and the physical host
// beneath it.
//
// This join is the thing no other tool does. `clusterctl describe` shows the
// Cluster API tree and stops at the infrastructure reference; a resource browser
// shows each object in isolation. Neither answers the question an operator
// actually has during a rollout, which is "which physical box is this Machine,
// and what is that box doing right now".
//
// Metal3Machine and BareMetalHost are nil when the corresponding object is
// absent. That is a normal state, not an error: a Machine is created before its
// provider machine is bound, and a Metal3Machine exists briefly before a host is
// selected. A nil here means "not yet", and a row is still emitted so the
// pending Machine is visible rather than missing.
type HostRow struct {
	Machine       model.Machine
	Metal3Machine *model.Metal3Machine
	BareMetalHost *model.BareMetalHost
	// Role is the machine's role, from RoleFunc. Empty when unknown.
	Role string
	// ControlPlane reports whether this Machine is owned by a control plane.
	ControlPlane bool
}

// HostName returns the physical host's name, or the empty string when no host is
// bound yet.
func (r HostRow) HostName() string {
	if r.BareMetalHost == nil {
		return ""
	}
	return r.BareMetalHost.Name
}

// Provisioned reports whether a host is bound and in its provisioned state.
func (r HostRow) Provisioned() bool {
	return r.BareMetalHost != nil && r.BareMetalHost.State == "provisioned"
}

// Activity ranks, lowest first. A pane renders as many rows as it has lines and
// replaces the rest with "+ N more", so on a fleet larger than the tile this
// ordering is what decides whether an operator has to zoom to find the machine
// that is actually doing something.
const (
	// RankInFlight is a Machine or host mid-transition: provisioning,
	// deprovisioning, or a host that has come back to available.
	RankInFlight = 0
	// RankAttention is a failed Machine or a host reporting an error, and is
	// deliberately *behind* in-flight rows: a host that has been failed for a
	// week would otherwise pin itself above the reprovision happening now.
	RankAttention = 1
	// RankSettled is the bulk of a healthy fleet.
	RankSettled = 2
)

// machineInFlightPhases are the Cluster API phases that mean the Machine itself
// is mid-transition.
//
// Running, Provisioned, Failed and Unknown are deliberately absent. Provisioned
// in particular is a machine whose hardware is up and is waiting only for its
// Node to register — the rest of the dashboard treats it as settled, and
// promoting it here would keep a rollout's worth of finished machines at the top.
var machineInFlightPhases = map[string]bool{
	"Pending":      true,
	"Provisioning": true,
	"Deleting":     true,
	"Deleted":      true,
}

// hostInFlightStates are the Metal3 provisioning states that mean the physical
// host is being worked on.
//
// "available" is here even though it is a healthy state, because an available
// host still bound to a Machine is one that has just been released — the tail of
// a deprovision, and worth seeing. "ready" is the older name for it.
//
// This is not the same set the pane colors amber: there, available is green.
// The two answer different questions — "is this host healthy" and "is this host
// changing" — so they are kept apart rather than shared and bent to fit both.
var hostInFlightStates = map[string]bool{
	"registering":             true,
	"inspecting":              true,
	"matchprofile":            true,
	"preparing":               true,
	"available":               true,
	"ready":                   true,
	"provisioning":            true,
	"deprovisioning":          true,
	"poweringOffBeforeDelete": true,
	"deleting":                true,
}

// ActivityRank groups a row by whether it needs a reader's eyes right now.
//
// A bare-metal fleet is mostly settled, and the handful of rows worth looking at
// are the ones changing: a Machine being provisioned or deleted, a host being
// wiped and handed back. Those are exactly the rows a large fleet pushes below
// the fold of a small tile, so they lead.
//
// A state this function does not recognize ranks as settled, on the same
// reasoning the pane colors one neutrally: the Metal3 state machine gains states
// over time, and promoting an unknown one would make the top of the pane
// unreliable in the one situation it exists for.
func ActivityRank(r HostRow) int {
	if machineInFlightPhases[r.Machine.Phase] || hostInFlight(r) {
		return RankInFlight
	}
	if r.Machine.Phase == "Failed" ||
		(r.BareMetalHost != nil && r.BareMetalHost.ErrorMessage != "") {
		return RankAttention
	}
	return RankSettled
}

// hostInFlight reports whether the host beneath a row is mid-transition.
func hostInFlight(r HostRow) bool {
	if r.BareMetalHost != nil {
		return hostInFlightStates[r.BareMetalHost.State]
	}
	// A provider machine with no host bound is mid-provision by definition:
	// either a host is being selected or none matched. Both are the pane's
	// "awaiting host" row, and both want to be seen.
	return r.Metal3Machine != nil
}

// RoleFunc derives a Machine's role. It exists so the join stays independent of
// the profile package: the caller supplies whatever site convention applies.
type RoleFunc func(model.Machine) string

// Join links Machines to their Metal3Machines and BareMetalHosts.
//
// Two paths find the host, because neither is reliable alone. The forward path
// reads the Metal3Machine's metal3.io/BareMetalHost annotation. The reverse path
// looks for a host whose spec.consumerRef points back at the Metal3Machine, and
// is used when the annotation is missing — it lives in spec rather than metadata,
// so it appears as soon as the host is claimed and survives annotation loss.
//
// roleFn may be nil, in which case only control-plane membership is derived, from
// the ownerReference that Cluster API always sets.
func Join(
	machines []model.Machine,
	m3ms []model.Metal3Machine,
	bmhs []model.BareMetalHost,
	roleFn RoleFunc,
) []HostRow {
	type nsName struct{ ns, name string }

	m3mByName := make(map[nsName]*model.Metal3Machine, len(m3ms))
	for i := range m3ms {
		m3mByName[nsName{m3ms[i].Namespace, m3ms[i].Name}] = &m3ms[i]
	}

	bmhByName := make(map[nsName]*model.BareMetalHost, len(bmhs))
	bmhByConsumer := make(map[nsName]*model.BareMetalHost, len(bmhs))
	for i := range bmhs {
		b := &bmhs[i]
		bmhByName[nsName{b.Namespace, b.Name}] = b
		if b.ConsumerKind == Metal3MachineKind && b.ConsumerName != "" {
			ns := b.ConsumerNamespace
			if ns == "" {
				ns = b.Namespace
			}
			bmhByConsumer[nsName{ns, b.ConsumerName}] = b
		}
	}

	out := make([]HostRow, 0, len(machines))
	for _, m := range machines {
		row := HostRow{
			Machine:      m,
			ControlPlane: m.OwnerKind == controlPlaneOwnerKind,
		}

		if m.InfraKind == Metal3MachineKind && m.InfraName != "" {
			if m3m, ok := m3mByName[nsName{m.Namespace, m.InfraName}]; ok {
				row.Metal3Machine = m3m

				if m3m.BMHName != "" {
					ns := m3m.BMHNamespace
					if ns == "" {
						ns = m3m.Namespace
					}
					row.BareMetalHost = bmhByName[nsName{ns, m3m.BMHName}]
				}
				if row.BareMetalHost == nil {
					row.BareMetalHost = bmhByConsumer[nsName{m3m.Namespace, m3m.Name}]
				}
			}
		}

		switch {
		case row.ControlPlane:
			row.Role = ControlPlaneRole
		case roleFn != nil:
			row.Role = roleFn(m)
		}
		out = append(out, row)
	}

	// Moving rows first, then broken ones, then the settled fleet. Within a rank:
	// control plane first, then by role, then by namespace and name. Control-plane
	// machines lead because that is the rollout stage an operator watches most
	// closely.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ra, rb := ActivityRank(a), ActivityRank(b); ra != rb {
			return ra < rb
		}
		if a.ControlPlane != b.ControlPlane {
			return a.ControlPlane
		}
		if a.Role != b.Role {
			return a.Role < b.Role
		}
		if a.Machine.Namespace != b.Machine.Namespace {
			return a.Machine.Namespace < b.Machine.Namespace
		}
		return a.Machine.Name < b.Machine.Name
	})
	return out
}

// UnclaimedHosts returns the hosts no cluster has claimed, sorted by namespace
// and name.
//
// These are the spare capacity in the fleet: available, being inspected, or
// stuck in an error state. They belong on screen during a rollout, since a
// scale-up that cannot find a host is a failure that shows up here first and
// nowhere else.
//
// Claims are read from *every* Metal3Machine and from the host's own consumerRef,
// deliberately not from the joined rows. Those rows may be narrowed to one Cluster
// API cluster, and a host consumed by a different cluster is emphatically not
// spare: offering it as free capacity is how a rollout ends up provisioning over
// something that is in use. Both signals are consulted because either can lead —
// a Metal3Machine names its host as soon as it claims one, while consumerRef is
// set by the bare-metal operator and is also present on a host provisioned outside
// Cluster API entirely.
func UnclaimedHosts(m3ms []model.Metal3Machine, bmhs []model.BareMetalHost) []model.BareMetalHost {
	claimed := make(map[string]bool, len(m3ms))
	for _, m := range m3ms {
		if m.BMHName == "" {
			continue
		}
		// Same namespace fallback as the join: the annotation may name a host
		// without qualifying it, meaning one alongside the machine.
		ns := m.BMHNamespace
		if ns == "" {
			ns = m.Namespace
		}
		claimed[ns+"/"+m.BMHName] = true
	}
	for _, b := range bmhs {
		if b.ConsumerName != "" {
			claimed[b.Namespace+"/"+b.Name] = true
		}
	}
	out := make([]model.BareMetalHost, 0, len(bmhs))
	for _, b := range bmhs {
		if !claimed[b.Namespace+"/"+b.Name] {
			out = append(out, b)
		}
	}
	sortByNamespacedName(out, func(b model.BareMetalHost) (string, string) { return b.Namespace, b.Name })
	return out
}
