package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/runlevel-six/binnacle/internal/build"
	"github.com/runlevel-six/binnacle/internal/remote"
	"github.com/runlevel-six/binnacle/internal/ui"
	"github.com/runlevel-six/binnacle/pkg/profile"
	"github.com/runlevel-six/binnacle/pkg/tui"
)

// ServerOptions is what fleet mode needs to connect and render.
//
// It is a struct rather than a parameter list because every field but the last
// two is a string, and four adjacent strings at a call site are four chances to
// swap two of them silently.
type ServerOptions struct {
	// URL is the root of the binnacle deployment (e.g. "http://binnacle:8080").
	URL string
	// Token supplies the bearer credential for each request. A session renews
	// it as it ages, which is why this is a function and not a string — see
	// [remote.TokenFunc].
	Token remote.TokenFunc
	// Cluster, when non-empty, skips the fleet list and goes straight to that
	// cluster's dashboard. It is a "namespace/name" pair identifying one
	// cluster in the fleet; Esc returns to the fleet list.
	Cluster string
	// Profile carries the site conventions the per-cluster dashboards render
	// with. See [ServerClusterConfig.Profile] for why the client resolves one
	// even though the data is collected by the server.
	Profile profile.Profile
	// Theme is applied before the UI starts, so the first frame is in the
	// right palette.
	Theme tui.Theme
	Build build.Info
}

// RunServer connects to a binnacle server and runs the fleet TUI.
//
// The remote source's SSE subscriber runs in the background and drives redraws
// through Changed.
func RunServer(ctx context.Context, opts ServerOptions) error {
	tui.ApplyTheme(opts.Theme)

	src := remote.New(opts.URL, opts.Token)

	// Start the SSE subscriber in the background. It drives Changed, which
	// the model waits on between fetches.
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- src.Run(subCtx)
	}()

	fleetModel := ui.NewFleet(src, opts.URL, opts.Build)

	// The cluster builder opens a per-cluster SSE stream, populates a store,
	// and builds a dashboard model. Cleanup cancels the stream when the
	// operator returns to the fleet.
	clusterCfg := ServerClusterConfig{
		ServerURL: opts.URL,
		Token:     opts.Token,
		Theme:     opts.Theme,
		BuildInfo: opts.Build,
		Profile:   opts.Profile,
	}
	builder := func(ns, name string) (*ui.Model, func(), error) {
		return BuildServerClusterModel(clusterCfg, ns, name)
	}
	fleetModel.SetBuilder(builder)

	router := ui.NewSextant(fleetModel, builder, opts.Build)

	// --server-cluster skips the fleet list and goes straight to the
	// cluster's dashboard. The initial fleet fetch still runs, so Esc from
	// the dashboard returns to a populated list.
	if opts.Cluster != "" {
		parts := strings.SplitN(opts.Cluster, "/", 2)
		if len(parts) == 2 {
			router.Update(ui.DrillDownMsg{Namespace: parts[0], Name: parts[1]})
		}
	}

	program := tea.NewProgram(router,
		tea.WithAltScreen(),
		tea.WithContext(ctx),
	)
	_, err := program.Run()
	cancel()

	// If the SSE subscriber exited with an error before the program did,
	// surface it — but a canceled context is expected on normal shutdown.
	if err == nil {
		select {
		case subErr := <-errCh:
			if subErr != nil && !errors.Is(subErr, context.Canceled) {
				return fmt.Errorf("server connection: %w", subErr)
			}
		default:
		}
	}
	return err
}
