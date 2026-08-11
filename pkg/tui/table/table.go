// Package table renders fixed-column tables and empty states inside a pane
// body, clipped to a bounding box.
//
// The width negotiation is the interesting part. Columns take their natural
// content width; one column may be marked [Column.Stretch] to absorb slack.
// When the content is wider than the space available, the stretch column gives
// up width first and the widest remaining column shrinks after that. When
// there is slack, the stretch column grows but only up to [MaxStretchPad] —
// past that the leftover becomes edge padding, so a table in a very wide pane
// stays readable instead of spreading one column across the screen.
//
// Height is negotiated too, but only in one direction: rows that do not fit are
// cut and counted. The exception is a pane wide enough to hold the table twice
// over, where the rows are dealt into side-by-side groups instead of being
// discarded — see [FlowGroups].
//
// Panes that draw several tables and want them visually aligned should compute
// a single left pad with [PaneLeftPad] over each table's columns, then call
// [LayoutInner] per table rather than [Layout]. [Table.Render] does the
// single-table case for you.
package table

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/runlevel-six/sextant/pkg/tui"
)

// Layout tuning constants.
const (
	// MinGap is the minimum number of spaces between adjacent columns.
	MinGap = 2

	// MinStretchWidth is the floor guaranteed to a stretch column before
	// other columns are squeezed to make room for it.
	MinStretchWidth = 12

	// MaxStretchPad caps how much slack a stretch column absorbs beyond its
	// natural content width. Past this, slack flows to edge padding instead,
	// so a wide pane does not bloat one column into a field of whitespace.
	MaxStretchPad = 16

	// EdgePadCap caps the padding on each side. Beyond it, slack is left as
	// trailing space on the right, keeping the table left-anchored rather
	// than drifting toward the middle of a very wide pane.
	EdgePadCap = 4

	// FlowGutter is the blank channel between two side-by-side row groups. Wider
	// than [MinGap], because the boundary between two groups has to read more
	// strongly than the boundary between two columns of one.
	FlowGutter = 4

	// FixedPaneVPad is the number of body lines a fixed-height pane reserves
	// for vertical padding, one above its content and one below. Panes in the
	// ordinary grid get the same effect for free whenever the layout hands
	// them more height than they need.
	FixedPaneVPad = 2

	// minColWidth is the narrowest a column may be squeezed to. Below about
	// four cells a column shows nothing but truncation.
	minColWidth = 4
)

// Column describes one table column.
//
// A Width of zero or less auto-sizes the column to its widest cell. At most one
// column should set Stretch; if several do, the leftmost wins.
type Column struct {
	// Header is the column's title, also its minimum auto-sized width.
	Header string
	// Width pins the column to an exact width. Zero or less auto-sizes.
	Width int
	// Stretch marks this column as the one that absorbs horizontal slack.
	Stretch bool
	// Transient marks a column whose content is written by the situation rather
	// than by the schema: a Metal3 error, the list of nodes still behind, a
	// Kubernetes message. It is excluded from [AppetiteWidth] — see there — and
	// has no effect on rendering.
	Transient bool
	// Style is an optional default style for the column's cells. It is
	// overridden by a row style, which is in turn overridden by a cell style.
	Style lipgloss.Style
}

// Table is a set of columns and rows ready to render.
//
// Styling resolves most-specific-first: CellStyles, then RowStyles, then
// [Column.Style]. Header cells always use [tui.StyleHeader].
type Table struct {
	// Cols defines the columns. Required.
	Cols []Column
	// Rows holds the cell text. A row shorter than Cols renders blanks for
	// the missing trailing cells.
	Rows [][]string
	// RowStyles, when non-nil, must be the same length as Rows. A zero style
	// entry means "fall through to the column default".
	RowStyles []lipgloss.Style
	// CellStyles, when non-nil, overrides per cell. Inner slices may be
	// shorter than Cols; missing entries fall through to row then column.
	CellStyles [][]lipgloss.Style
}

// Render draws the table clipped to width x height, including a header row.
//
// Rows that do not fit are replaced by a trailing "+ N more" line, so a
// truncated table always says so rather than silently ending early. When the
// rows overflow the height and the pane is wide enough to hold the table more
// than once, they are flowed into side-by-side groups instead — see [FlowGroups].
// Returns the empty string when either dimension is non-positive.
func (t Table) Render(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}

	groups := FlowGroups(t.Cols, t.Rows, width, height)
	if groups > 1 {
		return t.renderFlowed(width, height, groups)
	}
	return t.renderGroup(t.Rows, 0, t.layoutFor(width), width, height, 0)
}

// rowLayout is one table's settled horizontal geometry: per-column widths, the
// gaps between them, and the indent every line carries.
type rowLayout struct {
	widths, gaps []int
	leftPad      int
}

// layoutFor sizes the columns from the whole table's rows at the given width, so
// that a group drawing a slice of the rows still places its columns where the
// other groups place theirs.
func (t Table) layoutFor(width int) rowLayout {
	widths, gaps, leftPad := Layout(t.Cols, t.Rows, width)
	return rowLayout{widths: widths, gaps: gaps, leftPad: leftPad}
}

// renderGroup draws rows into one column group of the given size.
//
// at is the index the first row has in the whole table, so a flowed group's
// styles line up with the rows it is actually drawing. extra is the number of rows
// no group will reach at all, which only the last group can know and only it
// reports: a "+ N more" that counted just this group's leftovers would understate
// what the reader is missing.
func (t Table) renderGroup(rows [][]string, at int, lay rowLayout, width, height, extra int) string {
	if width <= 0 || height <= 0 {
		return ""
	}

	widths, gaps := lay.widths, lay.gaps
	var sb strings.Builder
	pad := strings.Repeat(" ", lay.leftPad)

	sb.WriteString(pad)
	sb.WriteString(RenderRow(t.Cols, widths, gaps, Headers(t.Cols), tui.StyleHeader, true))
	sb.WriteByte('\n')

	dataLines := height - 1 // the header consumed one
	if dataLines < 1 {
		return clip(sb.String(), width, height)
	}

	maxRows, hidden := dataLines, 0
	if len(rows) > maxRows || extra > 0 {
		maxRows = max(dataLines-1, 0) // the footer costs a line of its own
		hidden = extra + max(len(rows)-maxRows, 0)
	}

	for i := 0; i < maxRows && i < len(rows); i++ {
		var rs lipgloss.Style
		if t.RowStyles != nil && at+i < len(t.RowStyles) {
			rs = t.RowStyles[at+i]
		}
		var cs []lipgloss.Style
		if t.CellStyles != nil && at+i < len(t.CellStyles) {
			cs = t.CellStyles[at+i]
		}
		sb.WriteString(pad)
		sb.WriteString(RenderRowCells(t.Cols, widths, gaps, rows[i], rs, cs, false))
		sb.WriteByte('\n')
	}
	if hidden > 0 {
		sb.WriteString(pad)
		sb.WriteString(tui.StyleMuted.Render(fmt.Sprintf("+ %d more", hidden)))
		sb.WriteByte('\n')
	}

	// Clip as a hard guarantee. Column shrinking has a floor of minColWidth, so
	// enough columns in a narrow enough pane cannot be made to fit however much
	// they give up — at which point a row must be cut rather than allowed to
	// overrun. Callers size their layout from this promise, so it has to hold for
	// every input rather than only reasonable ones.
	return clip(strings.TrimRight(sb.String(), "\n"), width, height)
}

// renderFlowed lays the rows out in groups side by side, filling each group top to
// bottom before starting the next.
//
// Column-major rather than row-major because the rows arrive in an order that
// means something — machines in flight are floated to the top of the fleet — and
// dealing them across the groups like cards would scatter the first ten rows over
// the whole pane. Filling downwards keeps "the top of the list" in the top left,
// where it was when the table had one column.
func (t Table) renderFlowed(width, height, groups int) string {
	groupW := (width - FlowGutter*(groups-1)) / groups
	perGroup := max(height-1, 1) // every group repeats the header

	// One geometry, sized to the narrowest group, shared by all of them. Letting
	// each group size its own columns would let the last one — which absorbs the
	// rounding remainder and so is a cell or two wider — grow its stretch column
	// past its neighbors', and the cells that should read as one table would no
	// longer line up.
	lay := t.layoutFor(groupW)

	// Rows past the last group are nobody's to draw, so the last group reports
	// them.
	beyond := max(len(t.Rows)-groups*perGroup, 0)

	blocks := make([][]string, 0, groups)
	widest := make([]int, 0, groups)
	for g := range groups {
		at := g * perGroup
		if at >= len(t.Rows) {
			break
		}
		end := min(at+perGroup, len(t.Rows))
		extra, w := 0, groupW
		if g == groups-1 {
			extra = beyond
			// The final group absorbs the rounding remainder, so the flowed table
			// occupies exactly the width it was given.
			w = width - g*(groupW+FlowGutter)
		}
		blocks = append(blocks, strings.Split(t.renderGroup(t.Rows[at:end], at, lay, w, height, extra), "\n"))
		widest = append(widest, w)
	}
	if len(blocks) == 0 {
		return ""
	}

	lines := make([]string, height)
	for y := range height {
		var sb strings.Builder
		for g, b := range blocks {
			if g > 0 {
				sb.WriteString(strings.Repeat(" ", FlowGutter))
			}
			var ln string
			if y < len(b) {
				ln = b[y]
			}
			// Pad to the group's own width: a short line in one group must not pull
			// the next group's columns leftwards.
			sb.WriteString(PadOrTrunc(ln, widest[g]))
		}
		lines[y] = sb.String()
	}
	return clip(strings.Join(lines, "\n"), width, height)
}

// FlowGroups reports how many side-by-side groups the rows should be dealt into at
// this size. One means an ordinary single-column table.
//
// Two conditions, and both are necessary. The rows have to overflow the height,
// because splitting a table that already fits buys nothing and costs the reader a
// second place to look. And the pane has to be wide enough for another whole
// table — [NaturalRowWidth] plus a gutter — because a group narrower than the
// content shrinks columns, which is how a wide pane full of truncated cells gets
// made in the first place.
//
// This is what a zoomed pane spends its width on. A pane at natural width has
// nothing left to reveal horizontally: every cell is already whole, and the
// remaining space can only buy rows.
func FlowGroups(cols []Column, rows [][]string, width, height int) int {
	natural := NaturalRowWidth(cols, rows)
	if natural <= 0 || height < 2 {
		return 1
	}
	perGroup := height - 1 // the header costs each group a line
	if len(rows) <= perGroup {
		return 1
	}

	byWidth := (width + FlowGutter) / (natural + FlowGutter)
	byRows := ceilDiv(len(rows), perGroup)
	return max(min(byWidth, byRows), 1)
}

func ceilDiv(n, d int) int {
	if n <= 0 || d <= 0 {
		return 0
	}
	return (n + d - 1) / d
}

// clip trims a rendered block to exactly width x height.
//
// Width trimming goes through ansi.Truncate rather than cutting runes. A naive
// cut can land inside an escape sequence, and the surviving fragment then counts
// as printable text — which makes the line *wider* than the limit it was being
// trimmed to. That failure only appears once cells are styled, so it is easy to
// introduce and hard to spot.
func clip(s string, width, height int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for i, ln := range lines {
		if lipgloss.Width(ln) > width {
			lines[i] = ansi.Truncate(ln, width, "")
		}
	}
	return strings.Join(lines, "\n")
}

// Headers returns each column's header text.
func Headers(cols []Column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Header
	}
	return out
}

// Layout sizes the columns of a single-table pane, choosing the surrounding
// edge padding in the same step.
//
// widths is per column, gaps is per inter-column gap (length len(cols)-1), and
// leftPad is the indent to apply to every line. A pane drawing several tables
// that must align should use [PaneLeftPad] and [LayoutInner] instead.
func Layout(cols []Column, rows [][]string, totalWidth int) (widths, gaps []int, leftPad int) {
	leftPad = PaneLeftPad(totalWidth, cols)
	widths, gaps = LayoutInner(cols, rows, totalWidth-leftPad)
	return widths, gaps, leftPad
}

// LayoutInner returns per-column widths and per-gap widths sized to fit
// innerWidth exactly, assuming the caller has already reserved any edge pad.
//
// A stretch column whose cells are all blank is demoted to non-stretch: letting
// it absorb slack would only produce a wide invisible right margin.
func LayoutInner(cols []Column, rows [][]string, innerWidth int) (widths, gaps []int) {
	widths = make([]int, len(cols))
	stretchIdx := -1

	for i, c := range cols {
		if c.Stretch && stretchIdx < 0 {
			stretchIdx = i
		}
		if c.Width > 0 {
			widths[i] = c.Width
			continue
		}
		widths[i] = lipgloss.Width(c.Header)
		for _, row := range rows {
			if i < len(row) && lipgloss.Width(row[i]) > widths[i] {
				widths[i] = lipgloss.Width(row[i])
			}
		}
	}

	if stretchIdx >= 0 && stretchColEmpty(rows, stretchIdx) {
		stretchIdx = -1
	}
	naturalStretch := 0
	if stretchIdx >= 0 {
		naturalStretch = widths[stretchIdx]
	}

	gaps = make([]int, max0(len(cols)-1))
	for i := range gaps {
		gaps[i] = MinGap
	}

	used := sum(widths) + sum(gaps)
	switch {
	case used == innerWidth:
		return widths, gaps

	case used > innerWidth:
		// Overflow: the stretch column gives up width first, down to its
		// header, then the widest remaining column shrinks.
		deficit := used - innerWidth
		if stretchIdx >= 0 {
			floor := max(lipgloss.Width(cols[stretchIdx].Header), 1)
			if widths[stretchIdx] > floor {
				give := min(widths[stretchIdx]-floor, deficit)
				widths[stretchIdx] -= give
				deficit -= give
			}
		}
		if deficit > 0 {
			shrink(widths, deficit)
		}
		return widths, gaps

	default:
		// Underflow: feed the stretch column up to its cap. Whatever is left
		// stays as trailing whitespace — the last column has no trailing pad.
		slack := innerWidth - used
		if stretchIdx >= 0 {
			floor := max(MinStretchWidth, lipgloss.Width(cols[stretchIdx].Header))
			ceil := max(naturalStretch+MaxStretchPad, floor)
			if widths[stretchIdx] < ceil {
				widths[stretchIdx] += min(ceil-widths[stretchIdx], slack)
			}
		}
		return widths, gaps
	}
}

// NaturalRowWidth returns the unpadded width one row would occupy: each
// column's natural (or pinned) width plus [MinGap] between neighbors.
//
// Use it to size a pane-wide left pad before committing to a layout.
func NaturalRowWidth(cols []Column, rows [][]string) int {
	total := sum(naturalColWidths(cols, rows))
	if len(cols) > 1 {
		total += MinGap * (len(cols) - 1)
	}
	return total
}

// naturalColWidths is each column's pinned width, or the width of its widest cell.
func naturalColWidths(cols []Column, rows [][]string) []int {
	out := make([]int, len(cols))
	for i, c := range cols {
		if c.Width > 0 {
			out[i] = c.Width
			continue
		}
		out[i] = lipgloss.Width(c.Header)
		for _, row := range rows {
			if i < len(row) && lipgloss.Width(row[i]) > out[i] {
				out[i] = lipgloss.Width(row[i])
			}
		}
	}
	return out
}

// StretchAppetiteCap is the most a stretch column contributes to
// [AppetiteWidth], however long its content is.
//
// A stretch column is the one a table gives up width on first, so its content is
// by declaration the content that tolerates truncation. Charging it in full would
// let one pane's appetite swallow its row. Sixty is about a readable clause, and a
// wider tile still spends its slack on the column, so nothing is lost by asking
// for less of it.
const StretchAppetiteCap = 60

// AppetiteWidth is the width at which this table shows everything a reader
// identifies a row by.
//
// Each column's natural width, except that a [Column.Transient] one is charged
// only its header and no column is charged more than [StretchAppetiteCap]. This is
// the number a pane reports to [tui.ContentWidthPane], as distinct from
// [NaturalRowWidth], which is what the table would occupy if nothing were ever
// truncated.
//
// Excluding transient content is what makes a layout sized from this hold still.
// Measured on a rolling fleet: charging Machines & Hosts for its HOST STATE cell
// moved the pane's appetite by 37 cells the moment a host reported "deprovisioning
// powered off" or a Metal3 error, so every tile boundary in that band slid sideways
// on a twenty-second timer while the reader was trying to read a row. What a row is
// *identified* by — its machine, its host, its node — has a length that changes when
// the fleet changes and not otherwise, which is the only thing worth resizing a
// screen for.
//
// It is the pane, not this function, that knows which of its columns is which: a
// stretch column holds a disposable message in Machines & Hosts and the node's FQDN
// in Nodes, and only one of those is worth width.
func AppetiteWidth(cols []Column, rows [][]string) int {
	widths := naturalColWidths(cols, rows)
	for i, c := range cols {
		header := max(lipgloss.Width(c.Header), 1)
		if c.Transient {
			widths[i] = header
			continue
		}
		widths[i] = max(min(widths[i], StretchAppetiteCap), header)
	}
	// The edge pad is width the table will hold back from itself at any
	// comfortable size, so a pane given exactly its appetite and no allowance for
	// the pad would truncate by that much — see [PaneLeftPad].
	return sum(widths) + MinGap*max(len(cols)-1, 0) + EdgePadCap
}

// PaneLeftPad returns a left pad shared by every table in one pane, so tables
// with different column schemas still line up at their left edge.
//
// totalWidth is the pane's body width and colSets are the schemas to align. Room
// is reserved for a stretch column to grow into, and the remainder is split
// between the two edges, capped at [EdgePadCap].
//
// It is decided from the *headers*, not from the rows, and that is the whole
// point. This pad used to be measured against [NaturalRowWidth], which tracks the
// longest cell exactly: one crash-looping pod with a long name took the pods table
// past the threshold and slid every row of the pane four cells left, and back again
// when it recovered — inside a tile whose borders never moved, which is the kind of
// movement no layout test sees and every reader does. A cosmetic margin must not be
// computed from data that changes every poll, so this one is computed from the
// schema, which changes never.
//
// The pad is therefore held even when the rows would rather have those cells. That
// is deliberate: the table already knows how to give up width, and it degrades a
// stretch column by four cells rather than moving the whole pane.
func PaneLeftPad(totalWidth int, colSets ...[]Column) int {
	widest := 0
	for _, cols := range colSets {
		if w := HeaderRowWidth(cols); w > widest {
			widest = w
		}
	}
	slack := totalWidth - widest - MaxStretchPad
	if slack <= 0 {
		return 0
	}
	return min(slack/2, EdgePadCap)
}

// HeaderRowWidth is the width of a table's header row: every column at its header's
// own width, with the minimum gaps between them.
//
// The narrowest a schema can be drawn at without truncating a header, and the
// stable half of [NaturalRowWidth] — same shape, no dependence on the rows.
func HeaderRowWidth(cols []Column) int {
	total := 0
	for _, c := range cols {
		total += max(lipgloss.Width(c.Header), 1)
	}
	return total + MinGap*max(len(cols)-1, 0)
}

// IndentLines prefixes every non-empty line of body with leftPad spaces.
//
// Blank lines are left bare so that separator lines do not acquire trailing
// whitespace, which a renderer might otherwise treat as content.
func IndentLines(body string, leftPad int) string {
	if leftPad <= 0 || body == "" {
		return body
	}
	pad := strings.Repeat(" ", leftPad)
	lines := strings.Split(body, "\n")
	for i, ln := range lines {
		if ln != "" {
			lines[i] = pad + ln
		}
	}
	return strings.Join(lines, "\n")
}

// RenderRow joins cells using the widths and gaps from [Layout] or
// [LayoutInner]. gaps must have length len(cols)-1; the final column gets no
// trailing pad.
func RenderRow(cols []Column, widths, gaps []int, cells []string, rowStyle lipgloss.Style, isHeader bool) string {
	return RenderRowCells(cols, widths, gaps, cells, rowStyle, nil, isHeader)
}

// RenderRowCells is [RenderRow] with per-cell style overrides. cellStyles may
// be shorter than cols; missing entries fall through to rowStyle and then the
// column default. Header rows always use [tui.StyleHeader].
func RenderRowCells(cols []Column, widths, gaps []int, cells []string, rowStyle lipgloss.Style, cellStyles []lipgloss.Style, isHeader bool) string {
	var sb strings.Builder
	for i := range cols {
		var cell string
		if i < len(cells) {
			cell = cells[i]
		}
		if i < len(widths) {
			cell = PadOrTrunc(cell, widths[i])
		}

		style := resolveStyle(cols[i], rowStyle, cellStyles, i, isHeader)
		if tui.HasStyle(style) {
			sb.WriteString(style.Render(cell))
		} else {
			sb.WriteString(cell)
		}

		if i < len(cols)-1 && i < len(gaps) {
			sb.WriteString(strings.Repeat(" ", gaps[i]))
		}
	}
	return sb.String()
}

// PadOrTrunc pads s with spaces or truncates it to exactly width display
// cells. Truncation is by rune and is ANSI-naive, so pass unstyled cell text
// and apply styling afterwards.
func PadOrTrunc(s string, width int) string {
	if width <= 0 {
		return ""
	}
	w := lipgloss.Width(s)
	switch {
	case w == width:
		return s
	case w > width:
		runes := []rune(s)
		for len(runes) > 0 && lipgloss.Width(string(runes)) > width {
			runes = runes[:len(runes)-1]
		}
		return string(runes)
	default:
		return s + strings.Repeat(" ", width-w)
	}
}

// ClipLines truncates s to at most maxLines lines.
func ClipLines(s string, maxLines int) string {
	if maxLines <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	return strings.Join(lines, "\n")
}

// Placeholder centers msg in a block of exactly width x height, for "no data
// yet" and similar empty states.
func Placeholder(width, height int, msg string) string {
	if height <= 0 {
		return ""
	}
	lines := make([]string, 0, height)
	for range (height - 1) / 2 {
		lines = append(lines, "")
	}
	// Truncate before centring: PlaceHorizontal pads a short string but leaves a
	// long one intact, so a message wider than the pane would overflow.
	if lipgloss.Width(msg) > width {
		msg = ansi.Truncate(msg, width, "")
	}
	lines = append(lines, lipgloss.PlaceHorizontal(width, lipgloss.Center, tui.StyleMuted.Render(msg)))
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// ErrorBody surfaces a failure in a clearly visible band at the top of a pane
// body sized width x height.
func ErrorBody(width, height int, err error) string {
	if height <= 0 {
		return ""
	}
	lines := []string{PadOrTrunc(tui.StyleErr.Render("error: ")+err.Error(), width)}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// FmtCount formats a ready/total pair like "3/3", green when they match and
// red when they do not.
func FmtCount(ready, total int32) string {
	s := fmt.Sprintf("%d/%d", ready, total)
	if ready != total {
		return tui.StyleErr.Render(s)
	}
	return tui.StyleOK.Render(s)
}

// ShortAge formats a duration in seconds as a single token close to kubectl's
// AGE column: "3m", "2h", "12d". Sub-minute ages round up to "1m", since
// second-level precision is noise at a dashboard's refresh rate.
func ShortAge(seconds float64) string {
	switch {
	case seconds < 60:
		return "1m"
	case seconds < 3600:
		return fmt.Sprintf("%dm", int(seconds/60))
	case seconds < 86400:
		return fmt.Sprintf("%dh", int(seconds/3600))
	default:
		return fmt.Sprintf("%dd", int(seconds/86400))
	}
}

// resolveStyle picks the style for one cell, most specific first: an explicit
// cell style, then the row style, then the column default. A header cell always
// uses [tui.StyleHeader]. The zero style is returned when no candidate carries
// any attribute, so the caller can write the cell bare.
//
// "Carries any attribute" is [tui.HasStyle], not a nil check — a
// [lipgloss.Style] is a struct, so an unset one is a usable zero value rather
// than nil, and only inspecting its attributes distinguishes "no style given"
// from "styled".
func resolveStyle(col Column, rowStyle lipgloss.Style, cellStyles []lipgloss.Style, i int, isHeader bool) lipgloss.Style {
	switch {
	case isHeader:
		return tui.StyleHeader
	case i < len(cellStyles) && tui.HasStyle(cellStyles[i]):
		return cellStyles[i]
	case tui.HasStyle(rowStyle):
		return rowStyle
	case tui.HasStyle(col.Style):
		return col.Style
	}
	return lipgloss.Style{}
}

// stretchColEmpty reports whether every row's cell at idx is blank.
func stretchColEmpty(rows [][]string, idx int) bool {
	for _, r := range rows {
		if idx < len(r) && strings.TrimSpace(r[idx]) != "" {
			return false
		}
	}
	return true
}

func sum(xs []int) int {
	total := 0
	for _, x := range xs {
		total += x
	}
	return total
}

func max0(n int) int {
	return max(n, 0)
}

// shrink removes `by` cells from the widest columns, one at a time, stopping
// once every column has reached minColWidth.
func shrink(widths []int, by int) {
	for by > 0 {
		idx := 0
		for i := range widths {
			if widths[i] > widths[idx] {
				idx = i
			}
		}
		if widths[idx] <= minColWidth {
			return
		}
		widths[idx]--
		by--
	}
}
