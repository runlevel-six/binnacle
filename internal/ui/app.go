// Package ui wires the panes, the layout engine and the Bubble Tea program
// together. It owns the store subscription that drives redraws and the keyboard
// focus state.
package ui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/sextant/internal/config"
	corepanes "github.com/runlevel-six/sextant/internal/core/panes"
	"github.com/runlevel-six/sextant/pkg/plugin"
	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
	"github.com/runlevel-six/sextant/pkg/tui/grid"
)

// Rows reserved for chrome and unavailable to panes: two header rows plus one
// footer.
const (
	headerRows = 2
	footerRows = 1
	chromeRows = headerRows + footerRows
)

// Model is the Bubble Tea model.
//
// Watchers are started outside it; the model only reads the store and re-renders
// when it changes.
type Model struct {
	resolved config.Resolved
	store    *store.Store
	registry *plugin.Registry
	sub      <-chan struct{}
	keys     keymap

	width, height int

	panes      []tui.Pane
	paneByID   map[string]tui.Pane
	orderIndex map[string]int
	focused    string

	// settled is the geometry already on screen, kept so that a re-measured
	// layout does not move tiles the reader is reading. See [Model.settle].
	settled *settledGeometry
	// peaks is each pane's appetite as the layout sees it: the highest it has
	// reported lately, rather than what it reports this instant. See
	// [Model.appetites].
	peaks map[string]widthPeak
	// now is the clock the peaks age against, replaced in tests.
	now func() time.Time

	showHelp bool
	// paused freezes the display by ignoring store notifications. The watchers
	// keep running, so unfreezing shows current state rather than replaying a
	// backlog — the point is to read a row without it moving, not to suspend
	// data collection.
	paused bool
	zoomed bool
	// colsOverride pins the column count when above zero; zero follows the
	// breakpoint.
	colsOverride int
}

// Panes returns the panes in placement order.
//
// Exported for tests that need to reach every pane a fully assembled dashboard
// carries, including the plugin panes that do not exist until a registry has
// detected their subsystems.
func (m *Model) Panes() []tui.Pane {
	return append([]tui.Pane(nil), m.panes...)
}

// New builds a Model.
//
// Panes are sorted by priority with ties keeping registration order, so a caller
// controls intra-priority ordering — which decides both jump-key digits and
// which single pane is visible at the narrowest breakpoint.
func New(resolved config.Resolved, s *store.Store, registry *plugin.Registry, ps []tui.Pane) *Model {
	sorted := append([]tui.Pane(nil), ps...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Priority() < sorted[j].Priority()
	})

	byID := make(map[string]tui.Pane, len(sorted))
	idx := make(map[string]int, len(sorted))
	for i, p := range sorted {
		byID[p.ID()] = p
		idx[p.ID()] = i
	}

	focus := ""
	if len(sorted) > 0 {
		focus = sorted[0].ID()
	}

	return &Model{
		resolved:   resolved,
		store:      s,
		registry:   registry,
		sub:        s.Subscribe(),
		keys:       defaultKeymap(),
		panes:      sorted,
		paneByID:   byID,
		orderIndex: idx,
		focused:    focus,
		peaks:      map[string]widthPeak{},
		now:        time.Now,
	}
}

// peakHold is how long a pane keeps an appetite it no longer has.
//
// Long enough to outlast the thing that caused it. A crash-looping pod restarts on
// a backoff measured in tens of seconds and a drain takes minutes, so a window of
// a couple of minutes covers a pane whose widest row keeps coming and going, which
// is the case that makes a screen swing. Short enough that an incident which is
// genuinely over gives the width back while the operator is still watching.
const peakHold = 2 * time.Minute

// widthPeak is the appetite a pane is being sized from and when it last actually
// claimed it.
type widthPeak struct {
	want  int
	claim time.Time
}

// appetites is what the layout sizes columns from: each pane's highest appetite
// over the last [peakHold], not the one it reports this instant.
//
// A pane's appetite is a measurement of live data, and two states of a cluster can
// genuinely want different widths — a crash-looping pod with a 60-character name
// needs room its neighbors are using, and stops needing it the moment the pod
// recovers. Sizing from the instant reading means each state starves whichever pane
// the other one fed, so the screen swings between two layouts for as long as the
// pod flaps: every rule about *when* to redraw is downstream of an input that will
// not sit still, and cannot fix it.
//
// Holding the peak collapses that to one move. The width is taken when it is first
// needed and given back once nothing has needed it for a while, so a flapping pod
// costs the reader a single re-layout instead of one per poll.
func (m *Model) appetites() map[string]int {
	now := m.now()
	out := make(map[string]int, len(m.panes))
	for _, p := range m.panes {
		cw, ok := p.(tui.ContentWidthPane)
		if !ok {
			continue
		}
		want := cw.ContentWidth()
		peak, seen := m.peaks[p.ID()]
		switch {
		case !seen || want >= peak.want || now.Sub(peak.claim) >= peakHold:
			peak = widthPeak{want: want, claim: now}
		}
		m.peaks[p.ID()] = peak
		if peak.want > 0 {
			out[p.ID()] = peak.want
		}
	}
	return out
}

// dataUpdateMsg fires when the store reports a change.
type dataUpdateMsg struct{}

func (m *Model) waitForUpdate() tea.Cmd {
	return func() tea.Msg {
		<-m.sub
		return dataUpdateMsg{}
	}
}

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd { return m.waitForUpdate() }

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case dataUpdateMsg:
		// Keep consuming notifications while frozen, or the subscription's
		// buffer would stall and the first unfreeze would show stale data.
		return m, m.waitForUpdate()
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Quit):
		return m, tea.Quit
	case key.Matches(msg, m.keys.Help):
		m.showHelp = !m.showHelp
	case key.Matches(msg, m.keys.Tab):
		m.cycleFocus(+1)
	case key.Matches(msg, m.keys.ShiftTab):
		m.cycleFocus(-1)
	case key.Matches(msg, m.keys.Pause):
		m.paused = !m.paused
	case key.Matches(msg, m.keys.Zoom):
		m.zoomed = !m.zoomed
	case key.Matches(msg, m.keys.ColsMore):
		m.colsOverride = min(m.effectiveCols()+1, grid.MaxColumns)
	case key.Matches(msg, m.keys.ColsFewer):
		m.colsOverride = max(m.effectiveCols()-1, 1)
	case key.Matches(msg, m.keys.ColsAuto):
		m.colsOverride = 0
	case key.Matches(msg, m.keys.Theme):
		// Safe here and only here: Bubble Tea serializes Update against View,
		// so the palette is never rewritten part-way through a frame.
		tui.ApplyTheme(tui.NextTheme(tui.CurrentTheme().Name))
	case key.Matches(msg, m.keys.Jump):
		if len(msg.Runes) > 0 {
			if i := int(msg.Runes[0] - '1'); i >= 0 && i < len(m.panes) {
				m.focused = m.panes[i].ID()
			}
		}
	}
	return m, nil
}

func (m *Model) cycleFocus(step int) {
	if len(m.panes) == 0 {
		return
	}
	cur, ok := m.orderIndex[m.focused]
	if !ok {
		cur = 0
	}
	m.focused = m.panes[(cur+step+len(m.panes))%len(m.panes)].ID()
}

// effectiveCols reports the column count the layout will use, so the footer can
// show what is actually drawn.
func (m *Model) effectiveCols() int {
	c := m.colsOverride
	if c <= 0 {
		c = columnCountForWidth(m.width)
	}
	c = min(c, grid.MaxColumns)
	if len(m.panes) > 0 {
		c = min(c, len(m.panes))
	}
	return max(c, 1)
}

// columnCountForWidth mirrors the grid package's breakpoints so the footer can
// report the count without the layout exposing its internals.
//
// These two must stay in step; a mismatch shows a wrong number in the footer but
// does not affect the layout itself.
func columnCountForWidth(w int) int {
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

// View implements tea.Model.
func (m *Model) View() string {
	if m.width == 0 || m.height == 0 {
		return ""
	}

	zoomedID := ""
	if m.zoomed {
		zoomedID = m.focused
	}
	appetite := m.appetites()
	lay := m.settle(grid.Compute(grid.Options{
		Width:        m.width,
		Height:       m.height,
		Panes:        m.panes,
		FocusedID:    m.focused,
		HeaderH:      chromeRows,
		OverrideCols: m.colsOverride,
		ZoomedID:     zoomedID,
		Appetite:     appetite,
	}), appetite)

	return lipgloss.JoinVertical(lipgloss.Left,
		m.renderHeader(m.width),
		m.renderBody(lay),
		m.footerLine(lay),
	)
}

// stableSlack is how far a tile has to want to move before the screen is redrawn
// at the new size.
//
// Sizing the grid from the panes' content means re-measuring it on every store
// update, and a dashboard whose borders slide two cells every twenty seconds is
// harder to read than one that is two cells wrong. Four cells is under a word:
// below it, nothing a reader was looking at has become truncated, so the movement
// buys nothing and costs them their place.
const stableSlack = 4

// settledGeometry is the arrangement currently on screen: the terminal and mode it
// was decided for, which cell of the grid each pane sits in, and the rectangle it
// was given.
type settledGeometry struct {
	width, height, cols int
	zoomed              bool
	// structure is which pane is in which grid cell, independent of how wide or
	// tall the cells are. See [structureOf].
	structure string
	// measured records whether any pane had content to size the layout from when
	// it was decided. A layout decided from nothing is provisional: the first
	// frame is drawn before the informers have delivered anything, and freezing
	// the even division that produces would mean the dashboard never adopted a
	// content-sized layout at all.
	measured bool
	tiles    map[string][4]int // pane key -> x, y, w, h
}

// structureOf describes the arrangement — which pane sits in which grid cell —
// without describing its size.
//
// Tile coordinates cannot be compared directly for this, because a pane's x and y
// move whenever a *neighbor* is resized, and that is the movement worth ignoring.
// Ranking the distinct coordinates instead turns the rectangles back into the row
// and column indices the grid assigned, so this changes when a pane appears, is
// hidden, or moves cell, and not when the boundaries between cells shift.
func structureOf(tiles []grid.Tile) string {
	rows := rankOf(tiles, func(t grid.Tile) int { return t.Y })
	cols := rankOf(tiles, func(t grid.Tile) int { return t.X })

	cells := make([]string, 0, len(tiles))
	for _, t := range tiles {
		cells = append(cells, fmt.Sprintf("%s@%d,%d", tileKey(t), rows[t.Y], cols[t.X]))
	}
	sort.Strings(cells)
	return strings.Join(cells, "|")
}

// rankOf maps each distinct coordinate the tiles start at to its position in the
// sorted set of them.
func rankOf(tiles []grid.Tile, of func(grid.Tile) int) map[int]int {
	seen := map[int]bool{}
	for _, t := range tiles {
		seen[of(t)] = true
	}
	vals := make([]int, 0, len(seen))
	for v := range seen {
		vals = append(vals, v)
	}
	sort.Ints(vals)
	rank := make(map[int]int, len(vals))
	for i, v := range vals {
		rank[v] = i
	}
	return rank
}

// settle keeps the geometry already on screen unless the new one is materially
// different, and returns a layout carrying whichever won.
//
// The alternative the layout invites — recomputing freely, since Compute is cheap
// and pure — is what makes the display twitch: the first frame is drawn before any
// pane has data, informers land a second later, and each plugin's first poll lands
// after that, so a dashboard that honored every measurement would resize three
// times before it settled and again on every poll that changed a cell's length.
//
// So the first measurement of a given terminal wins, and later ones are adopted only
// when the arrangement itself changed — a pane appeared, was hidden, or moved cell —
// or when holding still would cost the reader something. That is the difference
// between a layout that adapts and one that fidgets; a fleet that genuinely grows
// still gets the width for it, on the frame it grows.
//
// Only the geometry is retained. Focus, hidden panes and ordering come from the
// fresh layout, so tabbing between panes still repaints immediately.
func (m *Model) settle(next grid.Layout, appetite map[string]int) grid.Layout {
	key := settledGeometry{
		width: m.width, height: m.height, cols: m.effectiveCols(), zoomed: m.zoomed,
		structure: structureOf(next.Tiles),
	}
	// Measured once per frame and shared with adopt: a pane derives its appetite
	// from the rows it is about to draw, which on a large fleet is not free.
	fresh := make(map[string][4]int, len(next.Tiles))
	demand := make(map[string]int, len(next.Tiles))
	for _, t := range next.Tiles {
		k := tileKey(t)
		fresh[k] = [4]int{t.X, t.Y, t.W, t.H}
		demand[k] = m.contentDemand(t, appetite)
		key.measured = key.measured || demand[k] > 0
	}

	if m.adopt(key, next, demand) {
		key.tiles = fresh
		m.settled = &key
		return next
	}
	for i := range next.Tiles {
		if geom, ok := m.settled.tiles[tileKey(next.Tiles[i])]; ok {
			next.Tiles[i].X, next.Tiles[i].Y = geom[0], geom[1]
			next.Tiles[i].W, next.Tiles[i].H = geom[2], geom[3]
		}
	}
	return next
}

// adopt reports whether the new geometry should replace what is on screen.
//
// Vertically a tile that wants to grow or shrink by [stableSlack] is taken at its
// word: rows are trimmed to the content that exists, so a row changing height means
// lines are appearing or disappearing either way.
//
// Horizontally the test is not whether the ideal boundaries moved but whether the
// ones on screen have become too narrow for what they hold. Width is divided in
// proportion to appetite and the slack is shared out with it, so *any* pane's
// content changing length re-proportions the whole band — a single crash-looping
// pod with a long name moved every border on a four-column screen by up to
// thirty-four cells, and moved them back when it recovered, even though no pane was
// short of room before or after. A boundary that moves without relieving a
// truncation has bought the reader nothing and cost them their place, so the
// question asked here is per pane and one-directional: is this tile now too narrow
// for its own content, and would the fresh layout give it materially more?
//
// Both halves of that carry [stableSlack], and the second half is not redundant. A
// pane can be short of room for as long as its content stays long — a fleet of
// 60-character pod names never fits a quarter of the screen — and a permanently
// starved tile whose ideal width drifts by a cell as ages and restart counts tick
// over would otherwise re-lay out the whole screen on every poll, which is the
// original complaint wearing a different hat.
func (m *Model) adopt(key settledGeometry, next grid.Layout, demand map[string]int) bool {
	switch {
	case m.settled == nil:
		return true
	// A resize, a column override or a zoom is the reader asking for a new
	// arrangement, so there is nothing to preserve.
	case m.settled.width != key.width, m.settled.height != key.height,
		m.settled.cols != key.cols, m.settled.zoomed != key.zoomed:
		return true
	// A pane appeared, was hidden, or moved to a different cell.
	case m.settled.structure != key.structure:
		return true
	// The layout on screen was decided before there was anything to size it from.
	case !m.settled.measured && key.measured:
		return true
	}
	for _, t := range next.Tiles {
		k := tileKey(t)
		was, ok := m.settled.tiles[k]
		if !ok {
			return true // this pane was not on screen, or is in a different stack
		}
		if t.Y-was[1] >= stableSlack || was[1]-t.Y >= stableSlack ||
			t.H-was[3] >= stableSlack || was[3]-t.H >= stableSlack {
			return true
		}
		if t.W-was[2] >= stableSlack && demand[k]-was[2] >= stableSlack {
			return true
		}
	}
	return false
}

// contentDemand is the tile width a pane's content could use, chrome included —
// what the grid would size its column from. A stacked tile answers for the
// hungriest pane sharing it, since they render at one width.
//
// Zero for a pane that declares no appetite: it has no opinion about its width, so
// it is never the reason to redraw. The floor is included because a tile below
// [tui.Pane.MinWidth] is not merely cramped, it is dropping columns.
func (m *Model) contentDemand(t grid.Tile, appetite map[string]int) int {
	if len(t.Stacked) == 0 {
		return m.paneDemand(t.PaneID, appetite)
	}
	most := 0
	for _, s := range t.Stacked {
		most = max(most, m.paneDemand(s.PaneID, appetite))
	}
	return most
}

// paneDemand answers from the same smoothed appetite the layout was sized from, so
// the question "is this tile too narrow" is asked about the width the grid was
// trying to give it.
func (m *Model) paneDemand(id string, appetite map[string]int) int {
	p, ok := m.paneByID[id]
	if !ok {
		return 0
	}
	w := appetite[id]
	if w <= 0 {
		return 0
	}
	return max(w+tui.PaneChromeH, p.MinWidth())
}

// tileKey identifies a tile across frames. A stacked tile has no pane of its own,
// so it is keyed by the panes sharing it, in order.
func tileKey(t grid.Tile) string {
	if len(t.Stacked) == 0 {
		return t.PaneID
	}
	ids := make([]string, 0, len(t.Stacked))
	for _, s := range t.Stacked {
		ids = append(ids, s.PaneID)
	}
	return "stack:" + strings.Join(ids, "+")
}

// renderBody composites the tiles at their positions, one output line at a time.
//
// Compositing rather than joining rows is what allows a tile to span rows. With
// row spans the tiles no longer form horizontal bands — a two-row Machines & Hosts
// sits beside a Pod Health in the first band and an Events in the second — so
// there is no row left to join, and grouping by Y coordinate stacked the same
// vertical space twice and overran the terminal.
//
// The body is exactly its allotted height and width. Bubble Tea only repaints what
// it is given, so an under-tall body leaves the previous frame on screen; anything
// no tile covers is painted with the screen ground rather than left alone.
func (m *Model) renderBody(lay grid.Layout) string {
	bodyH := m.height - chromeRows
	if bodyH < 1 {
		return ""
	}

	// Render each tile once. A tile that declines to render — no such pane, or a
	// rectangle too small to be useful — contributes nothing, and its area falls
	// through to the backdrop.
	type block struct {
		x, w, top int
		lines     []string
	}
	byLine := make([][]block, bodyH)
	for _, t := range lay.Tiles {
		var s string
		if len(t.Stacked) > 0 {
			s = m.renderStackedTile(t)
		} else {
			s = m.renderTile(t.PaneID, t.W, t.H, t.Focused)
		}
		if s == "" {
			continue
		}
		b := block{x: t.X, w: t.W, top: t.Y - chromeRows, lines: strings.Split(s, "\n")}
		for i := range b.lines {
			if y := b.top + i; y >= 0 && y < bodyH {
				byLine[y] = append(byLine[y], b)
			}
		}
	}

	out := make([]string, bodyH)
	for y := range bodyH {
		blocks := byLine[y]
		sort.SliceStable(blocks, func(i, j int) bool { return blocks[i].x < blocks[j].x })

		var sb strings.Builder
		x := 0
		for _, b := range blocks {
			switch {
			case b.x > x:
				sb.WriteString(backdrop(b.x - x))
				x = b.x
			case b.x < x:
				// Overlapping tiles would make the line the wrong width. The layout
				// does not produce them; skip rather than emit a corrupt line.
				continue
			}
			sb.WriteString(b.lines[y-b.top])
			x += b.w
		}
		if x < m.width {
			sb.WriteString(backdrop(m.width - x))
		}
		out[y] = sb.String()
	}
	return strings.Join(out, "\n")
}

// renderTile draws one pane inside its frame.
func (m *Model) renderTile(paneID string, w, h int, focused bool) string {
	p := m.paneByID[paneID]
	if p == nil {
		return ""
	}
	innerW := w - tui.PaneChromeH
	innerH := h - tui.PaneChromeV
	if innerW < 1 || innerH < 1 {
		return ""
	}
	title := tui.CurrentTheme().PaneTitle(paneID, p.Title())
	if digit := m.jumpDigitFor(paneID); digit != "" {
		title += styleTitleDim().Render(" [" + digit + "]")
	}
	return paneFrame(paneBox{
		Title:   title,
		Width:   w,
		Height:  h,
		Focused: focused,
		Index:   m.orderIndex[paneID],
		Body:    p.Render(innerW, innerH, focused),
	})
}

// renderStackedTile draws the panes sharing one tile, top to bottom.
//
// The last sub-tile absorbs the rounding remainder so the heights sum to exactly
// the parent's, which is what keeps the grid from drifting a row.
func (m *Model) renderStackedTile(t grid.Tile) string {
	parts := make([]string, 0, len(t.Stacked))
	remaining := t.H
	for i, sub := range t.Stacked {
		subH := remaining
		if i < len(t.Stacked)-1 {
			subH = min(max(int(float64(t.H)*sub.HRatio), 1), remaining)
		}
		remaining -= subH
		if s := m.renderTile(sub.PaneID, t.W, subH, sub.Focused); s != "" {
			parts = append(parts, s)
		}
	}
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// jumpDigitFor returns "1" through "9" for the first nine panes in order.
func (m *Model) jumpDigitFor(id string) string {
	if i, ok := m.orderIndex[id]; ok && i < 9 {
		return string(rune('1' + i))
	}
	return ""
}

func (m *Model) footerLine(lay grid.Layout) string {
	cols := fmt.Sprintf("%dcol", m.effectiveCols())
	if m.colsOverride > 0 {
		cols += "*"
	}
	if m.zoomed {
		cols += " zoom"
	}

	th := tui.CurrentTheme()
	status := styleStatusBar()
	minor := th.MinorSeparator()
	parts := []string{
		status.Render(fmt.Sprintf("%s%s%s%s%d panes",
			lay.Breakpoint, minor, cols, minor, len(m.panes))),
	}
	// The active theme is named only when it is not the default, so the footer
	// stays quiet in the ordinary case but a switched theme is never a mystery.
	if th.Name != tui.DefaultTheme().Name {
		parts = append(parts, status.Render(th.Name))
	}
	if len(lay.Hidden) > 0 {
		parts = append(parts, status.Render(
			fmt.Sprintf("%d hidden (tab to reach)", len(lay.Hidden))))
	}
	if m.showHelp {
		parts = append(parts, tui.StyleAccent.Render(strings.Join(m.keys.helpLines(), minor)))
	} else {
		parts = append(parts, status.Render("? help"+minor+"q quit"))
	}
	// The footer sits on the screen ground rather than a bar of its own, so a
	// grounded theme gives it the same band as the backdrop above it.
	return groundLine(strings.Join(parts, status.Render(th.Separator)), m.width,
		th.Text, th.ScreenBG())
}

// CorePanes builds the panes that need nothing beyond Kubernetes, Cluster API
// and Metal3.
//
// The order is the priority and jump-key order, and it is deliberate: the
// overview first because it is the summary, then machines because the physical
// fleet is what a rollout moves, then nodes, pods and events.
func CorePanes(s *store.Store, resolved config.Resolved,
	summaries func() []tui.SummaryBlock) []tui.Pane {
	prof := resolved.Profile
	return []tui.Pane{
		corepanes.NewOverview(s, prof.NodeRoles, resolved.TargetVersion).
			WithSummaries(summaries),
		corepanes.NewMachines(s, prof.NodeRoles),
		corepanes.NewNodes(s, prof.NodeRoles, resolved.TargetVersion),
		corepanes.NewPodHealth(s, prof.CriticalWorkloads),
		corepanes.NewEvents(s, resolved.TargetVersion),
	}
}
