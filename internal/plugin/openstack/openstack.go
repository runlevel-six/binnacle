// Package openstack reports an OpenStack cloud's control-plane health: whether
// each service's agents are up, and whether any have been deliberately disabled.
//
// This plugin is unlike the others. It talks to an API rather than a cluster, so
// it detects on a resolvable `clouds.yaml` profile instead of probing Kubernetes,
// and it has no exec tier — it either authenticates or it does not.
//
// # Up and enabled are different questions
//
// OpenStack reports two independent states per agent, and conflating them is the
// classic mistake. `State` is up or down: whether the agent is reporting in.
// `Status` is enabled or disabled: whether an operator has told the scheduler to
// stop using it. A disabled agent is *intentional* — that is how a compute node is
// drained before maintenance — while a down agent is a failure. Rendering both as
// "not working" would cry wolf through every planned maintenance window, which is
// precisely when this dashboard is being watched.
package openstack

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	blockservices "github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/services"
	novaservices "github.com/gophercloud/gophercloud/v2/openstack/compute/v2/services"
	"github.com/gophercloud/gophercloud/v2/openstack/config"
	"github.com/gophercloud/gophercloud/v2/openstack/config/clouds"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/agents"

	"github.com/runlevel-six/sextant/pkg/store"
	osstate "github.com/runlevel-six/sextant/pkg/subsystem/openstack"
	"github.com/runlevel-six/sextant/pkg/tui"
)

// Name is the plugin's registration name.
const Name = "openstack"

// KeyState holds a State.
const KeyState = osstate.KeyState

// The OpenStack state types live in pkg/subsystem/openstack so a consumer
// outside this module can read them. Aliased here so this package's own code is
// unchanged.
type (
	// Count is an alias for [osstate.Count].
	Count = osstate.Count
	// Inventory is an alias for [osstate.Inventory].
	Inventory = osstate.Inventory
	// Migration is an alias for [osstate.Migration].
	Migration = osstate.Migration
	// BrokenServer is an alias for [osstate.BrokenServer].
	BrokenServer = osstate.BrokenServer
	// Drain is an alias for [osstate.Drain].
	Drain = osstate.Drain
	// Migrations is an alias for [osstate.Migrations].
	Migrations = osstate.Migrations
	// Shown is an alias for [osstate.Shown].
	Shown = osstate.Shown
	// Agent is an alias for [osstate.Agent].
	Agent = osstate.Agent
	// ServiceSummary is an alias for [osstate.ServiceSummary].
	ServiceSummary = osstate.ServiceSummary
	// State is an alias for [osstate.State].
	State = osstate.State
)

// FailedWindow is how long a failed migration stays worth showing.
const FailedWindow = osstate.FailedWindow

// Service names, used as the grouping key.
const (
	ServiceCompute      = osstate.ServiceCompute
	ServiceNetwork      = osstate.ServiceNetwork
	ServiceBlockStorage = osstate.ServiceBlockStorage
)

// Interpretation that traveled with the types.
var (
	Active          = osstate.Active
	Failed          = osstate.Failed
	LatestPerServer = osstate.LatestPerServer
	ShortType       = osstate.ShortType
	ShortStatus     = osstate.ShortStatus
	DrainingHosts   = osstate.DrainingHosts
)

// pollInterval is how often the cloud is queried. Several API round trips per
// poll, so this is unhurried.
const pollInterval = 30 * time.Second

// inventoryEvery is how many polls pass between inventory counts.
//
// The agent and migration polls are two cheap calls; the inventory is eight
// list calls, two of them across every project in the cloud. Those counts are
// also the slowest-changing thing on the dashboard — a cloud does not gain
// projects by the minute — so paying for them every two minutes rather than
// every thirty seconds costs the operator nothing they would notice and saves
// the cloud three quarters of the load.
const inventoryEvery = 4

// Settings is the plugin's configuration: the profile's plugin block, with the
// flag- and environment-sourced values layered over it.
type Settings struct {
	// Cloud is the clouds.yaml profile name. Required: there is no sensible
	// default, since the name is chosen by whoever wrote the file.
	Cloud string
	// Namespace pins where the OpenStack workloads run, for the service-version
	// view. Empty derives it from the workloads themselves.
	Namespace string
	// TargetVersion is the Kubernetes version being rolled out, if the operator
	// asserted one. It is not a profile setting — it comes from --target-version
	// or the environment — and it reaches this plugin because the mode-aware
	// pane needs the same rollout signal the core panes use.
	TargetVersion string
}

// SettingsFrom reads a profile's plugin block.
func SettingsFrom(raw map[string]any) Settings {
	var s Settings
	if v, ok := raw["cloud"].(string); ok {
		s.Cloud = v
	}
	if v, ok := raw["namespace"].(string); ok {
		s.Namespace = v
	}
	return s
}

// Plugin observes an OpenStack cloud.
type Plugin struct {
	settings Settings

	provider *gophercloud.ProviderClient
	endpoint gophercloud.EndpointOpts

	// migrationsMV is the Nova microversion negotiated for /os-migrations, and
	// migrationsMVKnown records that the negotiation reached a verdict — an
	// empty string is a real answer, meaning the cloud offers nothing this poll
	// can use. See [Plugin.migrationsMicroversion].
	//
	// Written from the poll goroutine only, as with provider and endpoint above:
	// Detect runs before Run starts, and Run polls sequentially.
	migrationsMV      string
	migrationsMVKnown bool
}

// New builds the plugin.
func New(settings Settings) *Plugin { return &Plugin{settings: settings} }

// Name implements plugin.Plugin.
func (p *Plugin) Name() string { return Name }

// Detect reports whether an OpenStack cloud is wanted here.
//
// Absent configuration is not an error: most users of this tool do not run
// OpenStack, so an unnamed cloud simply means the plugin is not wanted, and that
// is the one case that answers no.
//
// A *named* cloud answers yes even when it cannot be reached, and this is
// deliberate. Detection runs once, at startup, and reachability is not a property
// of the configuration — it is a property of this second. Keystone runs on the
// cluster being upgraded, so the moment most likely to fail authentication is a
// control-plane rollout, which is exactly the moment the migrations pane is worth
// having. Answering no there would delete the pane, its banner cell and the
// server-migration view for the rest of the session, over a cloud that came back
// thirty seconds later.
//
// The connection is still attempted, so a genuine misconfiguration is reported
// rather than waited on: the error goes into the published state, where the pane
// shows it and the next poll retries. See [Plugin.poll].
func (p *Plugin) Detect(ctx context.Context) (bool, error) {
	if p.settings.Cloud == "" {
		return false, nil
	}

	// Bounded, because detection blocks the first frame and this is the one probe
	// that talks to something outside the cluster. Neither gophercloud's client nor
	// the context it is handed carries a deadline of its own, so a Keystone that is
	// simply not answering — a control plane mid-rollout, a VIP that has moved —
	// stalls startup for as long as the operating system's connect timeout, which
	// is far longer than anyone will wait at a blank screen. Authentication against
	// a healthy cloud is well under a second.
	authCtx, cancel := context.WithTimeout(ctx, detectTimeout)
	defer cancel()
	if _, _, err := p.connect(authCtx); err != nil {
		// Present but unreachable. Reported, not disqualifying — and not cached, so
		// the next poll tries again with its own deadline.
		return true, err
	}
	return true, nil
}

// detectTimeout bounds the authentication probe in [Plugin.Detect].
const detectTimeout = 5 * time.Second

// connect authenticates once and caches the provider.
//
// gophercloud reauthenticates internally when AllowReauth is set, so the token
// expiring mid-session does not need handling here.
func (p *Plugin) connect(ctx context.Context) (*gophercloud.ProviderClient, gophercloud.EndpointOpts, error) {
	if p.provider != nil {
		return p.provider, p.endpoint, nil
	}

	authOpts, endpointOpts, tlsConfig, err := parseClouds(p.settings.Cloud)
	if err != nil {
		return nil, gophercloud.EndpointOpts{}, err
	}
	authOpts.AllowReauth = true

	provider, err := config.NewProviderClient(ctx, authOpts, config.WithTLSConfig(tlsConfig))
	if err != nil {
		return nil, gophercloud.EndpointOpts{}, fmt.Errorf("authenticate to cloud %q: %w", p.settings.Cloud, err)
	}
	p.provider, p.endpoint = provider, endpointOpts
	return provider, endpointOpts, nil
}

// parseClouds loads a clouds.yaml profile.
//
// The recover is not defensive habit: gophercloud's loader *panics* on a
// malformed entry rather than returning an error, and the usual cause is the flat
// legacy layout that older python-openstackclient tolerates but which requires a
// nested `auth:` block here. Letting that panic escape would take the whole
// dashboard down over one misconfigured file, so it is converted into an error
// that names the likely cause.
//
// The native clouds loader is used rather than utils/clientconfig, because
// clientconfig treats the `cloud:` field as a reference into clouds-public.yaml
// and fails when the profile is not there.
func parseClouds(cloud string) (
	ao gophercloud.AuthOptions,
	eo gophercloud.EndpointOpts,
	tlsConfig *tls.Config,
	err error,
) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf(
				"clouds.yaml entry %q is malformed (most often a missing nested `auth:` block): %v",
				cloud, r)
		}
	}()
	return clouds.Parse(clouds.WithCloudName(cloud))
}

// Run polls until ctx is canceled.
//
// All three sources are polled regardless of which the mode-aware pane is
// currently showing. Gating a poll on the mode would look like an easy saving and
// would misfire in both directions: a rollout starting is exactly when a cold
// migration list is least welcome, and inventory counts frozen at whatever they
// were when the rollout began would be displayed, undated, as current.
func (p *Plugin) Run(ctx context.Context, s *store.Store) error {
	polls := 0
	publish := func() {
		// The agent poll comes first because the migration poll consumes it: a
		// disabled compute host is what a drain looks like, and that is what
		// decides whether an old unresolved failure is in the way. Reading it
		// from the same publish keeps the two consistent — a pane joining two
		// independently-timed snapshots would promote and demote rows on the
		// seam between them.
		state := p.poll(ctx)
		s.Put(KeyState, state)
		s.Put(KeyMigrations, p.pollMigrations(ctx, DrainingHosts(state)))
		if polls%inventoryEvery == 0 {
			s.Put(KeyInventory, p.pollInventory(ctx))
		}
		polls++
	}
	publish()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			publish()
		}
	}
}

func (p *Plugin) poll(ctx context.Context) State {
	state := State{Cloud: p.settings.Cloud, UpdatedAt: time.Now()}

	provider, endpointOpts, err := p.connect(ctx)
	if err != nil {
		state.Err = err
		return state
	}
	state.Region = endpointOpts.Region

	// Each service is queried independently. One unavailable service — Cinder not
	// deployed, say — must not hide the others, so its failure is recorded on its
	// own summary.
	for _, fetch := range []func(context.Context, *gophercloud.ProviderClient, gophercloud.EndpointOpts) ([]Agent, error){
		computeAgents, networkAgents, blockStorageAgents,
	} {
		agents, err := fetch(ctx, provider, endpointOpts)
		state.Agents = append(state.Agents, agents...)
		if err != nil {
			state.Services = append(state.Services, ServiceSummary{
				Service: serviceOf(err), Err: err,
			})
		}
	}

	state.Services = append(state.Services, summarize(state.Agents)...)
	sort.Slice(state.Services, func(i, j int) bool {
		return state.Services[i].Service < state.Services[j].Service
	})
	sort.Slice(state.Agents, func(i, j int) bool {
		a, b := state.Agents[i], state.Agents[j]
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		if a.Binary != b.Binary {
			return a.Binary < b.Binary
		}
		return a.Host < b.Host
	})
	return state
}

// serviceError tags an error with the service that produced it, so a failure can
// be attributed on its own summary row rather than failing the poll.
type serviceError struct {
	service string
	err     error
}

func (e *serviceError) Error() string { return e.err.Error() }
func (e *serviceError) Unwrap() error { return e.err }

func serviceOf(err error) string {
	var se *serviceError
	if ok := asServiceError(err, &se); ok {
		return se.service
	}
	return "unknown"
}

func asServiceError(err error, target **serviceError) bool {
	// errors.As walks the chain, and unlike a hand-rolled loop it also handles an
	// error that unwraps to several.
	return errors.As(err, target)
}

func computeAgents(ctx context.Context, provider *gophercloud.ProviderClient, eo gophercloud.EndpointOpts) ([]Agent, error) {
	client, err := openstack.NewComputeV2(provider, eo)
	if err != nil {
		return nil, &serviceError{ServiceCompute, err}
	}
	page, err := novaservices.List(client, novaservices.ListOpts{}).AllPages(ctx)
	if err != nil {
		return nil, &serviceError{ServiceCompute, err}
	}
	items, err := novaservices.ExtractServices(page)
	if err != nil {
		return nil, &serviceError{ServiceCompute, err}
	}

	out := make([]Agent, 0, len(items))
	for _, s := range items {
		out = append(out, Agent{
			Service:   ServiceCompute,
			Binary:    s.Binary,
			Host:      s.Host,
			Zone:      s.Zone,
			Up:        strings.EqualFold(s.State, "up"),
			Enabled:   !strings.EqualFold(s.Status, "disabled"),
			UpdatedAt: s.UpdatedAt,
		})
	}
	return out, nil
}

func networkAgents(ctx context.Context, provider *gophercloud.ProviderClient, eo gophercloud.EndpointOpts) ([]Agent, error) {
	client, err := openstack.NewNetworkV2(provider, eo)
	if err != nil {
		return nil, &serviceError{ServiceNetwork, err}
	}
	page, err := agents.List(client, agents.ListOpts{}).AllPages(ctx)
	if err != nil {
		return nil, &serviceError{ServiceNetwork, err}
	}
	items, err := agents.ExtractAgents(page)
	if err != nil {
		return nil, &serviceError{ServiceNetwork, err}
	}

	out := make([]Agent, 0, len(items))
	for _, a := range items {
		out = append(out, Agent{
			Service: ServiceNetwork,
			Binary:  a.Binary,
			Host:    a.Host,
			// Neutron reports liveness as a boolean rather than a string, and
			// administrative state separately — the same two questions under
			// different names.
			Up:        a.Alive,
			Enabled:   a.AdminStateUp,
			UpdatedAt: a.HeartbeatTimestamp,
		})
	}
	return out, nil
}

func blockStorageAgents(ctx context.Context, provider *gophercloud.ProviderClient, eo gophercloud.EndpointOpts) ([]Agent, error) {
	client, err := openstack.NewBlockStorageV3(provider, eo)
	if err != nil {
		return nil, &serviceError{ServiceBlockStorage, err}
	}
	page, err := blockservices.List(client, blockservices.ListOpts{}).AllPages(ctx)
	if err != nil {
		return nil, &serviceError{ServiceBlockStorage, err}
	}
	items, err := blockservices.ExtractServices(page)
	if err != nil {
		return nil, &serviceError{ServiceBlockStorage, err}
	}

	out := make([]Agent, 0, len(items))
	for _, s := range items {
		out = append(out, Agent{
			Service:   ServiceBlockStorage,
			Binary:    s.Binary,
			Host:      s.Host,
			Zone:      s.Zone,
			Up:        strings.EqualFold(s.State, "up"),
			Enabled:   !strings.EqualFold(s.Status, "disabled"),
			UpdatedAt: s.UpdatedAt,
		})
	}
	return out, nil
}

// Summarize groups agents by service.
//
// Exported for testing, since the grouping carries the up-versus-enabled logic
// that the whole plugin turns on.
func Summarize(agents []Agent) []ServiceSummary { return summarize(agents) }

func summarize(agents []Agent) []ServiceSummary {
	index := map[string]*ServiceSummary{}
	var order []string
	downBinaries := map[string]map[string]bool{}

	for _, a := range agents {
		s, seen := index[a.Service]
		if !seen {
			s = &ServiceSummary{Service: a.Service}
			index[a.Service] = s
			order = append(order, a.Service)
			downBinaries[a.Service] = map[string]bool{}
		}
		s.Total++
		if a.Up {
			s.Up++
		}
		if !a.Enabled {
			s.Disabled++
		}
		// Only an enabled agent that is down is a problem. A disabled one is a
		// drained node, which is the normal state during maintenance.
		if !a.Healthy() {
			downBinaries[a.Service][a.Binary] = true
		}
	}

	out := make([]ServiceSummary, 0, len(order))
	for _, name := range order {
		s := index[name]
		for binary := range downBinaries[name] {
			s.DownBinaries = append(s.DownBinaries, binary)
		}
		sort.Strings(s.DownBinaries)
		out = append(out, *s)
	}
	return out
}

// Cells implements plugin.BannerProvider.
func (p *Plugin) Cells(s *store.Store) []tui.BannerCell {
	state, ok := store.Get[State](s, KeyState)
	if !ok {
		return nil
	}

	cell := tui.BannerCell{Name: "OpenStack"}
	switch {
	case state.Err != nil:
		cell.Status = tui.BannerErr
		cell.Detail = "unreachable"
	case len(state.Agents) == 0 && len(state.Services) == 0:
		cell.Status = tui.BannerLoading
	default:
		down := state.DownAgents()
		disabled := state.DisabledAgents()
		switch {
		case len(down) > 0:
			cell.Status = tui.BannerErr
			cell.Detail = fmt.Sprintf("%d agent(s) down", len(down))
		case len(disabled) > 0:
			// Deliberate, so amber rather than red — but still worth showing,
			// since a node left disabled after maintenance is a common oversight.
			cell.Status = tui.BannerWarn
			cell.Detail = fmt.Sprintf("%d disabled", len(disabled))
		default:
			cell.Status = tui.BannerOK
		}
	}

	// Whether the deployed services match their charts, from the cluster rather
	// than the cloud — the agent APIs cover three projects out of a dozen and
	// carry no version at all, so this is the only place it can come from.
	if svcs, ok := CollectServices(s, p.settings.Namespace); ok && !svcs.Converged() {
		note := fmt.Sprintf("%d pod(s) behind", svcs.StalePods())
		if svcs.NeedsOperator() {
			note += " (manual)"
			cell.Status = cell.Status.Worse(tui.BannerWarn)
		}
		if cell.Detail != "" {
			note = cell.Detail + ", " + note
		}
		cell.Detail = note
	}
	return []tui.BannerCell{cell}
}

// Panes implements plugin.PaneProvider.
//
// Control-plane health comes first: an agent that is down is a fault, whereas the
// second pane is either activity to watch or an inventory to browse.
func (p *Plugin) Panes(s *store.Store) []tui.Pane {
	return []tui.Pane{
		newPane(s),
		newServicesPane(s, p.settings.Namespace),
		newResourcesPane(s),
		newCloudPane(s, p.settings.TargetVersion),
	}
}
