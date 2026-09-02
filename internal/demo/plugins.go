package demo

import (
	"context"
	"time"

	"github.com/runlevel-six/binnacle/internal/plugin/ceph"
	"github.com/runlevel-six/binnacle/internal/plugin/cilium"
	"github.com/runlevel-six/binnacle/internal/plugin/kube"
	"github.com/runlevel-six/binnacle/internal/plugin/metallb"
	"github.com/runlevel-six/binnacle/internal/plugin/openstack"
	"github.com/runlevel-six/binnacle/internal/plugin/ovn"
	"github.com/runlevel-six/binnacle/pkg/store"
)

// Plugins returns the five built-in plugins wrapped so they detect as present
// and poll nothing.
//
// The wrappers embed the real plugins, so Name, Panes and Cells are the real
// implementations reading the real store keys — only acquisition is replaced.
// The embedded plugins are constructed with nil clients, and that is safe by
// construction rather than by luck: nothing but each plugin's poll method
// touches a client, and poll is reachable only from the Run this shadows.
func Plugins() []interface{ Name() string } {
	return []interface{ Name() string }{
		// Same order as internal/app/plugins.go, which decides left-to-right
		// placement among equal priorities. A demo that ordered them differently
		// would render a layout the real dashboard never produces.
		demoMetalLB{metallb.New(nil, nil, metallb.Settings{})},
		demoCilium{cilium.New(nil, cilium.Settings{})},
		demoCeph{ceph.New(nil, ceph.Settings{})},
		demoOVN{ovn.New(nil, ovn.Settings{})},
		demoOpenStack{openstack.New(openstack.Settings{
			Cloud: "demo", TargetVersion: TargetVersion,
		})},
	}
}

// The wrappers are one concrete type per plugin, each *embedding* its plugin, and
// that is not incidental verbosity. A generic wrapper cannot work: Go does not
// permit embedding a type parameter, so a generic version has to hold the plugin
// in a named field — and a named field promotes no methods, so Panes and Cells
// stop satisfying the registry's type assertions and every plugin pane silently
// vanishes from the dashboard. Embedding also means a plugin that grows a new
// provider interface is picked up here for free rather than needing a new
// forwarding method nobody will remember to write.
//
// Detect and Run shadow the embedded implementations. Detect must report present
// because Registry.Panes filters on the active set; Run publishes nothing because
// the fixture is already in the store, which keeps every invented value in one
// file rather than split between a fixture and a set of fake sources.

type demoMetalLB struct{ *metallb.Plugin }

func (demoMetalLB) Detect(context.Context) (bool, error)    { return true, nil }
func (demoMetalLB) Run(context.Context, *store.Store) error { return nil }

type demoCilium struct{ *cilium.Plugin }

func (demoCilium) Detect(context.Context) (bool, error)    { return true, nil }
func (demoCilium) Run(context.Context, *store.Store) error { return nil }

type demoOVN struct{ *ovn.Plugin }

func (demoOVN) Detect(context.Context) (bool, error)    { return true, nil }
func (demoOVN) Run(context.Context, *store.Store) error { return nil }

type demoCeph struct{ *ceph.Plugin }

func (demoCeph) Detect(context.Context) (bool, error)    { return true, nil }
func (demoCeph) Run(context.Context, *store.Store) error { return nil }

type demoOpenStack struct{ *openstack.Plugin }

func (demoOpenStack) Detect(context.Context) (bool, error)    { return true, nil }
func (demoOpenStack) Run(context.Context, *store.Store) error { return nil }

// putPlugins fills the keys the plugin panes read.
//
// As with the core fixture, this is a working cluster with real problems rather
// than an all-green one: Ceph is rebalancing after losing an OSD with the node,
// one Cilium controller is failing, and a nova-compute agent is down.
func putPlugins(s *store.Store, now time.Time) {
	s.Put(metallb.KeyState, metallb.State{
		Namespace:      "metallb-system",
		SpeakerReady:   4,
		SpeakerDesired: 5, // compute-node-3 is gone, so one speaker is missing.
		Pools: []metallb.Pool{
			{
				Namespace: "metallb-system", Name: "default",
				Addresses:  []string{"192.0.2.200-192.0.2.249"},
				AutoAssign: true, Advertised: []string{"L2"},
				// Fifty addresses, three out. The counts are MetalLB's own, as
				// they are on any release that publishes a pool status.
				Assigned: 3, Available: 47, Usage: metallb.UsageStatus,
			},
			{
				// Deliberately unadvertised: a pool that hands out addresses
				// nothing announces, which is the silent misconfiguration the
				// pane exists to surface.
				Namespace: "metallb-system", Name: "reserved",
				Addresses:  []string{"192.0.2.250-192.0.2.254"},
				AutoAssign: false,
				Assigned:   0, Available: 5, Usage: metallb.UsageStatus,
			},
		},
		Services: []metallb.Service{
			{Namespace: "ingress", Name: "ingress-nginx", ExternalIP: "192.0.2.200", Pool: "default"},
			{Namespace: "monitoring", Name: "grafana", ExternalIP: "192.0.2.201", Pool: "default"},
			{Namespace: "demo-apps", Name: "api-gateway", ExternalIP: "192.0.2.202", Pool: "default"},
		},
		UpdatedAt: now,
	})

	s.Put(cilium.KeyState, cilium.State{
		Tier:          kube.TierFull,
		AgentsReady:   4,
		AgentsDesired: 5,
		// The agent DaemonSet is part-way through its own roll, which readiness
		// alone would not reveal.
		Rollout: kube.Rollout{Desired: 5, Updated: 4, Ready: 4},
		Pod:     "cilium-k7d2n",
		Status: cilium.Status{
			Version:              "1.17.1",
			State:                "Ok",
			KubeProxyReplacement: "true",
			EncryptionMode:       "WireGuard",
			IPAM:                 cilium.IPAM{Used: 132, Available: 254},
			Hubble:               cilium.Hubble{Enabled: true, State: "Ok", SeenFlows: 41_882_713, FlowsPerSecond: 1904.5},
			Controllers:          cilium.Controllers{Total: 748, Failing: 1},
		},
		UpdatedAt: now,
	})

	s.Put(ovn.KeyState, ovn.State{
		Tier: kube.TierFull,
		Statuses: []ovn.ClusterStatus{
			raft("OVN_Northbound", "nb"),
			raft("OVN_Southbound", "sb"),
		},
		// Mid-upgrade, in the order OVN requires: databases and northd are
		// through, the per-host controllers are rolling, and Open vSwitch has not
		// moved because it cannot move on its own. That last row is the state no
		// other tool reports — see [kube.Rollout].
		Components: []ovn.Component{
			{Name: "ovsdb-nb", Rollout: kube.Rollout{Desired: 3, Updated: 3, Ready: 3}},
			{Name: "ovsdb-sb", Rollout: kube.Rollout{Desired: 3, Updated: 3, Ready: 3}},
			{Name: "ovn-northd", Rollout: kube.Rollout{Desired: 3, Updated: 3, Ready: 3}},
			{Name: "ovn-controller", Rollout: kube.Rollout{
				Desired: 5, Updated: 3, Ready: 5,
				StaleNodes: []string{
					"compute-node-2.site-a.demo.example",
					"compute-node-3.site-a.demo.example",
				},
			}},
			{Name: "openvswitch", Rollout: kube.Rollout{
				Desired: 5, Updated: 1, Ready: 5, Manual: true,
				StaleNodes: []string{
					"compute-node-1.site-a.demo.example",
					"compute-node-2.site-a.demo.example",
					"compute-node-3.site-a.demo.example",
					"control-node-3.site-a.demo.example",
				},
			}},
		},
		UpdatedAt: now,
	})

	s.Put(ceph.KeyState, ceph.State{
		Tier: kube.TierFull,
		Pod:  "rook-ceph-tools-6d4f8b9c7-x2mkq",
		Status: ceph.Status{
			FSID: "1f2e3d4c-5b6a-7980-a1b2-c3d4e5f60718",
			// Rebalancing after compute-node-3 took an OSD with it. Not an outage, but
			// not the all-clear either, which is the interesting middle state.
			Health: "HEALTH_WARN",
			Checks: []ceph.Check{{
				Name: "OSD_DOWN", Severity: "HEALTH_WARN",
				Message: "1 osds down",
			}},
			Mons: ceph.Mons{Total: 3, InQuorum: 3},
			Mgr:  ceph.Mgr{Available: true, Standbys: 2, Active: "a", Modules: 6},
			OSDs: ceph.OSDs{Total: 30, Up: 29, In: 29, RemappedPG: 41, Epoch: 8842},
			PGs: ceph.PGs{
				Total: 1041, Pools: 21, Objects: 4_812_006,
				// Without the breakdown the pane reads "0/1041 clean", which
				// looks like a total outage rather than a rebalance.
				ByState: []ceph.PGState{
					{Name: "active+clean", Count: 1000},
					{Name: "active+remapped+backfilling", Count: 33},
					{Name: "active+undersized+degraded", Count: 8},
				},
				DataBytes:  9_895_604_649_984,
				UsedBytes:  31_268_536_614_912,
				AvailBytes: 106_326_205_071_360,
				TotalBytes: 137_594_741_686_272,
			},
			IO: ceph.IO{
				ReadBytesPerSec: 12_582_912, WriteBytesPerSec: 27_262_976,
				ReadOpsPerSec: 1841, WriteOpsPerSec: 2204,
			},
		},
		UpdatedAt: now,
	})

	s.Put(openstack.KeyState, openstack.State{
		Cloud:  "demo",
		Region: "RegionOne",
		Services: []openstack.ServiceSummary{
			{Service: "compute", Total: 8, Up: 7, DownBinaries: []string{"nova-compute"}},
			{Service: "network", Total: 10, Up: 10},
			{Service: "block-storage", Total: 3, Up: 3},
		},
		Agents: []openstack.Agent{
			{Service: "compute", Binary: "nova-conductor", Host: "control-node-1", Zone: "internal", Up: true, Enabled: true, UpdatedAt: now},
			{Service: "compute", Binary: "nova-scheduler", Host: "control-node-1", Zone: "internal", Up: true, Enabled: true, UpdatedAt: now},
			{Service: "compute", Binary: "nova-compute", Host: "compute-node-1", Zone: "nova", Up: true, Enabled: true, UpdatedAt: now},
			{Service: "compute", Binary: "nova-compute", Host: "compute-node-2", Zone: "nova", Up: true, Enabled: false, UpdatedAt: now},
			{Service: "compute", Binary: "nova-compute", Host: "compute-node-3", Zone: "nova", Up: false, Enabled: true, UpdatedAt: now.Add(-14 * time.Minute)},
			{Service: "network", Binary: "neutron-ovn-metadata-agent", Host: "compute-node-1", Zone: "", Up: true, Enabled: true, UpdatedAt: now},
			{Service: "network", Binary: "neutron-ovn-metadata-agent", Host: "compute-node-2", Zone: "", Up: true, Enabled: true, UpdatedAt: now},
			{Service: "block-storage", Binary: "cinder-volume", Host: "control-node-1", Zone: "nova", Up: true, Enabled: true, UpdatedAt: now},
		},
		UpdatedAt: now,
	})

	putOpenStackWork(s, now)
}

// putOpenStackWork fills the two keys the mode-aware cloud pane switches
// between. Both are filled, not just the one a rollout shows, so a screenshot
// taken outside rollout mode is not left with a pane saying "polling…".
func putOpenStackWork(s *store.Store, now time.Time) {
	s.Put(openstack.KeyMigrations, openstack.Migrations{
		Items: []openstack.Migration{
			{
				ID: 4471, Status: "migrating", Type: "live-migration",
				InstanceUUID:  "6f1c2b8e-4a3d-4f19-9c7e-2b8a5d1e0f34",
				SourceCompute: "compute-node-3", DestCompute: "compute-node-1",
				CreatedAt: now.Add(-9 * time.Minute), UpdatedAt: now.Add(-40 * time.Second),
			},
			{
				ID: 4472, Status: "preparing", Type: "live-migration",
				InstanceUUID:  "b3d9a7f2-1c8e-4b60-8f2a-9e7c4d3b1a58",
				SourceCompute: "compute-node-3", DestCompute: "compute-node-2",
				CreatedAt: now.Add(-3 * time.Minute), UpdatedAt: now.Add(-12 * time.Second),
			},
			{
				ID: 4468, Status: "error", Type: "live-migration",
				InstanceUUID:  "e8c4b1a9-7d2f-4e35-b6c8-1a9f3e7d2b40",
				SourceCompute: "compute-node-3", DestCompute: "compute-node-1",
				CreatedAt: now.Add(-22 * time.Minute), UpdatedAt: now.Add(-18 * time.Minute),
			},
			{
				// Two days old, instance still broken, and nobody is draining
				// the host it landed on: the summary counts it and zoom lists
				// it. This is the row the age window used to throw away.
				ID: 4102, Status: "error", Type: "live-migration",
				InstanceUUID:  "c7a2f5d1-9b34-4e08-a1d6-5f8b2c4e9037",
				SourceCompute: "compute-node-4", DestCompute: "compute-node-2",
				CreatedAt: now.Add(-49 * time.Hour), UpdatedAt: now.Add(-48 * time.Hour),
			},
		},
		// The instance behind 4468 recovered; the one behind 4102 did not. The
		// pane colors those two failures differently because of this.
		BrokenKnown: true,
		Broken: map[string]openstack.BrokenServer{
			"c7a2f5d1-9b34-4e08-a1d6-5f8b2c4e9037": {
				UUID: "c7a2f5d1-9b34-4e08-a1d6-5f8b2c4e9037",
				Name: "web-frontend-07",
				Host: "compute-node-2",
				Fault: "Live migration operation has aborted, " +
					"instance is not running on destination",
			},
		},
		Draining: map[string]bool{"compute-node-3": true},
		// Nine servers still on the host, two of them moving, and one that
		// cannot move at all — the shape of a drain most of the way through
		// with something in its way.
		Drains:    []openstack.Drain{{Host: "compute-node-3", Remaining: 9, Moving: 2, Stuck: 1}},
		UpdatedAt: now,
	})

	s.Put(openstack.KeyInventory, openstack.Inventory{
		Counts:    inventoryCounts(),
		UpdatedAt: now,
	})
}

// inventoryCounts is the resource census the cloud pane shows when no rollout is
// in flight. Octavia is deliberately absent rather than errored: a cloud without
// it is correctly configured, and the pane distinguishes the two.
func inventoryCounts() []openstack.Count {
	return []openstack.Count{
		{Label: "Servers", Total: 148, ByState: map[string]int{
			"ACTIVE": 141, "SHUTOFF": 5, "ERROR": 2,
		}},
		{Label: "Volumes", Total: 302, ByState: map[string]int{
			"in-use": 288, "available": 13, "error": 1,
		}},
		{Label: "Networks", Total: 24},
		{Label: "Routers", Total: 11},
		{Label: "Floating IPs", Total: 63},
		{Label: "Load Balancers", Absent: true},
	}
}

// raft builds a healthy three-member Raft cluster, seen from the leader.
//
// From the leader on purpose. Only the leader hears from followers, so a
// follower's view of another follower's last message measures the age of the last
// election rather than anything about that peer — the false alarm that cost real
// time in M4. A fixture that captured a follower's view would teach the pane's
// reader the wrong thing about what the numbers mean.
func raft(database, short string) ovn.ClusterStatus {
	return ovn.ClusterStatus{
		Database:  database,
		ClusterID: "c0ffee" + short,
		ServerID:  "aaa1",
		Status:    "cluster member",
		Role:      "leader",
		Leader:    "self",
		Term:      318,
		Address:   "ssl:192.0.2.11:6643",
		Pod:       "ovsdb-" + short + "-0",
		LogLow:    1204, LogHigh: 41882,
		Servers: []ovn.Server{
			{ID: "aaa1", Name: "ovsdb-" + short + "-0", Address: "ssl:192.0.2.11:6643", Self: true},
			{ID: "bbb2", Name: "ovsdb-" + short + "-1", Address: "ssl:192.0.2.13:6643",
				LastMsg: 212 * time.Millisecond, LastMsgKnown: true, MatchIndex: 41882, MatchIndexKnown: true},
			{ID: "ccc3", Name: "ovsdb-" + short + "-2", Address: "ssl:192.0.2.14:6643",
				LastMsg: 198 * time.Millisecond, LastMsgKnown: true, MatchIndex: 41882, MatchIndexKnown: true},
		},
	}
}
