package fleet

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/runlevel-six/sextant/pkg/collect"
	"github.com/runlevel-six/sextant/pkg/health"
	"github.com/runlevel-six/sextant/pkg/model"
	"github.com/runlevel-six/sextant/pkg/plugin"
	"github.com/runlevel-six/sextant/pkg/profile"
	"github.com/runlevel-six/sextant/pkg/rollout"
	"github.com/runlevel-six/sextant/pkg/store"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Options configures a Fleet.
type Options struct {
	// Management is the cluster hosting Cluster API. Required.
	Management *rest.Config
	// Namespace scopes cluster discovery. Empty means all namespaces.
	Namespace string
	// Profile describes how these sites are laid out. Every cluster in the
	// fleet gets the same one, which holds while a fleet is homogeneous; a
	// per-cluster profile is the obvious extension when it stops being.
	Profile profile.Profile
	// OSCloud names the clouds.yaml entry to use, for clusters whose own
	// credentials do not name one. A fleet-wide fallback: each cluster is
	// generally its own cloud, so per-cluster credentials are the normal case
	// and this is what a single-cluster or uniform-naming site can set instead.
	OSCloud string
	// CloudsDir is where per-cluster clouds.yaml files are written for
	// gophercloud to read. Empty uses a directory under the system temp dir.
	//
	// In a deployment this should be a memory-backed volume: these are
	// credentials, and gophercloud will only read them from a file.
	CloudsDir string
	// RediscoverEvery sets how often the cluster list is re-read. Zero uses a
	// sensible default.
	RediscoverEvery time.Duration
	// CoalesceWindow is the shortest gap between two change notifications. It
	// exists because a fleet of watched clusters produces a continuous stream
	// of store writes and a browser does not need every one of them. Zero uses
	// a sensible default.
	CoalesceWindow time.Duration
}

const (
	defaultRediscover = 60 * time.Second
	defaultCoalesce   = 750 * time.Millisecond
)

// tracked is one cluster's running collector.
type tracked struct {
	discovered Discovered
	store      *store.Store
	registry   *plugin.Registry
	cancel     context.CancelFunc
	// cloudsPath is the file written for this cluster, removed when it stops.
	cloudsPath string

	// startedAt is when this cluster's collector began. Silence needs a clock
	// against it: without one, "has not reported yet" is indistinguishable
	// from "will never report", and the second is a fault.
	startedAt time.Time

	mu       sync.Mutex
	watchErr error
	// reachable is nil until the management watcher has reported, so "not
	// heard from yet" stays distinguishable from "unreachable".
	reachable   *bool
	serverErr   error
	kubeVersion string
	// workloadErr is the workload API server's own answer to a direct probe.
	//
	// sextant reports reachability for the management cluster only, and the
	// workload watcher on failure simply never publishes — so without asking
	// the workload API server ourselves, an unreachable one is silence, and
	// silence carries no reason a reader can act on.
	workloadErr   error
	workloadProbe bool
	// cloudsErr records credentials that exist but could not be used.
	cloudsErr error
}

// Fleet keeps one sextant collector per discovered cluster and answers what the
// whole fleet currently looks like.
type Fleet struct {
	opts       Options
	discoverer *Discoverer

	mu       sync.Mutex
	clusters map[string]*tracked

	// changed is a coalesced redraw trigger, the same idea as the dashboard's
	// store subscription: only the latest state matters, so a subscriber that
	// misses a tick catches up on the next one.
	changed chan struct{}
}

// New builds a Fleet. It does not contact any cluster; call [Fleet.Run].
func New(opts Options) (*Fleet, error) {
	d, err := NewDiscoverer(opts.Management, opts.Namespace)
	if err != nil {
		return nil, err
	}
	if opts.RediscoverEvery == 0 {
		opts.RediscoverEvery = defaultRediscover
	}
	if opts.CoalesceWindow == 0 {
		opts.CoalesceWindow = defaultCoalesce
	}
	if opts.CloudsDir == "" {
		opts.CloudsDir = filepath.Join(os.TempDir(), "binnacle-clouds")
	}
	return &Fleet{
		opts:       opts,
		discoverer: d,
		clusters:   map[string]*tracked{},
		changed:    make(chan struct{}, 1),
	}, nil
}

// Changed returns a channel that ticks when something in the fleet may have
// moved. Sends are non-blocking, so a slow reader misses ticks rather than
// stalling a collector.
func (f *Fleet) Changed() <-chan struct{} { return f.changed }

func (f *Fleet) notify() {
	select {
	case f.changed <- struct{}{}:
	default:
	}
}

// Run reconciles the cluster list until ctx is canceled.
//
// Discovery repeats rather than watching, because the reconcile is cheap and a
// list is self-correcting: a watch that silently dies leaves a fleet page
// frozen at whatever it last saw, which is the failure mode a status board can
// least afford.
func (f *Fleet) Run(ctx context.Context) error {
	if err := f.reconcile(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(f.opts.RediscoverEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			f.stopAll()
			return ctx.Err()
		case <-ticker.C:
			// A failed re-list leaves the existing collectors running. The
			// management cluster being briefly unreadable is not a reason to
			// tear down every workload cluster's view.
			_ = f.reconcile(ctx)
		}
	}
}

func (f *Fleet) reconcile(ctx context.Context) error {
	found, err := f.discoverer.List(ctx)
	if err != nil {
		return err
	}

	seen := map[string]bool{}
	for _, d := range found {
		seen[d.Key()] = true
		f.mu.Lock()
		existing, running := f.clusters[d.Key()]
		f.mu.Unlock()
		if running {
			// Credentials that appeared after the cluster did: restart so the
			// collector picks them up.
			if existing.discovered.Config == nil && d.Config != nil {
				f.stop(d.Key())
			} else {
				continue
			}
		}
		f.start(ctx, d)
	}

	f.mu.Lock()
	var gone []string
	for key := range f.clusters {
		if !seen[key] {
			gone = append(gone, key)
		}
	}
	f.mu.Unlock()
	for _, key := range gone {
		f.stop(key)
	}

	f.notify()
	return nil
}

// start launches one cluster's collector.
func (f *Fleet) start(ctx context.Context, d Discovered) {
	cctx, cancel := context.WithCancel(ctx)
	t := &tracked{
		discovered: d,
		store:      store.New(),
		registry:   plugin.NewRegistry(),
		cancel:     cancel,
		startedAt:  time.Now(),
	}

	f.mu.Lock()
	f.clusters[d.Key()] = t
	f.mu.Unlock()

	// A cluster with no credentials still gets a slot on the page — it shows
	// what discovery found and why it cannot be read — but nothing to collect.
	if d.Config == nil {
		return
	}

	// OpenStack credentials, when this cluster has any. A failure here costs
	// the OpenStack pane and nothing else: the cluster is still worth watching
	// without it.
	cloud, cloudsPath := f.opts.OSCloud, ""
	if d.Clouds != nil {
		path, err := writeClouds(f.opts.CloudsDir, d.Key(), d.Clouds)
		if err != nil {
			t.mu.Lock()
			t.cloudsErr = err
			t.mu.Unlock()
		} else {
			t.cloudsPath = path
			cloudsPath = path
			if d.Clouds.Cloud != "" {
				cloud = d.Clouds.Cloud
			}
		}
	} else if d.CloudsErr != nil {
		t.mu.Lock()
		t.cloudsErr = d.CloudsErr
		t.mu.Unlock()
	}

	// Probe the workload API server directly, alongside the collectors. It is
	// the only way an unreachable workload cluster produces a sentence rather
	// than an absence.
	go func() {
		err := probe(cctx, d.Config)
		t.mu.Lock()
		t.workloadErr, t.workloadProbe = err, true
		t.mu.Unlock()
		f.notify()
	}()

	go func() {
		err := collect.Watch(cctx, collect.Options{
			Store:      t.store,
			Registry:   t.registry,
			Management: f.opts.Management,
			Workload:   d.Config,
			Profile:    f.opts.Profile,
			// Narrow the management-side objects to this cluster. Without it
			// every collector would publish every cluster's Machines into its
			// own store and each row would report the whole fleet's numbers.
			CAPIClusterName:      d.Name,
			ManagementNamespaces: []string{d.Namespace},
			OSCloud:              cloud,
			OSCloudsPath:         cloudsPath,
		}, func(version string, err error) {
			t.mu.Lock()
			ok := err == nil
			t.reachable, t.serverErr, t.kubeVersion = &ok, err, version
			t.mu.Unlock()
			f.notify()
		})
		if err != nil {
			t.mu.Lock()
			t.watchErr = err
			t.mu.Unlock()
			f.notify()
			return
		}
		collect.Activate(cctx, t.registry, t.store)

		// Republish every store change as a fleet change, so the page updates
		// when a cluster does rather than on a timer.
		sub := t.store.Subscribe()
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
}

// workloadGrace is how long a workload cluster may stay silent before silence
// is treated as a fault.
//
// Informers sync in seconds, so this is generous. It exists only to cover
// startup: the cost of it being too short is a page that flashes amber on
// restart, and the cost of having none at all is a cluster that is
// permanently unreachable rendering permanently green.
const workloadGrace = 45 * time.Second

// probe asks the workload API server for its version.
func probe(ctx context.Context, cfg *rest.Config) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// A copy: the collectors share this config and must not inherit a timeout
	// meant for one probe.
	probeCfg := rest.CopyConfig(cfg)
	probeCfg.Timeout = 15 * time.Second
	client, err := kubernetes.NewForConfig(probeCfg)
	if err != nil {
		return err
	}
	_, err = client.Discovery().ServerVersion()
	if ctxErr := ctx.Err(); ctxErr != nil && err != nil {
		return fmt.Errorf("%w", err)
	}
	return err
}

func (f *Fleet) stop(key string) {
	f.mu.Lock()
	t, ok := f.clusters[key]
	delete(f.clusters, key)
	f.mu.Unlock()
	if ok {
		t.cancel()
		t.removeClouds()
	}
}

func (f *Fleet) stopAll() {
	f.mu.Lock()
	all := f.clusters
	f.clusters = map[string]*tracked{}
	f.mu.Unlock()
	for _, t := range all {
		t.cancel()
		t.removeClouds()
	}
}

// removeClouds deletes the credentials written for this cluster.
//
// Best effort: a file left behind in a memory-backed directory disappears with
// the pod, and failing to remove it is not a reason to hold up a shutdown.
func (t *tracked) removeClouds() {
	if t.cloudsPath != "" {
		_ = os.Remove(t.cloudsPath)
	}
}

// NodeCount summarizes the workload cluster's nodes.
type NodeCount struct {
	Ready    int
	Total    int
	Cordoned int
}

// NodePool is one control plane or worker pool as Cluster API reports it.
type NodePool struct {
	Name    string
	Role    string
	Ready   int32
	Desired int32
	Version string
	Paused  bool
	Rolling bool
}

// Capacity is what the workload cluster has committed against what it has.
//
// Requests rather than usage: this is what the scheduler has already promised,
// which is the number deciding whether the next workload fits. Usage would need
// a metrics pipeline that not every cluster runs, and a figure that is missing
// on half the fleet is worse than one that is merely conservative.
type Capacity struct {
	CPURequested   int64 // millicores
	CPUAllocatable int64
	MemRequested   int64 // bytes
	MemAllocatable int64
}

// Known reports whether there is anything to show.
func (c Capacity) Known() bool { return c.CPUAllocatable > 0 || c.MemAllocatable > 0 }

// CPUPercent is committed CPU as a percentage of allocatable.
func (c Capacity) CPUPercent() int { return percent(c.CPURequested, c.CPUAllocatable) }

// MemPercent is committed memory as a percentage of allocatable.
func (c Capacity) MemPercent() int { return percent(c.MemRequested, c.MemAllocatable) }

func percent(part, whole int64) int {
	if whole <= 0 {
		return 0
	}
	return int(float64(part) / float64(whole) * 100)
}

// WorkloadCount rolls up one workload kind: how many are at full replicas.
type WorkloadCount struct {
	Kind  string
	Ready int
	Total int
}

// Degraded reports whether any workload of this kind is short of its replicas.
func (w WorkloadCount) Degraded() bool { return w.Ready < w.Total }

// ClusterView is one row of the fleet page: everything decided, nothing styled.
//
// Every judgement in here was made by sextant — the health cells, the rollout
// state, whether a cordon counts. A template renders these fields; it does not
// interpret them.
type ClusterView struct {
	Namespace string
	Name      string

	// Status is the whole cluster folded to one severity, for sorting and for
	// the indicator a reader scans first.
	Status health.Status
	Cells  []health.Cell

	Version string
	Phase   string
	Paused  bool

	ControlPlane model.ReplicaBucket
	Workers      model.ReplicaBucket

	// Pools is the control plane and each worker pool, with its own version.
	// During an upgrade that per-pool version is the progress report: the
	// cluster-level version says nothing about which pools have moved.
	Pools []NodePool

	Nodes NodeCount
	// NodesKnown separates "no nodes" from "we could not read the nodes".
	// Without it an unreadable workload cluster renders 0/0, which looks like
	// an empty cluster rather than an unanswered question.
	NodesKnown bool

	Capacity      Capacity
	Workloads     []WorkloadCount
	UnhealthyPods int

	// WorkloadProblem says why the workload cluster could not be read, when the
	// management side is answering but the workload side is not.
	//
	// This is the failure a fleet page most needs to name. Binnacle reads two
	// clusters per card, and the management side alone produces a plausible,
	// entirely healthy-looking one: Cluster API says Provisioned, Machines are
	// Running, hosts are fine. Every signal that could contradict it — nodes,
	// pods, the CNI, storage — comes from the side that is unreachable, and a
	// cell with no data is simply omitted rather than shown as missing. Silence
	// from half the sources must not read as good news.
	WorkloadProblem string

	// CloudsProblem is set when this cluster has OpenStack credentials that
	// could not be used. Its own field, not folded into WorkloadProblem,
	// because the consequence is different and much narrower: the cluster is
	// read normally and only the OpenStack pane is missing. Reported all the
	// same — a pane absent because nobody configured it and one absent because
	// the configuration is broken look identical, and only one of them is
	// somebody's to fix.
	CloudsProblem string

	Rollout rollout.State

	// Problem is set when the cluster cannot be read at all: no credentials, a
	// watcher that would not start, an unreachable API server. It is rendered
	// in place of the numbers, never alongside them, so a row can never show
	// stale counts under a connection error.
	Problem   string
	UpdatedAt time.Time
}

// Rolling reports whether this cluster has an upgrade in flight.
func (c ClusterView) Rolling() bool { return c.Rollout.Active }

// View returns the current state of every cluster, worst first, then by name.
func (f *Fleet) View() []ClusterView {
	f.mu.Lock()
	tracked := make([]*tracked, 0, len(f.clusters))
	for _, t := range f.clusters {
		tracked = append(tracked, t)
	}
	f.mu.Unlock()

	out := make([]ClusterView, 0, len(tracked))
	for _, t := range tracked {
		out = append(out, t.view(f.opts.Profile))
	}
	sortViews(out)
	return out
}

// sortViews orders clusters worst first, then by namespace and name.
//
// Ordering is a deliberate choice rather than a stable list: a fleet page is
// read top-down under time pressure, and the cluster that needs attention
// should not be the one that has to be scrolled to. Ties break on name so the
// page does not reshuffle itself under a reader between updates.
func sortViews(views []ClusterView) {
	sort.Slice(views, func(i, j int) bool {
		if views[i].Status != views[j].Status {
			return views[i].Status > views[j].Status
		}
		if views[i].Namespace != views[j].Namespace {
			return views[i].Namespace < views[j].Namespace
		}
		return views[i].Name < views[j].Name
	})
}

// Len reports how many clusters are being tracked.
func (f *Fleet) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.clusters)
}

func (t *tracked) view(prof profile.Profile) ClusterView {
	v := ClusterView{Namespace: t.discovered.Namespace, Name: t.discovered.Name}

	t.mu.Lock()
	switch {
	case t.discovered.Err != nil:
		v.Problem = t.discovered.Err.Error()
	case t.watchErr != nil:
		v.Problem = t.watchErr.Error()
	case t.reachable != nil && !*t.reachable:
		v.Problem = t.serverErr.Error()
	}
	t.mu.Unlock()

	if v.Problem != "" {
		v.Status = health.StatusErr
		return v
	}

	v.Cells = append(health.CoreCells(t.store, prof.NodeRoles), t.registry.BannerCells(t.store)...)
	v.Status = health.Worst(v.Cells)
	v.Rollout = rollout.Detect(t.store, "")

	v.readManagement(t.store)

	// A cluster only half of which can be read must not be able to render as
	// healthy. The management side alone says Provisioned, Running, fine —
	// every signal that could contradict it lives on the side we cannot see.
	//
	// Not yet having reported is excluded, and the distinction is the whole
	// point: at startup every cluster is silent, and a fleet that flashed amber
	// on every restart would teach a reader to ignore amber.
	t.mu.Lock()
	probeErr, probed := t.workloadErr, t.workloadProbe
	if t.cloudsErr != nil {
		v.CloudsProblem = t.cloudsErr.Error()
	}
	t.mu.Unlock()

	// A zero startedAt means the age is unknown, not infinite. Treating unknown
	// as "long past the grace period" would make every such cluster a fault.
	var age time.Duration
	if !t.startedAt.IsZero() {
		age = time.Since(t.startedAt)
	}
	if broken := v.readWorkload(t.store, prof, probeErr, probed, age); broken {
		v.Status = v.Status.Worse(health.StatusWarn)
	}
	return v
}

// readManagement fills in what Cluster API reports: the cluster itself, the
// control plane, and each worker pool.
func (v *ClusterView) readManagement(s *store.Store) {
	if snap, ok := store.Get[model.Snapshot[model.Cluster]](s, model.KeyMgmtClusters); ok {
		for _, c := range snap.Items {
			if c.Name != v.Name {
				continue
			}
			v.Version, v.Phase, v.Paused = c.Version, c.Phase, c.Paused
			v.ControlPlane, v.Workers = c.ControlPlane, c.Workers
		}
		v.UpdatedAt = snap.UpdatedAt
	}

	if snap, ok := store.Get[model.Snapshot[model.KubeadmControlPlane]](s, model.KeyMgmtKCPs); ok {
		for _, k := range snap.Items {
			v.Pools = append(v.Pools, NodePool{
				Name: k.Name, Role: "Control Plane",
				Ready: k.ReadyReplicas, Desired: k.DesiredReplicas,
				Version: k.Version, Paused: k.Paused, Rolling: k.Rolling(),
			})
			// A cluster built without a ClusterClass has no .spec.topology, so
			// its Cluster object carries no version at all and the card would
			// read "version unknown" forever. The control plane's version is
			// the cluster's version by any useful definition.
			if v.Version == "" {
				v.Version = k.Version
			}
			if v.ControlPlane.Desired == 0 {
				v.ControlPlane = model.ReplicaBucket{
					Desired: k.DesiredReplicas, Ready: k.ReadyReplicas,
					UpToDate: k.UpToDateReplicas, Available: k.AvailableReplicas,
				}
			}
		}
	}

	if snap, ok := store.Get[model.Snapshot[model.MachineDeployment]](s, model.KeyMgmtMachineDeployments); ok {
		var workers model.ReplicaBucket
		for _, m := range snap.Items {
			v.Pools = append(v.Pools, NodePool{
				Name: m.Name, Role: "Workers",
				Ready: m.ReadyReplicas, Desired: m.DesiredReplicas,
				Version: m.Version, Paused: m.Paused, Rolling: m.Rolling(),
			})
			workers.Desired += m.DesiredReplicas
			workers.Ready += m.ReadyReplicas
			workers.UpToDate += m.UpToDateReplicas
		}
		if v.Workers.Desired == 0 {
			v.Workers = workers
		}
	}
	sort.Slice(v.Pools, func(i, j int) bool {
		if v.Pools[i].Role != v.Pools[j].Role {
			// Control plane first: it is the half of a cluster that has to be
			// healthy for the other half to matter.
			return v.Pools[i].Role == "Control Plane"
		}
		return v.Pools[i].Name < v.Pools[j].Name
	})
}

// readWorkload fills in what the workload cluster reports, or records why it
// could not be read.
//
// The bool reports whether the workload side is *broken*, as opposed to merely
// quiet. Both set WorkloadProblem, because a reader deserves to know either
// way, but only the first is a reason to darken the cluster's status.
func (v *ClusterView) readWorkload(
	s *store.Store, prof profile.Profile, probeErr error, probed bool, age time.Duration,
) bool {
	nodes, ok := store.Get[model.Snapshot[model.Node]](s, model.KeyWorkloadNodes)
	switch {
	case !ok && probed && probeErr != nil:
		// The workload API server was asked directly and said why. This is the
		// answer worth having: "dial tcp ...: i/o timeout" tells an operator
		// what to fix, where silence tells them nothing.
		v.WorkloadProblem = probeErr.Error()
		return true
	case !ok && age > workloadGrace:
		// Silence past the grace period is a fault even without a reason.
		// Reporting it as "still starting up" would be a card that stays green
		// for as long as the process runs.
		v.WorkloadProblem = fmt.Sprintf(
			"nothing has been read from the workload cluster in %s", age.Round(time.Second))
		return true
	case !ok:
		v.WorkloadProblem = "the workload cluster has not reported yet"
		return false
	case nodes.Err != nil:
		v.WorkloadProblem = nodes.Err.Error()
		return true
	case len(nodes.Items) == 0 && nodes.Note != "":
		// The source says it is not ready and why. That is a state, not a fault.
		v.WorkloadProblem = nodes.Note
		return false
	case len(nodes.Items) == 0:
		// Reachable and genuinely empty is not a thing a Cluster API workload
		// cluster is, so this is the shape an unusable credential takes when
		// nothing failed loudly enough to be recorded as an error.
		v.WorkloadProblem = "the workload cluster reported no nodes"
		return true
	}

	v.NodesKnown = true
	for _, n := range nodes.Items {
		v.Nodes.Total++
		if n.Ready() {
			v.Nodes.Ready++
		}
		// Only a cordon the profile has not declared expected is counted, for
		// the same reason the health cell ignores the others: a fleet of
		// permanently cordoned hypervisors would otherwise report a standing
		// drain.
		if n.Cordoned && prof.NodeRoles.CordonIsNews(n.Role, n.Ready()) {
			v.Nodes.Cordoned++
		}
		v.Capacity.CPUAllocatable += n.AllocatableCPU
		v.Capacity.CPURequested += n.RequestedCPU
		v.Capacity.MemAllocatable += n.AllocatableMemory
		v.Capacity.MemRequested += n.RequestedMemory
	}
	if nodes.UpdatedAt.After(v.UpdatedAt) {
		v.UpdatedAt = nodes.UpdatedAt
	}

	if snap, ok := store.Get[model.Snapshot[model.Workload]](s, model.KeyWorkloadWorkloads); ok {
		byKind := map[string]*WorkloadCount{}
		for _, w := range snap.Items {
			c, seen := byKind[w.Kind]
			if !seen {
				c = &WorkloadCount{Kind: w.Kind}
				byKind[w.Kind] = c
			}
			c.Total++
			if w.Ready >= w.Desired {
				c.Ready++
			}
		}
		for _, c := range byKind {
			v.Workloads = append(v.Workloads, *c)
		}
		sort.Slice(v.Workloads, func(i, j int) bool { return v.Workloads[i].Kind < v.Workloads[j].Kind })
	}

	if snap, ok := store.Get[model.Snapshot[model.Pod]](s, model.KeyWorkloadPods); ok {
		for _, pod := range snap.Items {
			if !pod.IsHealthy {
				v.UnhealthyPods++
			}
		}
	}
	return false
}
