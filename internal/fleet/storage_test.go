package fleet

import (
	"testing"

	"github.com/runlevel-six/sextant/pkg/model"
)

// cephHost builds a labeled Ceph host.
func cephHost(name, role, fsid string) model.BareMetalHost {
	return model.BareMetalHost{
		Namespace: "machines", Name: name, State: "provisioned", OperationalStatus: "OK",
		Labels: map[string]string{LabelRole: role, LabelClusterID: fsid},
	}
}

// undercloud builds a host consumed by a cluster's machine, labeled the way the
// inventory labels one: a role, and a cluster-id that is a CAPI shortname
// rather than an fsid.
func undercloud(name, role, shortname string) model.BareMetalHost {
	return model.BareMetalHost{
		Namespace: "machines", Name: name, State: "provisioned", OperationalStatus: "OK",
		ConsumerNamespace: "machines", ConsumerName: name + "-m3m",
		Labels: map[string]string{LabelRole: role, LabelClusterID: shortname},
	}
}

// The mistake that would look entirely plausible on the page. cluster-id holds
// an fsid on a Ceph host and a Cluster API shortname on an undercloud host, so
// grouping by it before filtering by role invents one storage cluster per
// undercloud, named after the undercloud and full of its controllers.
func TestStorageFor_UndercloudHostsAreNotAStorageCluster(t *testing.T) {
	s := StorageFor([]model.BareMetalHost{
		undercloud("a03-17-controller", "controller", "k8s00"),
		undercloud("a03-20-compute", "controller", "k8s01"),
		undercloud("a03-26-compute", "managed-services", "k8s00"),
		cephHost("a03-05-cephosd", "cephosd", "fsid-one"),
		cephHost("a03-11-cephmon", "cephmon", "fsid-one"),
	})

	if len(s.Clusters) != 1 {
		var got []string
		for _, c := range s.Clusters {
			got = append(got, c.FSID)
		}
		t.Fatalf("grouped into %d storage clusters (%v), want 1", len(s.Clusters), got)
	}
	if s.Clusters[0].FSID != "fsid-one" {
		t.Errorf("fsid = %q, want fsid-one", s.Clusters[0].FSID)
	}
	if n := s.Clusters[0].Hosts.Total(); n != 2 {
		t.Errorf("storage cluster holds %d hosts, want the 2 Ceph ones", n)
	}
	// A labeled, consumed host is somebody's machine, not unlabeled hardware.
	if s.Unlabeled != 0 {
		t.Errorf("Unlabeled = %d, want 0", s.Unlabeled)
	}
}

// One datacenter has two Ceph clusters, for isolation, and the mapping is in
// the data rather than in any configuration.
func TestStorageFor_TwoFSIDsAreTwoClusters(t *testing.T) {
	s := StorageFor([]model.BareMetalHost{
		cephHost("r0102-01-cephosd", "cephosd", "fsid-a"),
		cephHost("r0306-01-cephosd", "cephosd", "fsid-b"),
		cephHost("r0307-09-cephmon", "cephmon", "fsid-b"),
	})

	if len(s.Clusters) != 2 {
		t.Fatalf("got %d storage clusters, want 2", len(s.Clusters))
	}
	byFSID := map[string]int{}
	for _, c := range s.Clusters {
		byFSID[c.FSID] = c.Hosts.Total()
	}
	if byFSID["fsid-a"] != 1 || byFSID["fsid-b"] != 2 {
		t.Errorf("hosts per fsid = %v, want fsid-a:1 fsid-b:2", byFSID)
	}
}

// A role list would drop a cephmgr the day somebody deploys one, and it would
// do it silently.
func TestStorageFor_MatchesEveryCephRole(t *testing.T) {
	s := StorageFor([]model.BareMetalHost{
		cephHost("h1", "cephosd", "fsid"),
		cephHost("h2", "cephmon", "fsid"),
		cephHost("h3", "cephmgr", "fsid"),
		cephHost("h4", "cephrgw", "fsid"),
		cephHost("h5", "compute", "fsid"),
	})

	if len(s.Clusters) != 1 {
		t.Fatalf("got %d storage clusters, want 1", len(s.Clusters))
	}
	if n := s.Clusters[0].Hosts.Total(); n != 4 {
		t.Errorf("kept %d hosts, want the 4 ceph roles", n)
	}
	roles := map[string]RoleCount{}
	for _, r := range s.Clusters[0].ByRole {
		roles[r.Role] = r
	}
	for _, want := range []string{"cephosd", "cephmon", "cephmgr", "cephrgw"} {
		if _, ok := roles[want]; !ok {
			t.Errorf("role %q was dropped", want)
		}
	}
	if _, ok := roles["compute"]; ok {
		t.Error("a compute host was counted as storage")
	}
}

// The rule that is not optional: an unlabeled datacenter must not render as a
// datacenter with no storage.
func TestStorageFor_UnlabeledHardwareIsCounted(t *testing.T) {
	s := StorageFor([]model.BareMetalHost{
		// Unclaimed and unlabeled: could be anything, including Ceph.
		{Namespace: "machines", Name: "spare-1", State: "available"},
		{Namespace: "machines", Name: "spare-2", State: "available"},
		// Claimed and unlabeled: somebody's machine, already on their page.
		{Namespace: "machines", Name: "in-use", ConsumerName: "cluster-kcp-abc"},
	})

	if len(s.Clusters) != 0 {
		t.Errorf("invented %d storage clusters from unlabeled hosts", len(s.Clusters))
	}
	if s.Unlabeled != 2 {
		t.Errorf("Unlabeled = %d, want 2 (the unclaimed hosts only)", s.Unlabeled)
	}
	if !s.Known {
		t.Error("Known should be true: the inventory was read, it just says little")
	}
}

// Reading no inventory at all is a third state, distinct from an unlabeled site
// and from a site with no Ceph.
func TestStorage_UnreadableInventoryIsNotAnEmptyOne(t *testing.T) {
	f := &Fleet{clusters: map[string]*tracked{}}
	s := f.Storage()

	if s.Known {
		t.Error("Known is true with no cluster to borrow a snapshot from")
	}
	if len(s.Clusters) != 0 || s.Unlabeled != 0 {
		t.Errorf("got %+v, want nothing claimed", s)
	}
}

// A Ceph host with no fsid is a labeling job half done. Showing it under an
// empty fsid is honest; dropping it makes hardware disappear.
func TestStorageFor_CephHostWithNoFSIDIsStillShown(t *testing.T) {
	s := StorageFor([]model.BareMetalHost{
		{Namespace: "machines", Name: "a03-05-cephosd", State: "provisioned",
			Labels: map[string]string{LabelRole: "cephosd"}},
	})

	if len(s.Clusters) != 1 {
		t.Fatalf("got %d storage clusters, want 1", len(s.Clusters))
	}
	if s.Clusters[0].FSID != "" {
		t.Errorf("fsid = %q, want empty", s.Clusters[0].FSID)
	}
	if got := s.Clusters[0].Short(); got != "" {
		t.Errorf("Short() = %q, want empty", got)
	}
}

// Hardware complaining sorts the cluster to the top and marks it degraded, the
// same rank the host tables are ordered on.
func TestStorageFor_ErroredHardwareRanksFirst(t *testing.T) {
	broken := cephHost("r0104-03-cephosd", "cephosd", "fsid-broken")
	broken.OperationalStatus = "error"
	broken.ErrorMessage = "timed out waiting for the deploy image"

	s := StorageFor([]model.BareMetalHost{
		cephHost("r0102-01-cephmon", "cephmon", "fsid-fine"),
		broken,
	})

	if len(s.Clusters) != 2 {
		t.Fatalf("got %d storage clusters, want 2", len(s.Clusters))
	}
	if s.Clusters[0].FSID != "fsid-broken" {
		t.Errorf("first cluster is %q, want the one with errored hardware", s.Clusters[0].FSID)
	}
	if !s.Clusters[0].Degraded() {
		t.Error("a cluster with an errored host is not reported degraded")
	}
	if s.Clusters[1].Degraded() {
		t.Error("a healthy storage cluster is reported degraded")
	}
	// Per-role readiness is what a single total cannot say.
	if r := s.Clusters[0].ByRole[0]; r.Ready != 0 || r.Total != 1 {
		t.Errorf("cephosd = %d/%d, want 0/1", r.Ready, r.Total)
	}
}

// An fsid is a UUID and unreadable as a heading, but two of them have to be
// told apart.
func TestStorageCluster_ShortIsTheFSIDPrefix(t *testing.T) {
	c := StorageCluster{FSID: "24413730-08bc-11ef-b140-23a2dd2fc842"}
	if got := c.Short(); got != "24413730" {
		t.Errorf("Short() = %q, want 24413730", got)
	}
}
