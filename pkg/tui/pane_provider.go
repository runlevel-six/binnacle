package tui

import (
	"fmt"

	"github.com/runlevel-six/binnacle/pkg/plugin"
	"github.com/runlevel-six/binnacle/pkg/store"
)

// PaneProvider contributes widgets to the dashboard. It is called once during
// startup, after detection, so a provider may assume its subsystem is present.
//
// This interface lives in pkg/tui, not pkg/plugin, because it returns [Pane] —
// a terminal-rendered interface. A web server that imports pkg/plugin for
// [plugin.BannerProvider] and [plugin.SummaryProvider] must not transitively
// depend on a terminal renderer. A plugin's pane code lives in a subpackage
// (e.g. internal/plugin/ceph/pane) that is imported only by the terminal
// client, not by the server.
type PaneProvider interface {
	Name() string
	Panes(s *store.Store) []Pane
}

// Panes collects widgets from every provider whose plugin is active in the
// registry, in the order the providers appear in the slice.
//
// A provider's Name must match the name of its data plugin in the registry.
// Providers whose plugin was not detected (or not registered) are skipped: a
// pane for an absent subsystem would render empty.
//
// It returns an error if two providers contribute the same pane ID. Pane IDs
// key focus tracking and jump keys, so a collision would make one of the two
// panes unreachable — better to fail at startup than to ship a dashboard with
// an inaccessible pane. The returned slice is still complete on error, so a
// caller that prefers to continue may.
func Panes(r *plugin.Registry, s *store.Store, providers []PaneProvider) ([]Pane, error) {
	active := map[string]bool{}
	for _, res := range r.Results() {
		active[res.Name] = res.Active
	}

	var out []Pane
	owner := map[string]string{}
	var err error
	for _, pp := range providers {
		if !active[pp.Name()] {
			continue
		}
		for _, pane := range pp.Panes(s) {
			if prev, dup := owner[pane.ID()]; dup && err == nil {
				err = fmt.Errorf("plugin: pane ID %q contributed by both %q and %q", pane.ID(), prev, pp.Name())
			}
			owner[pane.ID()] = pp.Name()
			out = append(out, pane)
		}
	}
	return out, err
}

// Summaries collects overview blocks from providers that also implement
// [plugin.SummaryProvider], skipping those whose plugin is not active in the
// registry. A provider that declines (returning false) contributes nothing.
func Summaries(r *plugin.Registry, s *store.Store, providers []PaneProvider) []plugin.SummaryBlock {
	active := map[string]bool{}
	for _, res := range r.Results() {
		active[res.Name] = res.Active
	}

	var out []plugin.SummaryBlock
	for _, pp := range providers {
		if !active[pp.Name()] {
			continue
		}
		sp, ok := pp.(plugin.SummaryProvider)
		if !ok {
			continue
		}
		if block, want := sp.Summary(s); want {
			out = append(out, block)
		}
	}
	return out
}
