// Package app wires configuration, kubeconfig resolution and data sources
// together. It is the seam between the command-line interface and the watchers,
// so that main stays flag parsing and nothing else.
package app

import (
	"context"
	"fmt"
	"io"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/runlevel-six/sextant/internal/build"
	"github.com/runlevel-six/sextant/internal/config"
	"github.com/runlevel-six/sextant/internal/kube"
	"github.com/runlevel-six/sextant/internal/ui"
	"github.com/runlevel-six/sextant/pkg/collect"
	"github.com/runlevel-six/sextant/pkg/plugin"
	"github.com/runlevel-six/sextant/pkg/profile"
	"github.com/runlevel-six/sextant/pkg/store"
	"github.com/runlevel-six/sextant/pkg/tui"
)

// Setup is everything resolved and ready to run.
type Setup struct {
	Resolved config.Resolved
	// Build is the binary's own version, which the dashboard shows in its
	// header. It arrives as a parameter rather than being filled in afterwards
	// so that a Setup is never half-built.
	Build      build.Info
	Kubeconfig *kube.Kubeconfig
	Store      *store.Store
	// Registry holds the plugins. Core panes are contributed directly rather
	// than through it, since they are unconditional; the registry exists for the
	// optional subsystems that have to detect themselves first.
	Registry *plugin.Registry
}

// Prepare loads the kubeconfig, selects a profile, and resolves the config
// against them.
//
// picker may be nil, in which case an ambiguous context pattern is an error
// naming the candidates rather than a prompt.
func Prepare(cfg config.Config, info build.Info, picker kube.Picker) (*Setup, error) {
	kc, err := kube.Load(cfg.KubeconfigPath)
	if err != nil {
		return nil, err
	}

	prof, err := selectProfile(cfg.Profile)
	if err != nil {
		return nil, err
	}

	resolved, err := cfg.Resolve(kc.Contexts(), prof, picker)
	if err != nil {
		return nil, err
	}
	return &Setup{
		Resolved:   resolved,
		Build:      info,
		Kubeconfig: kc,
		Store:      store.New(),
		Registry:   plugin.NewRegistry(),
	}, nil
}

// selectProfile loads the named profile.
//
// An unknown name is an error rather than a silent fallback: quietly ignoring a
// requested profile would produce a dashboard that starts, looks right, and
// reports the wrong things.
func selectProfile(name string) (profile.Profile, error) {
	return profile.NewLoader().Load(name)
}

// ListProfiles writes every loadable profile, marking the default.
func ListProfiles(w io.Writer) error {
	l := profile.NewLoader()
	names := l.Available()
	if len(names) == 0 {
		fmt.Fprintln(w, "No profiles found.")
		return nil
	}

	fmt.Fprintf(w, "%-16s  %s\n", "PROFILE", "DESCRIPTION")
	for _, name := range names {
		p, err := l.Load(name)
		if err != nil {
			fmt.Fprintf(w, "%-16s  error: %v\n", name, err)
			continue
		}
		desc := p.Description
		if name == profile.Default().Name {
			desc += " (default)"
		}
		fmt.Fprintf(w, "%-16s  %s\n", name, desc)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Searched:", strings.Join(l.Dirs, ", "), "and the built-in set.")
	fmt.Fprintln(w, "A profile may also be given as a path to a YAML file.")
	return nil
}

// ListThemes writes every color scheme, marking the default.
func ListThemes(w io.Writer) error {
	fmt.Fprintf(w, "%-10s  %s\n", "THEME", "DESCRIPTION")
	for _, t := range tui.Themes() {
		desc := t.Description
		if t.Name == tui.DefaultTheme().Name {
			desc += " (default)"
		}
		fmt.Fprintf(w, "%-10s  %s\n", t.Name, desc)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Select one with --theme, SEXTANT_THEME, or `theme:` in the config file.")
	fmt.Fprintln(w, "Press T in the dashboard to cycle through them.")
	return nil
}

// StartWatchers resolves the kubeconfig contexts and starts the collectors.
//
// It is the CLI's adapter onto [collect.Watch]: contexts in, rest.Configs out.
// Everything past that point is shared with any other consumer of the data
// layer, so the terminal dashboard cannot drift from what a second front end
// sees.
func (s *Setup) StartWatchers(ctx context.Context, reachability func(version string, err error)) error {
	mgmtCfg, err := s.Kubeconfig.RestConfig(s.Resolved.ManagementContext)
	if err != nil {
		return err
	}
	var workloadCfg *rest.Config
	if !s.Resolved.WorkloadIsManagement {
		workloadCfg, err = s.Kubeconfig.RestConfig(s.Resolved.WorkloadContext)
		if err != nil {
			return err
		}
	}
	return collect.Watch(ctx, collect.Options{
		Store:                s.Store,
		Registry:             s.Registry,
		Management:           mgmtCfg,
		Workload:             workloadCfg,
		Profile:              s.Resolved.Profile,
		ManagementNamespaces: s.Resolved.ManagementNamespaces,
		CAPIClusterName:      s.Resolved.CAPIClusterName,
		OSCloud:              s.Resolved.OSCloud,
		TargetVersion:        s.Resolved.TargetVersion,
	}, reachability)
}

// ListContexts writes every kubeconfig context, marking the current one and
// noting which sextant would select.
//
// This is the first thing to reach for when resolution picks the wrong cluster,
// so it shows what the resolver sees rather than a summary of it.
func ListContexts(w io.Writer, cfg config.Config) error {
	kc, err := kube.Load(cfg.KubeconfigPath)
	if err != nil {
		return err
	}
	entries := kc.Contexts()
	if len(entries) == 0 {
		fmt.Fprintln(w, "No contexts found. Check $KUBECONFIG or ~/.kube/config.")
		return nil
	}

	// Resolution may legitimately fail here; the listing is still useful, so
	// report the failure inline rather than aborting.
	var selectedMgmt, selectedWorkload string
	if prof, perr := selectProfile(cfg.Profile); perr == nil {
		if resolved, rerr := cfg.Resolve(entries, prof, nil); rerr == nil {
			selectedMgmt = resolved.ManagementContext
			selectedWorkload = resolved.WorkloadContext
		} else {
			fmt.Fprintf(w, "Resolution failed: %v\n\n", rerr)
		}
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	width := 0
	for _, n := range names {
		width = max(width, len(n))
	}

	fmt.Fprintf(w, "%-*s  %-9s  %s\n", width, "CONTEXT", "ROLE", "SERVER")
	for _, e := range sortedByName(entries) {
		role := ""
		bothSelected := selectedMgmt == selectedWorkload
		switch {
		case e.Name == selectedMgmt && bothSelected:
			// One context serving as both, which is the default single-cluster case.
			role = "both"
		case e.Name == selectedMgmt:
			role = "mgmt"
		case e.Name == selectedWorkload:
			role = "workload"
		case e.Current:
			role = "(current)"
		}
		fmt.Fprintf(w, "%-*s  %-9s  %s\n", width, e.Name, role, e.Server)
	}
	return nil
}

func sortedByName(entries []kube.ContextEntry) []kube.ContextEntry {
	out := append([]kube.ContextEntry(nil), entries...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Name < out[j-1].Name; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// silenceLibraryLogging stops client-go writing to stderr.
//
// A failing watch makes client-go's reflector log an error every few seconds, and
// stderr is the terminal the dashboard has taken over with an alternate screen —
// so the "helpful" message lands in the middle of the panes and garbles the
// display until the next full redraw. The failure itself is not lost: the watchers
// publish it into the store, which is what puts it in the pane it belongs to
// rather than across all of them.
//
// Only the interactive path silences this. `--debug-snapshot` is a diagnostic
// where a raw complaint from client-go may be exactly what someone needs to see.
func silenceLibraryLogging() {
	klog.LogToStderr(false)
	klog.SetOutput(io.Discard)
}

// Run starts the watchers and the dashboard, blocking until the user quits or
// ctx is canceled.
func Run(ctx context.Context, s *Setup) error {
	silenceLibraryLogging()

	// Before any pane is built, so nothing can render a frame in the palette it
	// is about to lose.
	tui.ApplyTheme(s.Resolved.Theme)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := s.StartWatchers(runCtx, nil); err != nil {
		return err
	}

	// Detection runs before panes are built so a plugin whose subsystem is
	// absent never reaches the catalog.
	collect.Activate(runCtx, s.Registry, s.Store)

	return s.runUI(runCtx)
}

// BuildModel assembles the dashboard from whatever is in the store.
//
// It is the one place panes are collected, so every entry point — the live
// dashboard, the demo, a headless render — shows the same screen. A demo that
// assembled its own would stop being evidence about the real one.
func (s *Setup) BuildModel() (*ui.Model, error) {
	// The overview asks the registry for contributed blocks on every render, so a
	// subsystem's headline appears the moment it has one. Wired here because this
	// is where both halves are in scope.
	panes := ui.CorePanes(s.Store, s.Resolved, func() []tui.SummaryBlock {
		return s.Registry.Summaries(s.Store)
	})
	pluginPanes, err := s.Registry.Panes(s.Store)
	if err != nil {
		// A duplicate pane ID is a programming error, and one of the two panes
		// would be unreachable by focus and jump keys. Fail rather than ship a
		// dashboard with an inaccessible pane.
		return nil, err
	}
	// Grouping runs here, after every pane is known, because a group's members
	// come from different plugins and no plugin can see the others. A group whose
	// second member was not detected collapses to a single section rather than
	// disappearing.
	all := tui.Group(append(panes, pluginPanes...))
	return ui.New(s.Resolved, s.Store, s.Registry, all).WithBuild(s.Build), nil
}

// runUI builds the dashboard and blocks until it exits.
func (s *Setup) runUI(ctx context.Context) error {
	model, err := s.BuildModel()
	if err != nil {
		return err
	}
	// No mouse tracking. Asking for it puts the terminal in a mode where clicks and
	// drags are delivered to us instead of to the terminal, which takes away
	// select-and-copy — and this dashboard's whole output is names an operator
	// copies into the next command. Nothing here reads a mouse event, so the
	// tracking was pure cost.
	//
	// Anything added later that wants the mouse — click a pane to focus it, say —
	// has to be worth losing selection for, or has to keep it: a terminal only
	// gives selection back on a modifier drag, and which modifier depends on the
	// terminal.
	program := tea.NewProgram(model,
		tea.WithAltScreen(),
		tea.WithContext(ctx),
	)
	_, err = program.Run()
	return err
}
