package web

import (
	"context"
	"testing"
	"time"
)

// contextWithFrame gives the SSE handler just long enough to write its first
// frame and then cancels, so a test does not have to wait on a stream that is
// designed never to end.
func contextWithFrame(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 250*time.Millisecond)
}
