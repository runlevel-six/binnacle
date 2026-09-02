package fleet

import (
	"sort"
	"strings"

	"github.com/runlevel-six/binnacle/pkg/model"
	"github.com/runlevel-six/binnacle/pkg/store"
	"github.com/runlevel-six/binnacle/pkg/subsystem/ceph"
)

// Label keys the hardware inventory sets on every BareMetalHost.
//
// Neither is a Kubernetes convention; both are this fleet's own, which is why
// they are named here rather than in sextant. sextant carries the labels
// unfiltered and knows about none of them.
const (
	// LabelRole says what a host is: cephosd, cephmon, controller, compute,
	// managed-services.
	LabelRole = "atmosphere-role"
	// LabelClusterID groups hosts, and means two different things depending on
	// the role. On a Ceph host it is the Ceph fsid. On an undercloud host it is
	// a Cluster API cluster shortname. See StorageFor for why that matters.
	LabelClusterID = "cluster-id"
	// cephRolePrefix matches every Ceph role. A prefix rather than a list, so a
	// cephmgr or a cephrgw is not silently dropped from the storage layer the
	// day somebody deploys one.
	cephRolePrefix = "ceph"
)

// StorageCluster is one Ceph cluster's hardware, as the management cluster's
// host inventory describes it.
//
// It is not a [ClusterView]. Nothing binnacle otherwise models hangs off
// anything but a Cluster API Cluster — the credentials, the store, the health
// cells — and a storage layer has none of that. What it has is hosts, which is
// what this reports.
//
// Its health is borrowed rather than collected, and rendered from the typed
// state rather than from the plugin's own text — see the ceph-status template
// for why that is presentation rather than a second opinion. Ceph's own status arrives
// through a subsystem plugin keyed to a *workload cluster's* store, and there
// is no datacenter-level collector — but `ceph -s` reports the fsid, and the
// fsid is exactly what the cluster-id label holds, so a cluster's report can be
// attributed to a storage cluster with no guessing. That is what makes this
// work in a datacenter with two Ceph clusters as well as one with a shared one.
type StorageCluster struct {
	// FSID is the Ceph cluster's own identifier: the cluster-id label on its
	// hosts, and the fsid Ceph itself reports. It is what makes a datacenter
	// with two Ceph clusters two rather than one, and it is what joins the two
	// halves of this type together.
	FSID string
	// Hosts is ranked and folded like every other host table. Empty when no
	// host carries this fsid, which happens in a datacenter whose hardware
	// inventory has not been labeled: Ceph still reports itself, so the panel
	// still has something true to say.
	Hosts Split[model.BareMetalHost]
	// ByRole is the per-role count: cephmon 3/3, cephosd 3/3. A total of 6/6 is
	// reassuring right up until the missing one is the last monitor, which is
	// the same reason the cards carry node readiness per role.
	ByRole []RoleCount
	// Status is Ceph's own account of itself, as one of the clusters using it
	// reports. Nil when no cluster reports this fsid — which is a fact worth
	// saying rather than an empty section: hardware labeled for a Ceph nobody
	// is talking to.
	Status *ceph.Status
	// ReportedBy names the clusters reporting this fsid, in the order the fleet
	// lists them. Several is the normal case for a shared Ceph and is worth
	// showing: it is what says this storage is shared rather than one
	// cluster's. Namespaced, because that is what a link to a cluster page
	// needs and a bare name cannot supply it.
	ReportedBy []ClusterRef
}

// ClusterRef is enough of a cluster to name it and link to it.
type ClusterRef struct {
	Namespace string
	Name      string
}

// Storage is the datacenter's storage layer, and what could be said about it.
type Storage struct {
	// Clusters is one entry per fsid, worst first. An fsid earns an entry by
	// appearing on a host's labels or in a cluster's Ceph report — either half
	// is worth showing on its own, and which halves are missing is itself the
	// most useful thing the panel can say about a site mid-rollout.
	Clusters []StorageCluster
	// Unlabeled is how many unclaimed hosts carry no role label.
	//
	// The distinction this exists for: a site whose hardware inventory has not
	// been labeled yet looks exactly like a site with no storage in it, and must
	// not render as one. NodesKnown and WorkloadProblem exist for the same
	// reason — silence from a source must not read as good news.
	Unlabeled int
	// Known is false when no host inventory could be read at all, which is
	// different again from reading one that holds no Ceph.
	Known bool
}

// CephReport is one cluster's account of the Ceph it uses.
//
// The fsid is what makes it attributable. Everything else is borrowed from the
// plugin that produced it: a datacenter panel that re-derived Ceph's health from
// the same fields would eventually disagree with the cluster pane that renders
// them, and there would be no way to tell which was right.
type CephReport struct {
	// Cluster names the cluster reporting, for attribution and for a link.
	Cluster ClusterRef
	Status  ceph.Status
}

// RoleHosts counts hosts by their role label, for one storage cluster.
type roleTally struct {
	role  string
	total int
	ready int
}

// StorageFor groups a datacenter's hosts into storage clusters.
//
// **The order of the two steps is the whole correctness of this function.**
// Filter to Ceph roles first, then group the survivors by cluster-id. Grouping
// by cluster-id first produces the real storage clusters *plus* one invented
// storage cluster per undercloud — named after the undercloud, full of its
// controllers and computes — because that label holds an fsid on a Ceph host
// and a cluster shortname on an undercloud host. Confirmed against two
// datacenters on 2026-09-01, and it is the one mistake here that would look
// entirely plausible on the page.
//
// Hosts with a Ceph role and no cluster-id are grouped under an empty fsid
// rather than dropped: that is a labeling job half done, and hiding it would
// make the hardware disappear.
//
// Reports are the clusters' own Ceph status, joined on the fsid. A report with
// no matching hosts still gets a panel: an unlabeled datacenter has no Ceph
// hosts to find, and the storage layer is no less real for that — it is the
// hardware behind it that is unidentified, which is what the panel then says.
func StorageFor(hosts []model.BareMetalHost, reports []CephReport) Storage {
	s := Storage{Known: true}

	byFSID := map[string][]model.BareMetalHost{}
	var order []string
	for _, h := range hosts {
		role := h.Labels[LabelRole]
		if role == "" {
			// Only unclaimed hardware counts as unlabeled. A consumed host with
			// no labels is a cluster's own machine, already on that cluster's
			// page, and says nothing about the storage layer.
			if h.ConsumerName == "" {
				s.Unlabeled++
			}
			continue
		}
		if !strings.HasPrefix(role, cephRolePrefix) {
			continue
		}
		fsid := h.Labels[LabelClusterID]
		if _, seen := byFSID[fsid]; !seen {
			order = append(order, fsid)
		}
		byFSID[fsid] = append(byFSID[fsid], h)
	}

	// An fsid nobody has labeled hardware for still earns its panel from the
	// report alone. Appended after the labeled ones so the order stays stable.
	byReport := map[string][]CephReport{}
	for _, r := range reports {
		fsid := r.Status.FSID
		if fsid == "" {
			// A report with no fsid cannot be attributed to anything, and
			// guessing which storage cluster it describes is exactly the kind of
			// invention this design exists to avoid.
			continue
		}
		if _, seen := byFSID[fsid]; !seen {
			if _, seen := byReport[fsid]; !seen {
				order = append(order, fsid)
			}
		}
		byReport[fsid] = append(byReport[fsid], r)
	}

	for _, fsid := range order {
		group := byFSID[fsid]
		c := StorageCluster{
			FSID:   fsid,
			Hosts:  splitHosts(group),
			ByRole: storageRoles(group),
		}
		for _, r := range byReport[fsid] {
			c.ReportedBy = append(c.ReportedBy, r.Cluster)
			if c.Status == nil {
				// The first report wins the detail. Several clusters using one
				// Ceph are describing the same thing, so the choice does not
				// matter; who is reporting does, and every one of them is named.
				status := r.Status
				c.Status = &status
			}
		}
		s.Clusters = append(s.Clusters, c)
	}
	sort.SliceStable(s.Clusters, func(i, j int) bool {
		// Worst first, by the same host rank the table is ordered on, then by
		// fsid so the panel does not reshuffle itself between updates.
		wi, wj := s.Clusters[i].worst(), s.Clusters[j].worst()
		if wi != wj {
			return wi < wj
		}
		return s.Clusters[i].FSID < s.Clusters[j].FSID
	})
	return s
}

// worst is the rank of the most troubled host in the cluster.
func (c StorageCluster) worst() int {
	worst := hostQuiet
	for _, h := range c.Hosts.All() {
		if r := hostRank(h); r < worst {
			worst = r
		}
	}
	return worst
}

// Degraded reports whether this storage cluster needs looking at, by either
// half: hardware the inventory is complaining about, or Ceph's own verdict on
// itself. Both, because they fail independently — a HEALTH_WARN with every host
// provisioned cleanly is the normal shape of a Ceph problem.
func (c StorageCluster) Degraded() bool {
	if c.worst() < hostQuiet {
		return true
	}
	return c.Status != nil && !c.Status.HealthOK()
}

// Silent reports hardware labeled for a Ceph that no cluster is talking to.
//
// Its own state rather than a missing section: the panel has to distinguish "no
// cluster reports this fsid" from "this Ceph is fine", and an absent health
// block on its own reads as the second.
func (c StorageCluster) Silent() bool { return c.Status == nil }

// Short is the first eight characters of the fsid.
//
// A UUID is unreadable as a heading, but a datacenter can hold two storage
// clusters and they have to be told apart, so the identity cannot be dropped
// either. Eight characters is how an operator recognizes an fsid in practice.
func (c StorageCluster) Short() string {
	if len(c.FSID) <= 8 {
		return c.FSID
	}
	return c.FSID[:8]
}

// storageRoles counts each Ceph role's hosts and how many are provisioned
// without error.
func storageRoles(hosts []model.BareMetalHost) []RoleCount {
	tallies := map[string]*roleTally{}
	var order []string
	for _, h := range hosts {
		role := h.Labels[LabelRole]
		t, ok := tallies[role]
		if !ok {
			t = &roleTally{role: role}
			tallies[role] = t
			order = append(order, role)
		}
		t.total++
		if hostRank(h) >= hostQuiet {
			t.ready++
		}
	}
	sort.Strings(order)
	out := make([]RoleCount, 0, len(order))
	for _, role := range order {
		t := tallies[role]
		out = append(out, RoleCount{Role: t.role, Ready: t.ready, Total: t.total})
	}
	return out
}

// Storage returns the datacenter's storage layer.
//
// # Where the hosts come from, and why it is borrowed
//
// The BareMetalHost snapshot is namespace-wide — it describes every host in the
// building, which is the whole reason hostsFor exists — and every per-cluster
// collector holds one, because they all watch the same namespace on the same
// management cluster. So the lists are identical by construction and any of
// them will do; this takes the first that has arrived without an error.
//
// It is still a borrowed read, and two consequences follow. A datacenter with
// Ceph and no underclouds has no store to borrow from and reports no storage,
// which is wrong in principle and harmless today. And this is the honest
// argument for one shared management watcher feeding per-cluster views: that
// would make the datacenter's own hardware a first-class read rather than a
// side effect of watching a cluster.
func (f *Fleet) Storage() Storage {
	f.mu.Lock()
	tracked := make([]*tracked, 0, len(f.clusters))
	for _, t := range f.clusters {
		tracked = append(tracked, t)
	}
	f.mu.Unlock()

	// Named order, so the reporting clusters are listed the same way twice
	// running and the panel does not reshuffle under a reader.
	sort.Slice(tracked, func(i, j int) bool {
		a, b := tracked[i].discovered, tracked[j].discovered
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})

	var hosts []model.BareMetalHost
	var found bool
	var reports []CephReport
	for _, t := range tracked {
		if !found {
			if snap, ok := store.Get[model.Snapshot[model.BareMetalHost]](t.store, model.KeyMgmtBareMetalHosts); ok && snap.Err == nil {
				hosts, found = snap.Items, true
			}
		}
		if state, ok := store.Get[ceph.State](t.store, ceph.KeyState); ok {
			reports = append(reports, CephReport{
				Cluster: ClusterRef{Namespace: t.discovered.Namespace, Name: t.discovered.Name},
				Status:  state.Status,
			})
		}
	}

	if !found && len(reports) == 0 {
		// Nothing readable at all. Known stays false, so the page can say it
		// does not know rather than that there is nothing there.
		return Storage{}
	}
	s := StorageFor(hosts, reports)
	// The hosts are what could not be read, not the reports: a site whose Ceph
	// answers while the host inventory does not is a real state and the panel
	// should not claim otherwise.
	s.Known = found
	return s
}
