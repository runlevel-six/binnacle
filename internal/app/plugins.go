package app

import (
	"fmt"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/runlevel-six/sextant/internal/plugin/ceph"
	"github.com/runlevel-six/sextant/internal/plugin/cilium"
	"github.com/runlevel-six/sextant/internal/plugin/kube"
	"github.com/runlevel-six/sextant/internal/plugin/metallb"
	"github.com/runlevel-six/sextant/internal/plugin/openstack"
	"github.com/runlevel-six/sextant/internal/plugin/ovn"
	"github.com/runlevel-six/sextant/internal/profile"
)

// registerPlugins adds every built-in plugin to the registry.
//
// Registration is unconditional; detection decides what actually runs. That
// separation is what lets a plugin be added without any risk to users who do not
// run its subsystem — an absent one contributes no source, no pane and no banner
// cell.
//
// Plugins observe the *workload* cluster, since that is where a CNI, a load
// balancer and a storage layer live. The management cluster runs controllers.
func (s *Setup) registerPlugins(cfg *rest.Config) error {
	client, err := kube.NewClient(cfg)
	if err != nil {
		return fmt.Errorf("plugin client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("plugin dynamic client: %w", err)
	}

	settings := s.Resolved.Profile.Plugins
	for _, p := range []interface {
		Name() string
	}{
		// Registration order decides left-to-right placement among panes of equal
		// priority, and a merged frame takes its first member's slot. This order
		// yields the bottom row as Network, Ceph, Cloud, OpenStack.
		metallb.New(client, dyn, metallb.SettingsFrom(settingsFor(settings, metallb.Name))),
		cilium.New(client, cilium.SettingsFrom(settingsFor(settings, cilium.Name))),
		ceph.New(client, ceph.SettingsFrom(settingsFor(settings, ceph.Name))),
		ovn.New(client, ovn.SettingsFrom(settingsFor(settings, ovn.Name))),
		// OpenStack talks to an API rather than the cluster, so it takes no
		// kube client — it detects on a resolvable clouds.yaml profile.
		openstack.New(s.openStackSettings(settings)),
	} {
		if err := s.Registry.Register(p); err != nil {
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
// The precedence is the same chain the rest of the configuration uses: the
// --os-cloud flag, then $OS_CLOUD, then the site profile's own plugin block. That
// ordering is what an operator running several clouds needs — the profile
// describes how a cloud is *laid out*, which is the same for all of theirs, while
// the cloud name says *which one*, and that changes per session.
func (s *Setup) openStackSettings(all map[string]profile.Settings) openstack.Settings {
	settings := openstack.SettingsFrom(settingsFor(all, openstack.Name))
	if s.Resolved.OSCloud != "" {
		settings.Cloud = s.Resolved.OSCloud
	}
	// Not configuration but runtime state: the plugin's mode-aware pane switches
	// on the same rollout signal the core panes use, and an asserted target
	// version is half of that signal.
	settings.TargetVersion = s.Resolved.TargetVersion
	return settings
}
