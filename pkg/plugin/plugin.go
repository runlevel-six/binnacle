// Package plugin defines how optional subsystems contribute to the dashboard.
//
// A plugin bundles up to three capabilities, each an optional interface:
// a [Source] that populates the store, a [BannerProvider] that contributes
// health-strip cells, and a [SummaryProvider] that contributes overview
// blocks. A plugin implementing only [Plugin] is legal but inert.
//
// Pane contribution — [tui.PaneProvider] — lives in [pkg/tui], not here,
// because it returns [tui.Pane], a terminal-rendered interface. A web server
// that imports this package for [BannerProvider] and [SummaryProvider] must
// not transitively depend on a terminal renderer. The [tui.Panes] function
// in [pkg/tui] collects panes from a [Registry]'s active plugins.
//
// # Detection, not configuration
//
// The guiding rule is that a subsystem the cluster does not have must not
// produce configuration errors or empty panes — it must be absent. A [Source]
// reports via [Source.Detect] whether its prerequisites exist, and a plugin
// that fails detection contributes nothing at all. Adding a plugin is
// therefore safe for every user, not only the ones running that subsystem.
//
// Detection failure is not fatal: an error is retained in the [DetectResult] for
// diagnostics, and a cluster we cannot probe should lose one pane rather than the
// whole dashboard.
//
// But absence and unreachability are different answers, and only the plugin can
// tell them apart. "The cluster does not run this" is permanent and means no
// pane; "I could not reach it just now" is a fact about one second, and detection
// runs only once, at startup. So a plugin decides its own answer: returning
// (false, err) means absent, and (true, err) means present but currently
// unreadable, which keeps the pane and lets its own polling recover. Returning
// absent for a transient failure deletes the pane for the whole session — see the
// OpenStack case, where the probe most likely to fail is a control-plane rollout
// and the pane most wanted is the one showing server migrations through it.
//
// # Dependencies come from the constructor
//
// Nothing here mentions Kubernetes, OpenStack, or a config file. A plugin
// receives its cluster clients, credentials, and settings when it is
// constructed, and closes over them. That keeps this package free of every
// data-source dependency and lets a test register a plugin backed by fixtures
// with no faking of client machinery.
package plugin

import (
	"context"
	"fmt"
	"sync"

	"github.com/runlevel-six/binnacle/pkg/health"
	"github.com/runlevel-six/binnacle/pkg/store"
)

// Plugin is the minimum contract: a stable name used for registration,
// diagnostics, and profile references.
//
// Names should be short, lowercase, and stable across releases — a profile
// may refer to one by name.
type Plugin interface {
	Name() string
}

// Source populates a [store.Store] with snapshots.
//
// Detect runs first and reports whether this source's prerequisites exist in
// the target environment. Run then executes until its context is canceled,
// publishing snapshots as it goes; it is expected to block.
type Source interface {
	Plugin

	// Detect reports whether this source can operate. Returning false, or an
	// error, excludes the plugin from the run.
	Detect(ctx context.Context) (bool, error)

	// Run publishes into s until ctx is canceled. Returning a nil error on
	// cancellation is conventional.
	Run(ctx context.Context, s *store.Store) error
}

// BannerProvider contributes cells to the health strip. Unlike panes, cells are
// rebuilt on every render, so this must be cheap and must not block.
type BannerProvider interface {
	Plugin
	Cells(s *store.Store) []health.Cell
}

// SummaryBlock is a titled group of lines contributed to the overview pane.
//
// It exists so that a subsystem can put its headline where the eye starts without
// the overview knowing what the subsystem is. The overview is core; Ceph, Cilium
// and OpenStack are plugins, and core must not import them — so a plugin hands
// over rendered lines and core decides where they go. A cluster without that
// subsystem contributes nothing, which is how the CAPI-and-Metal3-first shape
// survives: the slot exists, and on most clusters it stays empty.
//
// Two rules the overview enforces on whatever it is given, because a contributed
// block must not be able to disturb the layout above every other pane:
//
// A block gets a column of its own or it does not appear. Stacking it under an
// existing block would make the row taller, and the overview is a fixed-height
// row at the top of the grid — growing it pushes everything else down, at the
// exact moment a subsystem has started misbehaving.
//
// A block is trimmed to the height of the tallest core block, for the same
// reason. A plugin cannot lengthen the row by sending more lines.
type SummaryBlock struct {
	// Title labels the block, in the same style as the overview's own.
	Title string
	// Lines are the body, already formatted. Keep to three: see the height rule.
	Lines []string
}

// SummaryProvider is implemented by plugins that contribute a block to the
// overview pane.
//
// The false return is the whole point: a plugin is asked on every frame and
// answers no when its subsystem has nothing worth the space. That is what keeps a
// summary slot from becoming a permanent tax on the pane above everything — a
// healthy Ceph says nothing, a degraded one puts its headline where the eye
// already starts. See [SummaryBlock] for the rules the overview enforces on
// whatever it is handed.
type SummaryProvider interface {
	Plugin
	Summary(s *store.Store) (SummaryBlock, bool)
}

// DetectResult records one plugin's detection outcome.
type DetectResult struct {
	Name   string
	Active bool
	// Err is set when Detect failed. Active is then false, but the error is
	// kept so a diagnostic mode can explain why the plugin is missing —
	// "no permission to list CRDs" is far more useful than silence.
	Err error
}

// Registry holds the registered plugins and tracks which ones detection
// activated. A Registry is safe for concurrent use.
type Registry struct {
	mu      sync.Mutex
	plugins []Plugin
	names   map[string]bool
	// active is nil until Detect runs. Nil means "detection has not run", so
	// the accessors below can distinguish that from "nothing is active".
	active map[string]bool
	// errs holds the most recent Detect error per plugin, so Results can
	// explain an inactive plugin rather than just reporting it missing.
	errs map[string]error
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{names: map[string]bool{}}
}

// Register adds p. It returns an error if p has an empty name or if that name
// is already registered.
func (r *Registry) Register(p Plugin) error {
	name := p.Name()
	if name == "" {
		return fmt.Errorf("plugin: refusing to register a plugin with an empty name (%T)", p)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.names[name] {
		return fmt.Errorf("plugin: %q is already registered", name)
	}
	r.names[name] = true
	r.plugins = append(r.plugins, p)
	return nil
}

// MustRegister is [Registry.Register] that panics on failure. Intended for
// package-level registration of built-in plugins, where a duplicate name is a
// programming error rather than a runtime condition.
func (r *Registry) MustRegister(p Plugin) {
	if err := r.Register(p); err != nil {
		panic(err)
	}
}

// Plugins returns every registered plugin in registration order, whether or
// not detection activated it.
func (r *Registry) Plugins() []Plugin {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Plugin(nil), r.plugins...)
}

// Detect runs detection across every registered plugin and records which are
// active. Plugins that do not implement [Source] have nothing to probe and are
// always active.
//
// Results are returned in registration order. Detect may be called again to
// re-probe; the previous outcome is replaced.
func (r *Registry) Detect(ctx context.Context) []DetectResult {
	plugins := r.Plugins()

	type outcome struct {
		name   string
		active bool
		err    error
		probed bool
	}
	outcomes := make([]outcome, len(plugins))

	// Probes run concurrently, and this is a latency fix rather than a tidiness
	// one. Detection sits between the watchers starting and the first frame being
	// drawn, so its total is dead time on a black screen — and every probe is a
	// network round trip that can time out rather than fail: an exec into a pod,
	// an authentication against a cloud API. Serially, five plugins each waiting
	// out a timeout added up to tens of seconds before anything appeared. Run
	// together, the cost is the slowest single probe.
	//
	// Safe to parallelise: each plugin's Detect touches only its own fields, the
	// shared Kubernetes client is goroutine-safe, and the registry's own maps are
	// written after the wait.
	var wg sync.WaitGroup
	for i, p := range plugins {
		src, ok := p.(Source)
		if !ok {
			outcomes[i] = outcome{name: p.Name(), active: true, probed: true}
			continue
		}
		wg.Add(1)
		go func(i int, name string, src Source) {
			defer wg.Done()
			// The plugin's own answer is honored even when it also returns an
			// error. Forcing absent-on-error took away a plugin's ability to say
			// "present but not reachable right now", which is the truth during a
			// rollout: a cloud whose Keystone is mid-restart is still a cloud, and
			// deleting its pane for the session because one probe failed removes
			// the view at exactly the moment it is wanted. A plugin that means
			// absent returns false explicitly.
			active, err := src.Detect(ctx)
			outcomes[i] = outcome{name: name, active: active, err: err, probed: true}
		}(i, p.Name(), src)
	}
	wg.Wait()

	for _, o := range outcomes {
		if !o.probed {
			continue
		}
		r.setActive(o.name, o.active)
		r.setErr(o.name, o.err)
	}
	return r.Results()
}

// Results reports the outcome of the most recent [Registry.Detect] in
// registration order. Before Detect has run, every plugin reports inactive.
func (r *Registry) Results() []DetectResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]DetectResult, 0, len(r.plugins))
	for _, p := range r.plugins {
		out = append(out, DetectResult{
			Name:   p.Name(),
			Active: r.active[p.Name()],
			Err:    r.errs[p.Name()],
		})
	}
	return out
}

// ActiveSources returns the [Source] implementations that detection activated,
// in registration order. The caller runs them.
func (r *Registry) ActiveSources() []Source {
	var out []Source
	for _, p := range r.activePlugins() {
		if src, ok := p.(Source); ok {
			out = append(out, src)
		}
	}
	return out
}

// ActivePlugins returns the plugins that detection activated, in registration
// order. This is the exported accessor for [tui.Panes] and other callers that
// need to iterate active plugins without going through a specific interface.
func (r *Registry) ActivePlugins() []Plugin {
	return r.activePlugins()
}

// BannerCells collects health-strip cells from every active [BannerProvider],
// in registration order.
func (r *Registry) BannerCells(s *store.Store) []health.Cell {
	var out []health.Cell
	for _, p := range r.activePlugins() {
		if bp, ok := p.(BannerProvider); ok {
			out = append(out, bp.Cells(s)...)
		}
	}
	return out
}

// Summaries collects the overview blocks the active plugins offer, in
// registration order. A plugin that declines contributes nothing.
func (r *Registry) Summaries(s *store.Store) []SummaryBlock {
	var out []SummaryBlock
	for _, p := range r.activePlugins() {
		sp, ok := p.(SummaryProvider)
		if !ok {
			continue
		}
		if block, want := sp.Summary(s); want {
			out = append(out, block)
		}
	}
	return out
}

func (r *Registry) activePlugins() []Plugin {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Plugin, 0, len(r.plugins))
	for _, p := range r.plugins {
		if r.active[p.Name()] {
			out = append(out, p)
		}
	}
	return out
}

func (r *Registry) setActive(name string, active bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil {
		r.active = map[string]bool{}
	}
	r.active[name] = active
}

func (r *Registry) setErr(name string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.errs == nil {
		r.errs = map[string]error{}
	}
	if err == nil {
		delete(r.errs, name)
		return
	}
	r.errs[name] = err
}
