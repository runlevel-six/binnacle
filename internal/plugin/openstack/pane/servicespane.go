package pane

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/runlevel-six/binnacle/internal/plugin/openstack"
	"github.com/runlevel-six/binnacle/pkg/store"
	"github.com/runlevel-six/binnacle/pkg/tui"
	"github.com/runlevel-six/binnacle/pkg/tui/table"
)

// servicesPane reports every OpenStack service and whether it is up to date.
type servicesPane struct {
	store     *store.Store
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

func (p *servicesPane) GroupID() string    { return "cloud" }
func (p *servicesPane) GroupTitle() string { return "Cloud" }
func (p *servicesPane) GroupOrder() int    { return 1 }

var serviceVersionCols = []table.Column{
	{Header: "SERVICE"},
	{Header: "UP TO DATE"},
	{Header: "BEHIND", Stretch: true, Transient: true},
}

func (p *servicesPane) ContentWidth() int {
	svcs, ok := openstack.CollectServices(p.store, p.namespace)
	if !ok || len(svcs.Items) == 0 {
		return 0
	}
	rows, _ := p.versionRows(svcs.Items)
	return table.AppetiteWidth(serviceVersionCols, rows)
}

func (p *servicesPane) ContentHeight(int) int {
	svcs, ok := openstack.CollectServices(p.store, p.namespace)
	if !ok || len(svcs.Items) == 0 {
		return 0
	}
	if len(svcs.Pending()) == 0 {
		return 1
	}
	h := len(svcs.Items) + 1
	if servicesSummary(svcs) != "" {
		h++
	}
	return h
}

func (p *servicesPane) versionRows(svcs []Service) (rows [][]string, styles [][]lipgloss.Style) {
	rows = make([][]string, 0, len(svcs))
	styles = make([][]lipgloss.Style, 0, len(svcs))
	for _, svc := range svcs {
		rows = append(rows, serviceVersionRow(svc))
		styles = append(styles, serviceVersionStyles(svc))
	}
	return rows, styles
}

func (p *servicesPane) Render(w, h int, _ bool) string {
	svcs, ok := openstack.CollectServices(p.store, p.namespace)
	if !ok || len(svcs.Items) == 0 {
		return table.Placeholder(w, h, "no OpenStack workloads found")
	}

	pending := svcs.Pending()

	if h < 3 {
		line := servicesSummary(svcs)
		if line == "" {
			line = tui.StyleOK.Render(fmt.Sprintf("%d service(s) up to date", len(svcs.Items)))
		}
		return table.ClipLines(table.PadOrTrunc(line, w), h)
	}

	if len(pending) == 0 && len(svcs.Items)+1 > h {
		return table.ClipLines(table.PadOrTrunc(tui.StyleOK.Render(
			fmt.Sprintf("%d service(s) up to date", len(svcs.Items))), w), h)
	}

	var trailer []string
	shown := svcs.Items
	if len(shown)+1 > h && len(pending) > 0 {
		shown = pending
	}
	if summary := servicesSummary(svcs); summary != "" && len(shown)+2 <= h {
		trailer = append(trailer, summary)
	}
	tableH := max(h-len(trailer), 2)

	rows, styles := p.versionRows(shown)
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
			openstack.TrimComponent(svc.Name, c.Name), c.Updated, c.Desired))
	}
	return []string{svc.Name, count, strings.Join(parts, ", ")}
}

func serviceVersionStyles(svc Service) []lipgloss.Style {
	switch {
	case svc.Converged():
		return []lipgloss.Style{{}, tui.StyleOK, {}}
	case svc.Manual:
		return []lipgloss.Style{{}, tui.StyleWarn, tui.StyleWarn}
	default:
		return []lipgloss.Style{{}, tui.StyleAccent, tui.StyleMuted}
	}
}

func servicesSummary(svcs Services) string {
	if svcs.Converged() {
		return ""
	}
	msg := fmt.Sprintf("%d/%d rolling · %d pod(s) behind",
		len(svcs.Pending()), len(svcs.Items), svcs.StalePods())
	if svcs.NeedsOperator() {
		return tui.StyleWarn.Render(msg + " · needs an operator")
	}
	return tui.StyleAccent.Render(msg)
}
