package grid

import (
	"strings"
	"testing"

	"github.com/runlevel-six/sextant/pkg/tui"
)

type stubPane struct {
	id     string
	prio   tui.Priority
	weight int
}

func (s stubPane) ID() string                   { return s.id }
func (s stubPane) Title() string                { return s.id }
func (s stubPane) Priority() tui.Priority       { return s.prio }
func (s stubPane) MinWidth() int                { return 30 }
func (s stubPane) MinHeight() int               { return 5 }
func (s stubPane) HeightWeight() int            { return s.weight }
func (s stubPane) Render(int, int, bool) string { return "" }

type fixedStubPane struct {
	stubPane
	fixedHeight int
}

func (f fixedStubPane) FixedHeight(int) int { return f.fixedHeight }

type fixedSizedStubPane struct {
	stubPane
	fixedHeight   int
	widthFraction int // tile width = termWidth / widthFraction
}

func (f fixedSizedStubPane) FixedHeight(int) int          { return f.fixedHeight }
func (f fixedSizedStubPane) FixedWidth(termWidth int) int { return termWidth / f.widthFraction }

type stackedStubPane struct {
	stubPane
	host  string
	ratio float64
}

func (s stackedStubPane) StackUnder() string  { return s.host }
func (s stackedStubPane) StackRatio() float64 { return s.ratio }

func ps() []tui.Pane {
	return []tui.Pane{
		stubPane{id: "nodes", prio: tui.P0Critical, weight: 1},
		stubPane{id: "capi", prio: tui.P0Critical, weight: 1},
		stubPane{id: "unhealthy", prio: tui.P0Critical, weight: 1},
		stubPane{id: "events", prio: tui.P1Important, weight: 1},
		stubPane{id: "conditions", prio: tui.P1Important, weight: 1},
		stubPane{id: "networking", prio: tui.P2Useful, weight: 1},
		stubPane{id: "ceph", prio: tui.P2Useful, weight: 1},
		stubPane{id: "hypervisors", prio: tui.P3Optional, weight: 1},
		stubPane{id: "computeservices", prio: tui.P3Optional, weight: 1},
		stubPane{id: "netagents", prio: tui.P3Optional, weight: 1},
	}
}

func TestSmall_FocusMode(t *testing.T) {
	l := Compute(Options{Width: 80, Height: 24, Panes: ps(), FocusedID: "unhealthy", HeaderH: 3})
	if l.Breakpoint != Small {
		t.Fatalf("breakpoint: got %s want Small", l.Breakpoint)
	}
	if len(l.Tiles) != 1 {
		t.Fatalf("tiles: got %d want 1", len(l.Tiles))
	}
	if l.Tiles[0].PaneID != "unhealthy" {
		t.Errorf("focused tile: got %q want unhealthy", l.Tiles[0].PaneID)
	}
	if l.Tiles[0].W != 80 || l.Tiles[0].H != 21 {
		t.Errorf("focused tile size: got %dx%d want 80x21", l.Tiles[0].W, l.Tiles[0].H)
	}
	if len(l.Hidden) != len(ps())-1 {
		t.Errorf("hidden count: got %d want %d", len(l.Hidden), len(ps())-1)
	}
}

func TestSmall_DefaultsToHighestPriority(t *testing.T) {
	l := Compute(Options{Width: 80, Height: 24, Panes: ps(), HeaderH: 3})
	// Within P0Critical, ties break by registration order, so nodes wins.
	if l.Tiles[0].PaneID != "nodes" {
		t.Errorf("default focus: got %q want nodes", l.Tiles[0].PaneID)
	}
}

func TestSmall_UnknownFocusFallsBackToHighestPriority(t *testing.T) {
	l := Compute(Options{Width: 80, Height: 24, Panes: ps(), FocusedID: "no-such-pane", HeaderH: 3})
	if len(l.Tiles) != 1 {
		t.Fatalf("tiles: got %d want 1", len(l.Tiles))
	}
	if l.Tiles[0].PaneID != "nodes" {
		t.Errorf("fallback focus: got %q want nodes", l.Tiles[0].PaneID)
	}
}

func TestMedium_TwoColumns(t *testing.T) {
	l := Compute(Options{Width: 160, Height: 50, Panes: ps(), HeaderH: 3})
	if l.Breakpoint != Medium {
		t.Fatalf("breakpoint: got %s want Medium", l.Breakpoint)
	}
	// 50 - 3 header = 47 body / 8 minRowHeight = 5 rows; 5*2 = 10 tiles.
	if len(l.Tiles) != 10 {
		t.Errorf("tiles: got %d want 10", len(l.Tiles))
	}
	if l.Tiles[0].Y != l.Tiles[1].Y {
		t.Error("first row tiles should share Y")
	}
	if l.Tiles[0].X != 0 || l.Tiles[1].X != 80 {
		t.Errorf("row 0 X positions: got %d,%d want 0,80", l.Tiles[0].X, l.Tiles[1].X)
	}
}

func TestUltra_FourColumns(t *testing.T) {
	l := Compute(Options{Width: 300, Height: 60, Panes: ps(), HeaderH: 3})
	if l.Breakpoint != Ultra {
		t.Fatalf("breakpoint: got %s want Ultra", l.Breakpoint)
	}
	// All 10 panes fit in 4 columns x 3 rows = 12 slots.
	if len(l.Tiles) != 10 {
		t.Errorf("tiles: got %d want 10", len(l.Tiles))
	}
	if len(l.Hidden) != 0 {
		t.Errorf("hidden: got %v want []", l.Hidden)
	}
}

func TestGrid_LastTileFillsRemainder(t *testing.T) {
	// 161 / 2 = 80 remainder 1; the right tile picks up the leftover.
	l := Compute(Options{Width: 161, Height: 50, Panes: ps(), HeaderH: 3})
	right := l.Tiles[1]
	if right.X+right.W != 161 {
		t.Errorf("right edge: got %d want 161", right.X+right.W)
	}
}

func TestOrderIsByPriority(t *testing.T) {
	l := Compute(Options{Width: 300, Height: 60, Panes: ps(), HeaderH: 3})
	want := []string{"nodes", "capi", "unhealthy", "events", "conditions"}
	got := l.Order[:5]
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order prefix: got %v want %v", got, want)
	}
}

func TestComputeDoesNotMutateInput(t *testing.T) {
	in := ps()
	before := make([]string, len(in))
	for i, p := range in {
		before[i] = p.ID()
	}
	// Priority-sorting happens on a copy, so a caller's slice keeps its order.
	Compute(Options{Width: 300, Height: 60, Panes: in, HeaderH: 3})
	for i, p := range in {
		if p.ID() != before[i] {
			t.Fatalf("input reordered at %d: got %q want %q", i, p.ID(), before[i])
		}
	}
}

func TestHeaderHDefaultsToThree(t *testing.T) {
	l := Compute(Options{Width: 300, Height: 60, Panes: ps()})
	if l.HeaderH != 3 {
		t.Errorf("HeaderH: got %d want 3", l.HeaderH)
	}
	if l.Tiles[0].Y != 3 {
		t.Errorf("first tile Y: got %d want 3", l.Tiles[0].Y)
	}
}

// TestWeightedRowHeights checks that a row holding a heavy pane gets more
// height than a row of light ones.
func TestWeightedRowHeights(t *testing.T) {
	ws := []tui.Pane{
		stubPane{id: "a", prio: tui.P0Critical, weight: 1},
		stubPane{id: "b", prio: tui.P0Critical, weight: 1},
		stubPane{id: "c", prio: tui.P1Important, weight: 5},
		stubPane{id: "d", prio: tui.P1Important, weight: 5},
	}
	l := Compute(Options{Width: 200, Height: 63, Panes: ws, HeaderH: 3, OverrideCols: 2})
	if len(l.Tiles) != 4 {
		t.Fatalf("tiles: got %d want 4", len(l.Tiles))
	}
	// bodyH=60, weights 1+5=6, so row 0 ≈ 10 and row 1 ≈ 50.
	row0H, row1H := l.Tiles[0].H, l.Tiles[2].H
	if row0H >= row1H {
		t.Errorf("expected light row shorter than heavy row: row0=%d row1=%d", row0H, row1H)
	}
	if row0H+row1H != 60 {
		t.Errorf("rows should fill bodyH=60: got %d+%d", row0H, row1H)
	}
	if l.Tiles[0].H != l.Tiles[1].H {
		t.Errorf("row 0 tiles should share height: %d vs %d", l.Tiles[0].H, l.Tiles[1].H)
	}
}

// TestCols_CappedToPaneCount makes sure an override past the pane count does
// not produce empty trailing cells.
func TestCols_CappedToPaneCount(t *testing.T) {
	ws := []tui.Pane{
		stubPane{id: "a", prio: tui.P0Critical, weight: 1},
		stubPane{id: "b", prio: tui.P0Critical, weight: 1},
		stubPane{id: "c", prio: tui.P0Critical, weight: 1},
	}
	l := Compute(Options{Width: 400, Height: 30, Panes: ws, HeaderH: 3, OverrideCols: 6})
	if len(l.Tiles) != 3 {
		t.Fatalf("tiles: got %d want 3", len(l.Tiles))
	}
	if l.Tiles[0].Y != l.Tiles[2].Y {
		t.Errorf("expected single row, got y values %d/%d/%d", l.Tiles[0].Y, l.Tiles[1].Y, l.Tiles[2].Y)
	}
	if right := l.Tiles[2].X + l.Tiles[2].W; right != 400 {
		t.Errorf("right edge: got %d want 400", right)
	}
}

func TestCols_OverrideClampedToMaxColumns(t *testing.T) {
	ws := make([]tui.Pane, 0, 20)
	for i := range 20 {
		ws = append(ws, stubPane{id: string(rune('a' + i)), prio: tui.P0Critical, weight: 1})
	}
	l := Compute(Options{Width: 400, Height: 60, Panes: ws, HeaderH: 3, OverrideCols: 99})
	// With cols clamped to MaxColumns, row 0 holds exactly MaxColumns tiles.
	inRow0 := 0
	for _, tile := range l.Tiles {
		if tile.Y == l.Tiles[0].Y {
			inRow0++
		}
	}
	if inRow0 != MaxColumns {
		t.Errorf("row 0 tile count: got %d want %d", inRow0, MaxColumns)
	}
}

// TestFixedHeight_LiftsToOwnRow checks that a fixed-height pane gets a
// dedicated full-width top row sized to its request plus chrome, and that grid
// panes share what remains.
func TestFixedHeight_LiftsToOwnRow(t *testing.T) {
	ws := []tui.Pane{
		fixedStubPane{stubPane: stubPane{id: "overview", prio: tui.P0Critical, weight: 1}, fixedHeight: 7},
		stubPane{id: "nodes", prio: tui.P0Critical, weight: 12},
		stubPane{id: "machines", prio: tui.P1Important, weight: 12},
	}
	l := Compute(Options{Width: 200, Height: 50, Panes: ws, HeaderH: 3, OverrideCols: 2})
	if l.Tiles[0].PaneID != "overview" {
		t.Fatalf("row 0 pane: got %q want overview", l.Tiles[0].PaneID)
	}
	if l.Tiles[0].W != 200 {
		t.Errorf("overview width: got %d want 200", l.Tiles[0].W)
	}
	wantH := 7 + tui.PaneChromeV
	if l.Tiles[0].H != wantH {
		t.Errorf("overview height: got %d want %d (body+chrome)", l.Tiles[0].H, wantH)
	}
	if len(l.Tiles) != 3 {
		t.Fatalf("tiles: got %d want 3", len(l.Tiles))
	}
	// Body height 47, less the 9 taken above, leaves 38 for the grid row.
	wantGrid := 47 - wantH
	if l.Tiles[1].H != wantGrid || l.Tiles[2].H != wantGrid {
		t.Errorf("grid heights: got %d/%d want %d/%d", l.Tiles[1].H, l.Tiles[2].H, wantGrid, wantGrid)
	}
}

// TestFixedHeight_SkippedInSmallMode confirms fixed-height behavior does not
// apply in single-column focus mode.
func TestFixedHeight_SkippedInSmallMode(t *testing.T) {
	ws := []tui.Pane{
		fixedStubPane{stubPane: stubPane{id: "overview", prio: tui.P0Critical, weight: 1}, fixedHeight: 5},
		stubPane{id: "nodes", prio: tui.P0Critical, weight: 12},
	}
	l := Compute(Options{Width: 80, Height: 24, Panes: ws, FocusedID: "overview", HeaderH: 3})
	if len(l.Tiles) != 1 {
		t.Fatalf("tiles: got %d want 1 (Small focus mode)", len(l.Tiles))
	}
	if l.Tiles[0].H != 21 {
		t.Errorf("focused tile height: got %d want 21", l.Tiles[0].H)
	}
}

// TestFixedHeight_RespectsMinHeight checks that a pane asking for less than its
// MinHeight is still given MinHeight.
func TestFixedHeight_RespectsMinHeight(t *testing.T) {
	// stubPane.MinHeight is 5, so a request of 1 must be lifted to 5-chrome.
	ws := []tui.Pane{
		fixedStubPane{stubPane: stubPane{id: "tiny", prio: tui.P0Critical, weight: 1}, fixedHeight: 1},
		stubPane{id: "nodes", prio: tui.P0Critical, weight: 12},
	}
	l := Compute(Options{Width: 200, Height: 50, Panes: ws, HeaderH: 3, OverrideCols: 2})
	if got, want := l.Tiles[0].H, 5; got != want {
		t.Errorf("tiny pane height: got %d want %d (MinHeight floor)", got, want)
	}
}

// TestFixedSize_HalfWidthLeavesRowSpace checks that a lone fixed-size pane sits
// at its requested width, leaving the rest of the row for a future sibling.
func TestFixedSize_HalfWidthLeavesRowSpace(t *testing.T) {
	ws := []tui.Pane{
		fixedSizedStubPane{stubPane: stubPane{id: "overview", prio: tui.P0Critical, weight: 1}, fixedHeight: 7, widthFraction: 2},
		stubPane{id: "nodes", prio: tui.P0Critical, weight: 12},
	}
	l := Compute(Options{Width: 200, Height: 50, Panes: ws, HeaderH: 3, OverrideCols: 2})
	if l.Tiles[0].PaneID != "overview" {
		t.Fatalf("row 0 pane: got %q want overview", l.Tiles[0].PaneID)
	}
	if l.Tiles[0].W != 100 {
		t.Errorf("overview width: got %d want 100 (half of 200)", l.Tiles[0].W)
	}
	if wantH := 7 + tui.PaneChromeV; l.Tiles[0].H != wantH {
		t.Errorf("overview height: got %d want %d (body+chrome)", l.Tiles[0].H, wantH)
	}
	for _, tile := range l.Tiles[1:] {
		if tile.Y == l.Tiles[0].Y {
			t.Errorf("expected no other tile on overview's row, got %s at x=%d", tile.PaneID, tile.X)
		}
	}
}

// TestFixedSize_PacksTwoPanesIntoOneRow ensures two adjacent fixed-size panes
// share one row at the taller of their heights.
func TestFixedSize_PacksTwoPanesIntoOneRow(t *testing.T) {
	ws := []tui.Pane{
		fixedSizedStubPane{stubPane: stubPane{id: "k8s", prio: tui.P0Critical, weight: 1}, fixedHeight: 6, widthFraction: 2},
		fixedSizedStubPane{stubPane: stubPane{id: "openstack", prio: tui.P0Critical, weight: 1}, fixedHeight: 8, widthFraction: 2},
		stubPane{id: "nodes", prio: tui.P0Critical, weight: 12},
	}
	l := Compute(Options{Width: 200, Height: 50, Panes: ws, HeaderH: 3, OverrideCols: 2})
	if len(l.Tiles) < 2 {
		t.Fatalf("expected at least 2 tiles, got %d", len(l.Tiles))
	}
	if l.Tiles[0].PaneID != "k8s" || l.Tiles[1].PaneID != "openstack" {
		t.Errorf("row 0 panes: got %q,%q want k8s,openstack", l.Tiles[0].PaneID, l.Tiles[1].PaneID)
	}
	if l.Tiles[0].Y != l.Tiles[1].Y {
		t.Error("expected k8s and openstack to share Y")
	}
	wantH := 8 + tui.PaneChromeV
	if l.Tiles[0].H != wantH || l.Tiles[1].H != wantH {
		t.Errorf("row height should be max(6,8)+chrome=%d; got %d,%d", wantH, l.Tiles[0].H, l.Tiles[1].H)
	}
	if l.Tiles[0].X+l.Tiles[0].W != l.Tiles[1].X {
		t.Errorf("packed tiles should be edge-to-edge: x+w=%d, next x=%d", l.Tiles[0].X+l.Tiles[0].W, l.Tiles[1].X)
	}
}

// TestFixedSize_LastTileAbsorbsWidthRemainder checks that when several
// fixed-size panes share a row and their widths don't divide evenly, the last
// one expands so the row reaches the right edge.
func TestFixedSize_LastTileAbsorbsWidthRemainder(t *testing.T) {
	ws := []tui.Pane{
		fixedSizedStubPane{stubPane: stubPane{id: "a", prio: tui.P0Critical, weight: 1}, fixedHeight: 6, widthFraction: 3},
		fixedSizedStubPane{stubPane: stubPane{id: "b", prio: tui.P0Critical, weight: 1}, fixedHeight: 6, widthFraction: 3},
		fixedSizedStubPane{stubPane: stubPane{id: "c", prio: tui.P0Critical, weight: 1}, fixedHeight: 6, widthFraction: 3},
		stubPane{id: "nodes", prio: tui.P0Critical, weight: 12},
	}
	// 200/3 = 66, so three tiles cover 198 and would leave a 2-cell gap.
	l := Compute(Options{Width: 200, Height: 50, Panes: ws, HeaderH: 3, OverrideCols: 3})
	if len(l.Tiles) < 3 {
		t.Fatalf("expected at least 3 tiles, got %d", len(l.Tiles))
	}
	a, b, c := l.Tiles[0], l.Tiles[1], l.Tiles[2]
	if a.PaneID != "a" || b.PaneID != "b" || c.PaneID != "c" {
		t.Fatalf("top-row order: got %q,%q,%q want a,b,c", a.PaneID, b.PaneID, c.PaneID)
	}
	if a.Y != b.Y || b.Y != c.Y {
		t.Error("expected all three fixed-size tiles to share Y")
	}
	if a.W != 66 || b.W != 66 {
		t.Errorf("first two tile widths: got %d,%d want 66,66", a.W, b.W)
	}
	if c.X+c.W != 200 {
		t.Errorf("last tile should reach right edge: x+w=%d want 200", c.X+c.W)
	}
}

// TestZoom_LiftsToFullWidthRow checks that a zoomed pane owns a full-width row
// 0 and dominates height, while the others stay visible below.
func TestZoom_TakesTheWholeBody(t *testing.T) {
	l := Compute(Options{Width: 200, Height: 63, Panes: ps(), FocusedID: "capi", HeaderH: 3, OverrideCols: 2, ZoomedID: "capi"})

	// Zoom means full size. An operator presses z on a pane that is truncating,
	// so a wider-but-still-short row is not what was asked for.
	if len(l.Tiles) != 1 {
		t.Fatalf("zoom should leave exactly one tile, got %d", len(l.Tiles))
	}
	got := l.Tiles[0]
	if got.PaneID != "capi" {
		t.Errorf("zoomed pane: got %q want capi", got.PaneID)
	}
	if got.X != 0 || got.W != 200 {
		t.Errorf("zoomed tile span: got x=%d w=%d want 0,200", got.X, got.W)
	}
	if got.Y != 3 || got.H != 60 {
		t.Errorf("zoomed tile should own the body below the header: got y=%d h=%d want 3,60", got.Y, got.H)
	}
	// Everything else is reachable by focus cycling rather than lost.
	if len(l.Hidden) != len(l.Order)-1 {
		t.Errorf("hidden = %d of %d panes, want all but the zoomed one", len(l.Hidden), len(l.Order))
	}
}

func TestZoom_UnknownIDIsIgnored(t *testing.T) {
	l := Compute(Options{Width: 200, Height: 63, Panes: ps(), HeaderH: 3, OverrideCols: 2, ZoomedID: "no-such-pane"})
	// Row 0 should hold a normal pair of grid tiles, not a full-width tile.
	if l.Tiles[0].W == 200 {
		t.Errorf("unknown zoom ID produced a full-width row 0 tile %q", l.Tiles[0].PaneID)
	}
}

// TestStackedUnder_SplicesOntoHostTile checks that a stacked pane gives up its
// own cell and becomes a sub-tile of its host at the requested ratio.
func TestStackedUnder_SplicesOntoHostTile(t *testing.T) {
	ws := []tui.Pane{
		stubPane{id: "pods", prio: tui.P0Critical, weight: 1},
		stubPane{id: "nodes", prio: tui.P0Critical, weight: 1},
		stackedStubPane{stubPane: stubPane{id: "events", prio: tui.P1Important, weight: 1}, host: "pods", ratio: 0.45},
	}
	l := Compute(Options{Width: 200, Height: 50, Panes: ws, HeaderH: 3, OverrideCols: 2})

	var hostTile *Tile
	for i := range l.Tiles {
		if len(l.Tiles[i].Stacked) > 0 {
			hostTile = &l.Tiles[i]
			break
		}
	}
	if hostTile == nil {
		t.Fatal("no stacked tile emitted")
	}
	if hostTile.PaneID != "" {
		t.Errorf("stacked host tile should have empty PaneID, got %q", hostTile.PaneID)
	}
	if len(hostTile.Stacked) != 2 {
		t.Fatalf("sub-tiles: got %d want 2", len(hostTile.Stacked))
	}
	if hostTile.Stacked[0].PaneID != "pods" || hostTile.Stacked[1].PaneID != "events" {
		t.Errorf("sub-tile order: got %q,%q want pods,events",
			hostTile.Stacked[0].PaneID, hostTile.Stacked[1].PaneID)
	}
	if got := hostTile.Stacked[1].HRatio; got != 0.45 {
		t.Errorf("partner ratio: got %v want 0.45", got)
	}
	if got := hostTile.Stacked[0].HRatio; got != 0.55 {
		t.Errorf("host ratio: got %v want 0.55", got)
	}
	for _, id := range l.Hidden {
		if id == "events" {
			t.Error("stacked pane should not be reported hidden when spliced")
		}
	}
}

// TestStackedUnder_FallsBackWhenHostMissing checks that a stacked pane whose
// host isn't placed ends up hidden rather than silently dropped.
func TestStackedUnder_FallsBackWhenHostMissing(t *testing.T) {
	ws := []tui.Pane{
		stubPane{id: "nodes", prio: tui.P0Critical, weight: 1},
		stubPane{id: "pods", prio: tui.P0Critical, weight: 1},
		stackedStubPane{stubPane: stubPane{id: "events", prio: tui.P1Important, weight: 1}, host: "ghost", ratio: 0.4},
	}
	l := Compute(Options{Width: 200, Height: 50, Panes: ws, HeaderH: 3, OverrideCols: 2})

	if !containsID(l.Order, "events") {
		t.Fatal("events missing from Order")
	}
	placed := false
	for _, tile := range l.Tiles {
		if tile.PaneID == "events" {
			placed = true
		}
		for _, s := range tile.Stacked {
			if s.PaneID == "events" {
				placed = true
			}
		}
	}
	if placed {
		t.Fatal("events should not be placed when its host is absent")
	}
	if !containsID(l.Hidden, "events") {
		t.Errorf("events should be hidden, got Hidden=%v", l.Hidden)
	}
}

// TestStackedUnder_IgnoredInSmallMode checks that focus mode is unaffected by
// stacking, since there is only one visible pane at that width.
func TestStackedUnder_IgnoredInSmallMode(t *testing.T) {
	ws := []tui.Pane{
		stubPane{id: "pods", prio: tui.P0Critical, weight: 1},
		stackedStubPane{stubPane: stubPane{id: "events", prio: tui.P1Important, weight: 1}, host: "pods", ratio: 0.45},
	}
	l := Compute(Options{Width: 80, Height: 24, Panes: ws, FocusedID: "events", HeaderH: 3})
	if len(l.Tiles) != 1 {
		t.Fatalf("tiles: got %d want 1", len(l.Tiles))
	}
	if l.Tiles[0].PaneID != "events" {
		t.Errorf("focused pane: got %q want events", l.Tiles[0].PaneID)
	}
	if len(l.Tiles[0].Stacked) != 0 {
		t.Error("focus mode should not produce stacked sub-tiles")
	}
}

func TestEmptyPaneSet(t *testing.T) {
	l := Compute(Options{Width: 200, Height: 50, HeaderH: 3})
	if len(l.Tiles) != 0 {
		t.Errorf("tiles: got %d want 0", len(l.Tiles))
	}
	if len(l.Order) != 0 {
		t.Errorf("order: got %v want empty", l.Order)
	}
}

// TestTinyTerminalStillEmitsATile guards the "better to show something than
// nothing" rule when the body is shorter than one minimum row.
func TestTinyTerminalStillEmitsATile(t *testing.T) {
	l := Compute(Options{Width: 200, Height: 5, Panes: ps(), HeaderH: 3, OverrideCols: 2})
	if len(l.Tiles) == 0 {
		t.Fatal("expected at least one tile on a very short terminal")
	}
	for _, tile := range l.Tiles {
		if tile.H < 1 {
			t.Errorf("tile %s has non-positive height %d", tile.PaneID, tile.H)
		}
	}
}

// TestHeaderTallerThanTerminalIsClamped guards against a negative body height.
func TestHeaderTallerThanTerminalIsClamped(t *testing.T) {
	l := Compute(Options{Width: 200, Height: 2, Panes: ps(), HeaderH: 10, OverrideCols: 2})
	if l.HeaderH > 2 {
		t.Errorf("HeaderH: got %d want <= 2", l.HeaderH)
	}
	for _, tile := range l.Tiles {
		if tile.H < 1 {
			t.Errorf("tile %s has non-positive height %d", tile.PaneID, tile.H)
		}
	}
}

func TestBreakpointString(t *testing.T) {
	for bp, want := range map[Breakpoint]string{
		Small: "Small", Medium: "Medium", Wide: "Wide", Ultra: "Ultra", Breakpoint(99): "?",
	} {
		if got := bp.String(); got != want {
			t.Errorf("Breakpoint(%d).String(): got %q want %q", bp, got, want)
		}
	}
}

// --- row spans ------------------------------------------------------------

// tall is a pane that asks for several rows.
type tall struct {
	tui.Pane
	tallCount int
}

func (t tall) RowSpan() int { return t.tallCount }

// wideP is a pane that asks for several columns.
type wideP struct {
	tui.Pane
	widePCount int
}

func (w wideP) ColSpan() int { return w.widePCount }

// The layout this exists for: two panes whose row count scales with the cluster
// take two rows each on the left, the wide tables stack beside them, and the
// narrow subsystem panes fill the row beneath.
func TestRowSpan_TallPanesStackTablesBesideThem(t *testing.T) {
	panes := []tui.Pane{
		tall{Pane: stubPane{id: "machines", prio: tui.P0Critical, weight: 1}, tallCount: 2},
		tall{Pane: stubPane{id: "nodes", prio: tui.P0Critical, weight: 1}, tallCount: 2},
		wideP{Pane: stubPane{id: "pods", prio: tui.P0Critical, weight: 1}, widePCount: 2},
		wideP{Pane: stubPane{id: "events", prio: tui.P1Important, weight: 1}, widePCount: 2},
		stubPane{id: "a", prio: tui.P1Important, weight: 1},
		stubPane{id: "b", prio: tui.P1Important, weight: 1},
		stubPane{id: "c", prio: tui.P1Important, weight: 1},
		stubPane{id: "d", prio: tui.P1Important, weight: 1},
	}
	l := Compute(Options{Width: 240, Height: 90, Panes: panes, HeaderH: 3, OverrideCols: 4})

	at := map[string]Tile{}
	for _, tile := range l.Tiles {
		at[tile.PaneID] = tile
	}
	for _, id := range []string{"machines", "nodes", "pods", "events", "a", "b", "c", "d"} {
		if _, ok := at[id]; !ok {
			t.Fatalf("%s was not placed; hidden=%v", id, l.Hidden)
		}
	}

	// The tall panes are twice the height of the single-row tables beside them.
	if at["machines"].H <= at["pods"].H {
		t.Errorf("machines H=%d should exceed pods H=%d", at["machines"].H, at["pods"].H)
	}
	// Events sits beside machines, not below it: same top as pods' bottom.
	if at["events"].Y != at["pods"].Y+at["pods"].H {
		t.Errorf("events should start where pods ends: events Y=%d pods Y=%d H=%d",
			at["events"].Y, at["pods"].Y, at["pods"].H)
	}
	// And machines still covers both of their rows.
	if at["machines"].Y+at["machines"].H != at["events"].Y+at["events"].H {
		t.Errorf("machines should span both rows: ends at %d, events ends at %d",
			at["machines"].Y+at["machines"].H, at["events"].Y+at["events"].H)
	}
	// The four narrow panes share the row below, left to right.
	if at["a"].Y != at["machines"].Y+at["machines"].H {
		t.Errorf("narrow panes should start below the tall ones")
	}
	if at["a"].X >= at["b"].X || at["b"].X >= at["c"].X || at["c"].X >= at["d"].X {
		t.Errorf("narrow panes out of order: %d %d %d %d",
			at["a"].X, at["b"].X, at["c"].X, at["d"].X)
	}
}

// Tiles must tile: no overlaps, no gaps, and nothing outside the body. This is
// the invariant that row spans could most easily break, and the renderer relies
// on it to composite a line from the tiles covering it.
func TestRowSpan_TilesCoverTheBodyExactly(t *testing.T) {
	panes := []tui.Pane{
		tall{Pane: stubPane{id: "machines", prio: tui.P0Critical, weight: 1}, tallCount: 2},
		tall{Pane: stubPane{id: "nodes", prio: tui.P0Critical, weight: 1}, tallCount: 3},
		wideP{Pane: stubPane{id: "pods", prio: tui.P0Critical, weight: 1}, widePCount: 2},
		wideP{Pane: stubPane{id: "events", prio: tui.P1Important, weight: 1}, widePCount: 3},
		stubPane{id: "a", prio: tui.P1Important, weight: 1},
		stubPane{id: "b", prio: tui.P2Useful, weight: 1},
	}
	for _, size := range []struct{ w, h int }{{240, 90}, {200, 60}, {280, 40}, {160, 30}} {
		for _, cols := range []int{2, 3, 4} {
			l := Compute(Options{Width: size.w, Height: size.h, Panes: panes, HeaderH: 3, OverrideCols: cols})
			covered := map[[2]int]string{}
			for _, tile := range l.Tiles {
				for y := tile.Y; y < tile.Y+tile.H; y++ {
					for x := tile.X; x < tile.X+tile.W; x++ {
						key := [2]int{y, x}
						if prev, dup := covered[key]; dup {
							t.Fatalf("%dx%d cols=%d: %s overlaps %s at (%d,%d)",
								size.w, size.h, cols, tile.PaneID, prev, y, x)
						}
						covered[key] = tile.PaneID
						if x >= size.w || y >= size.h {
							t.Fatalf("%dx%d cols=%d: %s extends past the terminal at (%d,%d)",
								size.w, size.h, cols, tile.PaneID, y, x)
						}
					}
				}
			}
		}
	}
}

// A row span larger than the terminal has rows for is clamped, not dropped.
func TestRowSpan_ClampedOnAShortTerminal(t *testing.T) {
	panes := []tui.Pane{
		tall{Pane: stubPane{id: "greedy", prio: tui.P0Critical, weight: 1}, tallCount: 9},
		stubPane{id: "other", prio: tui.P1Important, weight: 1},
	}
	l := Compute(Options{Width: 200, Height: 24, Panes: panes, HeaderH: 3, OverrideCols: 2})
	if len(l.Tiles) == 0 {
		t.Fatal("a pane asking for more rows than exist should still be placed")
	}
	for _, tile := range l.Tiles {
		if tile.Y+tile.H > 24 {
			t.Errorf("%s runs past the terminal: Y=%d H=%d", tile.PaneID, tile.Y, tile.H)
		}
	}
}
