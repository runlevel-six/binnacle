package app

import (
	"bytes"
	"os"
	"testing"

	"k8s.io/klog/v2"
)

// client-go must not write to the terminal the dashboard has taken over. A failing
// watch makes its reflector log every few seconds, and stderr is the alternate
// screen — the message lands across the panes and garbles them. The failure still
// reaches the pane it belongs to, through the store.
func TestSilenceLibraryLogging(t *testing.T) {
	var buf bytes.Buffer
	klog.SetOutput(&buf)
	t.Cleanup(func() {
		klog.SetOutput(os.Stderr)
		klog.LogToStderr(true)
	})

	silenceLibraryLogging()
	klog.Error("a reflector complaining about a watch")
	klog.Flush()

	if buf.Len() != 0 {
		t.Errorf("library logging reached the terminal: %q", buf.String())
	}
}
