package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/runlevel-six/binnacle/pkg/plugin"
	"github.com/runlevel-six/binnacle/pkg/store"
)

// --- test doubles ---------------------------------------------------------

type namedPlugin struct{ name string }

func (p namedPlugin) Name() string { return p.name }

type stubPane struct{ id string }

func (s stubPane) ID() string                   { return s.id }
func (s stubPane) Title() string                { return s.id }
func (s stubPane) Priority() Priority           { return P0Critical }
func (s stubPane) MinWidth() int                { return 10 }
func (s stubPane) MinHeight() int               { return 3 }
func (s stubPane) HeightWeight() int            { return 1 }
func (s stubPane) Render(int, int, bool) string { return "" }

type stubSource struct {
	namedPlugin
	detected  bool
	detectErr error
}

func (s *stubSource) Detect(context.Context) (bool, error) { return s.detected, s.detectErr }
func (s *stubSource) Run(context.Context, *store.Store) error { return nil }

type stubPaneProvider struct {
	name    string
	paneIDs []string
}

func (p stubPaneProvider) Name() string { return p.name }
func (p stubPaneProvider) Panes(*store.Store) []Pane {
	out := make([]Pane, 0, len(p.paneIDs))
	for _, id := range p.paneIDs {
		out = append(out, stubPane{id: id})
	}
	return out
}

// --- tests ----------------------------------------------------------------

func TestPanes_ExcludesUndetectedPlugins(t *testing.T) {
	r := plugin.NewRegistry()
	r.MustRegister(&stubSource{namedPlugin: namedPlugin{"ceph"}, detected: true})
	r.MustRegister(&stubSource{namedPlugin: namedPlugin{"openstack"}, detected: false})
	r.Detect(context.Background())

	providers := []PaneProvider{
		stubPaneProvider{name: "ceph", paneIDs: []string{"ceph"}},
		stubPaneProvider{name: "openstack", paneIDs: []string{"openstack-overview"}},
	}
	panes, err := Panes(r, store.New(), providers)
	if err != nil {
		t.Fatalf("Panes: %v", err)
	}
	if len(panes) != 1 || panes[0].ID() != "ceph" {
		var ids []string
		for _, p := range panes {
			ids = append(ids, p.ID())
		}
		t.Errorf("panes: got %v want [ceph]", ids)
	}
}

func TestPanes_DuplicateIDIsAnError(t *testing.T) {
	r := plugin.NewRegistry()
	r.MustRegister(&stubSource{namedPlugin: namedPlugin{"first"}, detected: true})
	r.MustRegister(&stubSource{namedPlugin: namedPlugin{"second"}, detected: true})
	r.Detect(context.Background())

	providers := []PaneProvider{
		stubPaneProvider{name: "first", paneIDs: []string{"nodes"}},
		stubPaneProvider{name: "second", paneIDs: []string{"nodes"}},
	}
	panes, err := Panes(r, store.New(), providers)
	if err == nil {
		t.Fatal("expected an error for a duplicate pane ID")
	}
	for _, want := range []string{"nodes", "first", "second"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
	if len(panes) != 2 {
		t.Errorf("panes: got %d want 2 even on conflict", len(panes))
	}
}

func TestPanes_NonSourcePluginIsAlwaysActive(t *testing.T) {
	r := plugin.NewRegistry()
	r.MustRegister(namedPlugin{"core"})
	r.Detect(context.Background())

	providers := []PaneProvider{
		stubPaneProvider{name: "core", paneIDs: []string{"nodes"}},
	}
	panes, err := Panes(r, store.New(), providers)
	if err != nil {
		t.Fatalf("Panes: %v", err)
	}
	if len(panes) != 1 || panes[0].ID() != "nodes" {
		t.Errorf("panes: got %v want [nodes]", panes)
	}
}

func TestPanes_EmptyBeforeDetect(t *testing.T) {
	r := plugin.NewRegistry()
	r.MustRegister(&stubSource{namedPlugin: namedPlugin{"core"}, detected: true})

	providers := []PaneProvider{
		stubPaneProvider{name: "core", paneIDs: []string{"nodes"}},
	}
	if panes, _ := Panes(r, store.New(), providers); len(panes) != 0 {
		t.Errorf("no panes should be contributed before Detect: got %v", panes)
	}
}
