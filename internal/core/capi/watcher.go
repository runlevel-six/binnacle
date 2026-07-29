// Package capi watches Cluster API and Metal3 resources on a management cluster
// and publishes them as model snapshots.
//
// Two decisions here are what make the package portable.
//
// Kinds are resolved to a GroupVersionResource through the discovery RESTMapper
// rather than pinned to an API version, so Cluster API's v1beta1 to v1beta2 to
// v1 progression requires no change. Version pinning is the usual reason
// third-party Cluster API tooling breaks on an upgrade.
//
// Namespaces are read cluster-wide by default. Upstream has no conventional
// namespace for Cluster objects, and per-cluster namespaces are common, so any
// single hardcoded namespace would be wrong for most users. A profile may narrow
// the scope where a site does have a convention.
package capi

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/cache"

	"github.com/runlevel-six/sextant/internal/core"
	"github.com/runlevel-six/sextant/internal/core/model"
	"github.com/runlevel-six/sextant/pkg/store"
)

// resyncPeriod is how often informers do a full relist. Frequent enough that a
// missed watch event self-heals within one refresh a user would notice.
const resyncPeriod = 30 * time.Second

// Options configures a Watcher.
type Options struct {
	// Namespace scopes the watch. Empty means all namespaces, which is the
	// default and the right one for upstream Cluster API.
	Namespace string
	// EventNamespace scopes the Event watch. Events are high-volume, so this is
	// separate from Namespace: a site can read Cluster API objects cluster-wide
	// while restricting events to the namespace where rollouts happen.
	EventNamespace string
	// WatchEvents enables the Event informer. Off by default because on a large
	// management cluster an unscoped event watch is expensive and rarely what
	// the operator wants.
	WatchEvents bool
	// ClusterName restricts the cluster-scoped kinds to one Cluster API cluster.
	// Empty means every cluster the management cluster owns, which is correct
	// upstream, where the usual case is one.
	//
	// It matters where a management cluster owns several clusters and only one is
	// being watched: the workload panes read one cluster's nodes and pods, so
	// leaving the Machines beside them unfiltered puts two clusters' fleets in one
	// table and makes the counts disagree for no visible reason.
	ClusterName string
}

// Watcher observes Cluster API and Metal3 resources on one management cluster.
type Watcher struct {
	store *store.Store
	opts  Options

	dyn      dynamic.Interface
	typed    kubernetes.Interface
	mapper   meta.RESTMapper
	gvrCache sync.Map // schema.GroupKind -> schema.GroupVersionResource

	// rebuilds are the per-kind publish functions, kept so each can be called
	// once after cache sync. See the note in Run.
	rebuilds []func()
}

// New builds a Watcher. It performs no I/O; call Run to start watching.
func New(cfg *rest.Config, s *store.Store, opts Options) (*Watcher, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("typed client: %w", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("discovery client: %w", err)
	}
	return &Watcher{
		store:  s,
		opts:   opts,
		dyn:    dyn,
		typed:  typed,
		mapper: restmapper.NewDeferredDiscoveryRESTMapper(cachedDiscovery{disc}),
	}, nil
}

// HasMetal3 reports whether the Metal3 CRDs are registered on this cluster.
//
// Used to decide whether the bare-metal panes belong in the catalog at all. A
// Cluster API cluster on another infrastructure provider is a legitimate target;
// it simply has no BareMetalHosts to show.
func (w *Watcher) HasMetal3() bool {
	_, err := w.resolveGVR(gkBareMetalHost)
	return err == nil
}

// Run starts the informers and blocks until ctx is canceled.
//
// A kind that cannot be resolved — a CRD that is not installed, or one the
// caller may not read — publishes an error snapshot and is skipped. The
// remaining informers still run. Refusing to start because one optional CRD is
// absent would make the tool useless on any cluster that is not laid out exactly
// like the author's.
func (w *Watcher) Run(ctx context.Context) error {
	dynFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		w.dyn, resyncPeriod, w.opts.Namespace, nil)

	// Core Cluster API. Each cluster-scoped kind is narrowed to the watched
	// cluster, if one was named.
	w.watch(ctx, dynFactory, gkCluster, model.KeyMgmtClusters, func(o []*unstructured.Unstructured) any {
		// A Cluster is identified by its own name rather than by the label.
		return ofCluster(ProjectClusters(o), w.opts.ClusterName, func(c model.Cluster) string { return c.Name })
	})
	w.watch(ctx, dynFactory, gkKubeadmControlPlane, model.KeyMgmtKCPs, func(o []*unstructured.Unstructured) any {
		return ofCluster(ProjectKCPs(o), w.opts.ClusterName, func(k model.KubeadmControlPlane) string { return k.ClusterName })
	})
	w.watch(ctx, dynFactory, gkMachineDeployment, model.KeyMgmtMachineDeployments, func(o []*unstructured.Unstructured) any {
		return ofCluster(ProjectMachineDeployments(o), w.opts.ClusterName, func(m model.MachineDeployment) string { return m.ClusterName })
	})
	w.watch(ctx, dynFactory, gkMachine, model.KeyMgmtMachines, func(o []*unstructured.Unstructured) any {
		return ofCluster(ProjectMachines(o), w.opts.ClusterName, func(m model.Machine) string { return m.ClusterName })
	})

	// Metal3. Absent on a Cluster API cluster using a different provider, in
	// which case these publish an error snapshot and the panes drop out.
	w.watch(ctx, dynFactory, gkMetal3Cluster, model.KeyMgmtMetal3Clusters, func(o []*unstructured.Unstructured) any {
		return ofCluster(ProjectMetal3Clusters(o), w.opts.ClusterName, func(c model.Metal3Cluster) string { return c.ClusterName })
	})
	// Metal3Machines and BareMetalHosts are deliberately *not* filtered.
	//
	// Both feed the host join, which is anchored on Machines and so is already
	// narrowed by the filter above. Narrowing these as well would break the one
	// thing that has to stay global: a host is unclaimed only if *no* cluster
	// claims it, and hiding another cluster's Metal3Machines would report its hosts
	// as free inventory. A Metal3Machine also need not carry the cluster label at
	// all, so filtering on it would drop rows on some providers.
	w.watch(ctx, dynFactory, gkMetal3Machine, model.KeyMgmtMetal3Machines, func(o []*unstructured.Unstructured) any {
		return ProjectMetal3Machines(o)
	})
	w.watch(ctx, dynFactory, gkBareMetalHost, model.KeyMgmtBareMetalHosts, func(o []*unstructured.Unstructured) any {
		return ProjectBareMetalHosts(o)
	})

	dynFactory.Start(ctx.Done())
	dynFactory.WaitForCacheSync(ctx.Done())

	if w.opts.WatchEvents {
		typedFactory := informers.NewSharedInformerFactoryWithOptions(
			w.typed, resyncPeriod, informers.WithNamespace(w.opts.EventNamespace))
		w.watchEvents(ctx, typedFactory)
		typedFactory.Start(ctx.Done())
		typedFactory.WaitForCacheSync(ctx.Done())
	}

	// Publish every kind once, now that the caches are warm.
	//
	// Informer handlers only fire on add, update and delete, so a kind with no
	// objects never triggers one — and a key that is never written is
	// indistinguishable from a source that has not started. The pane then shows
	// "loading" forever rather than "none". An empty cluster is a normal state
	// and has to be reported as a result, not as an absence.
	for _, rebuild := range w.rebuilds {
		rebuild()
	}

	<-ctx.Done()
	return ctx.Err()
}

// watch resolves a kind, registers an informer, and republishes the projected
// snapshot on every change.
func (w *Watcher) watch(
	ctx context.Context,
	factory dynamicinformer.DynamicSharedInformerFactory,
	gk schema.GroupKind,
	key string,
	project func([]*unstructured.Unstructured) any,
) {
	gvr, err := w.resolveGVR(gk)
	if err != nil {
		w.store.Put(key, model.ErrorSnapshot(err))
		return
	}

	gvrInformer := factory.ForResource(gvr)
	publish := func() {
		raw, err := gvrInformer.Lister().List(labels.Everything())
		if err != nil {
			w.store.Put(key, model.ErrorSnapshot(err))
			return
		}
		objs := make([]*unstructured.Unstructured, 0, len(raw))
		for _, ro := range raw {
			if u, ok := ro.(*unstructured.Unstructured); ok {
				objs = append(objs, u)
			}
		}
		w.store.Put(key, project(objs))
	}
	// Coalesced: a rebuild re-lists and re-projects every object of this kind, so
	// one busy resource must not be able to drive that per callback.
	trigger := core.Coalesce(ctx, core.PublishInterval, publish)
	// The registration handle is unused and the only error is "informer already
	// stopped", which happens during shutdown and has nobody left to report to.
	_, _ = gvrInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { trigger() },
		UpdateFunc: func(_, _ any) { trigger() },
		DeleteFunc: func(any) { trigger() },
	})
	w.rebuilds = append(w.rebuilds, publish)
}

func (w *Watcher) watchEvents(ctx context.Context, factory informers.SharedInformerFactory) {
	lister := factory.Core().V1().Events().Lister()
	publish := func() {
		raw, err := lister.List(labels.Everything())
		if err != nil {
			w.store.Put(model.KeyMgmtEvents, model.ErrorSnapshot(err))
			return
		}
		w.store.Put(model.KeyMgmtEvents, ProjectEvents(raw))
	}
	// Events are the busiest resource on a management cluster mid-rollout, so this
	// one especially must not rebuild per callback.
	trigger := core.Coalesce(ctx, core.PublishInterval, publish)
	// As above: the handle is unused and the only error means shutdown.
	_, _ = factory.Core().V1().Events().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { trigger() },
		UpdateFunc: func(_, _ any) { trigger() },
		DeleteFunc: func(any) { trigger() },
	})
	w.rebuilds = append(w.rebuilds, publish)
}

// resolveGVR maps a kind to a resource, caching the result so repeated lookups
// do not hit the API server.
//
// The two failure modes are reported differently because they call for different
// action. A no-match error genuinely means the CRD is not installed, which for
// Metal3 kinds is an ordinary state. Anything else — an unreachable API server,
// an expired credential, a denied discovery request — is a problem with the
// connection, and describing it as "not installed" would send the reader looking
// for a missing CRD instead of at their kubeconfig.
func (w *Watcher) resolveGVR(gk schema.GroupKind) (schema.GroupVersionResource, error) {
	if v, ok := w.gvrCache.Load(gk); ok {
		return v.(schema.GroupVersionResource), nil
	}
	mapping, err := w.mapper.RESTMapping(gk)
	if err != nil {
		if meta.IsNoMatchError(err) {
			return schema.GroupVersionResource{}, fmt.Errorf("%s is not installed on this cluster", gk)
		}
		return schema.GroupVersionResource{}, fmt.Errorf("could not look up %s: %w", gk, err)
	}
	w.gvrCache.Store(gk, mapping.Resource)
	return mapping.Resource, nil
}

// Reachable checks that the API server answers, returning its version string.
//
// Called before the per-kind lookups so an unreachable cluster produces one
// clear message instead of the same connection error repeated once per kind.
func (w *Watcher) Reachable(ctx context.Context) (string, error) {
	info, err := w.typed.Discovery().ServerVersion()
	if err != nil {
		return "", fmt.Errorf("cannot reach the management cluster: %w", err)
	}
	return info.GitVersion, nil
}

// cachedDiscovery adapts a discovery client to the interface the deferred
// RESTMapper wants. Reporting itself as always fresh is correct for a
// short-lived observer: the API surface does not change under us mid-session,
// and re-running discovery on every lookup would be wasteful.
type cachedDiscovery struct {
	discovery.DiscoveryInterface
}

func (cachedDiscovery) Fresh() bool { return true }

func (cachedDiscovery) Invalidate() {}

func (c cachedDiscovery) WithLegacy() discovery.DiscoveryInterface { return c.DiscoveryInterface }

// ofCluster narrows a snapshot to the watched cluster.
//
// The comparison is equality *or* prefix, which is not laziness. A Cluster object
// is conventionally named after the cluster with a suffix — the cluster reached
// through context site-a-tenant-01 is Cluster
// tenant-01-cluster — and a name derived from a context name cannot know
// what that suffix is. Requiring equality there would match nothing and empty
// every Cluster API pane, which is the one failure this filter must not have.
//
// An item whose cluster cannot be determined is kept rather than dropped. The
// label is set by Cluster API's controllers and is almost always there, but an
// object made by hand or by an older release may lack it — and hiding a Machine
// because its provenance is unclear is the worse of the two mistakes. An extra row
// is visible and recoverable; a missing row looks like the cluster does not have
// that Machine at all.
func ofCluster[T any](snap model.Snapshot[T], cluster string, clusterOf func(T) string) model.Snapshot[T] {
	if cluster == "" || snap.Err != nil {
		return snap
	}
	kept := make([]T, 0, len(snap.Items))
	for _, item := range snap.Items {
		if name := clusterOf(item); name == "" || strings.HasPrefix(name, cluster) {
			kept = append(kept, item)
		}
	}
	snap.Items = kept
	return snap
}
