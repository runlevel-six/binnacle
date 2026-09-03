package fleet

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/runlevel-six/binnacle/internal/wire"
	"github.com/runlevel-six/binnacle/pkg/health"
	"github.com/runlevel-six/binnacle/pkg/model"
	"github.com/runlevel-six/binnacle/pkg/rollout"
	"github.com/runlevel-six/binnacle/pkg/subsystem"
	"github.com/runlevel-six/binnacle/pkg/subsystem/ceph"
	"github.com/runlevel-six/binnacle/pkg/subsystem/cilium"
	"github.com/runlevel-six/binnacle/pkg/subsystem/metallb"
	"github.com/runlevel-six/binnacle/pkg/subsystem/openstack"
	"github.com/runlevel-six/binnacle/pkg/subsystem/ovn"
)

// Demo is a fleet with no cluster behind it.
//
// It exists because the questions a fleet page has to answer well are the ones
// a healthy cluster will not pose on demand: what a stalled rollout looks like
// next to a healthy neighbor, whether an unreadable cluster is distinguishable
// from a quiet one, whether the worst cluster is where the eye lands. Waiting
// for a real cluster to misbehave is not a way to design that, and asking
// someone to install a binary and hold credentials before they can give an
// opinion on the layout is not either.
//
// The fixture is invented. Names follow the placeholders used throughout the
// tests, and nothing here resembles a real site.
type Demo struct {
	mu      sync.Mutex
	changed chan struct{}
	// progress advances the rolling cluster so that the live update path is
	// visibly doing something rather than being taken on trust.
	progress int32
}

// NewDemo builds the fixture.
func NewDemo() *Demo {
	return &Demo{changed: make(chan struct{}, 1), progress: 2}
}

// Changed satisfies the same contract as [Fleet.Changed].
func (d *Demo) Changed() <-chan struct{} { return d.changed }

// Problem reports nothing: the fixture is always readable, which is the point
// of it. A demo that could fail to load would be exercising the wrong thing.
func (d *Demo) Problem() string { return "" }

// StoreSnapshot returns nil for the demo. The fleet demo drills into
// the single-cluster demo fixture through the router, not through the
// store-stream API endpoint.
func (d *Demo) StoreSnapshot(_, _ string) ([]wire.Entry, bool) {
	return nil, false
}

// Run advances the fixture until ctx is canceled.
func (d *Demo) Run(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.mu.Lock()
			d.progress++
			if d.progress > 6 {
				d.progress = 0
			}
			d.mu.Unlock()
			select {
			case d.changed <- struct{}{}:
			default:
			}
		}
	}
}

// View returns the fixture, ordered the way the real fleet orders itself.
func (d *Demo) View() []ClusterView {
	d.mu.Lock()
	upToDate := d.progress
	d.mu.Unlock()

	rolling := ClusterView{
		Namespace: "managed-clusters", Name: "tenant-02-cluster",
		Status:  health.StatusWarn,
		Version: "v1.31.4", Phase: "Provisioned",
		Cells: []health.Cell{
			{Name: "CAPI", Status: health.StatusWarn, Detail: fmt.Sprintf("%d/6", upToDate)},
			{Name: "Machines", Status: health.StatusWarn, Detail: "1/9 moving"},
			{Name: "Hosts", Status: health.StatusOK},
			{Name: "Nodes", Status: health.StatusWarn, Detail: "1 cordoned"},
			{Name: "Pods", Status: health.StatusOK},
			{Name: "Cilium", Status: health.StatusOK},
			{Name: "Ceph", Status: health.StatusOK},
		},
		ControlPlane: model.ReplicaBucket{Desired: 3, Ready: 3, UpToDate: 3},
		Workers:      model.ReplicaBucket{Desired: 6, Ready: 6, UpToDate: upToDate},
		Pools: []NodePool{
			{Role: "Control Plane", Name: "tenant-02-cluster-cp", Ready: 3, Desired: 3, Version: "v1.32.0"},
			{Role: "Workers", Name: "tenant-02-cluster-compute", Ready: 4, Desired: 4, Version: "v1.31.4", Rolling: true},
			{Role: "Workers", Name: "tenant-02-cluster-mgd", Ready: 2, Desired: 2, Version: "v1.32.0"},
		},
		Nodes: NodeCount{Ready: 9, Total: 9, Cordoned: 1}, NodesKnown: true,
		Capacity: Capacity{CPURequested: 82900, CPUAllocatable: 342000,
			MemRequested: 356 << 30, MemAllocatable: 1004 << 30},
		Workloads: []WorkloadCount{
			{Kind: "DaemonSet", Ready: 31, Total: 31},
			{Kind: "Deployment", Ready: 147, Total: 148},
			{Kind: "StatefulSet", Ready: 71, Total: 74},
		},
		UnhealthyPods: 11,
		Rollout: rollout.State{
			Active:  true,
			Rolling: []string{"managed-clusters/tenant-02-cluster-md-0 (MachineDeployment)"},
		},
		UpdatedAt: time.Now().Add(-6 * time.Second),
	}

	out := []ClusterView{
		{
			Namespace: "managed-clusters", Name: "tenant-01",
			Status:  health.StatusOK,
			Version: "v1.32.0", Phase: "Provisioned",
			Cells: []health.Cell{
				{Name: "CAPI", Status: health.StatusOK},
				{Name: "Machines", Status: health.StatusOK},
				{Name: "Hosts", Status: health.StatusOK},
				{Name: "Nodes", Status: health.StatusOK},
				{Name: "Pods", Status: health.StatusOK},
				{Name: "Cilium", Status: health.StatusOK},
				{Name: "Ceph", Status: health.StatusOK},
			},
			ControlPlane: model.ReplicaBucket{Desired: 3, Ready: 3, UpToDate: 3},
			Workers:      model.ReplicaBucket{Desired: 12, Ready: 12, UpToDate: 12},
			Pools: []NodePool{
				{Role: "Control Plane", Name: "tenant-01-cp", Ready: 3, Desired: 3, Version: "v1.32.0"},
				{Role: "Workers", Name: "tenant-01-compute", Ready: 8, Desired: 8, Version: "v1.32.0"},
				{Role: "Workers", Name: "tenant-01-mgd", Ready: 4, Desired: 4, Version: "v1.32.0"},
			},
			Nodes: NodeCount{Ready: 15, Total: 15}, NodesKnown: true,
			Capacity: Capacity{CPURequested: 61000, CPUAllocatable: 224000,
				MemRequested: 210 << 30, MemAllocatable: 640 << 30},
			Workloads: []WorkloadCount{
				{Kind: "DaemonSet", Ready: 24, Total: 24},
				{Kind: "Deployment", Ready: 96, Total: 96},
				{Kind: "StatefulSet", Ready: 40, Total: 40},
			},
			UpdatedAt: time.Now().Add(-3 * time.Second),
		},
		rolling,
		{
			Namespace: "managed-clusters", Name: "tenant-03-cluster",
			Status:  health.StatusErr,
			Version: "v1.31.4", Phase: "Provisioned",
			Cells: []health.Cell{
				{Name: "CAPI", Status: health.StatusOK},
				{Name: "Machines", Status: health.StatusWarn, Detail: "1/7 moving"},
				{Name: "Hosts", Status: health.StatusErr, Detail: "1 errored"},
				{Name: "Nodes", Status: health.StatusErr, Detail: "1 NotReady"},
				{Name: "Pods", Status: health.StatusWarn, Detail: "4 unhealthy"},
				{Name: "Ceph", Status: health.StatusWarn, Detail: "HEALTH_WARN"},
			},
			ControlPlane: model.ReplicaBucket{Desired: 3, Ready: 3, UpToDate: 3},
			Workers:      model.ReplicaBucket{Desired: 7, Ready: 6, UpToDate: 7},
			Pools: []NodePool{
				{Role: "Control Plane", Name: "tenant-03-cluster-cp", Ready: 3, Desired: 3, Version: "v1.31.4"},
				{Role: "Workers", Name: "tenant-03-cluster-compute", Ready: 6, Desired: 7, Version: "v1.31.4"},
			},
			Nodes: NodeCount{Ready: 9, Total: 10}, NodesKnown: true,
			Capacity: Capacity{CPURequested: 96000, CPUAllocatable: 168000,
				MemRequested: 330 << 30, MemAllocatable: 480 << 30},
			Workloads: []WorkloadCount{
				{Kind: "DaemonSet", Ready: 21, Total: 22},
				{Kind: "Deployment", Ready: 88, Total: 91},
				{Kind: "StatefulSet", Ready: 30, Total: 33},
			},
			UnhealthyPods: 7,
			UpdatedAt:     time.Now().Add(-11 * time.Second),
		},
		{
			Namespace: "managed-clusters", Name: "tenant-04-cluster",
			Status: health.StatusErr,
			Problem: `no kubeconfig for cluster tenant-04-cluster: no secret named ` +
				`tenant-04-cluster-kubeconfig, and none labeled ` +
				`cluster.x-k8s.io/cluster-name=tenant-04-cluster holds a "value" key`,
		},
		{
			Namespace: "managed-clusters", Name: "tenant-06-cluster",
			Status:  health.StatusWarn,
			Version: "v1.31.4", Phase: "Provisioned",
			Cells: []health.Cell{
				{Name: "CAPI", Status: health.StatusOK},
				{Name: "Machines", Status: health.StatusOK},
				{Name: "Hosts", Status: health.StatusOK},
			},
			Pools: []NodePool{
				{Role: "Control Plane", Name: "tenant-06-cluster-cp", Ready: 3, Desired: 3, Version: "v1.31.4"},
				{Role: "Workers", Name: "tenant-06-cluster-compute", Ready: 3, Desired: 3, Version: "v1.31.4"},
			},
			WorkloadProblem: `Get "https://192.0.2.10:6443/api/v1/nodes": dial tcp 192.0.2.10:6443: ` +
				`i/o timeout — the kubeconfig Cluster API minted points at the cluster's own ` +
				`endpoint, which is not reachable from here`,
			UpdatedAt: time.Now().Add(-8 * time.Second),
		},
		{
			Namespace: "managed-clusters", Name: "tenant-05-cluster",
			Status: health.StatusLoading,
			Cells:  []health.Cell{{Name: "CAPI", Status: health.StatusLoading}},
		},
	}
	for i := range out {
		fillCard(&out[i])
	}
	sortViews(out)
	return out
}

// fillCard derives a fixture's expanded tier from what it already declares.
//
// Derived rather than written out per fixture for the same reason the detail
// page's node table is: six literals maintained by hand drift, and a card that
// disagrees with the page it links to is worse than one that says less.
func fillCard(v *ClusterView) {
	if v.Problem != "" || !v.NodesKnown {
		return
	}

	byRole := map[string]*RoleCount{}
	var order []string
	for _, pool := range v.Pools {
		role := "compute"
		switch {
		case pool.Role == "Control Plane":
			role = "control-plane"
		case strings.Contains(pool.Name, "-mgd"):
			role = "managed-services"
		}
		rc, seen := byRole[role]
		if !seen {
			rc = &RoleCount{Role: role}
			byRole[role] = rc
			order = append(order, role)
		}
		rc.Ready += int(pool.Ready)
		rc.Total += int(pool.Desired)
	}
	for _, role := range order {
		v.NodesByRole = append(v.NodesByRole, *byRole[role])
	}
	sort.Slice(v.NodesByRole, func(i, j int) bool { return v.NodesByRole[i].Role < v.NodesByRole[j].Role })

	// The same pods the detail page invents, so following the link does not
	// land on a different set of names.
	for i := 0; i < v.UnhealthyPods && i < maxCardPods; i++ {
		v.TopUnhealthyPods = append(v.TopUnhealthyPods, PodRef{
			Namespace: "example-system", Name: fmt.Sprintf("api-%d-5f9c8", i+1),
			Status: "CrashLoopBackOff", Restarts: int32(441 - i*7),
		})
	}

	for _, c := range v.Cells {
		if c.Name == "Ceph" {
			health := "HEALTH_OK"
			if c.Detail != "" {
				health = c.Detail
			}
			v.Summaries = append(v.Summaries, SummaryBlock{Title: "Ceph", Lines: []string{
				"health   " + health, "osds     36/36 up, 36 in", "capacity 13% used",
			}})
		}
	}
}

// Storage invents a datacenter with one Ceph cluster and a rack of hardware
// nobody has labeled, so the panel and its unlabeled-hardware line can both be
// worked on without a cluster.
func (d *Demo) Storage() Storage {
	host := func(name, role string, status, errMsg string) model.BareMetalHost {
		return model.BareMetalHost{
			Namespace: "managed-clusters", Name: name, State: "provisioned",
			OperationalStatus: status, ErrorMessage: errMsg, PoweredOn: true, Online: true,
			Labels: map[string]string{
				LabelRole:      role,
				LabelClusterID: "a7c3e9f1-4b2d-4e8a-9c1f-3d5b7e9a1c2d",
			},
		}
	}
	hosts := []model.BareMetalHost{
		host("r0102-01-cephmon", "cephmon", "OK", ""),
		host("r0102-02-cephmon", "cephmon", "OK", ""),
		host("r0102-03-cephmon", "cephmon", "OK", ""),
		host("r0104-01-cephosd", "cephosd", "OK", ""),
		host("r0104-02-cephosd", "cephosd", "OK", ""),
		// One with the hardware complaining, so the rank and the row style are
		// both exercised.
		host("r0104-03-cephosd", "cephosd", "error", "timed out waiting for the deploy image"),
	}
	// Unclaimed hardware carrying no role label: a rack the inventory has not
	// reached. Without these the demo cannot show the line that keeps an
	// unlabeled site from rendering as a site with no storage.
	for i := 1; i <= 4; i++ {
		hosts = append(hosts, model.BareMetalHost{
			Namespace: "managed-clusters", Name: fmt.Sprintf("r0108-%02d-unknown", i),
			State: "available", OperationalStatus: "OK",
		})
	}
	// One reporting cluster, so the demo exercises the join as well as the
	// hardware half. Capacity is given in bytes because that is what Ceph
	// reports and what UsedPercent divides: a fixture that carried the
	// percentage would not exercise the arithmetic the panel depends on.
	return StorageFor(hosts, []CephReport{{
		Cluster: ClusterRef{Namespace: "managed-clusters", Name: "tenant-01"},
		Status: ceph.Status{
			FSID: "a7c3e9f1-4b2d-4e8a-9c1f-3d5b7e9a1c2d", Health: "HEALTH_WARN",
			Mons: ceph.Mons{Total: 3, InQuorum: 3},
			OSDs: ceph.OSDs{Total: 36, Up: 36, In: 36},
			PGs: ceph.PGs{
				Total: 1953, Pools: 14, Objects: 4_812_663,
				ByState:    []ceph.PGState{{Name: "active+clean", Count: 1953}},
				UsedBytes:  13 * 1 << 40,
				AvailBytes: 87 * 1 << 40,
				TotalBytes: 100 * 1 << 40,
			},
			Checks: []ceph.Check{{
				Name: "AUTH_INSECURE_GLOBAL_ID_RECLAIM", Severity: "HEALTH_WARN",
				Message: "client is using insecure global_id reclaim",
			}},
		},
	}})
}

// Management returns a plausible management cluster view for the demo, so
// the management section can be developed and screenshotted without a real
// management cluster.
func (d *Demo) Management() ManagementView {
	return ManagementView{
		Reachable: true,
		Version:   "v1.31.4",
		Nodes: NodeCount{
			Total: 3,
			Ready: 3,
		},
		NodesKnown: true,
		ControllerHealth: &ControllerHealth{
			Unhealthy: 1,
			Critical: []CriticalWorkloadStatus{
				{Kind: "Deployment", Namespace: "capi-system", Name: "capi-controller-manager", Ready: 1, Desired: 1},
				{Kind: "Deployment", Namespace: "capi-kubeadm-bootstrap-system", Name: "capi-kubeadm-bootstrap-controller-manager", Ready: 1, Desired: 1},
				{Kind: "Deployment", Namespace: "capi-kubeadm-control-plane-system", Name: "capi-kubeadm-control-plane-controller-manager", Ready: 1, Desired: 1},
				{Kind: "Deployment", Namespace: "capm3-system", Name: "capm3-controller-manager", Ready: 0, Desired: 1},
				{Kind: "DaemonSet", Namespace: "baremetal-system", Name: "baremetal-operator", Ready: 3, Desired: 3},
			},
		},
		Cells: []health.Cell{
			{Name: "Nodes", Status: health.StatusOK, Detail: "3/3"},
			{Name: "Pods", Status: health.StatusWarn, Detail: "1 unhealthy"},
		},
		Status:    health.StatusWarn,
		UpdatedAt: time.Now(),
	}
}

// Cluster satisfies the same contract as [Fleet.Cluster], with invented detail
// for whichever fixture cluster was asked for.
//
// The tables are populated only for a cluster that has something to say. A
// fixture where every cluster is fully detailed would make the empty states —
// which are most of what a detail page has to get right — impossible to look at.
func (d *Demo) Cluster(namespace, name string) (ClusterDetail, bool) {
	var view ClusterView
	for _, c := range d.View() {
		if c.Namespace == namespace && c.Name == name {
			view = c
		}
	}
	if view.Name == "" {
		return ClusterDetail{}, false
	}

	detail := ClusterDetail{ClusterView: view}
	if view.Problem != "" || !view.NodesKnown {
		return detail, true
	}

	// Roles come from the cluster's own pools rather than a fixed list, so the
	// detail page agrees with the card above it and a fixture declaring fifteen
	// nodes renders fifteen. It also gives the fold something to fold.
	var roles []string
	for _, pool := range view.Pools {
		role := "compute"
		switch {
		case pool.Role == "Control Plane":
			role = "control-plane"
		case strings.Contains(pool.Name, "-mgd"):
			role = "managed-services"
		}
		for i := int32(0); i < pool.Desired; i++ {
			roles = append(roles, role)
		}
	}

	// One cordoned compute, not every compute: a cluster where the whole worker
	// pool is unschedulable is not a state worth designing the page around.
	cordon := -1
	for i, role := range roles {
		if role == "compute" {
			cordon = i
			break
		}
	}

	var (
		nodeRows []NodeRow
		machines []model.Machine
		hosts    []model.BareMetalHost
	)
	for i, role := range roles {
		n := model.Node{
			Name: fmt.Sprintf("%s-node-%d.site-a.example", name, i+1),
			Role: role, Status: "Ready", Version: view.Version,
			Age:               time.Duration(31*24+i) * time.Hour,
			AllocatableCPU:    64000,
			RequestedCPU:      int64(18000 + i*4000),
			AllocatableMemory: 192 << 30,
			RequestedMemory:   int64(64+i*12) << 30,
		}
		if i == cordon {
			n.Cordoned = true
		}
		if i == len(roles)-1 && view.Status == health.StatusErr {
			n.Status = "NotReady"
			n.MemoryPressure = true
		}
		nodeRows = append(nodeRows, NodeRow{
			Node:       n,
			CPUPercent: percent(n.RequestedCPU, n.AllocatableCPU),
			MemPercent: percent(n.RequestedMemory, n.AllocatableMemory),
		})
		machines = append(machines, model.Machine{
			Namespace: namespace, Name: fmt.Sprintf("%s-machine-%d", name, i+1),
			ClusterName: name, NodeName: n.Name, Phase: "Running",
			Version: view.Version, InfraKind: "Metal3Machine",
			InfraName: fmt.Sprintf("%s-machine-%d", name, i+1),
			Age:       n.Age,
		})
		host := model.BareMetalHost{
			Namespace: namespace, Name: fmt.Sprintf("host-%d.site-a.example", i+1),
			State: "provisioned", OperationalStatus: "OK", PoweredOn: true, Online: true,
			ConsumerKind: "Metal3Machine", ConsumerNamespace: namespace,
			ConsumerName: fmt.Sprintf("%s-machine-%d", name, i+1),
			Age:          n.Age,
		}
		if i == len(roles)-1 && view.Status == health.StatusErr {
			host.OperationalStatus = "error"
			host.ErrorMessage = "provisioning failed: timed out waiting for the deploy image"
			// The machine on that host is stuck behind it, which is the whole
			// story the three tables tell together: errored host, machine that
			// never finished provisioning, node that never came Ready.
			machines[len(machines)-1].Phase = "Provisioning"
		}
		hosts = append(hosts, host)
	}

	// The rest of the datacenter: another cluster's hardware and the Ceph nodes
	// that belong to no cluster at all. Present so the fixture exercises the
	// scoping — a snapshot containing only this cluster's hosts would let a
	// filter that does nothing look correct.
	for i := 0; i < 9; i++ {
		hosts = append(hosts, model.BareMetalHost{
			Namespace: namespace, Name: fmt.Sprintf("neighbor-%d.site-a.example", i+1),
			State: "provisioned", OperationalStatus: "OK", PoweredOn: true, Online: true,
			ConsumerKind: "Metal3Machine", ConsumerNamespace: namespace,
			ConsumerName: fmt.Sprintf("tenant-99-cluster-machine-%d", i+1),
			Age:          900 * 24 * time.Hour,
		})
	}
	for i := 0; i < 6; i++ {
		hosts = append(hosts, model.BareMetalHost{
			Namespace: namespace, Name: fmt.Sprintf("cephosd-%d.site-a.example", i+1),
			State: "provisioned", OperationalStatus: "OK", PoweredOn: true, Online: true,
			Age: 900 * 24 * time.Hour,
		})
	}

	detail.NodeRows = splitNodes(nodeRows)
	detail.Machines = splitMachines(machines)
	mine, elsewhere := hostsFor(hosts, detail.Machines.All())
	detail.HostsElsewhere = elsewhere
	detail.Hosts = splitHosts(mine)

	for i := 0; i < view.UnhealthyPods; i++ {
		detail.UnhealthyPods = append(detail.UnhealthyPods, model.Pod{
			Namespace: "example-system", Name: fmt.Sprintf("api-%d-5f9c8", i+1),
			ReadyReady: 0, ReadyTotal: 1, Status: "CrashLoopBackOff",
			Restarts: int32(441 - i*7), Node: nodeRows[i%len(nodeRows)].Name,
			Age: 26 * time.Hour, Phase: "Running",
		})
	}

	now := time.Now()
	raw := []model.Event{
		{Namespace: "example-system", Type: "Warning", Reason: "Unhealthy",
			ObjectKind: "Pod", ObjectName: "api-1-5f9c8", Count: 9148,
			Message:       "Readiness probe failed: HTTP probe failed with statuscode: 503",
			LastTimestamp: now.Add(-time.Minute)},
		{Namespace: "example-system", Type: "Warning", Reason: "BackOff",
			ObjectKind: "Pod", ObjectName: "api-2-5f9c8", Count: 3208,
			Message:       "Back-off restarting failed container",
			LastTimestamp: now.Add(-2 * time.Minute)},
		{Namespace: namespace, Type: "Normal", Reason: "SuccessfulCreate",
			ObjectKind: "MachineSet", ObjectName: name + "-md-0", Count: 1,
			Message:       "Created machine " + name + "-machine-5",
			LastTimestamp: now.Add(-14 * time.Minute)},
	}
	if view.Status != health.StatusErr {
		raw = raw[2:]
	} else {
		// One admission policy rejecting every replica set it sees. This is the
		// shape grouping exists for: ungrouped it is twenty-three rows that
		// differ only in a hash, and it crowds out everything else on the page.
		for i := 0; i < 23; i++ {
			raw = append(raw, model.Event{
				Namespace: "example-system", Type: "Warning", Reason: "PolicyViolation",
				ObjectKind: "ReplicaSet", ObjectName: "batch-worker-" + itoa(6000+i),
				Message:       "policy disallow-host-path/autogen-host-path fail: HostPath volumes are forbidden",
				LastTimestamp: now.Add(-time.Duration(i) * time.Minute)})
		}
	}
	// A backup CronJob's steady output. Real clusters produce forty rows of this
	// an hour, and the fixture needs it or the demo cannot show what the fold is
	// for: the layout being worked on here is the one with the audit trail
	// tucked behind a disclosure.
	for i, step := range []struct{ reason, message string }{
		{"SawCompletedJob", "Saw completed job: etcd-backup, condition: Complete"},
		{"SuccessfulDelete", "Deleted job etcd-backup-29804460"},
		{"Completed", "Job completed"},
		{"Started", "Container started"},
		{"Created", "Container created"},
		{"Pulling", "Pulling image \"docker.io/busybox\""},
		{"Pulled", "Successfully pulled image \"docker.io/busybox\" in 267ms"},
		{"Scheduled", "Successfully assigned kube-system/etcd-backup-29804490-6r55z to node-1"},
		{"SuccessfulCreate", "Created pod: etcd-backup-29804490-6r55z"},
		{"AddedInterface", "Add eth0 [172.18.6.231/32] from cilium"},
		{"NodeSchedulable", "Node node-3 status is now: NodeSchedulable"},
		{"LeaderElection", "node-2 became leader"},
	} {
		raw = append(raw, model.Event{
			Namespace: "kube-system", Type: "Normal", Reason: step.reason, Count: 1,
			ObjectKind: "Pod", ObjectName: fmt.Sprintf("etcd-backup-2980%d-6r55z", 4400+i),
			Message:       step.message,
			LastTimestamp: now.Add(-time.Duration(15*i) * time.Minute)})
	}
	detail.setEvents(GroupEvents(raw))
	// The management namespace holds the whole fleet, so a real page always has
	// some. Two clusters' worth of Cluster API chatter is about this many.
	detail.EventsElsewhere = 31

	detail.Subsystems = Subsystems{
		Cilium: &cilium.State{
			Tier: subsystem.TierFull, AgentsReady: 5, AgentsDesired: 5,
			Rollout: subsystem.Rollout{Desired: 5, Updated: 5, Ready: 5},
			Pod:     "cilium-czwz7",
			Status: cilium.Status{
				Version: "1.19.7", State: "Ok", KubeProxyReplacement: "true",
				EncryptionMode: "Ztunnel",
				IPAM:           cilium.IPAM{Used: 130, Available: 124},
				Controllers:    cilium.Controllers{Total: 717},
			},
		},
		OVN: &ovn.State{
			Tier: subsystem.TierFull,
			Statuses: []ovn.ClusterStatus{
				{Database: "nb", Role: "leader", Term: 385, Leader: "self",
					Servers: []ovn.Server{{ID: "a1b2"}, {ID: "c3d4"}, {ID: "e5f6"}}},
				{Database: "sb", Role: "leader", Term: 388, Leader: "self",
					Servers: []ovn.Server{{ID: "a1b2"}, {ID: "c3d4"}, {ID: "e5f6"}}},
			},
			Components: []ovn.Component{
				{Name: "ovn-northd", Rollout: subsystem.Rollout{Desired: 3, Updated: 3, Ready: 3}},
				{Name: "ovn-controller", Rollout: subsystem.Rollout{Desired: 5, Updated: 5, Ready: 5}},
				{Name: "openvswitch", Rollout: subsystem.Rollout{Desired: 5, Updated: 2, Ready: 5, Manual: true}},
			},
		},
		MetalLB: &metallb.State{
			Namespace: "kube-system", SpeakerReady: 4, SpeakerDesired: 4,
			Pools: []metallb.Pool{
				{Name: "default", Addresses: []string{"192.0.2.12-192.0.2.99"},
					Advertised: []string{"L2"}, Assigned: 8, Available: 80,
					Usage: metallb.UsageStatus},
				{Name: "reserved", Addresses: []string{"192.0.2.100-192.0.2.120"},
					Assigned: 0, Usage: metallb.UsageUnknown},
			},
		},
		OpenStack: &openstack.State{
			Cloud: "my-cloud", Region: "region-1",
			Services: []openstack.ServiceSummary{
				{Service: "block-storage", Total: 3, Up: 3},
				{Service: "compute", Total: 8, Up: 8},
				{Service: "network", Total: 13, Up: 12},
			},
		},
		Inventory: &openstack.Inventory{
			Counts: []openstack.Count{
				{Label: "Projects", Total: 16},
				{Label: "Servers", Total: 54, ByState: map[string]int{"ACTIVE": 50, "ERROR": 2, "BUILD": 2}},
				{Label: "Networks", Total: 24},
				{Label: "Load Balancers", Absent: true},
			},
		},
	}
	detail.Drains = []openstack.Drain{
		{Host: "compute-node-3.site-a.example", Remaining: 4, Moving: 1},
		{Host: "compute-node-7.site-a.example", Remaining: 2, Moving: 0, Stuck: 2},
	}
	// The migration table the drains above are producing. The fixture was
	// missing it, which is why nothing caught a field name in that pane that
	// had never existed: the demo is the only place a pane gets exercised
	// without waiting for a real cloud to do something.
	detail.Shown = openstack.Shown{Rows: []openstack.Migration{
		{ID: 70551, Status: "error", Type: "live-migration",
			InstanceUUID:  "3f2b1c8a-9d44-4c1e-8f77-2b6a5e0c1d93",
			SourceCompute: "compute-node-7.site-a.example",
			DestCompute:   "compute-node-2.site-a.example",
			UpdatedAt:     now.Add(-9 * time.Minute)},
		{ID: 70549, Status: "running", Type: "live-migration",
			InstanceUUID:  "b81f7e20-3a5c-41d8-9e62-77c4f1a0b512",
			SourceCompute: "compute-node-3.site-a.example",
			DestCompute:   "compute-node-1.site-a.example",
			UpdatedAt:     now.Add(-40 * time.Second)},
		{ID: 70540, Status: "completed", Type: "evacuation",
			InstanceUUID:  "c0a4d118-6f2b-4a90-b7e3-51d8ac2f6e07",
			SourceCompute: "compute-node-7.site-a.example",
			DestCompute:   "compute-node-4.site-a.example",
			UpdatedAt:     now.Add(-22 * time.Minute)},
	}}

	// Ceph as typed state, because that is what the pane renders from now. The
	// SummaryBlock this replaced is still the path a plugin with internal types
	// takes, and the fleet cards still use it — see [Demo.View].
	detail.Subsystems.Ceph = &ceph.State{
		Tier: subsystem.TierFull, Pod: "ceph-tools-0",
		Status: ceph.Status{
			FSID: "a7c3e9f1-4b2d-4e8a-9c1f-3d5b7e9a1c2d", Health: "HEALTH_WARN",
			Mons: ceph.Mons{Total: 3, InQuorum: 3},
			// One OSD down, so the tile disagrees with the total beside it —
			// which is the thing the pre-formatted block could not show.
			OSDs: ceph.OSDs{Total: 36, Up: 35, In: 36},
			PGs: ceph.PGs{
				Total: 1953, Pools: 14, Objects: 4_812_663,
				ByState:    []ceph.PGState{{Name: "active+clean", Count: 1951}},
				UsedBytes:  13 * 1 << 40,
				AvailBytes: 87 * 1 << 40,
				TotalBytes: 100 * 1 << 40,
			},
			Checks: []ceph.Check{
				{Name: "OSD_DOWN", Severity: "HEALTH_WARN", Message: "1 osds down"},
				{Name: "AUTH_INSECURE_GLOBAL_ID_RECLAIM", Severity: "HEALTH_WARN",
					Message: "client is using insecure global_id reclaim"},
			},
		},
	}
	return detail, true
}
