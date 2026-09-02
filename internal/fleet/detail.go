package fleet

import (
	"sort"
	"time"

	"github.com/runlevel-six/binnacle/internal/wire"
	"github.com/runlevel-six/binnacle/pkg/health"
	"github.com/runlevel-six/binnacle/pkg/model"
	"github.com/runlevel-six/binnacle/pkg/store"
	"github.com/runlevel-six/binnacle/pkg/subsystem/openstack"
)

// Caps on what one page will render.
//
// A busy cluster produces events and unhealthy pods without limit, and a page
// that renders all of them stops being readable long before it stops being
// correct. Both lists are ordered worst-first, so the cap drops the least
// interesting rows rather than an arbitrary tail.
//
// maxEvents counts groups, not events: see GroupEvents. Sixty distinct groups
// is sixty distinct problems, where sixty raw events was routinely one problem
// reported sixty times.
const (
	maxEvents        = 60
	maxUnhealthyPods = 60
)

// NodeRow is a node with its commitment already worked out.
type NodeRow struct {
	model.Node
	CPUPercent int
	MemPercent int
}

// Pressure reports whether the kubelet is complaining about any resource.
func (n NodeRow) Pressure() bool {
	return n.MemoryPressure || n.DiskPressure || n.PIDPressure || n.NetworkUnavail
}

// SummaryBlock is a subsystem's own headline, as its plugin renders it.
//
// The lines arrive already formatted, because the plugin that knows what Ceph's
// health means is the one that should decide how to say it. Binnacle lays them
// out and changes nothing.
type SummaryBlock struct {
	Title string
	Lines []string
}

// ClusterDetail is everything binnacle can show about one cluster.
//
// It reaches parity with sextant's core panes — the overview, machines and
// hosts, nodes, pod health, and events. It does not reach the plugin panes:
// their snapshot types live in sextant's internal packages, so what survives
// the boundary is each plugin's banner cell and, where a plugin offers one, its
// summary block. Promoting those types is what a network or cloud pane waits on.
type ClusterDetail struct {
	ClusterView

	// The three big tables are ordered worst-first and folded: on a production
	// undercloud these run to forty machines, seventy nodes and a datacenter's
	// worth of hosts, and alphabetical order buries the rows that matter. See
	// Split and collapseAfter.
	Machines Split[model.Machine]
	Hosts    Split[model.BareMetalHost]
	// HostsElsewhere is how many hosts in the datacenter-wide snapshot belong
	// to something other than this cluster. Reported rather than silently
	// dropped: the pane used to list every host in the building, and a reader
	// who knew that needs to be told where they went.
	HostsElsewhere int
	// NodeRows is the per-node table. Named for the rows rather than for the
	// nodes because ClusterView already has a Nodes field holding the counts,
	// and an embedded field that shadows its parent's is a silent trap: a
	// template asking for .Nodes.Ready would resolve to this slice and fail at
	// render time, halfway down an already-written page.
	NodeRows Split[NodeRow]

	UnhealthyPods []model.Pod
	// PodsTruncated is how many unhealthy pods are not listed. Shown, because
	// "60 unhealthy pods" and "60 of 400" are different situations, and a page
	// that silently truncates reports the first when it means the second.
	PodsTruncated int

	// Events is grouped, not raw: see GroupEvents. It is also ranked and folded,
	// like the three big tables: warnings are the pane and Normal events are the
	// audit trail behind it. EventsTruncated counts the groups that did not fit,
	// and EventsTotal the raw events behind all of them, so the header can say
	// "12 of 47 groups, 1,384 events" rather than implying the cluster only
	// produced what fits on the page.
	Events          Split[EventGroup]
	EventsTruncated int
	EventsTotal     int
	// EventsElsewhere is how many management events in the namespace-wide
	// snapshot concern some other cluster. Reported for the same reason
	// HostsElsewhere is: the reader who knows the snapshot is namespace-wide
	// needs to be told where the rest went.
	EventsElsewhere int

	// Subsystems is whatever optional subsystems this cluster runs. Absent ones
	// are nil rather than empty, so a cluster without Ceph gets no Ceph section
	// instead of an empty one.
	Subsystems Subsystems

	// Drains is the compute hosts being emptied, worked out by the OpenStack
	// plugin. Lifted out of Subsystems because it is the question an operator
	// mid-maintenance actually has, and it deserves the top of the cloud pane
	// rather than a row inside a migration table.
	Drains []openstack.Drain
	// Shown is the migration split: rows worth listing, and failures worth
	// counting.
	Shown openstack.Shown
}

// Cluster returns everything known about one cluster.
//
// The bool is false when no such cluster is tracked, which a caller should
// render as a 404 rather than as an empty cluster: a page saying a cluster has
// no nodes is a different claim from one saying it was never found.
func (f *Fleet) Cluster(namespace, name string) (ClusterDetail, bool) {
	f.mu.Lock()
	t, ok := f.clusters[namespace+"/"+name]
	f.mu.Unlock()
	if !ok {
		return ClusterDetail{}, false
	}

	d := ClusterDetail{ClusterView: t.view(f.opts.Profile)}
	if d.Problem != "" {
		// Nothing was read, so there is nothing to detail. The view already
		// carries the reason.
		return d, true
	}

	s := t.store
	if snap, ok := store.Get[model.Snapshot[model.Machine]](s, model.KeyMgmtMachines); ok {
		d.Machines = splitMachines(snap.Items)
	}
	if snap, ok := store.Get[model.Snapshot[model.BareMetalHost]](s, model.KeyMgmtBareMetalHosts); ok {
		// Scoped to this cluster before ranking: see hostsFor. Machines are read
		// first because they carry the reference the hosts are matched on.
		mine, elsewhere := hostsFor(snap.Items, d.Machines.All())
		d.HostsElsewhere = elsewhere
		// Errored hosts first: a rollout stalls on them more often than on
		// anything else, which is why sextant gives them their own cell.
		d.Hosts = splitHosts(mine)
	}

	d.readNodes(s)
	d.readPods(s)
	d.readEvents(s)

	d.Subsystems = readSubsystems(s)
	if m := d.Subsystems.Migrations; m != nil {
		// Relevant decides which failures are still worth a row: sextant's
		// judgement, not ours. Re-deriving it here would mean the fleet page
		// and the dashboard disagreeing about whether a migration still
		// matters, and there would be no way to tell which was right.
		d.Shown = m.Relevant(time.Now())
		d.Drains = m.Drains
	}
	return d, true
}

// StoreSnapshot returns the raw store contents for one cluster as wire
// entries, for streaming to a terminal client's dashboard. The dashboard
// panes read typed snapshots from the store; this is the same data,
// unfiltered and uncapped, as opposed to Cluster which is a curated
// projection.
//
// The bool is false when no such cluster is tracked.
func (f *Fleet) StoreSnapshot(namespace, name string) ([]wire.Entry, bool) {
	f.mu.Lock()
	t, ok := f.clusters[namespace+"/"+name]
	f.mu.Unlock()
	if !ok {
		return nil, false
	}
	return wire.Dump(t.store), true
}

func (d *ClusterDetail) readNodes(s *store.Store) {
	snap, ok := store.Get[model.Snapshot[model.Node]](s, model.KeyWorkloadNodes)
	if !ok {
		return
	}
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

func (d *ClusterDetail) readPods(s *store.Store) {
	snap, ok := store.Get[model.Snapshot[model.Pod]](s, model.KeyWorkloadPods)
	if !ok {
		return
	}
	var unhealthy []model.Pod
	for _, p := range snap.Items {
		// The same filter the card and the health cell use: see
		// [health.NeedsAttention]. A pane listing pods the cell does not count
		// is a page disagreeing with itself.
		if health.NeedsAttention(p) {
			unhealthy = append(unhealthy, p)
		}
	}
	sort.Slice(unhealthy, func(i, j int) bool {
		// Most restarts first: a pod that has restarted four hundred times is
		// the one somebody wants to see, and it is rarely the newest.
		if unhealthy[i].Restarts != unhealthy[j].Restarts {
			return unhealthy[i].Restarts > unhealthy[j].Restarts
		}
		return unhealthy[i].Namespace+"/"+unhealthy[i].Name <
			unhealthy[j].Namespace+"/"+unhealthy[j].Name
	})
	if len(unhealthy) > maxUnhealthyPods {
		d.PodsTruncated = len(unhealthy) - maxUnhealthyPods
		unhealthy = unhealthy[:maxUnhealthyPods]
	}
	d.UnhealthyPods = unhealthy
}

// EventGroup is a set of identical events collapsed into one row.
//
// Kubernetes already reports a repeated event against a single object as one
// record with a count. What it does not collapse is the same event fired by
// many objects at once — one admission policy rejecting forty ReplicaSets
// produces forty records that differ only in the name. Ungrouped, those forty
// fill the whole page and, worse, fill the cap: a real event gets truncated
// away by duplicates of a single problem. Grouping runs before the cap for
// exactly that reason.
type EventGroup struct {
	// Event is the most recent member of the group, and supplies everything the
	// group shares: type, reason, kind, message, and the timestamp to show.
	model.Event
	// Occurrences totals the reported counts across the group. An event that
	// reports no count still happened once.
	Occurrences int
	// Objects is how many distinct objects reported it. One means ObjectName is
	// the whole story; more means the name is only a sample.
	Objects int
}

// GroupEvents collapses identical events and orders them worst-first.
//
// Identical means same type, reason, object kind and message: the object's own
// name is deliberately not part of the key, because "which objects" is the
// thing being summarized. Ordering keeps the ungrouped rule — warnings first,
// then most recent — so a reader moving between binnacle and sextant is not
// re-learning it.
func GroupEvents(events []model.Event) []EventGroup {
	type key struct{ typ, reason, kind, message string }

	index := make(map[key]int, len(events))
	var groups []EventGroup
	seen := make(map[key]map[string]bool, len(events))

	for _, e := range events {
		k := key{e.Type, e.Reason, e.ObjectKind, e.Message}
		// A count of zero still describes one occurrence: the events API only
		// fills the field in once an event repeats.
		n := int(e.Count)
		if n < 1 {
			n = 1
		}

		i, ok := index[k]
		if !ok {
			index[k] = len(groups)
			seen[k] = map[string]bool{e.ObjectName: true}
			groups = append(groups, EventGroup{Event: e, Occurrences: n, Objects: 1})
			continue
		}

		g := &groups[i]
		g.Occurrences += n
		if names := seen[k]; !names[e.ObjectName] {
			names[e.ObjectName] = true
			g.Objects++
		}
		// Keep the newest member as the group's representative, so the age
		// column reports when this last happened rather than when it started.
		// The group's own FirstTimestamp outlives whichever member supplies the
		// rest of the fields, so it is merged either way.
		first := earliest(g.FirstTimestamp, e.FirstTimestamp)
		if e.LastTimestamp.After(g.LastTimestamp) {
			g.Event = e
		}
		g.FirstTimestamp = first
	}

	sort.SliceStable(groups, func(i, j int) bool {
		// Warnings first, then most recent. A Normal event from a second ago is
		// worth less of the top of the list than a Warning from a minute ago.
		wi, wj := groups[i].Type == "Warning", groups[j].Type == "Warning"
		if wi != wj {
			return wi
		}
		return groups[i].LastTimestamp.After(groups[j].LastTimestamp)
	})
	return groups
}

// earliest returns the older of two timestamps, ignoring zero values: an event
// with no FirstTimestamp has not reported one, which is not the same as having
// happened at the zero time.
func earliest(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero():
		return a
	case b.Before(a):
		return b
	}
	return a
}

// readEvents merges the two event sources, scoping the management side to this
// cluster.
//
// Must run after the machines and hosts are read: eventsFor matches on the
// names they carry, and an empty owned set would drop every management event
// rather than none.
func (d *ClusterDetail) readEvents(s *store.Store) {
	var all []model.Event
	// The workload cluster's events are all its own — they come from its own
	// API server — so they need no scoping.
	if snap, ok := store.Get[model.Snapshot[model.Event]](s, model.KeyWorkloadEvents); ok {
		all = append(all, snap.Items...)
	}
	if snap, ok := store.Get[model.Snapshot[model.Event]](s, model.KeyMgmtEvents); ok {
		mine, elsewhere := eventsFor(snap.Items, d.ownedNames())
		d.EventsElsewhere = elsewhere
		all = append(all, mine...)
	}

	d.setEvents(GroupEvents(all))
}

// setEvents stores grouped events, applying the cap and recording the totals.
// Shared with the demo profile so that the fixture and the real reader agree
// about what the header is counting.
func (d *ClusterDetail) setEvents(groups []EventGroup) {
	d.EventsTotal = 0
	for _, g := range groups {
		d.EventsTotal += g.Occurrences
	}
	if len(groups) > maxEvents {
		d.EventsTruncated = len(groups) - maxEvents
		groups = groups[:maxEvents]
	}
	// Capped before folding, so the cap keeps sixty groups worth reading rather
	// than sixty rows of which fifty are folded out of sight anyway.
	d.Events = splitEvents(groups)
}

// Compact renders a duration the way the tables do: one unit, no decimals.
func Compact(d time.Duration) string {
	switch {
	case d <= 0:
		return "-"
	case d < time.Minute:
		return itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return itoa(int(d.Hours())) + "h"
	default:
		return itoa(int(d.Hours()/24)) + "d"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
