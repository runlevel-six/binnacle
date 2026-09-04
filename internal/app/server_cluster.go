package app

import (
	"context"

	"github.com/runlevel-six/binnacle/internal/build"
	"github.com/runlevel-six/binnacle/internal/config"
	"github.com/runlevel-six/binnacle/internal/plugin/ceph"
	"github.com/runlevel-six/binnacle/internal/plugin/cilium"
	"github.com/runlevel-six/binnacle/internal/plugin/metallb"
	"github.com/runlevel-six/binnacle/internal/plugin/openstack"
	"github.com/runlevel-six/binnacle/internal/plugin/ovn"
	"github.com/runlevel-six/binnacle/internal/remote"
	"github.com/runlevel-six/binnacle/internal/ui"
	"github.com/runlevel-six/binnacle/pkg/plugin"
	"github.com/runlevel-six/binnacle/pkg/profile"
	"github.com/runlevel-six/binnacle/pkg/store"
	"github.com/runlevel-six/binnacle/pkg/tui"
)

// remotePlugins are wrapper types that embed the real plugins so Panes,
// Cells, and Summary use the real implementations — but Detect returns
// true and Run is a no-op, because the remote ClusterSource populates
// the store over HTTP. This mirrors what internal/demo does for the
// single-cluster demo fixture.
//
// Each wrapper is a distinct concrete type (not a generic) because Go
// does not promote methods from a named field, only from an embedded
// type — see the comment on demoMetalLB in internal/demo/plugins.go.

type remoteMetalLB struct{ *metallb.Plugin }

func (remoteMetalLB) Detect(context.Context) (bool, error)    { return true, nil }
func (remoteMetalLB) Run(context.Context, *store.Store) error { return nil }

type remoteCilium struct{ *cilium.Plugin }

func (remoteCilium) Detect(context.Context) (bool, error)    { return true, nil }
func (remoteCilium) Run(context.Context, *store.Store) error { return nil }

type remoteCeph struct{ *ceph.Plugin }

func (remoteCeph) Detect(context.Context) (bool, error)    { return true, nil }
func (remoteCeph) Run(context.Context, *store.Store) error { return nil }

type remoteOVN struct{ *ovn.Plugin }

func (remoteOVN) Detect(context.Context) (bool, error)    { return true, nil }
func (remoteOVN) Run(context.Context, *store.Store) error { return nil }

type remoteOpenStack struct{ *openstack.Plugin }

func (remoteOpenStack) Detect(context.Context) (bool, error)    { return true, nil }
func (remoteOpenStack) Run(context.Context, *store.Store) error { return nil }

// ServerClusterConfig holds everything the per-cluster builder needs to
// assemble a dashboard for one cluster on a remote server.
type ServerClusterConfig struct {
	ServerURL string
	Token     remote.TokenFunc
	Theme     tui.Theme
	BuildInfo build.Info
	// Profile supplies the site conventions the panes are rendered with:
	// critical workloads, node-role label keys and their display names, and
	// which roles are cordoned on purpose.
	//
	// It is the *client's* profile, not the server's. The data arriving over
	// the wire was collected with the server's, but these particular judgements
	// are applied at render time and have nowhere to travel: nothing publishes
	// a profile over the API. Reading the operator's own is what keeps
	// `--profile` from being silently discarded in fleet mode, which showed up
	// as a pods pane with no critical workloads and every reserved compute node
	// reported as a standing drain.
	//
	// The zero value falls back to [profile.Default], so a caller that does not
	// set it gets the same assumption-free profile as an unconfigured local run
	// rather than a profile with no node-role label keys at all.
	Profile profile.Profile
}

// BuildServerClusterModel creates a store and registry for one cluster,
// starts the remote ClusterSource streaming into the store, and builds
// a dashboard model from it. The caller should call the returned
// cleanup function when the model is discarded (e.g. on Esc).
func BuildServerClusterModel(cfg ServerClusterConfig, namespace, name string) (*ui.Model, func(), error) {
	ctx, cancel := context.WithCancel(context.Background())

	src := remote.NewClusterSource(cfg.ServerURL, cfg.Token, namespace, name)

	// Detect checks that the cluster exists on the server. If it doesn't,
	// there's nothing to stream.
	detected, err := src.Detect(ctx)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	if !detected {
		cancel()
		return nil, nil, nil
	}

	st := store.New()
	reg := plugin.NewRegistry()

	// Register all five plugins with wrappers that detect as present and
	// run nothing. The pane providers will find them in the active set and
	// build panes that read from the store, which the ClusterSource fills.
	for _, p := range []plugin.Plugin{
		remoteMetalLB{metallb.New(nil, nil, metallb.Settings{})},
		remoteCilium{cilium.New(nil, cilium.Settings{})},
		remoteCeph{ceph.New(nil, ceph.Settings{})},
		remoteOVN{ovn.New(nil, ovn.Settings{})},
		remoteOpenStack{openstack.New(openstack.Settings{})},
	} {
		if err := reg.Register(p); err != nil {
			cancel()
			return nil, nil, err
		}
	}
	reg.Detect(ctx)

	// Start streaming into the store.
	go func() { _ = src.Run(ctx, st) }()

	prof := cfg.Profile
	if prof.Name == "" {
		prof = profile.Default()
	}
	resolved := config.Resolved{
		Profile:       prof,
		Theme:         cfg.Theme,
		TargetVersion: "",
	}

	s := &Setup{
		Resolved: resolved,
		Build:    cfg.BuildInfo,
		Store:    st,
		Registry: reg,
	}

	model, err := s.BuildModel()
	if err != nil {
		cancel()
		return nil, nil, err
	}

	cleanup := func() { cancel() }
	return model, cleanup, nil
}
