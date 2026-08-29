package ui

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/runlevel-six/sextant/internal/build"
	"github.com/runlevel-six/sextant/internal/config"
	"github.com/runlevel-six/sextant/internal/testansi"
	"github.com/runlevel-six/sextant/pkg/model"
	"github.com/runlevel-six/sextant/pkg/plugin"
	"github.com/runlevel-six/sextant/pkg/profile"
	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
	"github.com/runlevel-six/sextant/pkg/tui/grid"
)

func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	os.Exit(m.Run())
}

func resolved() config.Resolved {
	return config.Resolved{
		ManagementContext:    "kind-capi-management",
		WorkloadContext:      "kind-capi-management",
		WorkloadIsManagement: true,
		Profile:              profile.Default(),
	}
}

func populatedStore() *store.Store {
	s := store.New()
	populate(s)
	return s
}

// populate fills a store that is already being watched, so a test can see what
// the dashboard does on the frame the informers first deliver.
func populate(s *store.Store) {
	s.Put(model.KeyMgmtKCPs, model.Snapshot[model.KubeadmControlPlane]{
		Items: []model.KubeadmControlPlane{{
			Namespace: "capi", Name: "cp", Version: "v1.32.0",
			DesiredReplicas: 3, UpToDateReplicas: 1, ReadyReplicas: 3,
		}},
		UpdatedAt: time.Now(),
	})
	s.Put(model.KeyMgmtMachines, model.Snapshot[model.Machine]{
		Items: []model.Machine{
			{Namespace: "capi", Name: "cp-1", Phase: "Running"},
			{Namespace: "capi", Name: "cp-2", Phase: "Provisioning"},
		},
		UpdatedAt: time.Now(),
	})
	s.Put(model.KeyMgmtBareMetalHosts, model.Snapshot[model.BareMetalHost]{
		Items: []model.BareMetalHost{
			{Namespace: "bmh", Name: "h1", State: "provisioned"},
			{Namespace: "bmh", Name: "h2", State: "registering", ErrorMessage: "BMC unreachable"},
		},
		UpdatedAt: time.Now(),
	})
	s.Put(model.KeyWorkloadNodes, model.Snapshot[model.Node]{
		Items: []model.Node{
			{Name: "n1", Status: "Ready", Role: "control-plane"},
			{Name: "n2", Status: "NotReady", Role: "compute"},
		},
		UpdatedAt: time.Now(),
	})
	s.Put(model.KeyWorkloadPods, model.Snapshot[model.Pod]{
		Items: []model.Pod{
			{Namespace: "kube-system", Name: "ok", IsHealthy: true, ReadyReady: 1, ReadyTotal: 1, Status: "Running"},
			{Namespace: "kube-system", Name: "bad", ReadyTotal: 1, Status: "CrashLoopBackOff"},
		},
		UpdatedAt: time.Now(),
	})
}

// withTheme applies th for the duration of the test, restoring the default
// afterwards. The palette is package state, so a test that switched it and left
// it switched would silently retheme every test that follows.
func withTheme(t *testing.T, th tui.Theme) {
	t.Helper()
	tui.ApplyTheme(th)
	t.Cleanup(func() { tui.ApplyTheme(tui.DefaultTheme()) })
}

// testVersion is the build the test models claim to be.
//
// A prerelease on purpose: it is the longest version the header will realistically
// carry, so the width assertions size the row against the worst case, and it holds
// letters, so a theme that shouts its chrome can be caught rewriting an
// identifier.
const testVersion = "1.4.0-rc.2"

func newModel(t *testing.T, s *store.Store, w, h int) *Model {
	t.Helper()
	m := New(resolved(), s, plugin.NewRegistry(), CorePanes(s, resolved(), nil)).
		WithBuild(build.Info{Version: testVersion, Commit: "abc1234", Date: "2026-08-25T00:00:00Z"})
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m
}

// The view must be exactly the terminal's size. Bubble Tea only repaints cells it
// is given, so an under-sized frame leaves the previous one on screen and an
// over-sized one scrolls the display.
func TestView_ExactDimensions(t *testing.T) {
	s := populatedStore()
	for _, th := range tui.Themes() {
		withTheme(t, th)
		for _, size := range []struct{ w, h int }{
			{80, 24}, {120, 40}, {200, 50}, {300, 60}, {100, 30},
		} {
			m := newModel(t, s, size.w, size.h)
			view := m.View()

			if got := lipgloss.Height(view); got != size.h {
				t.Errorf("%s %dx%d: height %d", th.Name, size.w, size.h, got)
			}
			for i, line := range strings.Split(view, "\n") {
				if got := lipgloss.Width(line); got > size.w {
					t.Errorf("%s %dx%d: line %d width %d exceeds terminal",
						th.Name, size.w, size.h, i, got)
				}
			}
		}
	}
}

func TestView_EmptyBeforeSizeKnown(t *testing.T) {
	s := store.New()
	m := New(resolved(), s, plugin.NewRegistry(), CorePanes(s, resolved(), nil))
	if got := m.View(); got != "" {
		t.Errorf("expected an empty view before the size is known, got %q", got)
	}
}

// The header must name the cluster being watched: acting on the wrong one during
// a maintenance window is the mistake this tool exists to prevent.
func TestHeader_NamesCluster(t *testing.T) {
	m := newModel(t, populatedStore(), 160, 40)
	view := testansi.StripANSI(m.View())
	if !strings.Contains(view, "kind-capi-management") {
		t.Errorf("header should name the context:\n%s", firstLines(view, 3))
	}
	if !strings.Contains(view, "sextant") {
		t.Error("header should identify the tool")
	}
}

func TestHeader_SeparateClustersBothNamed(t *testing.T) {
	r := resolved()
	r.WorkloadContext = "prod-workload"
	r.WorkloadIsManagement = false

	s := populatedStore()
	m := New(r, s, plugin.NewRegistry(), CorePanes(s, r, nil))
	m.Update(tea.WindowSizeMsg{Width: 200, Height: 40})

	view := testansi.StripANSI(m.View())
	if !strings.Contains(view, "kind-capi-management") || !strings.Contains(view, "prod-workload") {
		t.Errorf("both contexts should appear:\n%s", firstLines(view, 3))
	}
}

func TestHeader_ShowsRolloutState(t *testing.T) {
	m := newModel(t, populatedStore(), 200, 40)
	if !strings.Contains(testansi.StripANSI(m.View()), "ROLLOUT") {
		t.Errorf("a rolling cluster should be flagged in the header:\n%s", firstLines(testansi.StripANSI(m.View()), 3))
	}
}

func TestHeader_ShowsFrozenState(t *testing.T) {
	m := newModel(t, populatedStore(), 200, 40)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if !strings.Contains(testansi.StripANSI(m.View()), "FROZEN") {
		t.Error("freezing should be visible in the header")
	}
}

// Health cells summarize each subsystem; the leftmost survive at any width.
func TestBanner_Cells(t *testing.T) {
	m := newModel(t, populatedStore(), 200, 40)
	view := testansi.StripANSI(m.View())
	for _, want := range []string{"CAPI", "Nodes", "Pods", "Hosts"} {
		if !strings.Contains(view, want) {
			t.Errorf("banner missing %q:\n%s", want, firstLines(view, 3))
		}
	}
	// A NotReady node and an errored host are the states that must stand out.
	if !strings.Contains(view, "1 NotReady") {
		t.Error("banner should report the NotReady node")
	}
	if !strings.Contains(view, "1 errored") {
		t.Error("banner should report the errored host")
	}
}

func TestBanner_DropsCellsWhenNarrow(t *testing.T) {
	m := newModel(t, populatedStore(), 40, 24)
	view := m.View()
	for i, line := range strings.Split(view, "\n") {
		if got := lipgloss.Width(line); got > 40 {
			t.Fatalf("line %d width %d exceeds 40:\n%s", i, got, view)
		}
	}
	// The most important cell survives the squeeze.
	if !strings.Contains(testansi.StripANSI(view), "CAPI") {
		t.Error("the leftmost banner cell should survive truncation")
	}
}

// --- keys -----------------------------------------------------------------

func TestKeys_Quit(t *testing.T) {
	m := newModel(t, store.New(), 120, 40)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("q should return a command")
	}
	if msg := cmd(); msg == nil {
		t.Error("q should produce a quit message")
	}
}

func TestKeys_FocusCycles(t *testing.T) {
	m := newModel(t, populatedStore(), 200, 50)
	first := m.focused

	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.focused == first {
		t.Error("tab should move focus")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if m.focused != first {
		t.Errorf("shift+tab should return focus: got %q want %q", m.focused, first)
	}
}

func TestKeys_FocusWrapsAround(t *testing.T) {
	m := newModel(t, populatedStore(), 200, 50)
	for range len(m.panes) {
		m.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
	if m.focused != m.panes[0].ID() {
		t.Errorf("focus should wrap to the first pane, got %q", m.focused)
	}
}

func TestKeys_JumpDigits(t *testing.T) {
	m := newModel(t, populatedStore(), 200, 50)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}})
	if m.focused != m.panes[2].ID() {
		t.Errorf("3 should focus the third pane: got %q want %q", m.focused, m.panes[2].ID())
	}
	// A digit past the pane count must be ignored, not panic.
	before := m.focused
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'9'}})
	if len(m.panes) < 9 && m.focused != before {
		t.Error("an out-of-range digit should be ignored")
	}
}

func TestKeys_ColumnOverride(t *testing.T) {
	m := newModel(t, populatedStore(), 300, 60)
	auto := m.effectiveCols()

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{']'}})
	if m.effectiveCols() <= auto {
		t.Errorf("] should widen: got %d from %d", m.effectiveCols(), auto)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'['}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'['}})
	if m.effectiveCols() >= auto {
		t.Errorf("[ should narrow: got %d from %d", m.effectiveCols(), auto)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'\\'}})
	if m.colsOverride != 0 {
		t.Error("backslash should restore automatic columns")
	}
}

func TestKeys_ColumnOverrideClamped(t *testing.T) {
	m := newModel(t, populatedStore(), 300, 60)
	for range 20 {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{']'}})
	}
	if m.colsOverride > grid.MaxColumns {
		t.Errorf("override %d exceeds MaxColumns", m.colsOverride)
	}
	for range 20 {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'['}})
	}
	if m.effectiveCols() < 1 {
		t.Errorf("columns should never drop below 1, got %d", m.effectiveCols())
	}
}

func TestKeys_ZoomLiftsFocusedPane(t *testing.T) {
	m := newModel(t, populatedStore(), 200, 50)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}})
	if !m.zoomed {
		t.Fatal("z should enable zoom")
	}
	// Zoom is reported so the state is not invisible.
	if !strings.Contains(testansi.StripANSI(m.View()), "zoom") {
		t.Error("zoom should be visible in the footer")
	}
	// And the view must still fit.
	if got := lipgloss.Height(m.View()); got != 50 {
		t.Errorf("zoomed view height %d want 50", got)
	}

	// Zoom means full size: exactly one pane on screen, and the others reachable
	// by focus cycling rather than gone. A zoom that merely widened a pane left it
	// still too short to show what it was truncating, which is the only reason
	// anyone presses z.
	// Counted by frame corners rather than by title text: a pane's body can
	// legitimately contain another pane's name — Overview has a "Nodes" section —
	// and matching on that counts panes that are not there.
	view := testansi.StripANSI(m.View())
	if frames := strings.Count(view, "╭─"); frames != 1 {
		t.Errorf("%d pane frames visible while zoomed, want 1:\n%s", frames, view)
	}
	if !strings.Contains(view, "hidden (tab to reach)") {
		t.Error("the panes zoom displaced should be reported as reachable")
	}
}

func TestKeys_HelpOverlay(t *testing.T) {
	m := newModel(t, populatedStore(), 200, 50)
	before := testansi.StripANSI(m.View())
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	after := testansi.StripANSI(m.View())

	if before == after {
		t.Error("? should change the footer")
	}
	if !strings.Contains(after, "cycle focus") {
		t.Errorf("help should list bindings:\n%s", after)
	}
}

// --- themes ---------------------------------------------------------------

func TestKeys_ThemeCycles(t *testing.T) {
	t.Cleanup(func() { tui.ApplyTheme(tui.DefaultTheme()) })

	m := newModel(t, populatedStore(), 200, 50)
	before := tui.CurrentTheme().Name
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'T'}})
	if after := tui.CurrentTheme().Name; after == before {
		t.Fatalf("T should switch theme, still %q", after)
	}
	// A non-default theme names itself in the footer, so nobody is left
	// wondering what they just pressed.
	if !strings.Contains(testansi.StripANSI(m.View()), tui.CurrentTheme().Name) {
		t.Error("the active theme should be named in the footer")
	}

	// Cycling the whole catalog returns to where it started.
	for range len(tui.Themes()) - 1 {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'T'}})
	}
	if got := tui.CurrentTheme().Name; got != before {
		t.Errorf("cycling should wrap to %q, got %q", before, got)
	}
	// The default is not named, which keeps the ordinary footer quiet.
	if strings.Contains(testansi.StripANSI(m.View()), "· default") {
		t.Error("the default theme should not be announced")
	}
}

// LCARS shouts its chrome but must leave data alone: a kubeconfig context is an
// identifier, and a header that uppercases one is reporting something false.
func TestTheme_LCARSUppercasesChromeNotData(t *testing.T) {
	withTheme(t, tui.LCARSTheme())
	m := newModel(t, populatedStore(), 200, 50)
	view := testansi.StripANSI(m.View())

	if !strings.Contains(view, "SEXTANT") {
		t.Errorf("the tool name should be shouted:\n%s", firstLines(view, 3))
	}
	if !strings.Contains(view, "kind-capi-management") {
		t.Errorf("the context name must survive verbatim:\n%s", firstLines(view, 3))
	}
	if strings.Contains(view, "KIND-CAPI-MANAGEMENT") {
		t.Errorf("the context name must not be uppercased:\n%s", firstLines(view, 3))
	}
	// A version is an identifier too. "V1.4.0-RC.2" names a release that does not
	// exist.
	if !strings.Contains(view, "v"+testVersion) {
		t.Errorf("the version must survive verbatim:\n%s", firstLines(view, 3))
	}
}

// Pane titles carry a catalog number under LCARS. It has to be stable, or a
// cosmetic label would read as changing data.
func TestTheme_PaneTitleTags(t *testing.T) {
	withTheme(t, tui.LCARSTheme())
	m := newModel(t, populatedStore(), 200, 50)
	first := testansi.StripANSI(m.View())
	if !strings.Contains(first, "OVERVIEW ") {
		t.Errorf("pane titles should be shouted:\n%s", first)
	}
	if second := testansi.StripANSI(m.View()); first != second {
		t.Error("pane titles must not change between renders")
	}
}

func TestKeys_UnknownIsIgnored(t *testing.T) {
	m := newModel(t, populatedStore(), 200, 50)
	before := m.View()
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'Q'}})
	if m.View() != before {
		t.Error("an unbound key should change nothing")
	}
}

// Freezing stops the display changing but must keep consuming notifications, or
// the subscription would stall and the first unfreeze would show stale data.
func TestFreeze_KeepsConsumingUpdates(t *testing.T) {
	m := newModel(t, populatedStore(), 200, 50)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})

	_, cmd := m.Update(dataUpdateMsg{})
	if cmd == nil {
		t.Error("a data update should still re-arm the subscription while frozen")
	}
}

// --- header ---------------------------------------------------------------

// The version belongs to the tool's name and has to read as part of it. "Which
// version were you running?" is the first question of any bug report, and the
// header is what gets pasted into one.
func TestHeader_VersionSitsBesideTheName(t *testing.T) {
	m := newModel(t, populatedStore(), 200, 50)
	row := firstLines(testansi.StripANSI(m.View()), 1)

	want := "sextant v" + testVersion
	if !strings.Contains(row, want) {
		t.Errorf("the identity row should read %q: %q", want, row)
	}
	// Before the cluster identity, not after it: the left end of this row is the
	// end that survives a narrow terminal, and the right end is where the rollout
	// and FROZEN badges go.
	if v, ctx := strings.Index(row, want), strings.Index(row, "kind-capi-management"); v > ctx {
		t.Errorf("the version should precede the cluster identity: %q", row)
	}
}

// A model with no build metadata is still a working dashboard — that is what
// every test that does not care about the version gets — and it must not caption
// the tool's name with a stray separator or an empty span.
func TestHeader_NoBuildNoVersion(t *testing.T) {
	s := populatedStore()
	m := New(resolved(), s, plugin.NewRegistry(), CorePanes(s, resolved(), nil))
	m.Update(tea.WindowSizeMsg{Width: 200, Height: 50})

	row := firstLines(testansi.StripANSI(m.View()), 1)
	if got, want := row, "sextant"+tui.CurrentTheme().Separator; !strings.Contains(got, want) {
		t.Errorf("an unstamped build should leave the name alone, want %q: %q", want, got)
	}
}

// The version has to survive the narrowest terminal the dashboard supports, since
// it is the one piece of the row that cannot be recovered by looking at the
// cluster.
func TestHeader_VersionSurvivesNarrowWidths(t *testing.T) {
	for _, w := range []int{80, 100, 120} {
		m := newModel(t, populatedStore(), w, 30)
		row := firstLines(testansi.StripANSI(m.View()), 1)
		if !strings.Contains(row, "v"+testVersion) {
			t.Errorf("width %d dropped the version: %q", w, row)
		}
	}
}

// --- footer ---------------------------------------------------------------

func TestFooter_ReportsLayout(t *testing.T) {
	m := newModel(t, populatedStore(), 300, 60)
	view := testansi.StripANSI(m.View())
	if !strings.Contains(view, "col") {
		t.Error("footer should report the column count")
	}
	if !strings.Contains(view, "panes") {
		t.Error("footer should report the pane count")
	}
	if !strings.Contains(view, "q quit") {
		t.Error("footer should show the quit hint")
	}
}

// A pane that did not fit must be reported, or it would be invisible and
// unreachable in practice.
func TestFooter_ReportsHiddenPanes(t *testing.T) {
	m := newModel(t, populatedStore(), 80, 24)
	if !strings.Contains(testansi.StripANSI(m.View()), "hidden") {
		t.Errorf("narrow terminal should report hidden panes:\n%s", testansi.StripANSI(m.View()))
	}
}

// --- panes ----------------------------------------------------------------

func TestCorePanes_UniqueIDsAndOrder(t *testing.T) {
	s := store.New()
	panes := CorePanes(s, resolved(), nil)
	if len(panes) < 5 {
		t.Fatalf("expected at least 5 core panes, got %d", len(panes))
	}
	seen := map[string]bool{}
	for _, p := range panes {
		if seen[p.ID()] {
			t.Errorf("duplicate pane ID %q", p.ID())
		}
		seen[p.ID()] = true
	}
	// Overview leads because it is the summary the eye goes to first.
	if panes[0].ID() != "overview" {
		t.Errorf("first pane: got %q want overview", panes[0].ID())
	}
}

func TestJumpDigits_OnlyFirstNine(t *testing.T) {
	m := newModel(t, store.New(), 200, 50)
	if got := m.jumpDigitFor(m.panes[0].ID()); got != "1" {
		t.Errorf("first pane digit: got %q want 1", got)
	}
	if got := m.jumpDigitFor("no-such-pane"); got != "" {
		t.Errorf("unknown pane should have no digit, got %q", got)
	}
}

func TestColumnCountForWidth(t *testing.T) {
	tests := map[int]int{80: 1, 119: 1, 120: 2, 179: 2, 180: 3, 259: 3, 260: 4, 400: 4}
	for w, want := range tests {
		if got := columnCountForWidth(w); got != want {
			t.Errorf("columnCountForWidth(%d): got %d want %d", w, got, want)
		}
	}
}

// --- frame ----------------------------------------------------------------

// Every frame style must land on exactly the rectangle the layout allotted it.
// A frame one cell out is how the grid ends up taller than the terminal.
func TestPaneFrame_ExactSize(t *testing.T) {
	for _, th := range tui.Themes() {
		withTheme(t, th)
		for _, size := range []struct{ w, h int }{{20, 5}, {40, 10}, {80, 3}, {120, 30}, {12, 4}} {
			for _, idx := range []int{0, 1, 3, 7} {
				out := paneFrame(paneBox{
					Title: "Title", Width: size.w, Height: size.h, Index: idx,
					Body: "body\ncontent",
				})
				if got := lipgloss.Height(out); got != size.h {
					t.Errorf("%s %dx%d idx%d: height %d", th.Name, size.w, size.h, idx, got)
				}
				for i, line := range strings.Split(out, "\n") {
					if got := lipgloss.Width(line); got != size.w {
						t.Errorf("%s %dx%d idx%d: line %d width %d", th.Name, size.w, size.h, idx, i, got)
					}
				}
			}
		}
	}
}

// The blank line above a body is the vertical half of the one-cell gutter, and a
// pane that pads its own body out to the frame is entitled to it like any other.
//
// Every merged frame does pad — a section that came up short must not let the next
// one slide up — so measuring the body by its line count said "this pane fills the
// tile" for a body that was mostly blank, and the whole bottom row of the dashboard
// drew its first line hard against the border while the tables above it breathed.
func TestPaneFrame_GutterAboveASelfPaddedBody(t *testing.T) {
	for _, th := range tui.Themes() {
		withTheme(t, th)
		// Two lines of content in a body padded to the seven the frame holds.
		body := "Cilium\nagents 4/5 ready" + strings.Repeat("\n", 5)
		out := testansi.StripANSI(paneFrame(paneBox{
			Title: "Network", Width: 40, Height: 9, Body: body,
		}))
		lines := strings.Split(out, "\n")
		if len(lines) < 3 {
			t.Fatalf("%s: frame is too short to have a body:\n%s", th.Name, out)
		}
		if hasText(lines[1]) {
			t.Errorf("%s: first body row should be the gutter, got %q:\n%s", th.Name, lines[1], out)
		}
		if !strings.Contains(lines[2], "Cilium") {
			t.Errorf("%s: content should start on the second body row:\n%s", th.Name, out)
		}
	}
}

// A body whose content really does reach the bottom of the frame keeps its flush
// top: the gutter is spent from slack, and there is none to spend.
func TestPaneFrame_NoGutterWhenTheBodyIsFull(t *testing.T) {
	for _, th := range tui.Themes() {
		withTheme(t, th)
		out := testansi.StripANSI(paneFrame(paneBox{
			Title: "Network", Width: 40, Height: 5, Body: "one\ntwo\nthree",
		}))
		if !strings.Contains(strings.Split(out, "\n")[1], "one") {
			t.Errorf("%s: a full body should start on the first body row:\n%s", th.Name, out)
		}
	}
}

// hasText reports whether a stripped row carries anything a reader would call
// content. Asking that rather than naming the frame's glyphs keeps this from
// having to know each theme's rails and bars.
func hasText(row string) bool {
	return strings.ContainsFunc(row, func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsDigit(r)
	})
}

func TestPaneFrame_TitleInBorder(t *testing.T) {
	for _, th := range tui.Themes() {
		withTheme(t, th)
		out := testansi.StripANSI(paneFrame(paneBox{Title: "Machines", Width: 40, Height: 6, Body: "body"}))
		if !strings.Contains(strings.Split(out, "\n")[0], "Machines") {
			t.Errorf("%s: title should be in the top border:\n%s", th.Name, out)
		}
	}
}

// A title wider than the pane must be cut, not pushed past the border.
func TestPaneFrame_LongTitleTruncated(t *testing.T) {
	for _, th := range tui.Themes() {
		withTheme(t, th)
		out := paneFrame(paneBox{
			Title: "A Very Long Pane Title Indeed", Width: 20, Height: 5, Body: "body",
		})
		for i, line := range strings.Split(out, "\n") {
			if got := lipgloss.Width(line); got != 20 {
				t.Errorf("%s: line %d width %d want 20", th.Name, i, got)
			}
		}
	}
}

func TestPaneFrame_TooSmallForBorder(t *testing.T) {
	for _, th := range tui.Themes() {
		withTheme(t, th)
		out := paneFrame(paneBox{Title: "T", Width: 3, Height: 2, Body: "body"})
		if lipgloss.Height(out) > 2 {
			t.Errorf("%s: a pane too small for a border should still be bounded:\n%q", th.Name, out)
		}
	}
}

// The footer must be separated entirely by the theme's own glyph. It used to mix
// a hardcoded middot into three places, so a themed dashboard's footer read in two
// alphabets at once.
func TestFooter_SeparatorsAreAllThemed(t *testing.T) {
	s := populatedStore()
	for _, th := range tui.Themes() {
		glyph := strings.TrimSpace(th.Separator)
		if glyph == "·" {
			continue // The default's own glyph; nothing to tell apart.
		}
		withTheme(t, th)
		for _, help := range []bool{false, true} {
			m := newModel(t, s, 200, 50)
			m.showHelp = help
			footer := testansi.StripANSI(m.footerLine(grid.Layout{Breakpoint: grid.Wide}))
			if strings.Contains(footer, "·") {
				t.Errorf("%s (help=%v): footer still carries a hardcoded middot: %q",
					th.Name, help, footer)
			}
			if !strings.Contains(footer, glyph) {
				t.Errorf("%s (help=%v): footer uses none of its own %q: %q",
					th.Name, help, glyph, footer)
			}
		}
	}
}

// --- ground ---------------------------------------------------------------

// An ungrounded theme must come out of groundLine byte for byte as it went into
// padToWidth. This is what lets a ground be added to the render path without
// touching how the three themes that predate it look.
func TestGroundLine_UngroundedIsUnchanged(t *testing.T) {
	for _, content := range []string{"", "x", "already wide enough", tui.StyleOK.Render("✓") + " ok"} {
		if got, want := groundLine(content, 24, "", ""), padToWidth(content, 24); got != want {
			t.Errorf("groundLine(%q) = %q, want padToWidth = %q", content, got, want)
		}
	}
}

// The ground has to survive a nested style's reset. This is the whole reason
// groundLine exists rather than a Background style wrapped round the line.
func TestGroundLine_ReArmsAfterEveryReset(t *testing.T) {
	inner := lipgloss.NewStyle().Foreground(lipgloss.Color("#FF0000")).Render("bad")
	line := groundLine("status "+inner+" tail", 40, "#FFFFFF", "#0000AA")

	if got := lipgloss.Width(line); got != 40 {
		t.Fatalf("width %d, want 40", got)
	}
	testansi.AssertNoHoles(t, "groundLine", line)
}

// Every visible cell of a grounded pane must be painted — the border, the
// gutters, the body, the padding under a short body, and the top edge either side
// of the title. A single unpainted run is a hole in the panel.
//
// Every grounded theme, not just the one that motivated grounds: the two frame
// styles paint their chrome by different routes, and a block frame has a rail, a
// half-block bar and a deliberate break in that bar to account for.
func TestPaneFrame_GroundedPaneHasNoHoles(t *testing.T) {
	body := "plain row\n" +
		tui.StyleOK.Render("ready") + " then bare text\n" +
		tui.StyleErr.Render("failed")

	for _, th := range tui.Themes() {
		if !th.Grounded() {
			continue
		}
		withTheme(t, th)
		for _, focused := range []bool{false, true} {
			// Index varies where a block frame breaks its bars, and the break is
			// the part most likely to be left unpainted.
			for _, idx := range []int{0, 1, 3, 7} {
				out := paneFrame(paneBox{
					Title: "Machines", Width: 44, Height: 9,
					Focused: focused, Index: idx, Body: body,
				})
				for i, line := range strings.Split(out, "\n") {
					testansi.AssertNoHoles(t, fmt.Sprintf("%s focused=%v idx%d line %d",
						th.Name, focused, idx, i), line)
				}
			}
		}
	}
}

// The same, for the whole screen: the header rows, every pane, the strip beside a
// partial row of tiles, and the footer.
func TestView_GroundedScreenHasNoHoles(t *testing.T) {
	s := populatedStore()
	for _, th := range tui.Themes() {
		if !th.Grounded() {
			continue
		}
		withTheme(t, th)
		// 200x50 is a three-column layout with five core panes, so the last row
		// is partial — the case that used to leave the strip beside it bare.
		for _, size := range []struct{ w, h int }{{80, 24}, {200, 50}, {300, 60}} {
			m := newModel(t, s, size.w, size.h)
			for i, line := range strings.Split(m.View(), "\n") {
				testansi.AssertNoHoles(t, fmt.Sprintf("%s %dx%d line %d",
					th.Name, size.w, size.h, i), line)
			}
		}
	}
}

// A theme that names one ground must paint the whole screen with it — header
// rows, panes, the strip beside a partial row of tiles, and the footer alike.
//
// The space between panes is not a different surface. A second color there
// reads as an unfilled gap rather than as a choice, which is why AppBG has to be
// asked for; this pins the default for any theme that does not ask.
func TestView_SingleGroundThemePaintsOneGround(t *testing.T) {
	s := populatedStore()
	for _, th := range tui.Themes() {
		if !th.Grounded() || th.ScreenBG() != th.PaneBG || th.HeaderBG != th.PaneBG {
			continue
		}
		withTheme(t, th)
		// 200x50 leaves a partial last row; 300x60 leaves rows below it.
		for _, size := range []struct{ w, h int }{{80, 24}, {200, 50}, {300, 60}} {
			m := newModel(t, s, size.w, size.h)
			for i, line := range strings.Split(m.View(), "\n") {
				if got := testansi.Backgrounds(line); len(got) != 1 {
					t.Errorf("%s %dx%d line %d: %d grounds %v, want the one",
						th.Name, size.w, size.h, i, len(got), got)
				}
			}
		}
	}
}

// A theme with accents must color neighboring panes differently — that is the
// whole of what an accent list is for.
func TestPaneFrame_AccentsVaryBetweenPanes(t *testing.T) {
	withTheme(t, tui.LCARSTheme())
	first := paneFrame(paneBox{Title: "A", Width: 40, Height: 6, Index: 0, Body: "x"})
	second := paneFrame(paneBox{Title: "A", Width: 40, Height: 6, Index: 1, Body: "x"})
	if first == second {
		t.Error("adjacent panes should not render identically under an accented theme")
	}
	// And the same pane must be stable, or the frame would flicker between
	// redraws.
	if again := paneFrame(paneBox{Title: "A", Width: 40, Height: 6, Index: 0, Body: "x"}); again != first {
		t.Error("a pane's frame must be stable across renders")
	}
}

func TestPadToWidth(t *testing.T) {
	if got := lipgloss.Width(padToWidth("abc", 10)); got != 10 {
		t.Errorf("short: got width %d want 10", got)
	}
	if got := lipgloss.Width(padToWidth("abcdefghij", 5)); got != 5 {
		t.Errorf("long: got width %d want 5", got)
	}
	// A styled string must be measured by display width, not byte length.
	styled := tui.StyleErr.Render("abc")
	if got := lipgloss.Width(padToWidth(styled, 10)); got != 10 {
		t.Errorf("styled: got width %d want 10", got)
	}
}

// firstLines returns the leading n lines, for readable failure messages when
// only the header matters.
func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// --- settling -------------------------------------------------------------

// paneWidths reads the tile widths out of a rendered frame by measuring the runs
// between vertical borders on the row that holds the fleet tables.
func borderColumns(frame string) []int {
	var out []int
	for _, ln := range strings.Split(frame, "\n") {
		plain := testansi.StripANSI(ln)
		if !strings.Contains(plain, "Machines & Hosts") {
			continue
		}
		// Rune index, not byte offset: the box-drawing glyphs are three bytes
		// each, so ranging over the string would report inflated columns.
		for i, r := range []rune(plain) {
			if r == '╭' || r == '╮' {
				out = append(out, i)
			}
		}
		return out
	}
	return out
}

// The layout is re-measured on every frame, so a pane whose content grew by a cell
// must not be allowed to slide every border on screen. This is the twitch the
// content-sized grid would otherwise produce on every poll.
func TestSettle_SmallChangesDoNotMoveTheLayout(t *testing.T) {
	s := populatedStore()
	m := newModel(t, s, 240, 50)
	before := borderColumns(m.View())
	if len(before) == 0 {
		t.Fatal("could not find the Machines & Hosts row in the frame")
	}

	// One cell more of machine name: a real change, far too small to be worth
	// redrawing the screen for.
	s.Put(model.KeyMgmtMachines, model.Snapshot[model.Machine]{
		Items: []model.Machine{
			{Namespace: "capi", Name: "cp-1x", Phase: "Running"},
			{Namespace: "capi", Name: "cp-2", Phase: "Provisioning"},
		},
		UpdatedAt: time.Now(),
	})
	if after := borderColumns(m.View()); !slices.Equal(before, after) {
		t.Errorf("borders moved for a one-cell content change: %v then %v", before, after)
	}
}

// A change that outgrows the tile on screen is adopted: the reader cannot read a
// fleet whose names are truncated to a shared prefix, and the width to fix that is
// in the tile next door.
func TestSettle_ContentOutgrowingItsTileIsAdopted(t *testing.T) {
	s := populatedStore()
	m := newModel(t, s, 180, 50)
	before := borderColumns(m.View())

	s.Put(model.KeyMgmtMachines, model.Snapshot[model.Machine]{
		Items: []model.Machine{
			{Namespace: "capi-system-with-a-long-name", Phase: "Running",
				Name: "tenant-01-control-plane-5d9c7b8f4d-lm2vp-and-then-some-more"},
			{Namespace: "capi-system-with-a-long-name", Phase: "Provisioning",
				Name: "tenant-01-control-plane-5d9c7b8f4d-qt6rk-and-then-some-more"},
		},
		UpdatedAt: time.Now(),
	})
	if after := borderColumns(m.View()); slices.Equal(before, after) {
		t.Errorf("a fleet too wide for its tile should be adopted, but the borders stayed at %v", before)
	}
}

// The counterpart, and the reason [Model.adopt] asks about truncation rather than
// about how far the ideal boundaries moved: content can change by a lot and still
// fit, and re-proportioning the band for it moves every border on screen to no
// purpose. Machines & Hosts here grows by seventeen cells inside a tile with more
// than forty to spare.
func TestSettle_GrowthThatStillFitsDoesNotMoveTheLayout(t *testing.T) {
	s := populatedStore()
	m := newModel(t, s, 240, 50)
	before := borderColumns(m.View())

	s.Put(model.KeyMgmtMachines, model.Snapshot[model.Machine]{
		Items: []model.Machine{
			{Namespace: "capi", Name: "demo-workers-5d9c7-lm2vp", Phase: "Running"},
			{Namespace: "capi", Name: "demo-workers-5d9c7-qt6rk", Phase: "Provisioning"},
		},
		UpdatedAt: time.Now(),
	})
	if after := borderColumns(m.View()); !slices.Equal(before, after) {
		t.Errorf("borders moved for content that still fits: %v then %v", before, after)
	}
}

// The churn that prompted this rule. A crash-looping pod appears and clears, which
// is the most ordinary thing a cluster does, and neither Pod Health nor anything
// beside it is short of room in either state — so nothing may move.
func TestSettle_PodChurnDoesNotMoveTheLayout(t *testing.T) {
	pods := func(unhealthy ...model.Pod) model.Snapshot[model.Pod] {
		items := []model.Pod{{Namespace: "kube-system", Name: "ok",
			IsHealthy: true, ReadyReady: 1, ReadyTotal: 1, Status: "Running"}}
		return model.Snapshot[model.Pod]{Items: append(items, unhealthy...), UpdatedAt: time.Now()}
	}
	crashing := model.Pod{Namespace: "kube-system", Name: "bad",
		ReadyTotal: 1, Status: "CrashLoopBackOff", Node: "n2"}
	newlyCrashing := model.Pod{Namespace: "openstack", Node: "n2",
		Name: "nova-compute-5d9c7b8f4d-xk2mn", ReadyTotal: 1, Status: "CrashLoopBackOff"}

	// Four columns: the band has three tiles to re-proportion, so this is where
	// one pane's appetite changing was felt hardest.
	s := populatedStore()
	s.Put(model.KeyWorkloadPods, pods(crashing))
	m := newModel(t, s, 320, 80)
	before := borderColumns(m.View())

	s.Put(model.KeyWorkloadPods, pods(crashing, newlyCrashing))
	during := borderColumns(m.View())
	if !slices.Equal(before, during) {
		t.Errorf("borders moved when a pod started crashing: %v then %v", before, during)
	}

	s.Put(model.KeyWorkloadPods, pods(crashing))
	if after := borderColumns(m.View()); !slices.Equal(before, after) {
		t.Errorf("borders moved when it recovered: %v then %v", before, after)
	}
}

// The first frame is drawn before any informer has delivered, so the even division
// that produces is provisional: the layout the reader keeps is the first one
// measured against real content.
func TestSettle_LayoutDecidedWithoutContentIsProvisional(t *testing.T) {
	s := store.New()
	m := newModel(t, s, 320, 80)
	empty := borderColumns(m.View())

	populate(s)
	if after := borderColumns(m.View()); slices.Equal(empty, after) {
		t.Errorf("the first frame with content should re-decide, but borders stayed at %v", empty)
	}
}

// A pane appearing or disappearing is an arrangement change, not a resize, and it
// has to be adopted however little the surviving tiles move — otherwise the new
// pane is drawn into a hole the settled geometry never allocated.
func TestSettle_ArrangementChangeIsAdopted(t *testing.T) {
	s := populatedStore()
	res := resolved()
	all := CorePanes(s, res, nil)

	m := New(res, s, plugin.NewRegistry(), all[:len(all)-1])
	m.Update(tea.WindowSizeMsg{Width: 320, Height: 80})
	before := borderColumns(m.View())

	m2 := New(res, s, plugin.NewRegistry(), all)
	m2.Update(tea.WindowSizeMsg{Width: 320, Height: 80})
	m2.settled = m.settled
	if after := borderColumns(m2.View()); slices.Equal(before, after) {
		t.Errorf("a new pane should re-decide the layout, but the borders stayed at %v", before)
	}
}

// A resize is the reader asking for a new arrangement, so nothing is preserved
// across it — including the case where the settled geometry would still fit.
func TestSettle_ResizeReDecides(t *testing.T) {
	s := populatedStore()
	m := newModel(t, s, 240, 50)
	before := borderColumns(m.View())

	m.Update(tea.WindowSizeMsg{Width: 200, Height: 50})
	after := borderColumns(m.View())
	if slices.Equal(before, after) {
		t.Errorf("a resize should re-decide the layout, but borders stayed at %v", before)
	}
	for _, c := range after {
		if c >= 200 {
			t.Errorf("a retained border at column %d is outside the new terminal", c)
		}
	}
}

// Zoom and the column overrides are deliberate requests for a different
// arrangement, and must not be held back by what is on screen.
func TestSettle_ZoomAndColumnsReDecide(t *testing.T) {
	s := populatedStore()
	m := newModel(t, s, 240, 50)
	before := borderColumns(m.View())

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}})
	if zoomed := borderColumns(m.View()); slices.Equal(before, zoomed) {
		t.Error("zoom should re-decide the layout")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}})

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'['}})
	if fewer := borderColumns(m.View()); slices.Equal(before, fewer) {
		t.Error("a column override should re-decide the layout")
	}
}

// Focus is not geometry: retaining the settled tiles must not retain which pane
// was highlighted, or tab would stop repainting.
func TestSettle_FocusStillFollowsTab(t *testing.T) {
	m := newModel(t, populatedStore(), 240, 50)
	first := m.View()
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if second := m.View(); first == second {
		t.Error("tab should still change the frame while the geometry is held")
	}
}

// unhealthyPodPane returns a pods snapshot, optionally carrying a crash-looping
// pod whose name is long enough to change what Pod Health can use.
func podsSnapshot(crashing bool) model.Snapshot[model.Pod] {
	items := []model.Pod{
		{Namespace: "kube-system", Name: "ok", IsHealthy: true,
			ReadyReady: 1, ReadyTotal: 1, Status: "Running"},
		{Namespace: "storage", Name: "csi-node-driver-9mq2f", ReadyTotal: 3,
			Status: "CrashLoopBackOff", Restarts: 7, Node: "compute-node-2"},
	}
	if crashing {
		items = append(items, model.Pod{
			Namespace: "openstack", Name: "nova-compute-controller-manager-5d9c7b8f4d-xk2mn",
			ReadyTotal: 3, Status: "CrashLoopBackOff", Restarts: 41,
			Node: "compute-node-3.site-a.example.com"})
	}
	return model.Snapshot[model.Pod]{Items: items, UpdatedAt: time.Now()}
}

// A pod that crash-loops in and out of the unhealthy list genuinely changes what
// Pod Health can use, and the two states want different widths — so sizing from the
// instant reading makes each state starve whichever pane the other one fed, and the
// screen swings for as long as the pod flaps. The width is taken once and held.
func TestSettle_FlappingPodMovesTheLayoutOnce(t *testing.T) {
	s := populatedStore()
	s.Put(model.KeyWorkloadPods, podsSnapshot(false))
	m := newModel(t, s, 320, 60)
	clock := time.Now()
	m.now = func() time.Time { return clock }

	var seen []string
	for round := range 10 {
		s.Put(model.KeyWorkloadPods, podsSnapshot(round%2 == 1))
		clock = clock.Add(20 * time.Second) // one poll
		if sig := fmt.Sprint(borderColumns(m.View())); len(seen) == 0 || sig != seen[len(seen)-1] {
			seen = append(seen, sig)
		}
	}
	if len(seen) > 2 {
		t.Errorf("the layout moved %d times over ten polls of one flapping pod:\n  %s",
			len(seen)-1, strings.Join(seen, "\n  "))
	}
}

// The width is held, not taken for good. The release happens in the appetite: the
// pane stops being sized for an incident that is over, and the geometry follows the
// next time that actually decides something. It deliberately does not follow at
// once — moving every border to hand back room nobody was short of is the thing
// this whole mechanism exists to avoid.
func TestAppetite_PeakIsHeldThenReleased(t *testing.T) {
	s := populatedStore()
	m := newModel(t, s, 320, 60)
	clock := time.Now()
	m.now = func() time.Time { return clock }

	s.Put(model.KeyWorkloadPods, podsSnapshot(false))
	base := m.appetites()["pods"]
	s.Put(model.KeyWorkloadPods, podsSnapshot(true))
	peak := m.appetites()["pods"]
	if peak <= base {
		t.Fatalf("a crash-looping pod with a long name should raise the appetite: %d then %d", base, peak)
	}

	// Recovered, and still sized for it a poll later.
	s.Put(model.KeyWorkloadPods, podsSnapshot(false))
	clock = clock.Add(20 * time.Second)
	if got := m.appetites()["pods"]; got != peak {
		t.Errorf("appetite fell to %d one poll after recovery; want the peak %d held", got, peak)
	}

	// And released once nothing has needed it for the hold.
	clock = clock.Add(peakHold)
	if got := m.appetites()["pods"]; got != base {
		t.Errorf("after %s of quiet the appetite should be back to %d, got %d", peakHold, base, got)
	}
}

// The geometry side of the same story: the tile keeps what it was given rather than
// surrendering it the moment the pod recovers.
func TestSettle_HeldWidthSurvivesRecovery(t *testing.T) {
	s := populatedStore()
	m := newModel(t, s, 320, 60)
	clock := time.Now()
	m.now = func() time.Time { return clock }

	s.Put(model.KeyWorkloadPods, podsSnapshot(false))
	clock = clock.Add(20 * time.Second)
	before := borderColumns(m.View())

	s.Put(model.KeyWorkloadPods, podsSnapshot(true))
	clock = clock.Add(20 * time.Second)
	during := borderColumns(m.View())
	if slices.Equal(before, during) {
		t.Fatalf("a pod too wide for the tile should have been given the width: %v", before)
	}

	s.Put(model.KeyWorkloadPods, podsSnapshot(false))
	clock = clock.Add(20 * time.Second)
	if after := borderColumns(m.View()); !slices.Equal(during, after) {
		t.Errorf("the width was surrendered on the first clean poll: %v then %v", during, after)
	}
}

// A pane can be short of room for as long as its content stays long — a fleet of
// 60-character pod names never fits a quarter of the screen — and being short is
// the one state in which every rule here is willing to move something. So this
// pins the end of that: a starved pane whose rows churn underneath it, poll after
// poll, still may not drag the screen around.
func TestSettle_StarvedPaneStillHoldsStill(t *testing.T) {
	// 260 columns is where Pod Health lands just short of what these rows want.
	const width = 260
	s := populatedStore()
	pods := func(round int) model.Snapshot[model.Pod] {
		var items []model.Pod
		for i := range 3 {
			// A different workload crashes each poll, each named a shade longer
			// than the last: the widest row creeps by a cell and never settles.
			items = append(items, model.Pod{
				Namespace: "openstack-services", ReadyTotal: 3, Status: "CrashLoopBackOff",
				Name: fmt.Sprintf("nova-compute-controller-manager-5d9c7b8f4d-%s%d",
					strings.Repeat("x", round), i),
				Node:     "compute-node-3.site-a.example.com",
				Restarts: int32(9 + round*7), Age: time.Duration(round) * time.Minute,
			})
		}
		return model.Snapshot[model.Pod]{Items: items, UpdatedAt: time.Now()}
	}

	s.Put(model.KeyWorkloadPods, pods(0))
	m := newModel(t, s, width, 60)
	clock := time.Now()
	m.now = func() time.Time { return clock }

	var seen []string
	for round := range 12 {
		s.Put(model.KeyWorkloadPods, pods(round))
		clock = clock.Add(20 * time.Second)
		if sig := fmt.Sprint(borderColumns(m.View())); len(seen) == 0 || sig != seen[len(seen)-1] {
			seen = append(seen, sig)
		}
	}
	if len(seen) > 2 {
		t.Errorf("a starved pane whose want crept by a cell a poll moved the layout %d times:\n  %s",
			len(seen)-1, strings.Join(seen, "\n  "))
	}
}
