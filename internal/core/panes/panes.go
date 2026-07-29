// Package panes holds the core pane renderers: the widgets that need no
// subsystem beyond Kubernetes, Cluster API and Metal3.
//
// Every pane here is a pure function of a store snapshot plus a size. That is
// what makes them testable from literals, and it is why they hold no state:
// the program re-renders whenever the store changes, so a pane that cached
// anything would only be able to go stale.
//
// Anything requiring another subsystem — Ceph, Cilium, OpenStack — belongs in a
// plugin, not here.
package panes

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/runlevel-six/sextant/internal/core/model"
	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
	"github.com/runlevel-six/sextant/pkg/tui/table"
)

// base supplies the boilerplate half of the Pane interface so each pane only
// writes its Render method.
type base struct {
	id       string
	title    string
	priority tui.Priority
	minW     int
	minH     int
	weight   int
	// span is how many grid columns the pane asks for; zero means one. Set it on
	// a pane whose content is a table wide enough that a quarter of the terminal
	// truncates the column a reader identifies rows by. See [tui.WidePane].
	span int
	// rows is how many grid rows the pane asks for; zero means one. Set it on a
	// pane whose row count scales with the cluster, where no amount of width
	// helps. See [tui.TallPane].
	rows int
}

func (b base) ID() string             { return b.id }
func (b base) Title() string          { return b.title }
func (b base) Priority() tui.Priority { return b.priority }
func (b base) MinWidth() int          { return b.minW }
func (b base) MinHeight() int         { return b.minH }
func (b base) HeightWeight() int      { return b.weight }

// ColSpan implements [tui.WidePane]. Every pane answers it, and the ones that
// never set span answer 1 — which is what they got before spans existed.
func (b base) ColSpan() int { return max(b.span, 1) }

// RowSpan implements [tui.TallPane], on the same terms as ColSpan.
func (b base) RowSpan() int { return max(b.rows, 1) }

// snapshotOf reads a typed snapshot, returning a body to render instead when the
// data is missing or failed.
//
// The untyped fallback matters: a source that failed before it could build the
// real element type publishes Snapshot[any], so checking only the typed shape
// would show "loading" forever for a key that actually holds a permission error.
func snapshotOf[T any](s *store.Store, key string, w, h int, what string) (model.Snapshot[T], string, bool) {
	if snap, ok := store.Get[model.Snapshot[T]](s, key); ok {
		switch {
		case snap.Err != nil:
			return snap, table.ErrorBody(w, h, snap.Err), false
		case len(snap.Items) == 0 && snap.Note != "":
			// Not ready rather than empty, and the note says what it is waiting
			// for. Rendered as a placeholder, not an error: nothing is wrong.
			return snap, table.Placeholder(w, h, snap.Note), false
		}
		return snap, "", true
	}
	if snap, ok := store.Get[model.Snapshot[any]](s, key); ok && snap.Err != nil {
		return model.Snapshot[T]{}, table.ErrorBody(w, h, snap.Err), false
	}
	return model.Snapshot[T]{}, table.Placeholder(w, h, "loading "+what+"…"), false
}

// section renders a titled block of lines, for panes that stack several small
// groups rather than one table.
func section(title string, lines []string) string {
	out := make([]string, 0, len(lines)+1)
	out = append(out, tui.StyleHeader.Render(title))
	out = append(out, lines...)
	return strings.Join(out, "\n")
}

// keyValue formats an aligned label and value pair.
func keyValue(labelWidth int, label, value string) string {
	return tui.StyleMuted.Render(table.PadOrTrunc(label, labelWidth)) + " " + value
}

// progressBar renders a proportional bar of the given cell width.
//
// It is deliberately coarse. During a rollout the number that matters is
// "how many of how many", which is printed beside it; the bar exists so a
// glance across several rows shows which pool is behind.
func progressBar(width int, done, total int32) string {
	if width < 3 {
		return ""
	}
	if total <= 0 {
		return tui.StyleMuted.Render(strings.Repeat("·", width))
	}
	filled := int(int64(width) * int64(done) / int64(total))
	filled = min(max(filled, 0), width)

	style := tui.StyleWarn
	switch {
	case done >= total:
		style = tui.StyleOK
	case done == 0:
		style = tui.StyleMuted
	}
	return style.Render(strings.Repeat("█", filled)) +
		tui.StyleMuted.Render(strings.Repeat("░", width-filled))
}

// countStyle colors a done-of-total pair: green complete, amber in flight,
// muted when nothing has started or there is nothing to do.
func countStyle(done, total int32) lipgloss.Style {
	switch {
	case total == 0, done == 0:
		return tui.StyleMuted
	case done >= total:
		return tui.StyleOK
	}
	return tui.StyleWarn
}

// pct formats a percentage, guarding the zero denominator.
func pct(part, whole int64) string {
	if whole <= 0 {
		return "—"
	}
	return fmt.Sprintf("%d%%", part*100/whole)
}

// pctStyle colors a utilization percentage. The thresholds are deliberately
// forgiving: high request commitment is normal and healthy on a well-packed
// node, so only genuine over-commitment is called out.
func pctStyle(part, whole int64) lipgloss.Style {
	if whole <= 0 {
		return tui.StyleMuted
	}
	switch p := part * 100 / whole; {
	case p >= 100:
		return tui.StyleErr
	case p >= 85:
		return tui.StyleWarn
	}
	return tui.StyleOK
}

// humanBytes formats a byte count in binary units.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	value, exp := float64(n), 0
	for value >= unit && exp < 4 {
		value /= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", value, "KMGT"[exp-1])
}

// millicores formats a CPU quantity, switching to whole cores once the number
// of millicores stops being readable.
func millicores(m int64) string {
	if m < 10000 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%.1f", float64(m)/1000)
}

// timeSince returns the seconds elapsed since t. Wrapped so every pane shares
// one clock reference.
func timeSince(t time.Time) float64 {
	return time.Since(t).Seconds()
}

// clipTo trims a hand-built body to exactly width x height.
//
// Table-driven panes get this from the table renderer, but a pane that composes
// its own lines has to enforce it: the layout hands out a fixed rectangle, and a
// pane that overruns pushes every pane below it off the bottom of the terminal.
// Trimming is sequence-aware: a naive rune cut can land inside an escape
// sequence, leaving a fragment that counts as printable and makes the line wider
// than the limit it was trimmed to.
func clipTo(body string, width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	lines := strings.Split(body, "\n")
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
