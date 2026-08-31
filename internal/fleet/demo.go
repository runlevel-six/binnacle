package fleet

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/runlevel-six/sextant/pkg/health"
	"github.com/runlevel-six/sextant/pkg/model"
	"github.com/runlevel-six/sextant/pkg/rollout"
	"github.com/runlevel-six/sextant/pkg/subsystem"
	"github.com/runlevel-six/sextant/pkg/subsystem/cilium"
	"github.com/runlevel-six/sextant/pkg/subsystem/metallb"
	"github.com/runlevel-six/sextant/pkg/subsystem/openstack"
	"github.com/runlevel-six/sextant/pkg/subsystem/ovn"
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
	sortViews(out)
	return out
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
			Age: n.Age,
		})
		host := model.BareMetalHost{
			Namespace: namespace, Name: fmt.Sprintf("host-%d.site-a.example", i+1),
			State: "provisioned", OperationalStatus: "OK", PoweredOn: true, Online: true,
			ConsumerKind: "Metal3Machine", ConsumerName: fmt.Sprintf("%s-machine-%d", name, i+1),
			Age: n.Age,
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
	detail.NodeRows = splitNodes(nodeRows)
	detail.Machines = splitMachines(machines)
	detail.Hosts = splitHosts(hosts)

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
	detail.setEvents(GroupEvents(raw))

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

	detail.Summaries = []SummaryBlock{{
		Title: "Ceph",
		Lines: []string{"health  HEALTH_OK", "osds    36/36 up, 36 in", "capacity 13% used"},
	}}
	return detail, true
}
