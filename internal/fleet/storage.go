package fleet

import (
	"sort"
	"strings"

	"github.com/runlevel-six/sextant/pkg/model"
	"github.com/runlevel-six/sextant/pkg/store"
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
// Deliberately no Ceph health. HEALTH_WARN, the OSD counts and the PG state
// arrive through a subsystem plugin keyed to a *workload cluster's* store, and
// there is no datacenter-level source for them, so a panel claiming them here
// would be inventing them. The cluster cards keep that summary, where its data
// comes from.
type StorageCluster struct {
	// FSID is the Ceph cluster's own identifier, from the cluster-id label. It
	// is what makes a datacenter with two Ceph clusters two rather than one.
	FSID string
	// Hosts is ranked and folded like every other host table.
	Hosts Split[model.BareMetalHost]
	// ByRole is the per-role count: cephmon 3/3, cephosd 3/3. A total of 6/6 is
	// reassuring right up until the missing one is the last monitor, which is
	// the same reason the cards carry node readiness per role.
	ByRole []RoleCount
}

// Storage is the datacenter's storage layer, and what could be said about it.
type Storage struct {
	// Clusters is one entry per fsid, worst first.
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
func StorageFor(hosts []model.BareMetalHost) Storage {
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

	for _, fsid := range order {
		group := byFSID[fsid]
		s.Clusters = append(s.Clusters, StorageCluster{
			FSID:   fsid,
			Hosts:  splitHosts(group),
			ByRole: storageRoles(group),
		})
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

// Degraded reports whether any of this cluster's hardware needs looking at.
func (c StorageCluster) Degraded() bool { return c.worst() < hostQuiet }

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
	stores := make([]*store.Store, 0, len(f.clusters))
	for _, t := range f.clusters {
		stores = append(stores, t.store)
	}
	f.mu.Unlock()

	for _, s := range stores {
		snap, ok := store.Get[model.Snapshot[model.BareMetalHost]](s, model.KeyMgmtBareMetalHosts)
		if !ok || snap.Err != nil {
			continue
		}
		return StorageFor(snap.Items)
	}
	// Nothing readable. Known stays false, so the page can say it does not know
	// rather than that there is nothing there.
	return Storage{}
}
