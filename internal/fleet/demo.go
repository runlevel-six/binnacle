package fleet

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/runlevel-six/sextant/pkg/health"
	"github.com/runlevel-six/sextant/pkg/model"
	"github.com/runlevel-six/sextant/pkg/rollout"
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
