// Package grid decides where each pane goes for a given terminal size.
//
// A fixed-height header strip is reserved at the top, full width, and the panes
// are arranged below it. Column count comes from a four-tier breakpoint scheme:
//
//	width  <  120    Small      1 column (focus mode — one pane visible)
//	width 120-179    Medium     2 columns
//	width 180-259    Wide       3 columns
//	width 260+       Ultra      4 columns
//
// The column count sets how many tiles a row holds, not how wide they are. Cells
// are divided between the columns of a row in proportion to what the panes in them
// say their content can use ([tui.ContentWidthPane]), and rows are trimmed to what
// their panes say they can fill ([tui.ContentHeightPane]) with the surplus going to
// the rows that never say. A grid of panes that declare neither divides evenly, as
// this did before either existed.
//
// Three optional pane interfaces bend the arrangement itself.
// A [tui.FixedHeightPane] is lifted to a dedicated row above the grid, sized to
// exactly what it asks for. A [tui.FixedSizePane] additionally constrains its
// width, and adjacent ones are packed onto a shared row. A
// [tui.StackedUnderPane] gives up its own cell to share a column with a host
// pane. Each degrades to an ordinary grid cell when its request cannot be
// honored, so no pane is ever silently dropped.
//
// Panes that do not fit are returned in [Layout.Hidden] for the program to
// surface via focus cycling and jump keys.
package grid

import (
	"sort"

	"github.com/runlevel-six/sextant/pkg/tui"
)

// Breakpoint is the width tier a terminal falls into.
type Breakpoint int

// Width tiers, narrowest first.
const (
	Small Breakpoint = iota
	Medium
	Wide
	Ultra
)

// String returns the breakpoint's name, for diagnostics.
func (b Breakpoint) String() string {
	switch b {
	case Small:
		return "Small"
	case Medium:
		return "Medium"
	case Wide:
		return "Wide"
	case Ultra:
		return "Ultra"
	}
	return "?"
}

// MaxColumns is the highest column count [Compute] honors via
// [Options.OverrideCols]. No width-based ceiling is enforced here; that is the
// caller's decision.
const MaxColumns = 8

// minRowHeight is the shortest body height allocated to a row of tiles. Below
// this a tile is too cramped to be useful. If the body cannot fit even one row
// at this height, a single row is emitted anyway at whatever height exists —
// showing something beats showing nothing.
const minRowHeight = 8

// defaultHeaderH is used when [Options.HeaderH] is unset.
const defaultHeaderH = 3

// Tile is the rectangle assigned to one pane.
//
// When Stacked is non-empty the tile hosts several panes split top to bottom
// inside the same rectangle; PaneID is then empty and the renderer iterates
// Stacked instead. Each sub-tile gets its own border and title.
type Tile struct {
	PaneID  string
	X, Y    int
	W, H    int
	Focused bool
	Stacked []SubTile
}

// SubTile is one pane inside a stacked [Tile]. HRatio is its fraction of the
// parent tile's height; the renderer gives the remainder to the last SubTile so
// that rounding cannot drop a row.
type SubTile struct {
	PaneID  string
	HRatio  float64
	Focused bool
}

// Layout is the result of one [Compute] call.
type Layout struct {
	Breakpoint Breakpoint
	HeaderW    int
	HeaderH    int
	Tiles      []Tile
	// Hidden lists the pane IDs that did not fit, in priority order. The
	// first of them is what focus cycling reaches after the last visible pane.
	Hidden []string
	// Order is every pane ID, visible and hidden, in priority order. Used for
	// deterministic focus cycling.
	Order []string
}

// Options is the input to [Compute].
type Options struct {
	// Width and Height are the terminal's dimensions in cells.
	Width, Height int
	// Panes is the full catalog to place. Compute does not mutate it.
	Panes []tui.Pane
	// FocusedID names the pane owning keyboard focus. In single-column mode
	// it is the only visible pane; otherwise it is highlighted in place. When
	// empty or unrecognized, the highest-priority pane is used.
	FocusedID string
	// HeaderH is the number of rows the header strip consumes. Zero or less
	// selects a default of 3.
	HeaderH int
	// OverrideCols replaces the breakpoint-derived column count when above
	// zero, clamped to [MaxColumns] and to the number of placeable panes.
	OverrideCols int
	// ZoomedID, when it names a pane, gives that pane the entire body: every
	// other pane is hidden and the fixed rows are surrendered too. Zoom is asked
	// for in order to see all of one pane, so a partial enlargement is not what it
	// means.
	ZoomedID string
	// Appetite overrides what a pane reports for [tui.ContentWidthPane], by pane
	// ID and in the same units — a body width, net of chrome. A pane absent from
	// the map, or mapped to zero, is asked directly.
	//
	// This exists because appetite is a measurement of live data and the layout is
	// a thing a person is reading. A caller that redraws on every store update
	// will want to smooth what the panes report before sizing anything from it;
	// what that smoothing should be is the caller's business, not the grid's.
	Appetite map[string]int
}

// columnCount returns the grid column count for a terminal width.
func columnCount(w int) int {
	switch {
	case w < 120:
		return 1
	case w < 180:
		return 2
	case w < 260:
		return 3
	default:
		return 4
	}
}

func breakpointFor(w int) Breakpoint {
	switch columnCount(w) {
	case 1:
		return Small
	case 2:
		return Medium
	case 3:
		return Wide
	default:
		return Ultra
	}
}

// spanOf returns how many grid columns a pane asks for, clamped to what this
// layout has. A pane that declares nothing takes one, which is every pane that
// predates [tui.WidePane].
func spanOf(p tui.Pane, cols int) int {
	wp, ok := p.(tui.WidePane)
	if !ok {
		return 1
	}
	// A pane whose span is a preference rather than a need gives it up on a grid
	// too narrow to spare the column. See [tui.SpanFloorPane].
	if fp, floored := p.(tui.SpanFloorPane); floored && cols < fp.MinColsForSpan() {
		return 1
	}
	return min(max(wp.ColSpan(), 1), max(cols, 1))
}

// minColsForRowSpan is the narrowest grid on which a row span is honored.
//
// Below it, spanning rows starves everything else. Two tall panes hold a column
// each for two rows, so at three columns only one column is left across both —
// and a two-column table cannot be placed there at all, which pushed Pod Health
// and Events down past the subsystem panes and inverted the reading order. Four
// columns is the point at which two tall panes still leave two columns beside
// them, which is exactly what the wide tables need.
const minColsForRowSpan = 4

// rowSpanOf returns how many grid rows a pane asks for, clamped to what this
// layout has. A pane that declares nothing takes one, and so does every pane on a
// grid too narrow to spare the columns — see [minColsForRowSpan].
func rowSpanOf(p tui.Pane, rows, cols int) int {
	tp, ok := p.(tui.TallPane)
	if !ok || cols < minColsForRowSpan {
		return 1
	}
	return min(max(tp.RowSpan(), 1), max(rows, 1))
}

// placement is one pane's slot on the two-dimensional grid. col is -1 until a
// column is assigned; a placement that never gets one is treated as hidden.
// colEnd is one past the last column the tile occupies once it has grown into any
// free columns to its right, so it is colSpan or more.
type placement struct {
	pane             tui.Pane
	row, col, colEnd int
	colSpan, rowSpan int
}

// cellsFree reports whether a block of cells is entirely unoccupied.
func cellsFree(occupied [][]bool, row, rowSpan, col, colSpan int) bool {
	for r := row; r < row+rowSpan; r++ {
		if r >= len(occupied) {
			return false
		}
		for c := col; c < col+colSpan; c++ {
			if c >= len(occupied[r]) || occupied[r][c] {
				return false
			}
		}
	}
	return true
}

// markCells claims a block of cells.
func markCells(occupied [][]bool, row, rowSpan, col, colSpan int) {
	for r := row; r < row+rowSpan && r < len(occupied); r++ {
		for c := col; c < col+colSpan && c < len(occupied[r]); c++ {
			occupied[r][c] = true
		}
	}
}

type fixedTile struct {
	pane  tui.Pane
	width int // 0 means the full row width
}

type fixedRow struct {
	tiles  []fixedTile
	height int
}

// Compute lays out o.Panes inside an o.Width x o.Height terminal.
func Compute(o Options) Layout {
	width, height := o.Width, o.Height

	headerH := o.HeaderH
	if headerH <= 0 {
		headerH = defaultHeaderH
	}
	if headerH > height {
		headerH = height
	}

	sorted := append([]tui.Pane(nil), o.Panes...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Priority() < sorted[j].Priority()
	})

	order := make([]string, len(sorted))
	for i, p := range sorted {
		order[i] = p.ID()
	}

	bp := breakpointFor(width)
	cols := columnCount(width)
	if o.OverrideCols > 0 {
		cols = min(o.OverrideCols, MaxColumns)
	}
	// Never request more columns than there are panes — empty cells are dead
	// screen space.
	if len(sorted) > 0 && cols > len(sorted) {
		cols = len(sorted)
	}
	bodyH := max(height-headerH, 1)

	// Resolve the zoomed pane. It takes over the whole body below, so the rest of
	// the grid machinery never sees it.
	var zoomed tui.Pane
	rest := sorted
	if o.ZoomedID != "" {
		for i, p := range sorted {
			if p.ID() == o.ZoomedID {
				zoomed = p
				rest = append(append([]tui.Pane(nil), sorted[:i]...), sorted[i+1:]...)
				break
			}
		}
	}

	// Pull out StackedUnderPanes so they don't claim their own cell; they are
	// spliced back onto their host's tile after grid layout. One that has no
	// host tile falls through to Hidden rather than being dropped silently.
	stackUnders := map[string][]tui.Pane{} // host ID -> stacked panes, in declared order
	if cols >= 2 {
		filtered := make([]tui.Pane, 0, len(rest))
		for _, p := range rest {
			if sp, ok := p.(tui.StackedUnderPane); ok && sp.StackUnder() != "" {
				stackUnders[sp.StackUnder()] = append(stackUnders[sp.StackUnder()], p)
				continue
			}
			filtered = append(filtered, p)
		}
		rest = filtered
	}

	// Pull out fixed-height panes. They get dedicated rows above the grid,
	// sized to exactly what they request, so short content-defined panes do
	// not inherit unused space from a taller neighbor. A zoomed pane wins
	// over fixed-height behavior, since zoom is a deliberate user action.
	var fixedRows []fixedRow
	gridPanes := make([]tui.Pane, 0, len(rest))
	for _, p := range rest {
		fhp, ok := p.(tui.FixedHeightPane)
		if !ok || cols <= 1 {
			gridPanes = append(gridPanes, p)
			continue
		}

		tileW := 0
		if fsp, sized := p.(tui.FixedSizePane); sized {
			tileW = min(fsp.FixedWidth(width), width)
		}
		// Measure at the body width so the pane can reflow into the narrower
		// space, then add chrome back when sizing the row.
		measureW := tileW
		if measureW <= 0 {
			measureW = width
		}
		bodyW := max(measureW-tui.PaneChromeH, 1)
		h := max(fhp.FixedHeight(bodyW), p.MinHeight()-tui.PaneChromeV, 1) + tui.PaneChromeV

		// Pack into the previous row when this tile and every tile already
		// there are width-constrained, and there is still room.
		if tileW > 0 && len(fixedRows) > 0 {
			last := &fixedRows[len(fixedRows)-1]
			usedW, allSized := 0, true
			for _, t := range last.tiles {
				if t.width <= 0 {
					allSized = false
					break
				}
				usedW += t.width
			}
			if allSized && usedW+tileW <= width {
				last.tiles = append(last.tiles, fixedTile{p, tileW})
				last.height = max(last.height, h)
				continue
			}
		}
		fixedRows = append(fixedRows, fixedRow{
			tiles:  []fixedTile{{pane: p, width: tileW}},
			height: h,
		})
	}

	// Re-cap columns now that the real grid-host count is known. The
	// breakpoint count is purely width-based, so a wide terminal with few grid
	// hosts would otherwise leave dead columns at the right edge. The earlier
	// cap at len(sorted) was too generous because it counted fixed-row and
	// stacked panes too.
	//
	// The floor of 2 avoids accidentally entering single-pane focus mode,
	// which drops the fixed top row entirely. The single-pane-row branch below
	// already makes a lone tile span the full grid width, so cols=2 with one
	// grid pane still looks right.
	if len(gridPanes) > 0 && cols > len(gridPanes) {
		cols = max(len(gridPanes), 2)
	}

	// Single-pane mode: skip grid math and hand the whole body to one pane.
	//
	// Two ways in. The narrowest breakpoint, where a grid cannot be usefully
	// divided; and an explicit zoom, which is the case worth spelling out. An
	// operator presses z on a pane that is truncating — 54 machines showing 16 —
	// so zoom has to mean *full size*, not "a wider row that is still too short".
	// Giving the zoomed pane its own boosted row left the other rows in place and
	// bought it about nine lines, which looked like it had only got wider. It now
	// takes the fixed rows' height too: the whole point of asking is to see all of
	// one thing, and the header still says which cluster this is.
	if cols == 1 || zoomed != nil {
		visibleID := o.FocusedID
		if zoomed != nil {
			visibleID = zoomed.ID()
		} else if visibleID == "" || !containsID(order, visibleID) {
			visibleID = ""
			if len(order) > 0 {
				visibleID = order[0]
			}
		}
		var tiles []Tile
		var hidden []string
		if visibleID != "" {
			tiles = []Tile{{PaneID: visibleID, X: 0, Y: headerH, W: width, H: bodyH, Focused: true}}
			for _, id := range order {
				if id != visibleID {
					hidden = append(hidden, id)
				}
			}
		}
		return Layout{
			Breakpoint: bp,
			HeaderW:    width,
			HeaderH:    headerH,
			Tiles:      tiles,
			Hidden:     hidden,
			Order:      order,
		}
	}

	// Reserve fixed-row height before sizing the grid. On a very short
	// terminal, trim fixed rows from the bottom; the grid disappears first.
	fixedTotalH := 0
	for i := range fixedRows {
		if fixedTotalH+fixedRows[i].height > bodyH {
			fixedRows = fixedRows[:i]
			break
		}
		fixedTotalH += fixedRows[i].height
	}
	gridBodyH := max(bodyH-fixedTotalH, 0)

	maxRows := gridBodyH / minRowHeight
	if maxRows < 1 && gridBodyH > 0 {
		maxRows = 1
	}

	gridRows := maxRows

	// Place panes on a two-dimensional grid.
	//
	// Panes ask for columns ([tui.WidePane]) and rows ([tui.TallPane]), so this is
	// a fill rather than a chunking: each pane takes the first slot where its whole
	// block fits, scanning rows top to bottom in priority order. A pane whose block
	// fits nowhere falls through to Hidden rather than being squeezed.
	//
	// Row capacity is tracked in columns, and a pane spanning rows spends its
	// columns in every row it covers — which is the whole point, since the rows
	// beneath it are no longer free across their full width.
	var placements []placement
	if gridRows > 0 {
		capacity := make([]int, gridRows)
		for i := range capacity {
			capacity[i] = cols
		}
		// minRow never decreases, so rows fill top to bottom in priority order.
		// Scanning from zero each time would let a later narrow pane backfill the
		// spare column of an earlier row and appear *above* a more important one —
		// at three columns that put Ceph and Network ahead of Pod Health, which is
		// the pane the operator checks first. A row's leftover column is better
		// given to the tile already in it, which is what the growth rule does.
		minRow := 0
		for _, p := range gridPanes {
			cs, rs := spanOf(p, cols), rowSpanOf(p, gridRows, cols)
			row := -1
			for r := minRow; r+rs <= gridRows; r++ {
				fits := true
				for k := r; k < r+rs && fits; k++ {
					fits = capacity[k] >= cs
				}
				if fits {
					row = r
					break
				}
			}
			if row < 0 {
				continue
			}
			for k := row; k < row+rs; k++ {
				capacity[k] -= cs
			}
			// The pane's own row, not the last it covers: a shorter pane may
			// legitimately sit beside a tall one in the rows below its top.
			minRow = row
			placements = append(placements, placement{
				pane: p, row: row, col: -1, colSpan: cs, rowSpan: rs,
			})
		}
	}

	rowCount := 0
	for _, pl := range placements {
		rowCount = max(rowCount, pl.row+pl.rowSpan)
	}

	// Assign columns, row by row, in priority order.
	//
	// Tiles were once ordered within a row by column span, narrow first, to line
	// the wide tables up down the right of the screen. That was justified on the
	// grounds that position does not matter because focus order and jump digits
	// come from priority — which had it backwards. The digits are *printed in the
	// titles*, so a row rendering [7] [8] [6] left to right is a row where the
	// labels visibly disagree with the layout, and the reader has to search for
	// the pane they just read the number of. Priority order costs an aesthetic
	// alignment and buys a screen that can be read in the order it is numbered.
	occupied := make([][]bool, rowCount)
	for i := range occupied {
		occupied[i] = make([]bool, cols)
	}
	byRow := make([][]int, rowCount)
	for i, pl := range placements {
		byRow[pl.row] = append(byRow[pl.row], i)
	}
	for r := range rowCount {
		starting := byRow[r]
		for _, i := range starting {
			pl := &placements[i]
			for c := 0; c+pl.colSpan <= cols; c++ {
				if !cellsFree(occupied, pl.row, pl.rowSpan, c, pl.colSpan) {
					continue
				}
				pl.col = c
				markCells(occupied, pl.row, pl.rowSpan, c, pl.colSpan)
				break
			}
		}
	}

	// Grow tiles rightwards into any column that stays free across every row they
	// cover, in (row, column) order so the result is deterministic.
	//
	// One rule doing three jobs: it absorbs a column no pane could be placed in, it
	// makes a lone tile span the full grid width, and it closes the slack left when a
	// wide pane did not fit — at three columns, two single-column tiles plus a
	// two-column pane cannot tile evenly, and the alternative is a column-wide hole.
	order2D := make([]int, 0, len(placements))
	for i, pl := range placements {
		if pl.col >= 0 {
			order2D = append(order2D, i)
		}
	}
	sort.SliceStable(order2D, func(a, b int) bool {
		pa, pb := placements[order2D[a]], placements[order2D[b]]
		if pa.row != pb.row {
			return pa.row < pb.row
		}
		return pa.col < pb.col
	})
	for _, i := range order2D {
		pl := &placements[i]
		pl.colEnd = pl.col + pl.colSpan
		for pl.colEnd < cols && cellsFree(occupied, pl.row, pl.rowSpan, pl.colEnd, 1) {
			markCells(occupied, pl.row, pl.rowSpan, pl.colEnd, 1)
			pl.colEnd++
		}
	}

	// Column widths, per band, from what the panes say they can use.
	colX, colW := bandWidths(placements, bandOf(placements, rowCount), rowCount, cols, width, o.Appetite)

	// Per-row weights: the heaviest pane covering the row. A pane spanning rows
	// counts its share per row, so it neither dominates every row it touches nor
	// disappears from the ones after its first.
	rowWeights := make([]int, rowCount)
	for r := range rowWeights {
		rowWeights[r] = 1
	}
	for _, pl := range placements {
		if pl.col < 0 {
			continue
		}
		share := max(max(pl.pane.HeightWeight(), 1)/pl.rowSpan, 1)
		for rr := pl.row; rr < pl.row+pl.rowSpan; rr++ {
			rowWeights[rr] = max(rowWeights[rr], share)
		}
	}
	totalWeight := 0
	for _, w := range rowWeights {
		totalWeight += w
	}
	rowHeights := make([]int, rowCount)
	used := 0
	for ri, w := range rowWeights {
		rowHeights[ri] = max(gridBodyH*w/max(totalWeight, 1), 1)
		used += rowHeights[ri]
	}
	if used < gridBodyH && len(rowHeights) > 0 {
		rowHeights[len(rowHeights)-1] += gridBodyH - used
	}

	// Hand the height nobody can use to the rows that can.
	capRowHeights(rowHeights, rowWeights, placements, colW, stackUnders)

	// Emit tiles. Fixed-height panes claim the topmost rows.
	var tiles []Tile
	visible := map[string]struct{}{}
	y := headerH
	for _, fr := range fixedRows {
		x := 0
		for ti, t := range fr.tiles {
			tw := t.width
			if tw <= 0 {
				tw = width
			}
			// In a packed row, the last tile absorbs the integer-division
			// remainder so rounding cannot leave a gap at the right edge.
			if ti == len(fr.tiles)-1 && len(fr.tiles) > 1 {
				tw = width - x
			}
			tiles = append(tiles, Tile{
				PaneID:  t.pane.ID(),
				X:       x,
				Y:       y,
				W:       tw,
				H:       fr.height,
				Focused: t.pane.ID() == o.FocusedID,
			})
			visible[t.pane.ID()] = struct{}{}
			x += tw
		}
		y += fr.height
	}
	// Row tops, so a tile spanning rows can be given their combined height.
	rowY := make([]int, rowCount+1)
	if rowCount > 0 {
		rowY[0] = y
		for r := range rowCount {
			rowY[r+1] = rowY[r] + rowHeights[r]
		}
	}

	// Emit in the same (row, column) order the growth pass used.
	for _, i := range order2D {
		pl := placements[i]
		tiles = append(tiles, Tile{
			PaneID:  pl.pane.ID(),
			X:       colX[pl.row][pl.col],
			Y:       rowY[pl.row],
			W:       tileWidth(pl, colW),
			H:       rowY[pl.row+pl.rowSpan] - rowY[pl.row],
			Focused: pl.pane.ID() == o.FocusedID,
		})
		visible[pl.pane.ID()] = struct{}{}
	}

	// Splice stacked panes onto their host's tile: the host shrinks by the
	// partner's share and the partner renders with its own chrome in the same
	// column.
	for hostID, partners := range stackUnders {
		hostIdx := -1
		for i := range tiles {
			if tiles[i].PaneID == hostID && len(tiles[i].Stacked) == 0 {
				hostIdx = i
				break
			}
		}
		if hostIdx < 0 {
			continue // no host tile; partners fall through to Hidden
		}
		host := &tiles[hostIdx]
		sub := []SubTile{{PaneID: host.PaneID, Focused: host.Focused}}
		usedRatio := 0.0
		for _, p := range partners {
			r := 1.0 / 3.0
			if sup, ok := p.(tui.StackedUnderPane); ok {
				if v := sup.StackRatio(); v > 0 && v < 1 {
					r = v
				}
			}
			if usedRatio+r > 0.9 {
				// Cap so the host always keeps at least 10%.
				r = 0.9 - usedRatio
				if r <= 0 {
					break
				}
			}
			usedRatio += r
			sub = append(sub, SubTile{PaneID: p.ID(), HRatio: r, Focused: p.ID() == o.FocusedID})
			visible[p.ID()] = struct{}{}
		}
		sub[0].HRatio = 1 - usedRatio
		host.PaneID = ""
		host.Focused = false
		host.Stacked = sub
	}

	var hidden []string
	for _, id := range order {
		if _, ok := visible[id]; !ok {
			hidden = append(hidden, id)
		}
	}

	return Layout{
		Breakpoint: bp,
		HeaderW:    width,
		HeaderH:    headerH,
		Tiles:      tiles,
		Hidden:     hidden,
		Order:      order,
	}
}

// appetiteOf is the tile width a pane says its content can use, chrome included,
// or what the caller says on its behalf via [Options.Appetite]. Zero means neither
// said — see [tui.ContentWidthPane].
func appetiteOf(p tui.Pane, override map[string]int) int {
	if w, ok := override[p.ID()]; ok && w > 0 {
		return w + tui.PaneChromeH
	}
	cw, ok := p.(tui.ContentWidthPane)
	if !ok {
		return 0
	}
	if w := cw.ContentWidth(); w > 0 {
		return w + tui.PaneChromeH
	}
	return 0
}

// bandOf groups rows into bands: maximal runs of rows joined by a tile spanning
// them. The returned slice maps a row to its band.
//
// Widths are decided per band rather than once for the whole grid, because the
// rows want different things and only one of them can be right. Machines & Hosts
// beside Nodes is a row of tables that want 128 and 92 columns; the row beneath it
// holds subsystem frames that want 150 and 57. Sizing both from one set of column
// boundaries starves whichever row did not choose them — and choosing them evenly,
// as this used to, starves both.
//
// Per row is not available either: a tile spanning rows has one width, so the rows
// it covers must agree on the boundaries it sits between. A band is exactly the set
// of rows that have to agree, and no more of them than that.
func bandOf(placements []placement, rowCount int) []int {
	band := make([]int, rowCount)
	id := 0
	for r := 1; r < rowCount; r++ {
		joined := false
		for _, pl := range placements {
			if pl.col >= 0 && pl.row < r && pl.row+pl.rowSpan > r {
				joined = true
				break
			}
		}
		if !joined {
			id++
		}
		band[r] = id
	}
	return band
}

// bandWidths returns each row's column offsets and column widths, sized band by
// band. Rows in the same band share one vector, which is what keeps a
// row-spanning tile a single rectangle.
func bandWidths(placements []placement, band []int, rowCount, cols, width int,
	appetiteOverride map[string]int) (colX, colW [][]int) {
	colX, colW = make([][]int, rowCount), make([][]int, rowCount)
	if rowCount == 0 || cols <= 0 {
		return colX, colW
	}

	for b := 0; b <= band[rowCount-1]; b++ {
		appetite, floor := make([]int, cols), make([]int, cols)
		covered := make([]bool, cols)
		for _, pl := range placements {
			if pl.col < 0 || band[pl.row] != b {
				continue
			}
			// A tile spanning columns spreads its appetite across them, so a wide
			// pane's want is compared against the others per column rather than
			// counted whole in each.
			span := max(pl.colEnd-pl.col, 1)
			want := ceilDiv(appetiteOf(pl.pane, appetiteOverride), span)
			least := ceilDiv(pl.pane.MinWidth(), span)
			for c := pl.col; c < pl.colEnd && c < cols; c++ {
				covered[c] = true
				appetite[c] = max(appetite[c], want)
				floor[c] = max(floor[c], least)
			}
		}

		w := divideWidth(width, appetite, floor, covered)
		x, run := make([]int, cols), 0
		for c := range cols {
			x[c] = run
			run += w[c]
		}
		for r := range rowCount {
			if band[r] == b {
				colX[r], colW[r] = x, w
			}
		}
	}
	return colX, colW
}

// divideWidth splits a band's cells between its columns in proportion to what the
// panes in them can use.
//
// A column whose panes did not declare an appetite is given the band's average.
// That is what makes this degrade to the even division it replaced: a band where
// nobody declares anything has one appetite repeated, and proportional shares of
// equal appetites are equal shares.
//
// Slack is spread proportionally rather than capped at each column's appetite. A
// table is not harmed by a wider tile — it centers in the padding it already
// computes — and the alternative is a seam of dead screen between frames.
func divideWidth(total int, appetite, floor []int, covered []bool) []int {
	w := make([]int, len(appetite))

	// The average is taken over declared columns only, so an undeclared column
	// cannot drag down the mean it is about to be measured against.
	sum, declared := 0, 0
	for c := range appetite {
		if covered[c] && appetite[c] > 0 {
			sum += appetite[c]
			declared++
		}
	}
	fallback := 1
	if declared > 0 {
		fallback = max(sum/declared, 1)
	}

	wants, totalWant := make([]int, len(appetite)), 0
	for c := range appetite {
		switch {
		case !covered[c]:
			// A column no tile reaches is given nothing rather than a share to
			// waste; the tiles beside it close over the gap.
			continue
		case appetite[c] > 0:
			wants[c] = appetite[c]
		default:
			wants[c] = fallback
		}
		totalWant += wants[c]
	}
	if totalWant <= 0 {
		return w
	}

	used, last := 0, -1
	for c := range wants {
		if wants[c] <= 0 {
			continue
		}
		w[c] = total * wants[c] / totalWant
		used += w[c]
		last = c
	}
	// The rightmost occupied column absorbs the rounding remainder, so the band
	// reaches the right edge exactly.
	if last >= 0 && used < total {
		w[last] += total - used
	}
	liftToFloors(w, floor, covered)
	return w
}

// liftToFloors moves cells from the columns with the most to spare into any column
// below the narrowest width its panes can render in.
//
// Best effort by construction: when a band cannot fit even the floors there is
// nothing left to take, and the columns stay short — which the panes answer by
// dropping columns of their own. Honoring a floor by overrunning the terminal is
// not an improvement on that.
func liftToFloors(w, floor []int, covered []bool) {
	for {
		need, from := -1, -1
		for c := range w {
			if covered[c] && w[c] < floor[c] && (need < 0 || floor[c]-w[c] > floor[need]-w[need]) {
				need = c
			}
		}
		if need < 0 {
			return
		}
		for c := range w {
			if w[c]-floor[c] > 0 && (from < 0 || w[c]-floor[c] > w[from]-floor[from]) {
				from = c
			}
		}
		if from < 0 {
			return
		}
		w[from]--
		w[need]++
	}
}

// capRowHeights trims rows whose panes have all said how much height they can use
// and gives what they gave up to the rows that never say.
//
// The weights decide proportions; this decides whether a proportion is worth
// honoring. Measured on a 104-row terminal: the bottom row held 30 lines to show 13
// lines of Cilium, MetalLB and OVN state, while Machines & Hosts two rows above it
// hid three machines behind a "+ 3 more". No weight fixes that, because the right
// proportion depends on how much each subsystem currently has to say — which the
// panes know and the grid does not.
//
// A row is trimmed only when every tile covering it declares a ceiling: one pane
// that can still use height is reason enough to leave the row alone. And nothing is
// trimmed when no row would take the surplus, since shortening the grid to leave the
// bottom of the terminal blank helps nobody.
func capRowHeights(rowHeights, rowWeights []int, placements []placement,
	colW [][]int, stackUnders map[string][]tui.Pane) {
	ceiling := make([]int, len(rowHeights))
	capped := make([]bool, len(rowHeights))
	for r := range capped {
		capped[r] = true
	}

	for _, pl := range placements {
		if pl.col < 0 {
			continue
		}
		h := 0
		// A tile about to be shared with a stacked partner does not render at the
		// size measured here, so its ceiling would describe the wrong rectangle.
		if _, shared := stackUnders[pl.pane.ID()]; !shared {
			if ch, ok := pl.pane.(tui.ContentHeightPane); ok {
				bodyW := max(tileWidth(pl, colW)-tui.PaneChromeH, 1)
				if v := ch.ContentHeight(bodyW); v > 0 {
					// A tile spanning rows spends its ceiling across them, the same
					// way its weight is shared out.
					//
					// The ceiling covers the frame and [tui.PaneVPad] as well as the
					// content, because a tile sized to exactly what its pane said it
					// could fill leaves the renderer no slack to take the gutter
					// from: its first and last rows come out against the border
					// while every untrimmed pane on the screen breathes.
					h = ceilDiv(v+tui.PaneChromeV+tui.PaneVPad, pl.rowSpan)
				}
			}
		}
		for rr := pl.row; rr < pl.row+pl.rowSpan && rr < len(rowHeights); rr++ {
			if h <= 0 {
				capped[rr] = false
				continue
			}
			ceiling[rr] = max(ceiling[rr], h)
		}
	}

	surplus, freeWeight := 0, 0
	for r := range rowHeights {
		if !capped[r] {
			freeWeight += max(rowWeights[r], 1)
			continue
		}
		if ceiling[r] > 0 && rowHeights[r] > ceiling[r] {
			surplus += rowHeights[r] - ceiling[r]
		}
	}
	if surplus <= 0 || freeWeight <= 0 {
		return
	}

	for r := range rowHeights {
		if capped[r] && ceiling[r] > 0 && rowHeights[r] > ceiling[r] {
			rowHeights[r] = ceiling[r]
		}
	}
	given, last := 0, -1
	for r := range rowHeights {
		if capped[r] {
			continue
		}
		add := surplus * max(rowWeights[r], 1) / freeWeight
		rowHeights[r] += add
		given += add
		last = r
	}
	if last >= 0 && given < surplus {
		rowHeights[last] += surplus - given
	}
}

// tileWidth sums the widths of the columns a tile occupies.
func tileWidth(pl placement, colW [][]int) int {
	if pl.row < 0 || pl.row >= len(colW) {
		return 0
	}
	total := 0
	for c := pl.col; c < pl.colEnd && c < len(colW[pl.row]); c++ {
		total += colW[pl.row][c]
	}
	return total
}

func ceilDiv(n, d int) int {
	if n <= 0 || d <= 0 {
		return 0
	}
	return (n + d - 1) / d
}

func containsID(ids []string, target string) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}
