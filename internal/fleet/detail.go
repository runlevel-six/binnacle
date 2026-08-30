package fleet

import (
	"sort"
	"time"

	"github.com/runlevel-six/sextant/pkg/model"
	"github.com/runlevel-six/sextant/pkg/store"
)

// Caps on what one page will render.
//
// A busy cluster produces events and unhealthy pods without limit, and a page
// that renders all of them stops being readable long before it stops being
// correct. Both lists are ordered worst-first, so the cap drops the least
// interesting rows rather than an arbitrary tail.
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

	Machines []model.Machine
	Hosts    []model.BareMetalHost
	// NodeRows is the per-node table. Named for the rows rather than for the
	// nodes because ClusterView already has a Nodes field holding the counts,
	// and an embedded field that shadows its parent's is a silent trap: a
	// template asking for .Nodes.Ready would resolve to this slice and fail at
	// render time, halfway down an already-written page.
	NodeRows []NodeRow

	UnhealthyPods []model.Pod
	// PodsTruncated is how many unhealthy pods are not listed. Shown, because
	// "60 unhealthy pods" and "60 of 400" are different situations, and a page
	// that silently truncates reports the first when it means the second.
	PodsTruncated int

	Events          []model.Event
	EventsTruncated int

	Summaries []SummaryBlock
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
		d.Machines = append(d.Machines, snap.Items...)
		sort.Slice(d.Machines, func(i, j int) bool { return d.Machines[i].Name < d.Machines[j].Name })
	}
	if snap, ok := store.Get[model.Snapshot[model.BareMetalHost]](s, model.KeyMgmtBareMetalHosts); ok {
		d.Hosts = append(d.Hosts, snap.Items...)
		sort.Slice(d.Hosts, func(i, j int) bool {
			// Errored hosts first: a rollout stalls on them more often than on
			// anything else, which is why sextant gives them their own cell.
			ei, ej := d.Hosts[i].ErrorMessage != "", d.Hosts[j].ErrorMessage != ""
			if ei != ej {
				return ei
			}
			return d.Hosts[i].Name < d.Hosts[j].Name
		})
	}

	d.readNodes(s)
	d.readPods(s)
	d.readEvents(s)

	for _, b := range t.registry.Summaries(s) {
		d.Summaries = append(d.Summaries, SummaryBlock{Title: b.Title, Lines: b.Lines})
	}
	return d, true
}

func (d *ClusterDetail) readNodes(s *store.Store) {
	snap, ok := store.Get[model.Snapshot[model.Node]](s, model.KeyWorkloadNodes)
	if !ok {
		return
	}
	for _, n := range snap.Items {
		d.NodeRows = append(d.NodeRows, NodeRow{
			Node:       n,
			CPUPercent: percent(n.RequestedCPU, n.AllocatableCPU),
			MemPercent: percent(n.RequestedMemory, n.AllocatableMemory),
		})
	}
	sort.Slice(d.NodeRows, func(i, j int) bool {
		// Role then name, the same ordering the pools table uses, so a reader
		// moving between the two is not re-learning it.
		if d.NodeRows[i].Role != d.NodeRows[j].Role {
			return d.NodeRows[i].Role < d.NodeRows[j].Role
		}
		return d.NodeRows[i].Name < d.NodeRows[j].Name
	})
}

func (d *ClusterDetail) readPods(s *store.Store) {
	snap, ok := store.Get[model.Snapshot[model.Pod]](s, model.KeyWorkloadPods)
	if !ok {
		return
	}
	var unhealthy []model.Pod
	for _, p := range snap.Items {
		if !p.IsHealthy {
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

func (d *ClusterDetail) readEvents(s *store.Store) {
	var all []model.Event
	for _, key := range []string{model.KeyWorkloadEvents, model.KeyMgmtEvents} {
		if snap, ok := store.Get[model.Snapshot[model.Event]](s, key); ok {
			all = append(all, snap.Items...)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		// Warnings first, then most recent. A Normal event from a second ago is
		// worth less of the top of the list than a Warning from a minute ago.
		wi, wj := all[i].Type == "Warning", all[j].Type == "Warning"
		if wi != wj {
			return wi
		}
		return all[i].LastTimestamp.After(all[j].LastTimestamp)
	})
	if len(all) > maxEvents {
		d.EventsTruncated = len(all) - maxEvents
		all = all[:maxEvents]
	}
	d.Events = all
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
