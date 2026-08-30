package fleet

import (
	"context"
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
	// OSCloud names a clouds.yaml profile for the OpenStack plugin. A deployed
	// binnacle usually leaves this empty: without credentials the plugin fails
	// detection and contributes nothing, which is the designed behaviour.
	OSCloud string
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

	mu       sync.Mutex
	watchErr error
	// reachable is nil until the management watcher has reported, so "not
	// heard from yet" stays distinguishable from "unreachable".
	reachable  *bool
	serverErr  error
	kubeVerion string
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
	}

	f.mu.Lock()
	f.clusters[d.Key()] = t
	f.mu.Unlock()

	// A cluster with no credentials still gets a slot on the page — it shows
	// what discovery found and why it cannot be read — but nothing to collect.
	if d.Config == nil {
		return
	}

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
			OSCloud:              f.opts.OSCloud,
		}, func(version string, err error) {
			t.mu.Lock()
			ok := err == nil
			t.reachable, t.serverErr, t.kubeVerion = &ok, err, version
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

func (f *Fleet) stop(key string) {
	f.mu.Lock()
	t, ok := f.clusters[key]
	delete(f.clusters, key)
	f.mu.Unlock()
	if ok {
		t.cancel()
	}
}

func (f *Fleet) stopAll() {
	f.mu.Lock()
	all := f.clusters
	f.clusters = map[string]*tracked{}
	f.mu.Unlock()
	for _, t := range all {
		t.cancel()
	}
}

// NodeCount summarises the workload cluster's nodes.
type NodeCount struct {
	Ready    int
	Total    int
	Cordoned int
}

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
	Nodes        NodeCount
	Rollout      rollout.State

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
//
// Ordering is a deliberate choice rather than a stable list: a fleet page is
// read top-down under time pressure, and the cluster that needs attention
// should not be the one that has to be scrolled to.
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
	sort.Slice(out, func(i, j int) bool {
		if out[i].Status != out[j].Status {
			return out[i].Status > out[j].Status
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
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

	if snap, ok := store.Get[model.Snapshot[model.Cluster]](t.store, model.KeyMgmtClusters); ok {
		for _, c := range snap.Items {
			if c.Name != t.discovered.Name {
				continue
			}
			v.Version, v.Phase, v.Paused = c.Version, c.Phase, c.Paused
			v.ControlPlane, v.Workers = c.ControlPlane, c.Workers
		}
		v.UpdatedAt = snap.UpdatedAt
	}

	if snap, ok := store.Get[model.Snapshot[model.Node]](t.store, model.KeyWorkloadNodes); ok {
		for _, n := range snap.Items {
			v.Nodes.Total++
			if n.Ready() {
				v.Nodes.Ready++
			}
			// Only a cordon the profile has not declared expected is counted,
			// for the same reason the health cell ignores the others: a fleet
			// of permanently cordoned hypervisors would otherwise report a
			// standing drain.
			if n.Cordoned && prof.NodeRoles.CordonIsNews(n.Role, n.Ready()) {
				v.Nodes.Cordoned++
			}
		}
		if snap.UpdatedAt.After(v.UpdatedAt) {
			v.UpdatedAt = snap.UpdatedAt
		}
	}
	return v
}
