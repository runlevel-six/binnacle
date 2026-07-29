package ui

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/runlevel-six/sextant/internal/config"
	"github.com/runlevel-six/sextant/internal/core/model"
	"github.com/runlevel-six/sextant/internal/profile"
	"github.com/runlevel-six/sextant/internal/testansi"
	"github.com/runlevel-six/sextant/pkg/plugin"
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
	return s
}

// withTheme applies th for the duration of the test, restoring the default
// afterwards. The palette is package state, so a test that switched it and left
// it switched would silently retheme every test that follows.
func withTheme(t *testing.T, th tui.Theme) {
	t.Helper()
	tui.ApplyTheme(th)
	t.Cleanup(func() { tui.ApplyTheme(tui.DefaultTheme()) })
}

func newModel(t *testing.T, s *store.Store, w, h int) *Model {
	t.Helper()
	m := New(resolved(), s, plugin.NewRegistry(), CorePanes(s, resolved(), nil))
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

// The Nodes banner cell must not sit at amber for the life of a cluster whose
// hypervisors are cordoned on purpose. The color is the whole point of a banner:
// a permanent warning is indistinguishable from no warning at all.
func TestBanner_ExpectedCordonStaysGreen(t *testing.T) {
	s := store.New()
	s.Put(model.KeyWorkloadNodes, model.Snapshot[model.Node]{
		Items: []model.Node{
			{Name: "cp-1", Status: "Ready", Role: "control-plane"},
			{Name: "compute-1", Status: "Ready", Role: "compute", Cordoned: true},
			{Name: "compute-2", Status: "Ready", Role: "compute", Cordoned: true},
		},
		UpdatedAt: time.Now(),
	})

	res := resolved()
	res.Profile.NodeRoles.CordonExpected = []string{"compute"}
	m := New(res, s, plugin.NewRegistry(), CorePanes(s, res, nil))
	cell, ok := m.cellNodes()
	if !ok {
		t.Fatal("no nodes cell")
	}
	if cell.Status != tui.BannerOK {
		t.Errorf("status = %v (%q), want OK", cell.Status, cell.Detail)
	}

	// Without the setting the same fleet warns, which is right on a stock
	// cluster where a cordon really is a drain.
	plainModel := New(resolved(), s, plugin.NewRegistry(), CorePanes(s, resolved(), nil))
	if cell, _ := plainModel.cellNodes(); cell.Status != tui.BannerWarn {
		t.Errorf("unconfigured status = %v, want Warn", cell.Status)
	}
}

// An exemption must not swallow a real failure on an exempt node.
func TestBanner_ExpectedCordonStillReportsNotReady(t *testing.T) {
	s := store.New()
	s.Put(model.KeyWorkloadNodes, model.Snapshot[model.Node]{
		Items: []model.Node{
			{Name: "compute-1", Status: "NotReady", Role: "compute", Cordoned: true},
		},
		UpdatedAt: time.Now(),
	})

	res := resolved()
	res.Profile.NodeRoles.CordonExpected = []string{"compute"}
	m := New(res, s, plugin.NewRegistry(), CorePanes(s, res, nil))
	cell, _ := m.cellNodes()
	if cell.Status != tui.BannerErr {
		t.Errorf("status = %v (%q), want Err", cell.Status, cell.Detail)
	}
}
