package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/runlevel-six/binnacle/pkg/health"
	"github.com/runlevel-six/binnacle/pkg/store"
)

// --- test doubles ---------------------------------------------------------

type namedPlugin struct{ name string }

func (p namedPlugin) Name() string { return p.name }

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

type stubBanner struct {
	namedPlugin
	cells []health.Cell
}

func (s stubBanner) Cells(*store.Store) []health.Cell { return s.cells }

// stubBannerSource is both a Source and a BannerProvider, so its cells are
// gated behind detection.
type stubBannerSource struct {
	stubSource
	cells []health.Cell
}

func (s *stubBannerSource) Cells(*store.Store) []health.Cell { return s.cells }

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
	r.MustRegister(namedPlugin{"core"})

	for _, res := range r.Results() {
		if res.Active {
			t.Errorf("%s active before Detect ran", res.Name)
		}
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

func TestBannerCells_ExcludesUndetectedPlugins(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(stubBanner{
		namedPlugin: namedPlugin{"core"},
		cells:       []health.Cell{{Name: "Nodes", Status: health.StatusOK}},
	})
	r.MustRegister(&stubBannerSource{
		stubSource: stubSource{namedPlugin: namedPlugin{"ceph"}, detected: false},
		cells:      []health.Cell{{Name: "Ceph", Status: health.StatusErr}},
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
		cells:      []health.Cell{{Name: "Ceph", Status: health.StatusOK}},
	})
	r.Detect(context.Background())

	cells := r.BannerCells(store.New())
	if len(cells) != 1 || cells[0].Name != "Ceph" {
		t.Errorf("cells: got %+v want just Ceph", cells)
	}
}

func TestActiveSources_OmitsNonSourcePlugins(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(stubBanner{namedPlugin: namedPlugin{"core"}, cells: nil})
	r.MustRegister(&stubSource{namedPlugin: namedPlugin{"ceph"}, detected: true})
	r.Detect(context.Background())

	srcs := r.ActiveSources()
	if len(srcs) != 1 || srcs[0].Name() != "ceph" {
		t.Errorf("ActiveSources: got %v want [ceph]", srcs)
	}
}

func TestActivePlugins_ReturnsOnlyActive(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(&stubSource{namedPlugin: namedPlugin{"present"}, detected: true})
	r.MustRegister(&stubSource{namedPlugin: namedPlugin{"absent"}, detected: false})
	r.Detect(context.Background())

	active := r.ActivePlugins()
	if len(active) != 1 || active[0].Name() != "present" {
		var names []string
		for _, p := range active {
			names = append(names, p.Name())
		}
		t.Errorf("ActivePlugins: got %v want [present]", names)
	}
}
