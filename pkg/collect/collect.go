// Package collect runs sextant's data collectors against one cluster pair and
// publishes typed snapshots into a [store.Store].
//
// It is the seam between sextant's data layer and whatever presents it. The
// terminal dashboard is one consumer; a web front end that renders the same
// verdicts in a browser is another, and neither is privileged. Everything here
// is deliberately free of presentation: no theme, no pane, no terminal.
//
// # Why rest.Config and not a kubeconfig context
//
// The command-line tool starts from a kubeconfig because an operator thinks in
// context names. A service does not — it runs with a mounted token and, for the
// workload clusters, with credentials Cluster API minted into a Secret. Taking
// [rest.Config] values directly is what lets both start from what they actually
// have, and keeps kubeconfig resolution a concern of the CLI rather than of the
// collectors.
//
// # Verdicts belong here
//
// A caller reads two things back: typed snapshots via [store.Get] with the keys
// in [github.com/runlevel-six/sextant/pkg/model], and health verdicts via
// [plugin.Registry.BannerCells]. Both are already decided — whether a cordoned
// node is expected, which Raft member is entitled to report staleness, whether a
// rollout is under way. A consumer that re-derives any of that will eventually
// disagree with the dashboard about whether a cluster is healthy, and there is
// no reason for two answers to exist.
package collect

import (
	"context"
	"fmt"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/runlevel-six/sextant/internal/core/capi"
	"github.com/runlevel-six/sextant/internal/core/workload"
	"github.com/runlevel-six/sextant/internal/plugin/ceph"
	"github.com/runlevel-six/sextant/internal/plugin/cilium"
	pluginkube "github.com/runlevel-six/sextant/internal/plugin/kube"
	"github.com/runlevel-six/sextant/internal/plugin/metallb"
	"github.com/runlevel-six/sextant/internal/plugin/openstack"
	"github.com/runlevel-six/sextant/internal/plugin/ovn"
	"github.com/runlevel-six/sextant/pkg/plugin"
	"github.com/runlevel-six/sextant/pkg/profile"
	"github.com/runlevel-six/sextant/pkg/store"
)

// Options describes one cluster pair to collect from.
//
// Store and Registry are supplied by the caller rather than created here so that
// a consumer watching several clusters can decide for itself how to hold them —
// one pair each, keyed by cluster, is the usual arrangement.
type Options struct {
	// Store receives every snapshot. Required.
	Store *store.Store
	// Registry receives the built-in plugins. Required, and should be empty:
	// registration is unconditional, and [Activate] decides what runs.
	Registry *plugin.Registry

	// Management is the cluster hosting Cluster API and Metal3. Required.
	Management *rest.Config
	// Workload is the cluster hosting nodes, pods, and the subsystems the
	// plugins observe. Nil means the management cluster is also the workload
	// cluster, which is the single-cluster case.
	Workload *rest.Config

	// Profile describes how this site is laid out — node roles, event
	// filtering, per-plugin settings. Use [profile.Default] for the built-in.
	Profile profile.Profile

	// ManagementNamespaces scopes the Cluster API and management event reads.
	// Empty means all namespaces.
	ManagementNamespaces []string
	// CAPIClusterName narrows Cluster API objects to one cluster. Empty means
	// every cluster in scope, which is right where a management cluster owns
	// only one.
	CAPIClusterName string
	// OSCloud names the clouds.yaml profile for the OpenStack plugin. Empty
	// leaves the choice to the profile's own plugin settings.
	OSCloud string
	// TargetVersion is the Kubernetes version being rolled out, when an
	// operator has asserted one. It is runtime state rather than
	// configuration: the OpenStack plugin's mode-aware pane switches on the
	// same rollout signal the core panes use, and an asserted target version
	// is half of that signal.
	TargetVersion string
}

func (o Options) validate() error {
	switch {
	case o.Store == nil:
		return fmt.Errorf("collect: Store is required")
	case o.Registry == nil:
		return fmt.Errorf("collect: Registry is required")
	case o.Management == nil:
		return fmt.Errorf("collect: Management rest.Config is required")
	}
	return nil
}

// Watch launches the management and workload watchers in the background and
// registers the built-in plugins. It returns as soon as they are started; each
// runs until ctx is canceled.
//
// A watcher that cannot be constructed is reported, but one failing does not
// prevent the other from running: a management cluster that denies access to
// Cluster API is still worth watching for nodes and pods, and the reverse holds
// too.
//
// reachability, when non-nil, receives the management cluster's server version
// or the error from trying to reach it. Callers use it to report one clear
// connection failure rather than the same error repeated per resource kind.
//
// Watch registers plugins but does not start them. Call [Activate] next.
func Watch(ctx context.Context, opts Options, reachability func(version string, err error)) error {
	if err := opts.validate(); err != nil {
		return err
	}

	// Management events are what the events pane reads during a rollout, where
	// Machine and host activity is reported. They are scoped to the same
	// namespace as the Cluster API objects, so a profile that narrows one
	// narrows both — an unscoped event watch on a large management cluster is
	// expensive.
	ns := firstNamespace(opts.ManagementNamespaces)
	mgmt, err := capi.New(opts.Management, opts.Store, capi.Options{
		Namespace:      ns,
		EventNamespace: ns,
		WatchEvents:    true,
		ClusterName:    opts.CAPIClusterName,
	})
	if err != nil {
		return fmt.Errorf("management watcher: %w", err)
	}
	if reachability != nil {
		reachability(mgmt.Reachable(ctx))
	}
	go func() { _ = mgmt.Run(ctx) }()

	workloadCfg := opts.Workload
	if workloadCfg == nil {
		workloadCfg = opts.Management
	}
	wl, err := workload.New(workloadCfg, opts.Store, workload.Options{
		NodeRoles: opts.Profile.NodeRoles,
		Events:    opts.Profile.Events,
	})
	if err != nil {
		return fmt.Errorf("workload watcher: %w", err)
	}
	go func() { _ = wl.Run(ctx) }()

	return registerPlugins(opts, workloadCfg)
}

// Activate runs plugin detection and starts every source that detection found.
//
// It is separate from [Watch] because a caller may want the detection results
// before anything starts polling — the diagnostic mode prints them, and the
// demo detects with no watchers running at all.
//
// Only the detected plugins are started: a source for an absent subsystem would
// poll something that is not there.
func Activate(ctx context.Context, reg *plugin.Registry, s *store.Store) []plugin.DetectResult {
	results := reg.Detect(ctx)
	for _, src := range reg.ActiveSources() {
		source := src
		go func() { _ = source.Run(ctx, s) }()
	}
	return results
}

// registerPlugins adds every built-in plugin to the registry.
//
// Registration is unconditional; detection decides what actually runs. That
// separation is what lets a plugin be added without any risk to users who do not
// run its subsystem — an absent one contributes no source, no pane and no banner
// cell.
//
// Plugins observe the *workload* cluster, since that is where a CNI, a load
// balancer and a storage layer live. The management cluster runs controllers.
func registerPlugins(opts Options, cfg *rest.Config) error {
	client, err := pluginkube.NewClient(cfg)
	if err != nil {
		return fmt.Errorf("plugin client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("plugin dynamic client: %w", err)
	}

	settings := opts.Profile.Plugins
	for _, p := range []plugin.Plugin{
		// Registration order decides left-to-right placement among panes of equal
		// priority, and a merged frame takes its first member's slot. This order
		// yields the bottom row as Network, Ceph, Cloud, OpenStack.
		metallb.New(client, dyn, metallb.SettingsFrom(settingsFor(settings, metallb.Name))),
		cilium.New(client, cilium.SettingsFrom(settingsFor(settings, cilium.Name))),
		ceph.New(client, ceph.SettingsFrom(settingsFor(settings, ceph.Name))),
		ovn.New(client, ovn.SettingsFrom(settingsFor(settings, ovn.Name))),
		// OpenStack talks to an API rather than the cluster, so it takes no
		// kube client — it detects on a resolvable clouds.yaml profile.
		openstack.New(openStackSettings(opts)),
	} {
		if err := opts.Registry.Register(p); err != nil {
			return err
		}
	}
	return nil
}

// settingsFor returns a plugin's profile block, or nil when the profile does not
// mention it. A plugin with no block uses its own defaults, so naming one in a
// profile is only ever necessary to *change* something.
func settingsFor(all map[string]profile.Settings, name string) map[string]any {
	if all == nil {
		return nil
	}
	return all[name]
}

// openStackSettings resolves which cloud to watch.
//
// An explicit Options.Cloud wins over the site profile's own plugin block. That
// ordering is what an operator running several clouds needs — the profile
// describes how a cloud is *laid out*, which is the same for all of theirs,
// while the cloud name says *which one*, and that changes per session.
func openStackSettings(opts Options) openstack.Settings {
	settings := openstack.SettingsFrom(settingsFor(opts.Profile.Plugins, openstack.Name))
	if opts.OSCloud != "" {
		settings.Cloud = opts.OSCloud
	}
	settings.TargetVersion = opts.TargetVersion
	return settings
}

// firstNamespace collapses a namespace list to the single namespace the informer
// factory accepts.
//
// A factory scopes to one namespace or to all of them, so a multi-namespace
// profile currently widens to cluster-wide and filters nothing. That is the safe
// direction — showing more than asked rather than silently hiding objects in the
// namespaces beyond the first.
func firstNamespace(namespaces []string) string {
	if len(namespaces) == 1 {
		return namespaces[0]
	}
	return ""
}
