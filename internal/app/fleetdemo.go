package app

import (
	"context"
	"fmt"
	"io"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/runlevel-six/binnacle/internal/build"
	"github.com/runlevel-six/binnacle/internal/config"
	"github.com/runlevel-six/binnacle/internal/demo"
	"github.com/runlevel-six/binnacle/internal/fleet"
	"github.com/runlevel-six/binnacle/internal/ui"
	"github.com/runlevel-six/binnacle/pkg/plugin"
	"github.com/runlevel-six/binnacle/pkg/tui"
)

// RunFleetDemo runs the fleet screen against the fleet demo fixture.
//
// It is the fleet-screen counterpart to RunDemo: no server, no kubeconfig,
// just the fixture data from fleet.NewDemo. The fleet list updates as the
// demo's rolling cluster advances, so the live-update path is exercised
// without a deployment.
//
// Selecting a cluster drills into the single-cluster demo dashboard — the
// same fixture internal/demo builds for --demo. Esc returns to the fleet.
func RunFleetDemo(ctx context.Context, cfg config.Config, info build.Info) error {
	silenceLibraryLogging()

	theme, err := tui.LookupTheme(cfg.Theme)
	if err != nil {
		return err
	}
	tui.ApplyTheme(theme)

	demo := fleet.NewDemo()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go demo.Run(runCtx)

	fleetModel := ui.NewFleet(demo, "demo", info)

	// The cluster builder creates a fresh demo store + registry each time,
	// so every drill-down starts from the current fixture state.
	builder := func(_, _ string) (*ui.Model, func(), error) {
		m, err := buildDemoClusterModel(theme, info)
		return m, nil, err
	}
	fleetModel.SetBuilder(builder)

	router := ui.NewSextant(fleetModel, builder, info)
	program := tea.NewProgram(router,
		tea.WithAltScreen(),
		tea.WithContext(runCtx),
	)
	_, err = program.Run()
	cancel()
	return err
}

// buildDemoClusterModel assembles a single-cluster dashboard from the
// demo fixture, the same way PrepareDemo + BuildModel do.
func buildDemoClusterModel(theme tui.Theme, info build.Info) (*ui.Model, error) {
	resolved := demo.Resolved(theme)
	s := &Setup{
		Resolved: resolved,
		Build:    info,
		Store:    demo.Store(),
		Registry: plugin.NewRegistry(),
	}
	for _, p := range demo.Plugins() {
		if err := s.Registry.Register(p); err != nil {
			return nil, err
		}
	}
	s.Registry.Detect(context.Background())
	return s.BuildModel()
}

// RenderFleetDemo writes a single frame of the fleet screen at the given
// size and returns, for screenshots and tests.
func RenderFleetDemo(ctx context.Context, cfg config.Config, info build.Info, out io.Writer, width, height int) error {
	if width < 20 || height < 10 {
		return fmt.Errorf("render size %dx%d is too small to lay out a fleet screen", width, height)
	}

	theme, err := tui.LookupTheme(cfg.Theme)
	if err != nil {
		return err
	}
	tui.ApplyTheme(theme)

	demo := fleet.NewDemo()

	model := ui.NewFleet(demo, "demo", info)
	model.Update(tea.WindowSizeMsg{Width: width, Height: height})
	model.Update(ui.FleetUpdateMsg{
		Clusters: demo.View(),
		Storage:  demo.Storage(),
	})
	_, err = fmt.Fprintln(out, model.View())
	return err
}
