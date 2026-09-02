package app

import (
	"context"
	"fmt"
	"io"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/runlevel-six/binnacle/internal/build"
	"github.com/runlevel-six/binnacle/internal/config"
	"github.com/runlevel-six/binnacle/internal/fleet"
	"github.com/runlevel-six/binnacle/internal/ui"
	"github.com/runlevel-six/binnacle/pkg/tui"
)

// RunFleetDemo runs the fleet screen against the fleet demo fixture.
//
// It is the fleet-screen counterpart to RunDemo: no server, no kubeconfig,
// just the fixture data from fleet.NewDemo. The fleet list updates as the
// demo's rolling cluster advances, so the live-update path is exercised
// without a deployment.
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

	model := ui.NewFleet(demo, "demo", info)
	program := tea.NewProgram(model,
		tea.WithAltScreen(),
		tea.WithContext(runCtx),
	)
	_, err = program.Run()
	cancel()
	return err
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
