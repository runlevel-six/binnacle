package panes

import (
	"fmt"
	"sort"

	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/sextant/pkg/model"
	"github.com/runlevel-six/sextant/pkg/rollout"
	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
	"github.com/runlevel-six/sextant/pkg/tui/table"
)

// EventsPane shows recent cluster events, collapsed by reason.
//
// Collapsing is the point. A cluster under load emits the same reason hundreds
// of times, and a chronological list is then a single repeated line pushing
// everything else off screen. Grouping by (namespace, reason, type) with a count
// keeps one noisy source from hiding the rest.
//
// The pane is mode-aware: during a rollout it reads the management cluster's
// events, where Machine and host activity is reported, and otherwise the workload
// cluster's, where day-to-day trouble appears.
type EventsPane struct {
	base
	store         *store.Store
	targetVersion string
	// expanded switches from rollups to individual events. Toggled by the
	// program, since a specific message is sometimes exactly what is needed.
	expanded bool
}

// NewEvents builds the events pane.
func NewEvents(s *store.Store, targetVersion string) *EventsPane {
	return &EventsPane{
		base: base{
			id: "events", title: "Events",
			priority: tui.P1Important, minW: 46, minH: 6, weight: 7,
			// Two columns: LATEST OBJECT is a Kind/name pair and truncates to
			// "KubeadmControlPlane/demo-contr" at a quarter width, which names the
			// kind and hides which object it was.
			span: 2,
		},
		store:         s,
		targetVersion: targetVersion,
	}
}

// ToggleExpanded switches between rollup and per-event rendering.
func (p *EventsPane) ToggleExpanded() { p.expanded = !p.expanded }

// Expanded reports the current mode, for the footer hint.
func (p *EventsPane) Expanded() bool { return p.expanded }

// Title reflects which cluster is being read, so the reader is never in doubt
// about whose events these are.
func (p *EventsPane) Title() string {
	if rollout.Active(p.store, p.targetVersion) {
		return "Events (management)"
	}
	return "Events (workload)"
}

var (
	rollupCols = []table.Column{
		{Header: "COUNT"},
		{Header: "TYPE"},
		{Header: "REASON"},
		{Header: "NAMESPACE"},
		// Transient: a rollup row is identified by its namespace, reason and
		// type, and the object is an example of them — the newest one at the
		// moment of the poll, which is a different object with a different name
		// on the next one. Charging the pane for it would ask the layout to
		// resize itself around a name that is already gone. See
		// [table.AppetiteWidth].
		{Header: "LATEST OBJECT", Stretch: true, Transient: true},
		{Header: "AGE"},
	}
	eventCols = []table.Column{
		{Header: "TYPE"},
		{Header: "REASON"},
		{Header: "OBJECT"},
		{Header: "MESSAGE", Stretch: true, Transient: true},
		{Header: "AGE"},
	}
)

// sourceKey names the event stream this pane is reading: the management
// cluster's during a rollout, the workload cluster's otherwise.
func (p *EventsPane) sourceKey() (key, what string) {
	if rollout.Active(p.store, p.targetVersion) {
		return model.KeyMgmtEvents, "management events"
	}
	return model.KeyWorkloadEvents, "workload events"
}

// ContentWidth implements [tui.ContentWidthPane], measured in whichever mode the
// pane is currently in — the two have different schemas, and the one being drawn
// is the one whose columns have to fit.
func (p *EventsPane) ContentWidth() int {
	key, _ := p.sourceKey()
	snap, ok := store.Get[model.Snapshot[model.Event]](p.store, key)
	if !ok || len(snap.Items) == 0 {
		return 0
	}
	if p.expanded {
		cells, _ := eventRows(snap.Items)
		return table.AppetiteWidth(eventCols, cells)
	}
	cells, _ := rollupRows(rollups(snap.Items))
	return table.AppetiteWidth(rollupCols, cells)
}

// Render implements tui.Pane.
func (p *EventsPane) Render(w, h int, _ bool) string {
	key, what := p.sourceKey()

	snap, body, ok := snapshotOf[model.Event](p.store, key, w, h, what)
	if !ok {
		return body
	}
	if len(snap.Items) == 0 {
		return table.Placeholder(w, h, "no recent events")
	}

	if p.expanded {
		cells, styles := eventRows(snap.Items)
		return table.Table{Cols: eventCols, Rows: cells, CellStyles: styles}.Render(w, h)
	}
	cells, styles := rollupRows(rollups(snap.Items))
	return table.Table{Cols: rollupCols, Rows: cells, CellStyles: styles}.Render(w, h)
}

// rollup is one group of like events.
type rollup struct {
	namespace  string
	reason     string
	eventType  string
	count      int32
	latestName string
	latestKind string
	ageSeconds float64
}

// rollups groups events by namespace, reason and type.
//
// Counting uses each event's own Count field rather than the number of event
// objects, because Kubernetes already deduplicates repeats server-side: a single
// object may stand for hundreds of occurrences, and ignoring that would
// under-report the noisiest source by orders of magnitude.
func rollups(events []model.Event) []rollup {
	type key struct{ ns, reason, typ string }
	index := map[key]*rollup{}
	var order []key

	for _, e := range events {
		k := key{e.Namespace, e.Reason, e.Type}
		r, seen := index[k]
		if !seen {
			r = &rollup{
				namespace: e.Namespace, reason: e.Reason, eventType: e.Type,
				ageSeconds: -1,
			}
			index[k] = r
			order = append(order, k)
		}
		r.count += max(e.Count, 1)

		// Events arrive newest-first, so the first of a group is its latest.
		if r.latestName == "" {
			r.latestName, r.latestKind = e.ObjectName, e.ObjectKind
		}
		if !e.LastTimestamp.IsZero() {
			if age := timeSince(e.LastTimestamp); r.ageSeconds < 0 || age < r.ageSeconds {
				r.ageSeconds = age
			}
		}
	}

	out := make([]rollup, 0, len(order))
	for _, k := range order {
		out = append(out, *index[k])
	}

	// Warnings first, then by count. A single Warning matters more than a
	// hundred Normal events, so sorting by count alone would bury it.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.eventType == "Warning") != (b.eventType == "Warning") {
			return a.eventType == "Warning"
		}
		if a.count != b.count {
			return a.count > b.count
		}
		return a.reason < b.reason
	})
	return out
}

func rollupRows(rs []rollup) ([][]string, [][]lipgloss.Style) {
	cells := make([][]string, 0, len(rs))
	styles := make([][]lipgloss.Style, 0, len(rs))
	for _, r := range rs {
		object := r.latestName
		if r.latestKind != "" {
			object = r.latestKind + "/" + r.latestName
		}
		age := "—"
		if r.ageSeconds >= 0 {
			age = table.ShortAge(r.ageSeconds)
		}
		cells = append(cells, []string{
			fmt.Sprintf("%d", r.count),
			r.eventType,
			r.reason,
			r.namespace,
			object,
			age,
		})
		styles = append(styles, []lipgloss.Style{
			{},
			eventTypeStyle(r.eventType),
			eventTypeStyle(r.eventType),
			tui.StyleMuted,
			{},
			tui.StyleMuted,
		})
	}
	return cells, styles
}

func eventRows(events []model.Event) ([][]string, [][]lipgloss.Style) {
	cells := make([][]string, 0, len(events))
	styles := make([][]lipgloss.Style, 0, len(events))
	for _, e := range events {
		object := e.ObjectName
		if e.ObjectKind != "" {
			object = e.ObjectKind + "/" + e.ObjectName
		}
		age := "—"
		if !e.LastTimestamp.IsZero() {
			age = table.ShortAge(timeSince(e.LastTimestamp))
		}
		cells = append(cells, []string{e.Type, e.Reason, object, e.Message, age})
		styles = append(styles, []lipgloss.Style{
			eventTypeStyle(e.Type),
			eventTypeStyle(e.Type),
			{}, {},
			tui.StyleMuted,
		})
	}
	return cells, styles
}

func eventTypeStyle(t string) lipgloss.Style {
	switch t {
	case "Warning":
		return tui.StyleErr
	case "Normal":
		return tui.StyleMuted
	}
	return lipgloss.Style{}
}
