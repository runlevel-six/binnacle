package app

import (
	"context"
	"fmt"
	"io"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/runlevel-six/binnacle/internal/build"
	"github.com/runlevel-six/binnacle/internal/config"
	"github.com/runlevel-six/binnacle/internal/demo"
	"github.com/runlevel-six/binnacle/pkg/plugin"
	"github.com/runlevel-six/binnacle/pkg/tui"
)

// PrepareDemo builds a Setup backed by the demo fixture.
//
// It is a sibling of [Prepare] rather than a flag through it: Prepare loads a
// kubeconfig and resolves contexts against it, and the whole point of the demo is
// that neither exists. Only the theme is taken from the real configuration, since
// that is what a screenshot run varies.
//
// There is no Kubeconfig on the returned Setup, and nothing in the demo path may
// come to expect one.
func PrepareDemo(cfg config.Config, info build.Info) (*Setup, error) {
	theme, err := tui.LookupTheme(cfg.Theme)
	if err != nil {
		return nil, err
	}

	s := &Setup{
		Resolved: demo.Resolved(theme),
		Build:    info,
		Store:    demo.Store(),
		Registry: plugin.NewRegistry(),
	}
	for _, p := range demo.Plugins() {
		if err := s.Registry.Register(p); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// RunDemo starts the dashboard against the fixture.
//
// No watchers and no sources: the store is already full, and the demo plugins
// report present without polling. Everything after that is [Setup.runUI], the
// same assembly the live dashboard uses.
func RunDemo(ctx context.Context, s *Setup) error {
	silenceLibraryLogging()
	tui.ApplyTheme(s.Resolved.Theme)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Detection still runs, because Registry.Panes filters on the active set.
	s.Registry.Detect(runCtx)
	return s.runUI(runCtx)
}

// RenderDemo writes a single frame at the given size and returns.
//
// This is what makes a screenshot reproducible: no TTY, no alternate screen, no
// event loop, so the same fixture and size give the same bytes every run and the
// output can be piped straight into an ANSI-to-image converter. It is also the
// only way a test can assert anything about a full plugin-bearing dashboard.
func RenderDemo(ctx context.Context, s *Setup, out io.Writer, width, height int) error {
	if width < 20 || height < 10 {
		return fmt.Errorf("render size %dx%d is too small to lay out a dashboard", width, height)
	}
	tui.ApplyTheme(s.Resolved.Theme)
	s.Registry.Detect(ctx)

	model, err := s.BuildModel()
	if err != nil {
		return err
	}
	model.Update(tea.WindowSizeMsg{Width: width, Height: height})
	_, err = fmt.Fprintln(out, model.View())
	return err
}
