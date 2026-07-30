package openstack

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
	"github.com/runlevel-six/sextant/pkg/tui/table"
)

// servicesPane reports every OpenStack service and whether it is up to date.
//
// It exists partly to fix an omission the agent table cannot: only Nova, Neutron
// and Cinder register agents, so a cloud running fifteen services rendered three
// rows and gave no hint the other twelve were there. Keystone, Glance, Heat,
// Octavia, Barbican, Placement and Magnum have no agents to report and were
// simply absent from the dashboard.
type servicesPane struct {
	store *store.Store
	// namespace pins where the OpenStack workloads live; empty derives it.
	namespace string
}

func newServicesPane(s *store.Store, namespace string) *servicesPane {
	return &servicesPane{store: s, namespace: namespace}
}

func (p *servicesPane) ID() string             { return "openstack-services" }
func (p *servicesPane) Title() string          { return "Service Versions" }
func (p *servicesPane) Priority() tui.Priority { return tui.P2Useful }
func (p *servicesPane) MinWidth() int          { return 40 }
func (p *servicesPane) MinHeight() int         { return 4 }
func (p *servicesPane) HeightWeight() int      { return 2 }

// Group puts this pane in the shared "Cloud" frame; see [tui.GroupedPane].
func (p *servicesPane) GroupID() string    { return "cloud" }
func (p *servicesPane) GroupTitle() string { return "Cloud" }
func (p *servicesPane) GroupOrder() int    { return 1 }

var serviceVersionCols = []table.Column{
	{Header: "SERVICE"},
	{Header: "UP TO DATE"},
	{Header: "BEHIND", Stretch: true},
}

// Render implements tui.Pane.
func (p *servicesPane) Render(w, h int, _ bool) string {
	svcs, ok := CollectServices(p.store, p.namespace)
	if !ok || len(svcs.Items) == 0 {
		return table.Placeholder(w, h, "no OpenStack workloads found")
	}

	pending := svcs.Pending()

	// Below three rows there is no table worth drawing: a header plus "+ 3 more"
	// spends both lines saying that something exists without saying what. One
	// line of prose carries strictly more — how many are rolling, how far behind,
	// and whether any of it needs a person.
	if h < 3 {
		line := servicesSummary(svcs)
		if line == "" {
			line = tui.StyleOK.Render(fmt.Sprintf("%d service(s) up to date", len(svcs.Items)))
		}
		return table.ClipLines(table.PadOrTrunc(line, w), h)
	}

	// A converged cloud in a short frame says so in one line rather than listing
	// whichever services happen to sort first. Clipping the table here would show
	// three of eleven with nothing to indicate the other eight existed — which is
	// the exact failure this pane was added to correct, reintroduced by the
	// renderer instead of by the API.
	if len(pending) == 0 && len(svcs.Items)+1 > h {
		return table.ClipLines(table.PadOrTrunc(tui.StyleOK.Render(
			fmt.Sprintf("%d service(s) up to date", len(svcs.Items))), w), h)
	}

	// The trailer is composed first and its height reserved, because a summary
	// clipped off the bottom is worse than a row of the table given up for it:
	// the table says what is behind, the trailer says how much is not shown and
	// whether any of it needs a human.
	var trailer []string
	shown := svcs.Items
	if len(shown)+1 > h && len(pending) > 0 {
		// Converged services are the first thing to give up, and they cost
		// nothing: "keystone 3/3" says the same thing every day for months. No
		// "N others up to date" line to say they went — the summary below already
		// reads "3/11 rolling", and spending one of five rows to restate the
		// subtraction cost a service that is actually behind its place on screen.
		shown = pending
	}
	// The summary is added only when the table can still show every row it has.
	// Reserving it unconditionally cost more than it bought: at four lines the
	// table was left with three, spent one of those on its own "+ 2 more" footer,
	// and rendered a single service — so a line saying "3/11 rolling" displaced
	// two of the three things that were rolling. The rows carry the ⚠ marker
	// themselves, so what the summary adds when they are all visible is scale,
	// and scale is the part worth losing first.
	if summary := servicesSummary(svcs); summary != "" && len(shown)+2 <= h {
		trailer = append(trailer, summary)
	}
	tableH := max(h-len(trailer), 2)

	rows := make([][]string, 0, len(shown))
	styles := make([][]lipgloss.Style, 0, len(shown))
	for _, svc := range shown {
		rows = append(rows, serviceVersionRow(svc))
		styles = append(styles, serviceVersionStyles(svc))
	}

	lines := []string{table.Table{
		Cols: serviceVersionCols, Rows: rows, CellStyles: styles,
	}.Render(w, min(tableH, len(rows)+1))}
	for _, line := range trailer {
		lines = append(lines, table.PadOrTrunc(line, w))
	}
	return table.ClipLines(strings.Join(lines, "\n"), h)
}

func serviceVersionRow(svc Service) []string {
	count := fmt.Sprintf("%d/%d", svc.Updated, svc.Desired)
	if svc.Converged() {
		return []string{svc.Name, count, ""}
	}
	if svc.Manual {
		count += " ⚠"
	}

	// Name the components that are behind rather than only the service. "nova
	// 22/25" does not say whether the API is mid-restart or every hypervisor is
	// a release back, and those call for very different reactions.
	//
	// Unless the service *is* one component, in which case the breakdown repeats
	// the fraction already to its left — a single-workload service rendered
	// "libvirt 1/5 | libvirt 1/5", which spends a column to say nothing twice.
	behind := svc.Behind()
	if len(behind) == 1 && behind[0].Desired == svc.Desired {
		return []string{svc.Name, count, ""}
	}
	parts := make([]string, 0, len(behind))
	for i, c := range behind {
		if i == 2 {
			parts = append(parts, fmt.Sprintf("+%d", len(behind)-2))
			break
		}
		parts = append(parts, fmt.Sprintf("%s %d/%d",
			TrimComponent(svc.Name, c.Name), c.Updated, c.Desired))
	}
	return []string{svc.Name, count, strings.Join(parts, ", ")}
}

func serviceVersionStyles(svc Service) []lipgloss.Style {
	switch {
	case svc.Converged():
		return []lipgloss.Style{{}, tui.StyleOK, {}}
	case svc.Manual:
		// Amber rather than red. Nothing is broken; an operator has work to do,
		// which is a different thing and must not read as an outage.
		return []lipgloss.Style{{}, tui.StyleWarn, tui.StyleWarn}
	default:
		return []lipgloss.Style{{}, tui.StyleAccent, tui.StyleMuted}
	}
}

// servicesSummary is the headline, or empty when everything is current.
//
// Silent when converged, on the rule the overview blocks follow: a line that is
// always there is one nobody reads, and this pane's value is being noticed on the
// day it has something to say.
func servicesSummary(svcs Services) string {
	if svcs.Converged() {
		return ""
	}
	// Terse because this sits under a table in one grid column and a sentence
	// does not fit: the earlier wording lost its last two words to the border,
	// which is worse than no summary at all.
	msg := fmt.Sprintf("%d/%d rolling · %d pod(s) behind",
		len(svcs.Pending()), len(svcs.Items), svcs.StalePods())
	if svcs.NeedsOperator() {
		return tui.StyleWarn.Render(msg + " · needs an operator")
	}
	return tui.StyleAccent.Render(msg)
}
