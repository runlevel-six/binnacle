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
		Nodes:        NodeCount{Ready: 9, Total: 9, Cordoned: 1},
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
			Nodes:        NodeCount{Ready: 15, Total: 15},
			UpdatedAt:    time.Now().Add(-3 * time.Second),
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
			Nodes:        NodeCount{Ready: 9, Total: 10},
			UpdatedAt:    time.Now().Add(-11 * time.Second),
		},
		{
			Namespace: "managed-clusters", Name: "tenant-04-cluster",
			Status: health.StatusErr,
			Problem: `no kubeconfig for cluster tenant-04-cluster: no secret named ` +
				`tenant-04-cluster-kubeconfig, and none labeled ` +
				`cluster.x-k8s.io/cluster-name=tenant-04-cluster holds a "value" key`,
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
