package fleet

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/runlevel-six/binnacle/internal/core/workload"
	"github.com/runlevel-six/binnacle/pkg/health"
	"github.com/runlevel-six/binnacle/pkg/model"
	"github.com/runlevel-six/binnacle/pkg/profile"
	"github.com/runlevel-six/binnacle/pkg/store"
)

// ManagementView is the management cluster's own health, for the fleet page.
//
// It is not a ClusterView: the management cluster has no Cluster API Cluster
// object, no per-cluster machines, and no per-cluster hosts. What it has are
// nodes and pods — its own — and that is what this reports. The three
// fleet-wide cells (CAPI, Machines, Hosts) are deliberately absent: they are
// already on the page in their correct per-cluster form, and showing them
// here would be a second copy of the same data.
type ManagementView struct {
	// Reachable reports whether the management API server answered. When
	// false, ErrText carries the reason and every field below is empty:
	// nothing was read, so there is nothing to show but the failure.
	Reachable bool
	// ErrText is the connection error when Reachable is false, or the empty
	// string otherwise.
	ErrText string

	// Version is the management cluster's Kubernetes version, from the API
	// server's discovery endpoint. Empty until the first successful probe.
	Version string

	// Nodes is the management cluster's own node readiness.
	Nodes NodeCount
	// NodesKnown is false when no node snapshot has arrived — "we could not
	// read the nodes" rather than "zero nodes."
	NodesKnown bool

	// ControllerHealth summarizes the pods the management cluster's operation
	// depends on. Nil when the pod snapshot has not arrived.
	ControllerHealth *ControllerHealth

	// UnhealthyPods names the pods behind ControllerHealth.Unhealthy, worst
	// first, capped at maxManagementPods.
	//
	// The count alone was the section's original failure: it could say the
	// management cluster had failing pods and could not say which, so the one
	// thing it reported was the one thing nobody could act on. There is no
	// drill-down to defer to either — the management cluster has no Cluster
	// API Cluster object and therefore no cluster page — so whatever a reader
	// needs has to be here.
	UnhealthyPods []model.Pod
	// PodsTruncated is how many unhealthy pods are not listed. Shown for the
	// same reason the cluster pane shows it: "12 unhealthy pods" and "12 of
	// 90" are different situations.
	PodsTruncated int

	// Cells is the health strip: Nodes and Pods only, in that order. Two
	// cells rather than five, because the other three are fleet-wide and
	// already on the cards below.
	Cells []health.Cell

	// Status is the worst of Cells, for the section's status accent.
	Status health.Status

	// UpdatedAt is when the management cluster's data last moved.
	UpdatedAt time.Time
}

// ControllerHealth summarizes the management cluster's controller pods.
//
// These are the workloads whose failure stops every workload cluster's
// reconciliation: the CAPI controllers, Metal3's baremetal-operator, and any
// infrastructure provider controllers. They are identified by the profile's
// CriticalWorkloads list, which is the same mechanism sextant uses in local
// mode.
type ControllerHealth struct {
	// Critical lists each CriticalWorkload from the profile and whether its
	// pods are ready. A workload that has never appeared is reported as
	// absent rather than healthy: a controller that is not running and has
	// never been seen is the worst state, and "0/0 ready" reads as fine.
	Critical []CriticalWorkloadStatus
	// Unhealthy is the count of pods on the management cluster that need
	// attention, using the same health.NeedsAttention filter the cards use.
	// This is the management cluster's own pod cell number, broken out so
	// the section can name it alongside the critical-workload list.
	Unhealthy int
}

// maxManagementPods caps the management section's pod list.
//
// A fifth of the cluster pane's cap, because this section is on the fleet page
// and the fleet page's discipline is that nothing on it grows with what it
// describes: sixty rows here would push the cluster grid off the screen to
// report a problem a dozen rows already name. Twelve is enough to show a
// pattern — one namespace repeating, or one node — and the remainder is
// counted, so the number is never wrong even when the list is short.
const maxManagementPods = 12

// CriticalWorkloadStatus is one profile-declared critical workload and its
// current readiness on the management cluster.
type CriticalWorkloadStatus struct {
	Kind      string
	Namespace string
	Name      string
	Ready     int32
	Desired   int32
	// Absent is true when no pod matching this workload was found. Distinct
	// from Ready < Desired, because a missing controller is a different
	// failure from one whose pods are crashing.
	Absent bool
}

// mgmtCollector runs a workload watcher against the management cluster's own
// API server, publishing nodes and pods into a dedicated store. It is the
// same watcher that per-cluster collectors use against workload clusters,
// pointed at the management cluster instead.
//
// It does not run a capi.Watcher: the management cluster's CAPI objects are
// already collected by every per-cluster tracked, and a second copy would
// publish the same data under the same keys. What this adds is the nodes
// and pods that belong to the management cluster itself, which no
// per-cluster collector reads.
type mgmtCollector struct {
	store  *store.Store
	cancel context.CancelFunc

	mu          sync.Mutex
	reachable   *bool
	serverErr   error
	kubeVersion string
	watchErr    error
}

// serverVersion asks the discovery API for the server version, the same
// call probe makes internally but returning the version string.
func serverVersion(cfg *rest.Config) (string, error) {
	probeCfg := rest.CopyConfig(cfg)
	probeCfg.Timeout = 15 * time.Second
	client, err := kubernetes.NewForConfig(probeCfg)
	if err != nil {
		return "", err
	}
	info, err := client.Discovery().ServerVersion()
	if err != nil {
		return "", err
	}
	return info.GitVersion, nil
}

// managementCells builds the two-cell health strip for the management
// section. It is a cut-down version of health.CoreCells: Nodes and Pods
// only, because CAPI, Machines, and Hosts are fleet-wide and already on
// the cards below.
func managementCells(
	nodes []model.Node, nodesOK bool,
	pods []model.Pod, podsOK bool,
) []health.Cell {
	cells := make([]health.Cell, 0, 2)

	if !nodesOK {
		cells = append(cells, health.Cell{
			Name:   "Nodes",
			Status: health.StatusLoading,
		})
	} else {
		total := len(nodes)
		ready := 0
		for _, n := range nodes {
			if n.Ready() {
				ready++
			}
		}
		st := health.StatusOK
		if ready < total {
			st = health.StatusErr
		}
		cells = append(cells, health.Cell{
			Name:   "Nodes",
			Status: st,
			Detail: fmt.Sprintf("%d/%d", ready, total),
		})
	}

	if !podsOK {
		cells = append(cells, health.Cell{
			Name:   "Pods",
			Status: health.StatusLoading,
		})
	} else {
		unhealthy := 0
		for _, p := range pods {
			if health.NeedsAttention(p) {
				unhealthy++
			}
		}
		st := health.StatusOK
		if unhealthy > 0 {
			st = health.StatusWarn
		}
		cells = append(cells, health.Cell{
			Name:   "Pods",
			Status: st,
			Detail: fmt.Sprintf("%d unhealthy", unhealthy),
		})
	}

	return cells
}

// buildControllerHealth checks each profile-declared critical workload
// against the management cluster's pods. It is the same mechanism sextant
// uses in local mode — the management section just runs it against the
// management cluster's own pod snapshot instead of a workload cluster's.
func buildControllerHealth(pods []model.Pod, prof profile.Profile) *ControllerHealth {
	ch := &ControllerHealth{}

	for _, p := range pods {
		if health.NeedsAttention(p) {
			ch.Unhealthy++
		}
	}

	for _, cw := range prof.CriticalWorkloads {
		status := CriticalWorkloadStatus{
			Kind:      cw.Kind,
			Namespace: cw.Namespace,
			Name:      cw.Name,
		}
		for _, p := range pods {
			if cw.Matches(p.Namespace, p.Name) {
				status.Desired++
				if p.IsHealthy {
					status.Ready++
				}
			}
		}
		status.Absent = status.Desired == 0
		ch.Critical = append(ch.Critical, status)
	}

	return ch
}

// startManagement starts the management cluster collector. It mirrors what
// Fleet.start does for a per-cluster tracked, but pointed at the management
// cluster's own API server and writing to a dedicated store that is not
// scoped by CAPIClusterName.
func (f *Fleet) startManagement(ctx context.Context) {
	mc := &mgmtCollector{
		store:  store.New(),
		cancel: func() {},
	}

	probeCfg := rest.CopyConfig(f.opts.Management)
	probeCfg.Timeout = 15 * time.Second

	go func() {
		err := probe(ctx, f.opts.Management)
		mc.mu.Lock()
		ok := err == nil
		mc.reachable = &ok
		mc.serverErr = err
		mc.mu.Unlock()
		if err == nil {
			if v, err := serverVersion(f.opts.Management); err == nil {
				mc.mu.Lock()
				mc.kubeVersion = v
				mc.mu.Unlock()
			}
		}
		f.notify()
	}()

	cctx, cancel := context.WithCancel(ctx)
	mc.cancel = cancel

	go func() {
		wl, err := workload.New(f.opts.Management, mc.store, workload.Options{
			NodeRoles: f.opts.Profile.NodeRoles,
			Events:    f.opts.Profile.Events,
			// This section reads nodes and pods and nothing else — the
			// controller table matches the profile's workloads against pod
			// names, not against Deployments — so the three workload kinds
			// would be listed cluster-wide for a snapshot no reader consults.
			// The ServiceAccount is granted nodes and pods on the management
			// cluster deliberately narrowly, so asking for more is not merely
			// wasteful: it is a denial the informer retries forever.
			SkipWorkloads: true,
		})
		if err != nil {
			mc.mu.Lock()
			mc.watchErr = err
			mc.mu.Unlock()
			f.notify()
			return
		}
		_ = wl.Run(cctx)
	}()

	go func() {
		sub := mc.store.Subscribe()
		last := time.Time{}
		for {
			select {
			case <-cctx.Done():
				return
			case <-sub:
				if time.Since(last) < f.opts.CoalesceWindow {
					continue
				}
				last = time.Now()
				f.notify()
			}
		}
	}()

	f.mgmt = mc
}

// Management returns the management cluster's own health for the fleet page.
// It is not a ClusterView: see ManagementView for why.
func (f *Fleet) Management() ManagementView {
	if f.mgmt == nil {
		return ManagementView{Reachable: false, ErrText: "not started"}
	}
	mc := f.mgmt
	mc.mu.Lock()
	defer mc.mu.Unlock()

	view := ManagementView{
		Version:   mc.kubeVersion,
		UpdatedAt: time.Now(),
	}

	if mc.reachable != nil {
		view.Reachable = *mc.reachable
	}
	if mc.serverErr != nil {
		view.ErrText = mc.serverErr.Error()
	} else if mc.watchErr != nil {
		view.ErrText = mc.watchErr.Error()
	}

	if !view.Reachable {
		view.Status = health.StatusErr
		return view
	}

	nodes, nodesOK := store.Get[model.Snapshot[model.Node]](mc.store, model.KeyWorkloadNodes)
	pods, podsOK := store.Get[model.Snapshot[model.Pod]](mc.store, model.KeyWorkloadPods)

	if nodesOK && nodes.Err == nil {
		view.Nodes = countMgmtNodes(nodes.Items)
		view.NodesKnown = true
	}

	if podsOK && pods.Err == nil {
		view.ControllerHealth = buildControllerHealth(pods.Items, f.opts.Profile)
		view.UnhealthyPods, view.PodsTruncated = unhealthyPods(pods.Items, maxManagementPods)
	}

	view.Cells = managementCells(
		snapshotItems(nodes, nodesOK),
		nodesOK && nodes.Err == nil,
		snapshotItems(pods, podsOK),
		podsOK && pods.Err == nil,
	)
	view.Status = health.Worst(view.Cells)
	return view
}

func snapshotItems[T any](snap model.Snapshot[T], ok bool) []T {
	if !ok || snap.Err != nil {
		return nil
	}
	return snap.Items
}

func countMgmtNodes(nodes []model.Node) NodeCount {
	var nc NodeCount
	nc.Total = len(nodes)
	for _, n := range nodes {
		if n.Ready() {
			nc.Ready++
		}
	}
	return nc
}
