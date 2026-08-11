// Package tui defines the contracts a dashboard widget implements, plus the
// shared color palette its body is drawn with.
//
// A [Pane] is a self-contained widget that pulls its data from a store on
// every Render call. Panes hold no state of their own: the program re-renders
// whenever the store reports an update, so a pane is a pure function of
// (snapshot, size, focus).
//
// Optional interfaces let a pane influence where the layout puts it —
// [FixedHeightPane], [FixedSizePane] and [StackedUnderPane] — and how much of
// the space it is given it can use: [ContentWidthPane] and [ContentHeightPane].
// A pane that implements none of them is placed in the ordinary grid and given
// an average share of it.
package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Chrome dimensions a pane's outer tile loses to the surrounding frame.
//
// PaneChromeH is 2 border cells plus 2 inner-padding cells; PaneChromeV is the
// top and bottom border lines. Layout and render code subtracts these to get
// the body area a pane draws into. A pane implementing [FixedHeightPane]
// measures at the body width and returns a body height — the layout adds
// PaneChromeV itself when sizing the row.
const (
	PaneChromeH = 4
	PaneChromeV = 2
)

// WidestLine measures the widest of a set of composed body lines, in display
// cells and ignoring trailing padding.
//
// The measurement a hand-composed pane needs for [ContentWidthPane], as
// [table.AppetiteWidth] is the one a table-driven pane needs. Styling is measured
// through lipgloss, so escape sequences do not count toward the width.
func WidestLine(lines []string) int {
	out := 0
	for _, ln := range lines {
		out = max(out, lipgloss.Width(strings.TrimRight(ln, " ")))
	}
	return out
}

// Priority controls which panes stay visible as the terminal narrows. Lower
// numeric values are higher priority and are placed first.
type Priority int

const (
	// P0Critical panes are visible at every breakpoint, including an 80x24
	// minimum — the things you cannot operate without seeing.
	P0Critical Priority = iota
	// P1Important panes appear from the Medium breakpoint up.
	P1Important
	// P2Useful panes appear from Wide up.
	P2Useful
	// P3Optional panes appear only on ultra-wide terminals. Below that they
	// remain reachable via focus cycling and jump keys.
	P3Optional
)

// String returns the priority's name, for diagnostics.
func (p Priority) String() string {
	switch p {
	case P0Critical:
		return "P0Critical"
	case P1Important:
		return "P1Important"
	case P2Useful:
		return "P2Useful"
	case P3Optional:
		return "P3Optional"
	}
	return "P?"
}

// Pane is the contract every widget implements.
type Pane interface {
	// ID is a stable, lowercase identifier used for focus tracking and
	// jump-key wiring. It must be unique within a registry.
	ID() string

	// Title is shown in the pane's border. Keep it short — the renderer
	// appends the jump-key digit to it.
	Title() string

	// Priority drives placement when not every pane can fit.
	Priority() Priority

	// MinWidth is the narrowest tile width at which this pane still renders
	// something useful. The layout will not place it in a narrower tile.
	MinWidth() int

	// MinHeight is the shortest tile height at which this pane still renders
	// something useful.
	MinHeight() int

	// HeightWeight is a relative pull on vertical space when the layout has
	// more than one row. Each row takes the maximum weight among its tiles,
	// and body height is divided proportionally. Use 1 for "I show a handful
	// of rows and no more", up to 5 for "I will fill any height given".
	// Values below 1 are treated as 1.
	HeightWeight() int

	// Render returns the pane's body content sized to exactly width x height:
	// no border and no title bar, which the program draws. The renderer clips
	// rather than wraps overflow, so a pane must respect the bounds it is
	// given. focused reports whether this pane currently owns keyboard focus.
	Render(width, height int, focused bool) string
}

// WidePane is implemented by panes that need more than one grid column.
//
// This exists because the dashboard has two kinds of pane and one grid. A status
// block — a CNI's version and agent count, a Raft term — says everything it has
// in twenty columns. A table of pods or events does not: at a quarter of a wide
// terminal, Pod Health renders three different `rook-ceph/rook-…` rows that a
// reader cannot tell apart, and Events truncates every object name to
// `KubeadmControlPlane/demo-contr`. Dividing the width equally gives the status
// blocks room they cannot use and starves the tables that need it.
//
// A pane declares how many columns it wants and the packer places it; the count
// is clamped to the columns the breakpoint actually has, so the same declaration
// degrades on its own from an ultrawide terminal down to a laptop. Declaring
// intent rather than a position is what keeps the layout right as plugins come
// and go — a pane that appears only where Argo CD is installed cannot be given a
// fixed slot.
type WidePane interface {
	Pane
	// ColSpan is how many grid columns this pane wants. Values below 1 are
	// treated as 1, and anything above the current column count is clamped to it.
	ColSpan() int
}

// SpanFloorPane is implemented by panes whose extra columns improve them rather
// than being required.
//
// The distinction matters because a wide grid and a narrow one want different
// answers. Pod Health needs two columns wherever it is placed — a pod name and a
// node FQDN do not fit in one, and denying it the width truncates data. A merged
// frame is the other case: it asks for two columns so it can flow its sections
// side by side, and if it does not get them it stacks them instead and loses
// nothing but elegance.
//
// Granting the second kind its request on a three-column grid is actively worse
// than refusing. It leaves a single column for the rest of the row, which pushes
// a pane onto a new grid row, and the new row's height comes out of every row
// above it — measured on a 240x63 terminal, that turned a five-row section into
// two rows and a "+ 3 more".
//
// This is [minColsForRowSpan]'s reasoning applied to columns, except declared by
// the pane rather than fixed by the grid, because only the pane knows whether its
// span is a need or a preference.
type SpanFloorPane interface {
	Pane
	// MinColsForSpan is the narrowest grid on which this pane's ColSpan is
	// honored. Below it the pane is placed in a single column.
	MinColsForSpan() int
}

// TallPane is implemented by panes that need more than one grid row.
//
// Width and height starve different panes. [WidePane] answers the pane whose
// *columns* do not fit — a pod name truncated to `rook-ceph/rook-c`. This answers
// the pane whose *rows* do not fit, which is the one that scales with the cluster:
// a 54-node fleet renders 25 machines and `+ 29 more` no matter how wide the
// column is, because the limit is the row count and nothing about a wider tile
// changes it.
//
// The count is clamped to the rows the layout actually has, so the same
// declaration degrades from a tall terminal down to a short one without the pane
// knowing. Zoom remains the answer for "show me every row of this one"; this is
// for the default view of a cluster too large to fit in a quarter of it.
type TallPane interface {
	Pane
	// RowSpan is how many grid rows this pane wants. Values below 1 are treated
	// as 1, and anything above the available rows is clamped.
	RowSpan() int
}

// ContentWidthPane is implemented by panes that can say how wide their content
// wants to be, so the grid can divide width by appetite instead of evenly.
//
// An even division is the wrong answer whenever the panes in a row are not
// equally hungry, which is always. Measured on a 395-column terminal: four
// quarters of 99 columns gave Machines & Hosts 33 fewer than its rows needed, so
// every MACHINE cell truncated to `demo-workers-` and the fleet became
// unreadable — while the Cloud frame beside it, whose content is 57 columns wide,
// sat in the same 99 and left 42 of them blank. Neither pane can fix that alone;
// only the layout sees both.
//
// This is the horizontal half of [ContentHeightPane], and the two say different
// things on purpose. Width is a want: a table asks for the space its widest cells
// need and reads correctly with more or with less. Height is a ceiling: a pane
// with eight lines of content cannot use a ninth.
//
// The return value is a body width, net of [PaneChromeH], as [Pane.Render]
// receives it; the layout adds the chrome back. Report what showing *everything*
// would take — the full column set rather than the reduced one a narrow tile
// would fall back to — since that is the number the layout needs in order to
// decide whether the fallback is necessary at all. Zero or less means "no
// preference", and such a pane is given an average share of its row.
type ContentWidthPane interface {
	Pane
	ContentWidth() int
}

// ContentHeightPane is implemented by grid panes whose content has a ceiling:
// a height past which the pane can only add blank lines.
//
// [Pane.HeightWeight] cannot express this. Weights are relative and
// content-blind, so they divide a terminal in fixed proportions however much
// each row actually has to say. Measured on a 104-row terminal: the bottom row
// took 30 lines to show 13 lines of Cilium, MetalLB and OVN state, while
// Machines & Hosts hid three machines behind a `+ 3 more` two rows above it. No
// weight fixes that, because the right proportion depends on data neither the
// weights nor the grid can see.
//
// Implement it where the content is defined by the subsystem — a Raft quorum has
// three members, a Cilium status has eight lines — and leave it off where the
// content scales with the cluster, which is every pane whose rows are machines,
// nodes, pods or events. Those are exactly the panes a trimmed row should hand
// its surplus to, and a pane claiming a ceiling it does not have would be
// donating space it needed.
//
// bodyWidth is the width the pane will be rendered at, since content that wraps
// or flows is taller in a narrower tile. The return value is a body height, net
// of [PaneChromeV]. Zero or less means "no ceiling", the same as not
// implementing this at all.
//
// Overstating the ceiling by a line is harmless — the pane keeps a blank line it
// could have given away. Understating it clips content, so a pane whose height is
// awkward to predict should round up.
type ContentHeightPane interface {
	Pane
	ContentHeight(bodyWidth int) int
}

// FixedHeightPane is implemented by panes that want a dedicated row above the
// grid rather than a grid cell, sized to exactly the height they ask for.
//
// The width passed to FixedHeight is the body width, already net of chrome,
// and the return value is the body height wanted at that width. The layout
// adds [PaneChromeV] when sizing the row, so the pane never inherits unused
// vertical space from a taller neighbor. Useful for short, content-defined
// panes where blank space above the content is wasted.
type FixedHeightPane interface {
	Pane
	FixedHeight(bodyWidth int) int
}

// FixedSizePane lets a fixed-height pane also constrain its width. The layout
// packs adjacent FixedSizePanes side by side on one row, sized to the tallest
// of their FixedHeight values. A pane that does not share its row sits alone
// at the left, and the rest of the row is left blank for a future sibling.
//
// Returning a width of zero or less falls back to [FixedHeightPane] behavior,
// i.e. a full-width dedicated row.
type FixedSizePane interface {
	FixedHeightPane
	FixedWidth(termWidth int) int
}

// StackedUnderPane is implemented by panes that should share a column with
// another pane, occupying the bottom slice of it. The host shrinks to the top
// portion; both panes keep their own border and title and stay independently
// focusable.
//
// This degrades to an ordinary grid cell when the host is not placed — for
// example in single-column focus mode, or when the host is itself hidden.
type StackedUnderPane interface {
	Pane
	// StackUnder names the host pane's ID.
	StackUnder() string
	// StackRatio is this pane's fraction of the shared column height.
	// Values are clamped to the open interval (0, 1); 0.33 gives this pane
	// the bottom third and leaves the host the top two thirds.
	StackRatio() float64
}
