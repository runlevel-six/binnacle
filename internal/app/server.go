package app

import (
	"context"
	"fmt"

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
// theme is applied before the UI starts, so the first frame is in the right
// palette. The remote source's SSE subscriber runs in the background and
// drives redraws through Changed.
func RunServer(ctx context.Context, serverURL, token string, info build.Info, theme tui.Theme) error {
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

	model := ui.NewFleet(src, serverURL, info)
	program := tea.NewProgram(model,
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
			if subErr != nil && subErr != context.Canceled {
				return fmt.Errorf("server connection: %w", subErr)
			}
		default:
		}
	}
	return err
}
