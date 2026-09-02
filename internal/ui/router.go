package ui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/runlevel-six/binnacle/internal/build"
)

// DrillDownMsg signals that the operator selected a cluster from the
// fleet list and wants to see its full dashboard. The router catches it
// and builds a cluster screen.
type DrillDownMsg struct {
	Namespace string
	Name      string
}

// ClusterBuilder builds a single-cluster dashboard model for the named
// cluster. The router calls it on drill-down; the fleet model never
// builds a cluster screen itself. The returned cleanup function is
// called when the operator returns to the fleet list, and may be nil.
type ClusterBuilder func(namespace, name string) (*Model, func(), error)

// SextantModel is the top-level router for --server and --demo-fleet
// modes. It owns the fleet screen and, when the operator drills into a
// cluster, a cluster screen built by ClusterBuilder.
//
// In local mode the router does not exist: app.Run builds a single
// ui.Model directly. The router exists only when there is a fleet list
// to navigate from.
type SextantModel struct {
	fleet    *FleetModel
	builder  ClusterBuilder
	buildInfo build.Info

	screen  string // "fleet" or "cluster"
	cluster *Model
	cleanup func() // called when leaving the cluster screen; may be nil

	width, height int
}

// NewSextant builds a router around a fleet model. builder is called
// each time the operator drills into a cluster; the model it returns is
// discarded when the operator returns to the fleet.
func NewSextant(fleet *FleetModel, builder ClusterBuilder, info build.Info) *SextantModel {
	return &SextantModel{
		fleet:     fleet,
		builder:   builder,
		buildInfo: info,
		screen:    "fleet",
	}
}

// Init implements tea.Model.
func (m *SextantModel) Init() tea.Cmd {
	return m.fleet.Init()
}

// Update implements tea.Model.
func (m *SextantModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.screen == "cluster" && m.cluster != nil {
			m.cluster.Update(msg)
			return m, nil
		}
		fm, cmd := m.fleet.Update(msg)
		m.fleet = fm.(*FleetModel)
		return m, cmd

	case DrillDownMsg:
		cluster, cleanup, err := m.builder(msg.Namespace, msg.Name)
		if err != nil || cluster == nil {
			return m, nil
		}
		m.cluster = cluster
		m.cleanup = cleanup
		m.cluster.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
		m.screen = "cluster"
		return m, m.cluster.Init()

	case tea.KeyMsg:
		if m.screen == "cluster" && m.cluster != nil {
			if msg.Type == tea.KeyEsc || msg.String() == "backspace" {
				if m.cleanup != nil {
					m.cleanup()
				}
				m.cluster = nil
				m.cleanup = nil
				m.screen = "fleet"
				return m, nil
			}
			if msg.String() == "q" || msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			cm, cmd := m.cluster.Update(msg)
			m.cluster = cm.(*Model)
			return m, cmd
		}
		fm, cmd := m.fleet.Update(msg)
		m.fleet = fm.(*FleetModel)
		return m, cmd
	}

	// Forward all other messages to the active screen.
	if m.screen == "cluster" && m.cluster != nil {
		cm, cmd := m.cluster.Update(msg)
		m.cluster = cm.(*Model)
		return m, cmd
	}
	fm, cmd := m.fleet.Update(msg)
	m.fleet = fm.(*FleetModel)
	return m, cmd
}

// View implements tea.Model.
func (m *SextantModel) View() string {
	if m.screen == "cluster" && m.cluster != nil {
		return m.cluster.View()
	}
	return m.fleet.View()
}

var _ tea.Model = (*SextantModel)(nil)
