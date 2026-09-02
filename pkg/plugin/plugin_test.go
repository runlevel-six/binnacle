package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/runlevel-six/binnacle/pkg/store"
	"github.com/runlevel-six/binnacle/pkg/tui"
)

// --- test doubles ---------------------------------------------------------

type namedPlugin struct{ name string }

func (p namedPlugin) Name() string { return p.name }

type stubPane struct{ id string }

func (s stubPane) ID() string                   { return s.id }
func (s stubPane) Title() string                { return s.id }
func (s stubPane) Priority() tui.Priority       { return tui.P0Critical }
func (s stubPane) MinWidth() int                { return 10 }
func (s stubPane) MinHeight() int               { return 3 }
func (s stubPane) HeightWeight() int            { return 1 }
func (s stubPane) Render(int, int, bool) string { return "" }

// stubSource is a Source with scriptable detection, which also records whether
// Run was invoked.
type stubSource struct {
	namedPlugin
	detected  bool
	detectErr error
	ran       bool
}

func (s *stubSource) Detect(context.Context) (bool, error) { return s.detected, s.detectErr }

func (s *stubSource) Run(context.Context, *store.Store) error {
	s.ran = true
	return nil
}

// stubPaneSource is both a Source and a PaneProvider, the shape a real plugin
// takes.
type stubPaneSource struct {
	stubSource
	paneIDs []string
}

func (s *stubPaneSource) Panes(*store.Store) []tui.Pane {
	out := make([]tui.Pane, 0, len(s.paneIDs))
	for _, id := range s.paneIDs {
		out = append(out, stubPane{id: id})
	}
	return out
}

// stubPaneOnly provides panes with nothing to detect.
type stubPaneOnly struct {
	namedPlugin
	paneIDs []string
}

func (s stubPaneOnly) Panes(*store.Store) []tui.Pane {
	out := make([]tui.Pane, 0, len(s.paneIDs))
	for _, id := range s.paneIDs {
		out = append(out, stubPane{id: id})
	}
	return out
}

type stubBanner struct {
	namedPlugin
	cells []tui.BannerCell
}

func (s stubBanner) Cells(*store.Store) []tui.BannerCell { return s.cells }

// stubBannerSource is both a Source and a BannerProvider, so its cells are
// gated behind detection.
type stubBannerSource struct {
	stubSource
	cells []tui.BannerCell
}

func (s *stubBannerSource) Cells(*store.Store) []tui.BannerCell { return s.cells }

// --- registration ---------------------------------------------------------

func TestRegister_RejectsDuplicateName(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(namedPlugin{"ceph"}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	err := r.Register(namedPlugin{"ceph"})
	if err == nil {
		t.Fatal("expected an error registering a duplicate name")
	}
	if !strings.Contains(err.Error(), "ceph") {
		t.Errorf("error should name the plugin, got %q", err)
	}
}

func TestRegister_RejectsEmptyName(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(namedPlugin{""}); err == nil {
		t.Fatal("expected an error registering an empty name")
	}
}

func TestMustRegister_PanicsOnDuplicate(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(namedPlugin{"a"})
	defer func() {
		if recover() == nil {
			t.Error("expected a panic on duplicate MustRegister")
		}
	}()
	r.MustRegister(namedPlugin{"a"})
}

func TestPlugins_PreservesRegistrationOrder(t *testing.T) {
	r := NewRegistry()
	for _, n := range []string{"c", "a", "b"} {
		r.MustRegister(namedPlugin{n})
	}
	var got []string
	for _, p := range r.Plugins() {
		got = append(got, p.Name())
	}
	if strings.Join(got, ",") != "c,a,b" {
		t.Errorf("order: got %v want [c a b]", got)
	}
}

// --- detection ------------------------------------------------------------

func TestDetect_ActivatesOnlyDetectedSources(t *testing.T) {
	r := NewRegistry()
	present := &stubSource{namedPlugin: namedPlugin{"present"}, detected: true}
	absent := &stubSource{namedPlugin: namedPlugin{"absent"}, detected: false}
	r.MustRegister(present)
	r.MustRegister(absent)

	results := r.Detect(context.Background())
	if len(results) != 2 {
		t.Fatalf("results: got %d want 2", len(results))
	}
	byName := map[string]DetectResult{}
	for _, res := range results {
		byName[res.Name] = res
	}
	if !byName["present"].Active {
		t.Error("present source should be active")
	}
	if byName["absent"].Active {
		t.Error("absent source should be inactive")
	}

	srcs := r.ActiveSources()
	if len(srcs) != 1 || srcs[0].Name() != "present" {
		t.Errorf("ActiveSources: got %v want [present]", srcs)
	}
}

// A plugin with nothing to probe is always active — that is how core panes
// participate in the same registry as optional ones.
func TestDetect_NonSourcePluginIsAlwaysActive(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(stubPaneOnly{namedPlugin: namedPlugin{"core"}, paneIDs: []string{"nodes"}})

	results := r.Detect(context.Background())
	if len(results) != 1 || !results[0].Active {
		t.Fatalf("non-Source plugin should be active, got %+v", results)
	}
	panes, err := r.Panes(store.New())
	if err != nil {
		t.Fatalf("Panes: %v", err)
	}
	if len(panes) != 1 || panes[0].ID() != "nodes" {
		t.Errorf("panes: got %v want [nodes]", panes)
	}
}

// A Detect error means "absent", but the error is retained for diagnostics
// rather than discarded.
func TestDetect_HonoursThePluginsOwnAnswer(t *testing.T) {
	boom := errors.New("keystone is not answering")

	// Absent: the plugin says no, and the error explains why.
	r := NewRegistry()
	r.MustRegister(&stubSource{namedPlugin: namedPlugin{"absent"}, detected: false, detectErr: boom})
	results := r.Detect(context.Background())
	if len(results) != 1 {
		t.Fatalf("results: got %d want 1", len(results))
	}
	if results[0].Active {
		t.Error("a source that answered false must not be active")
	}
	if !errors.Is(results[0].Err, boom) {
		t.Errorf("Err: got %v want %v", results[0].Err, boom)
	}

	// Present but unreachable: the plugin says yes *and* reports an error, and
	// must stay active. Forcing it inactive deleted the pane for the whole
	// session over a subsystem that was merely mid-restart — during a rollout,
	// which is when its pane is most wanted.
	r = NewRegistry()
	r.MustRegister(&stubSource{namedPlugin: namedPlugin{"unreachable"}, detected: true, detectErr: boom})
	results = r.Detect(context.Background())
	if !results[0].Active {
		t.Error("a source that answered true must stay active even when it also " +
			"reported an error: absence and unreachability are different answers")
	}
	if !errors.Is(results[0].Err, boom) {
		t.Errorf("Err: got %v want %v", results[0].Err, boom)
	}
}

func TestResults_BeforeDetectReportsInactive(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(stubPaneOnly{namedPlugin: namedPlugin{"core"}})

	for _, res := range r.Results() {
		if res.Active {
			t.Errorf("%s active before Detect ran", res.Name)
		}
	}
	if panes, _ := r.Panes(store.New()); len(panes) != 0 {
		t.Errorf("no panes should be contributed before Detect: got %v", panes)
	}
}

func TestDetect_IsRepeatable(t *testing.T) {
	r := NewRegistry()
	src := &stubSource{namedPlugin: namedPlugin{"flappy"}, detected: false}
	r.MustRegister(src)

	if r.Detect(context.Background())[0].Active {
		t.Fatal("expected inactive on first detect")
	}
	src.detected = true
	if !r.Detect(context.Background())[0].Active {
		t.Error("expected active after re-detect")
	}
}

// --- contribution ---------------------------------------------------------

func TestPanes_ExcludesUndetectedPlugins(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(&stubPaneSource{
		stubSource: stubSource{namedPlugin: namedPlugin{"ceph"}, detected: true},
		paneIDs:    []string{"ceph"},
	})
	r.MustRegister(&stubPaneSource{
		stubSource: stubSource{namedPlugin: namedPlugin{"openstack"}, detected: false},
		paneIDs:    []string{"openstack-overview"},
	})
	r.Detect(context.Background())

	panes, err := r.Panes(store.New())
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
	r := NewRegistry()
	r.MustRegister(stubPaneOnly{namedPlugin: namedPlugin{"first"}, paneIDs: []string{"nodes"}})
	r.MustRegister(stubPaneOnly{namedPlugin: namedPlugin{"second"}, paneIDs: []string{"nodes"}})
	r.Detect(context.Background())

	panes, err := r.Panes(store.New())
	if err == nil {
		t.Fatal("expected an error for a duplicate pane ID")
	}
	for _, want := range []string{"nodes", "first", "second"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
	// The slice stays complete so a caller may choose to continue.
	if len(panes) != 2 {
		t.Errorf("panes: got %d want 2 even on conflict", len(panes))
	}
}

func TestBannerCells_ExcludesUndetectedPlugins(t *testing.T) {
	r := NewRegistry()
	// A pane-less, source-less banner contributor: always active.
	r.MustRegister(stubBanner{
		namedPlugin: namedPlugin{"core"},
		cells:       []tui.BannerCell{{Name: "Nodes", Status: tui.BannerOK}},
	})
	// A banner contributor gated behind detection, which fails here.
	r.MustRegister(&stubBannerSource{
		stubSource: stubSource{namedPlugin: namedPlugin{"ceph"}, detected: false},
		cells:      []tui.BannerCell{{Name: "Ceph", Status: tui.BannerErr}},
	})
	r.Detect(context.Background())

	cells := r.BannerCells(store.New())
	if len(cells) != 1 || cells[0].Name != "Nodes" {
		t.Errorf("cells: got %+v want just Nodes", cells)
	}
}

func TestBannerCells_IncludesDetectedSources(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(&stubBannerSource{
		stubSource: stubSource{namedPlugin: namedPlugin{"ceph"}, detected: true},
		cells:      []tui.BannerCell{{Name: "Ceph", Status: tui.BannerOK}},
	})
	r.Detect(context.Background())

	cells := r.BannerCells(store.New())
	if len(cells) != 1 || cells[0].Name != "Ceph" {
		t.Errorf("cells: got %+v want just Ceph", cells)
	}
}

func TestActiveSources_OmitsPaneOnlyPlugins(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(stubPaneOnly{namedPlugin: namedPlugin{"core"}, paneIDs: []string{"nodes"}})
	r.MustRegister(&stubSource{namedPlugin: namedPlugin{"ceph"}, detected: true})
	r.Detect(context.Background())

	srcs := r.ActiveSources()
	if len(srcs) != 1 || srcs[0].Name() != "ceph" {
		t.Errorf("ActiveSources: got %v want [ceph]", srcs)
	}
}
