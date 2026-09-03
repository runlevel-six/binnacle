package fleet

import (
	"github.com/runlevel-six/binnacle/pkg/model"
	"github.com/runlevel-six/binnacle/pkg/store"
)

// ManagementDetail is the management cluster's own page.
//
// It exists because the management cluster is the one thing binnacle watches
// that has nowhere to click. It is not a Cluster API Cluster — it hosts them —
// so there is no /cluster/... route for it, and the fleet panel is a
// fixed-height summary by design. A reader who saw "2 unhealthy pods" there had
// no way to find out which, and the events below had no home at all.
//
// It embeds [ManagementView] and reuses that type's UnhealthyPods field for the
// page's fuller list rather than adding a second one. Two fields both meaning
// "the unhealthy pods" is how a page ends up disagreeing with itself, and an
// embedded field that shadows its parent's is a silent template trap — see
// ClusterDetail.NodeRows for the last one of those. The cap is the only
// difference between the panel's list and this one, and the cap is the
// constructor's business.
type ManagementDetail struct {
	ManagementView

	// NodeRows is the management cluster's own nodes, ranked and folded like
	// every other node table. NodesKnown on the embedded view says whether a
	// snapshot arrived at all, so an unreadable node list does not render as a
	// cluster with no nodes.
	NodeRows Split[NodeRow]

	// Events is every event on the management cluster that no cluster page
	// shows: see [Fleet.managementEvents] for what that means and why there are
	// two sources.
	Events          Split[EventGroup]
	EventsTruncated int
	EventsTotal     int
}

// ManagementDetail returns the management cluster's page.
func (f *Fleet) ManagementDetail() ManagementDetail {
	d := ManagementDetail{ManagementView: f.Management()}
	if !d.Reachable || f.mgmt == nil {
		// Nothing was read, and the embedded view already carries the reason.
		return d
	}
	s := f.mgmt.store

	if snap, ok := store.Get[model.Snapshot[model.Node]](s, model.KeyWorkloadNodes); ok && snap.Err == nil {
		rows := make([]NodeRow, 0, len(snap.Items))
		for _, n := range snap.Items {
			rows = append(rows, NodeRow{
				Node:       n,
				CPUPercent: percent(n.RequestedCPU, n.AllocatableCPU),
				MemPercent: percent(n.RequestedMemory, n.AllocatableMemory),
			})
		}
		d.NodeRows = splitNodes(rows)
	}

	// The same list the panel shows, at the cluster page's cap instead of the
	// panel's twelve. The panel is a teaser on a page that must not grow; this
	// is the page that can afford the rows.
	if snap, ok := store.Get[model.Snapshot[model.Pod]](s, model.KeyWorkloadPods); ok && snap.Err == nil {
		d.UnhealthyPods, d.PodsTruncated = unhealthyPods(snap.Items, maxUnhealthyPods)
	}

	d.Events, d.EventsTruncated, d.EventsTotal = capEvents(GroupEvents(f.managementEvents()))
	return d
}

// managementEvents is every event on the management cluster that no cluster
// page shows. Two sources, and neither is rendered anywhere else in binnacle.
//
// The first is the management cluster's own events, read by its collector from
// its own API server and filtered to the profile's namespaces. Those are its
// own by definition and need no scoping.
//
// The second is the Cluster API namespace's events that belong to no cluster.
// That namespace holds every cluster in the datacenter, so each cluster page
// shows the subset matching its own object names and counts the rest as
// EventsElsewhere. Measured on dev1a, the remainder was forty-five Kyverno
// policy violations against management-cluster workloads: real events, owned by
// no cluster, and therefore visible on no page. This is where they land.
//
// The Cluster API snapshot is borrowed from an arbitrary cluster's store, the
// way Storage borrows the host inventory — every per-cluster collector watches
// the same namespace, so the snapshots are identical by construction. The owned
// names, though, are the *union* across every cluster, which is the part that
// has to be right: subtracting only the cluster whose snapshot we borrowed
// would report every other cluster's events as unclaimed.
func (f *Fleet) managementEvents() []model.Event {
	var all []model.Event
	// Guarded here as well as at the caller: the Cluster API half below is
	// worth returning on its own, so a fleet with no management collector is
	// not a reason to return nothing.
	if f.mgmt != nil {
		if snap, ok := store.Get[model.Snapshot[model.Event]](f.mgmt.store, model.KeyWorkloadEvents); ok && snap.Err == nil {
			all = append(all, snap.Items...)
		}
	}

	f.mu.Lock()
	tracked := make([]*tracked, 0, len(f.clusters))
	for _, t := range f.clusters {
		tracked = append(tracked, t)
	}
	f.mu.Unlock()

	owned := make(map[string]bool, 64)
	var capiEvents []model.Event
	var found bool
	for _, t := range tracked {
		ref := t.discovered
		// Cluster rather than the store directly: ownedNames is built from the
		// machines, pools and scoped hosts a page has already read, and
		// re-deriving that here is how the two would drift.
		if cd, ok := f.Cluster(ref.Namespace, ref.Name); ok {
			for name := range cd.ownedNames() {
				owned[name] = true
			}
		}
		if !found {
			if snap, ok := store.Get[model.Snapshot[model.Event]](t.store, model.KeyMgmtEvents); ok && snap.Err == nil {
				capiEvents, found = snap.Items, true
			}
		}
	}

	for _, e := range capiEvents {
		// The complement of what the cluster pages show, by the same matching
		// rule they use — so an event cannot be dropped from every page by
		// being claimed here and scoped out there.
		if !ownsName(owned, e.ObjectName) {
			all = append(all, e)
		}
	}
	return all
}
