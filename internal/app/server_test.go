package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/runlevel-six/binnacle/internal/testansi"
	"github.com/runlevel-six/binnacle/pkg/profile"
)

// stubServer answers Detect and lets the SSE stream close immediately. The
// model under test is assembled before any data arrives, which is the point:
// what a pane knows about the site comes from the profile, not from the wire.
func stubServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// headerOf renders one frame and returns its first line with the styling
// stripped, which is where the resolved profile is named.
func headerOf(t *testing.T, m interface {
	Update(tea.Msg) (tea.Model, tea.Cmd)
	View() string
},
) string {
	t.Helper()
	m.Update(tea.WindowSizeMsg{Width: 200, Height: 50})
	lines := strings.Split(testansi.StripANSI(m.View()), "\n")
	if len(lines) == 0 {
		t.Fatal("empty frame")
	}
	return lines[0]
}

// The regression this exists for: fleet mode built every per-cluster dashboard
// with profile.Default(), so an operator's --profile was accepted, reported
// nowhere, and silently discarded. The pods pane lost its critical workloads
// and every deliberately cordoned node was reported as a standing drain.
func TestBuildServerClusterModel_UsesTheConfiguredProfile(t *testing.T) {
	prof := profile.Profile{
		Name: "test-site",
		NodeRoles: profile.NodeRoles{
			LabelKeys: []string{"example.test/role"},
		},
		CriticalWorkloads: []profile.CriticalWorkload{
			{Kind: "StatefulSet", Namespace: "db", Name: "primary"},
		},
	}

	model, cleanup, err := BuildServerClusterModel(ServerClusterConfig{
		ServerURL: stubServer(t),
		Profile:   prof,
	}, "ns", "name")
	if err != nil {
		t.Fatalf("BuildServerClusterModel: %v", err)
	}
	if model == nil {
		t.Fatal("no model built; the stub server should have satisfied Detect")
	}
	defer cleanup()

	if header := headerOf(t, model); !strings.Contains(header, "test-site") {
		t.Errorf("header does not name the configured profile:\n%s", header)
	}
}

// A caller that sets no profile must get the assumption-free default rather
// than the zero value, which carries no node-role label keys at all and would
// report every node as unlabeled.
func TestBuildServerClusterModel_FallsBackToTheDefaultProfile(t *testing.T) {
	model, cleanup, err := BuildServerClusterModel(ServerClusterConfig{
		ServerURL: stubServer(t),
	}, "ns", "name")
	if err != nil {
		t.Fatalf("BuildServerClusterModel: %v", err)
	}
	if model == nil {
		t.Fatal("no model built; the stub server should have satisfied Detect")
	}
	defer cleanup()

	if header := headerOf(t, model); !strings.Contains(header, profile.Default().Name) {
		t.Errorf("header does not name the default profile:\n%s", header)
	}
}
