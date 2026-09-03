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
	"github.com/runlevel-six/binnacle/pkg/tui"
)

// RunServer connects to a binnacle server and runs the fleet TUI.
//
// The server URL is the root of the binnacle deployment (e.g.
// "http://binnacle:8080"). token, when non-empty, is sent as a Bearer header;
// a server running with --allow-unauthenticated does not need one.
//
// serverCluster, when non-empty, skips the fleet list and goes straight to
// that cluster's dashboard. It is a "namespace/name" pair identifying one
// cluster in the fleet. Esc returns to the fleet list.
//
// theme is applied before the UI starts, so the first frame is in the right
// palette. The remote source's SSE subscriber runs in the background and
// drives redraws through Changed.
func RunServer(ctx context.Context, serverURL, token, serverCluster string, info build.Info, theme tui.Theme) error {
	tui.ApplyTheme(theme)

	src := remote.New(serverURL, token)

	// Start the SSE subscriber in the background. It drives Changed, which
	// the model waits on between fetches.
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- src.Run(subCtx)
	}()

	fleetModel := ui.NewFleet(src, serverURL, info)

	// The cluster builder opens a per-cluster SSE stream, populates a store,
	// and builds a dashboard model. Cleanup cancels the stream when the
	// operator returns to the fleet.
	clusterCfg := ServerClusterConfig{
		ServerURL: serverURL,
		Token:     token,
		Theme:     theme,
		BuildInfo: info,
	}
	builder := func(ns, name string) (*ui.Model, func(), error) {
		return BuildServerClusterModel(clusterCfg, ns, name)
	}
	fleetModel.SetBuilder(builder)

	router := ui.NewSextant(fleetModel, builder, info)

	// --server-cluster skips the fleet list and goes straight to the
	// cluster's dashboard. The initial fleet fetch still runs, so Esc from
	// the dashboard returns to a populated list.
	if serverCluster != "" {
		parts := strings.SplitN(serverCluster, "/", 2)
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
