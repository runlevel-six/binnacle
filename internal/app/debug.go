package app

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/runlevel-six/sextant/internal/core/capi"
	"github.com/runlevel-six/sextant/internal/core/model"
	"github.com/runlevel-six/sextant/internal/core/rollout"
	"github.com/runlevel-six/sextant/internal/plugin/ceph"
	"github.com/runlevel-six/sextant/internal/plugin/cilium"
	"github.com/runlevel-six/sextant/internal/plugin/kube"
	"github.com/runlevel-six/sextant/internal/plugin/metallb"
	"github.com/runlevel-six/sextant/internal/plugin/openstack"
	"github.com/runlevel-six/sextant/internal/plugin/ovn"
	"github.com/runlevel-six/sextant/pkg/store"
)

// DebugSnapshot starts the watchers, waits for caches to warm, and writes one
// line per data source.
//
// This is the diagnostic that answers "can sextant see my cluster at all",
// separately from any question about rendering. It is the first thing to ask for
// in a bug report, which is why it prints what each source actually returned
// rather than a pass or fail.
func DebugSnapshot(ctx context.Context, s *Setup, w io.Writer, wait time.Duration, verbose bool) error {
	fmt.Fprintf(w, "management context: %s\n", s.Resolved.ManagementContext)
	if s.Resolved.WorkloadIsManagement {
		fmt.Fprintf(w, "workload context:   %s (same cluster)\n", s.Resolved.WorkloadContext)
	} else {
		fmt.Fprintf(w, "workload context:   %s\n", s.Resolved.WorkloadContext)
	}
	fmt.Fprintf(w, "profile:            %s\n", s.Resolved.Profile.Name)
	if ns := s.Resolved.ManagementNamespaces; len(ns) > 0 {
		fmt.Fprintf(w, "capi namespaces:    %v\n", ns)
	} else {
		fmt.Fprintf(w, "capi namespaces:    (all)\n")
	}
	// Worth a line of its own: on a management cluster owning several clusters,
	// this is the difference between one fleet and all of them, and a pattern that
	// failed to match is otherwise invisible.
	if name := s.Resolved.CAPIClusterName; name != "" {
		fmt.Fprintf(w, "capi cluster:       %s\n", name)
	} else {
		fmt.Fprintf(w, "capi cluster:       (all clusters in scope)\n")
	}
	fmt.Fprintln(w)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Report reachability once up front. Without this, an unreachable cluster
	// shows the same connection error on every line below and the real problem
	// is buried.
	reachable := true
	report := func(serverVersion string, err error) {
		if err != nil {
			reachable = false
			fmt.Fprintf(w, "management cluster: UNREACHABLE\n  %v\n\n", err)
			return
		}
		fmt.Fprintf(w, "management cluster: reachable (Kubernetes %s)\n\n", serverVersion)
	}
	if err := s.StartWatchers(runCtx, report); err != nil {
		return err
	}

	if !reachable {
		fmt.Fprintln(w, "Skipping the per-source report: nothing can be read until the")
		fmt.Fprintln(w, "cluster is reachable. Check your kubeconfig context and credentials,")
		fmt.Fprintln(w, "then run --list-contexts to confirm which context was selected.")
		return nil
	}

	// Detection before the wait, so its result is visible even if a source is
	// slow to publish.
	results := s.Registry.Detect(runCtx)
	if len(results) > 0 {
		fmt.Fprintln(w, "plugins:")
		for _, r := range results {
			switch {
			case r.Err != nil:
				fmt.Fprintf(w, "  %-12s not available: %v\n", r.Name, r.Err)
			case r.Active:
				fmt.Fprintf(w, "  %-12s detected\n", r.Name)
			default:
				fmt.Fprintf(w, "  %-12s absent\n", r.Name)
			}
		}
		fmt.Fprintln(w)
	}
	for _, src := range s.Registry.ActiveSources() {
		source := src
		go func() { _ = source.Run(runCtx, s.Store) }()
	}

	fmt.Fprintf(w, "waiting %s for caches to warm...\n\n", wait)
	select {
	case <-time.After(wait):
	case <-ctx.Done():
		return ctx.Err()
	}

	for _, r := range reportRows(s.Store) {
		fmt.Fprintf(w, "%-28s %s\n", r.key, r.summary)
		if verbose && r.sample != "" {
			fmt.Fprintf(w, "%-28s   e.g. %s\n", "", r.sample)
		}
	}

	fmt.Fprintln(w)
	st := rollout.Detect(s.Store, s.Resolved.TargetVersion)
	switch {
	case st.Active && st.Asserted:
		fmt.Fprintf(w, "rollout: asserted via target version %s\n", st.TargetVersion)
	case st.Active:
		fmt.Fprintf(w, "rollout: in progress — %v\n", st.Rolling)
	default:
		fmt.Fprintln(w, "rollout: none detected (steady state)")
	}
	return nil
}

type reportRow struct {
	key     string
	summary string
	sample  string
}

// reportRows summarizes every well-known key. A key with no data yet is reported
// as pending rather than omitted, so a source that never published is visible
// instead of merely absent from the output.
func reportRows(s *store.Store) []reportRow {
	rows := []reportRow{
		describe[model.Cluster](s, model.KeyMgmtClusters, func(c model.Cluster) string {
			return fmt.Sprintf("%s/%s phase=%s cp=%d/%d workers=%d/%d",
				c.Namespace, c.Name, c.Phase,
				c.ControlPlane.UpToDate, c.ControlPlane.Desired,
				c.Workers.UpToDate, c.Workers.Desired)
		}),
		describe[model.KubeadmControlPlane](s, model.KeyMgmtKCPs, func(k model.KubeadmControlPlane) string {
			return fmt.Sprintf("%s/%s %s uptodate=%d/%d", k.Namespace, k.Name, k.Version,
				k.UpToDateReplicas, k.DesiredReplicas)
		}),
		describe[model.MachineDeployment](s, model.KeyMgmtMachineDeployments, func(m model.MachineDeployment) string {
			return fmt.Sprintf("%s/%s %s uptodate=%d/%d", m.Namespace, m.Name, m.Version,
				m.UpToDateReplicas, m.DesiredReplicas)
		}),
		describe[model.Machine](s, model.KeyMgmtMachines, func(m model.Machine) string {
			return fmt.Sprintf("%s/%s phase=%s node=%s infra=%s/%s",
				m.Namespace, m.Name, m.Phase, m.NodeName, m.InfraKind, m.InfraName)
		}),
		describe[model.Metal3Cluster](s, model.KeyMgmtMetal3Clusters, func(c model.Metal3Cluster) string {
			return fmt.Sprintf("%s/%s ready=%v", c.Namespace, c.Name, c.Ready)
		}),
		describe[model.Metal3Machine](s, model.KeyMgmtMetal3Machines, func(m model.Metal3Machine) string {
			return fmt.Sprintf("%s/%s ready=%v bmh=%s/%s", m.Namespace, m.Name, m.Ready,
				m.BMHNamespace, m.BMHName)
		}),
		describe[model.BareMetalHost](s, model.KeyMgmtBareMetalHosts, func(b model.BareMetalHost) string {
			return fmt.Sprintf("%s/%s state=%s op=%s consumer=%s",
				b.Namespace, b.Name, b.State, b.OperationalStatus, b.ConsumerName)
		}),
		describe[model.Event](s, model.KeyMgmtEvents, eventSample),
		describe[model.Node](s, model.KeyWorkloadNodes, func(n model.Node) string {
			return fmt.Sprintf("%s %s role=%s %s cpu=%dm/%dm",
				n.Name, n.DisplayStatus(), n.Role, n.Version, n.RequestedCPU, n.AllocatableCPU)
		}),
		describe[model.Pod](s, model.KeyWorkloadPods, func(p model.Pod) string {
			return fmt.Sprintf("%s/%s %d/%d %s restarts=%d",
				p.Namespace, p.Name, p.ReadyReady, p.ReadyTotal, p.Status, p.Restarts)
		}),
		describe[model.Workload](s, model.KeyWorkloadWorkloads, func(wl model.Workload) string {
			return fmt.Sprintf("%s %s/%s %d/%d", wl.Kind, wl.Namespace, wl.Name, wl.Ready, wl.Desired)
		}),
		describe[model.Event](s, model.KeyWorkloadEvents, eventSample),
	}
	rows = append(rows, joinRow(s))
	rows = append(rows, pluginRows(s)...)
	return rows
}

// pluginRows summarizes each plugin's published state, including the tier it is
// operating at — the thing to check when a pane looks thinner than expected.
func pluginRows(s *store.Store) []reportRow {
	var out []reportRow

	if state, ok := store.Get[metallb.State](s, metallb.KeyState); ok {
		row := reportRow{key: metallb.KeyState}
		if state.Err != nil {
			row.summary = "error: " + state.Err.Error()
		} else {
			ns := state.Namespace
			if ns == "" {
				ns = "(not located)"
			}
			row.summary = fmt.Sprintf("ns=%s, %d pool(s), %d LB service(s), speaker %d/%d",
				ns, len(state.Pools), len(state.Services), state.SpeakerReady, state.SpeakerDesired)
			if pending := state.PendingServices(); pending > 0 {
				row.summary += fmt.Sprintf(", %d pending", pending)
			}
			if len(state.Pools) > 0 {
				p := state.Pools[0]
				row.sample = fmt.Sprintf("%s %v advertised=%v", p.Name, p.Addresses, p.Advertised)
			}
		}
		out = append(out, row)
	}

	if state, ok := store.Get[cilium.State](s, cilium.KeyState); ok {
		row := reportRow{key: cilium.KeyState}
		switch {
		case state.Err != nil:
			row.summary = "error: " + state.Err.Error()
		default:
			row.summary = fmt.Sprintf("tier=%s, agents %d/%d",
				state.Tier, state.AgentsReady, state.AgentsDesired)
			if state.TierReason != "" {
				row.summary += " (" + state.TierReason + ")"
			}
			// Name the sections that would not decode. Without this, an empty
			// version and a zero IPAM look identical to a cluster that simply does
			// not report them, and there is nothing to act on.
			if len(state.Status.Unreadable) > 0 {
				row.summary += fmt.Sprintf(", unreadable: %v", state.Status.Unreadable)
			}
			if state.Tier == kube.TierFull {
				// The raw mode, named by its actual field: "kube-proxy=True" reads
				// as kube-proxy being enabled, when it means Cilium has replaced it.
				row.sample = fmt.Sprintf("version=%q kube-proxy-replacement=%q ipam=%d/%d on %s",
					state.Status.Version, state.Status.KubeProxyReplacement,
					state.Status.IPAM.Used, state.Status.IPAM.Total(), state.Pod)
			}
		}
		out = append(out, row)
	}
	if state, ok := store.Get[ceph.State](s, ceph.KeyState); ok {
		row := reportRow{key: ceph.KeyState}
		switch {
		case state.Err != nil:
			row.summary = "error: " + state.Err.Error()
		case state.Tier != kube.TierFull:
			row.summary = fmt.Sprintf("tier=%s (%s)", state.Tier, state.TierReason)
		default:
			st := state.Status
			row.summary = fmt.Sprintf("%s, mons %d/%d, osds %d/%d, pgs %d/%d clean, %d%% used",
				st.Health, st.Mons.InQuorum, st.Mons.Total, st.OSDs.Up, st.OSDs.Total,
				st.PGs.CleanPGs(), st.PGs.Total, st.PGs.UsedPercent())
			if len(st.Unreadable) > 0 {
				row.summary += fmt.Sprintf(", unreadable: %v", st.Unreadable)
			}
			mgr := st.Mgr.Active
			if st.Mgr.ActiveUnknown() {
				mgr = "(name not reported)"
			}
			row.sample = fmt.Sprintf("mgr=%s +%d standby, %d checks on %s",
				mgr, st.Mgr.Standbys, len(st.Checks), state.Pod)
		}
		out = append(out, row)
	}

	if state, ok := store.Get[openstack.State](s, openstack.KeyState); ok {
		row := reportRow{key: openstack.KeyState}
		if state.Err != nil {
			row.summary = "error: " + state.Err.Error()
		} else {
			parts := make([]string, 0, len(state.Services))
			for _, svc := range state.Services {
				if svc.Err != nil {
					parts = append(parts, svc.Service+" unavailable")
					continue
				}
				text := fmt.Sprintf("%s %d/%d up", svc.Service, svc.Up, svc.Total)
				if svc.Disabled > 0 {
					text += fmt.Sprintf(" (%d disabled)", svc.Disabled)
				}
				parts = append(parts, text)
			}
			row.summary = fmt.Sprintf("cloud=%s region=%s: %s",
				state.Cloud, state.Region, strings.Join(parts, ", "))
			if down := state.DownAgents(); len(down) > 0 {
				row.sample = fmt.Sprintf("down: %s@%s", down[0].Binary, openstack.ShortHost(down[0].Host))
			}
		}
		out = append(out, row)
	}

	// Service versions come from the cluster rather than the cloud, so they are
	// their own row: an operator reading this needs to see that the two halves
	// disagree — agents up, versions behind — rather than have one summarized into
	// the other.
	//
	// Manual services are named even when they are up to date, on the same
	// reasoning as the OVN components. This is read by someone asking whether the
	// tool sees the cluster correctly, and a line confirming the OnDelete strategy
	// was detected is worth having months before the chart bump that makes it
	// matter. Waiting for the real event to discover the detector is broken is how
	// a detector ships broken.
	if svcs, ok := openstack.CollectServices(s, ""); ok {
		row := reportRow{key: "openstack/services"}
		row.summary = fmt.Sprintf("ns=%s, %d service(s), %d up to date",
			svcs.Namespace, len(svcs.Items), len(svcs.Items)-len(svcs.Pending()))
		var notes []string
		for _, svc := range svcs.Items {
			switch {
			case !svc.Converged():
				note := fmt.Sprintf("%s %d/%d up to date", svc.Name, svc.Updated, svc.Desired)
				if svc.Manual {
					note += " (manual: OnDelete)"
				}
				if behind := svc.Behind(); len(behind) > 0 {
					parts := make([]string, 0, len(behind))
					for _, c := range behind {
						parts = append(parts, fmt.Sprintf("%s %d/%d",
							openstack.TrimComponent(svc.Name, c.Name), c.Updated, c.Desired))
					}
					note += " behind: " + strings.Join(parts, ", ")
				}
				notes = append(notes, note)
			case svc.Manual:
				notes = append(notes, fmt.Sprintf("%s %d/%d up to date (manual: OnDelete)",
					svc.Name, svc.Updated, svc.Desired))
			}
		}
		if len(notes) > 0 {
			row.summary += "; " + strings.Join(notes, "; ")
		}
		out = append(out, row)
	}

	// Migrations and inventory are reported separately from the agent state
	// because they are separate polls against separate services: an operator
	// whose credential can list Nova services but not every project's servers
	// needs to see which of the three failed.
	if snap, ok := store.Get[openstack.Migrations](s, openstack.KeyMigrations); ok {
		row := reportRow{key: openstack.KeyMigrations}
		if snap.Err != nil {
			row.summary = "error: " + snap.Err.Error()
		} else {
			shown := snap.Relevant(time.Now())
			// Every number matters for a different reason: the history proves
			// the endpoint answers, the shown count is what the pane would
			// display, and the unresolved count is the backlog it is holding
			// back. Whether the ERROR probe ran at all is reported too, since
			// an unanswered probe silently reverts the retention rule to the
			// age window and would otherwise look like an empty backlog.
			row.summary = fmt.Sprintf("%d in history, %d shown (%d failed), %d unresolved",
				len(snap.Items), len(shown.Rows), shown.Failures(), len(shown.Unresolved))
			if !snap.BrokenKnown {
				row.summary += "; ERROR probe unavailable"
			}
			// Drains are named rather than counted: which host is being emptied
			// is the one fact that makes the rest of this line readable.
			for _, d := range snap.Drains {
				switch {
				case d.Err != nil:
					row.summary += fmt.Sprintf("; draining %s: %v",
						openstack.ShortHost(d.Host), d.Err)
				default:
					row.summary += fmt.Sprintf("; draining %s: %d left, %d moving, %d stuck",
						openstack.ShortHost(d.Host), d.Remaining, d.Moving, d.Stuck)
				}
			}
			if len(shown.Rows) > 0 {
				m := shown.Rows[0]
				row.sample = fmt.Sprintf("%s %s %s: %s -> %s", m.Status, m.Type, m.InstanceUUID,
					openstack.ShortHost(m.SourceCompute), openstack.ShortHost(m.DestCompute))
			}
		}
		out = append(out, row)
	}

	if inv, ok := store.Get[openstack.Inventory](s, openstack.KeyInventory); ok {
		row := reportRow{key: openstack.KeyInventory}
		if inv.Err != nil {
			row.summary = "error: " + inv.Err.Error()
		} else {
			parts := make([]string, 0, len(inv.Counts))
			for _, c := range inv.Counts {
				switch {
				case c.Absent:
					parts = append(parts, c.Label+" n/a")
				case c.Err != nil:
					parts = append(parts, c.Label+" failed")
				default:
					parts = append(parts, fmt.Sprintf("%s %d", c.Label, c.Total))
				}
			}
			row.summary = strings.Join(parts, ", ")
			for _, c := range inv.Counts {
				if c.Err != nil {
					row.sample = fmt.Sprintf("%s: %v", c.Label, c.Err)
					break
				}
			}
		}
		out = append(out, row)
	}

	if state, ok := store.Get[ovn.State](s, ovn.KeyState); ok {
		row := reportRow{key: ovn.KeyState}
		switch {
		case state.Err != nil:
			row.summary = "error: " + state.Err.Error()
		case state.Tier != kube.TierFull:
			row.summary = fmt.Sprintf("tier=%s (%s)", state.Tier, state.TierReason)
		default:
			// The plugin's own compact form, which names the leader and any lagging
			// member by pod rather than by Raft ID. No sample: the summary already
			// carries every database, so -v would only repeat itself.
			row.summary = strings.Join(ovn.Summary(state), "; ")
		}
		out = append(out, row)
	}
	return out
}

func eventSample(e model.Event) string {
	return fmt.Sprintf("[%s] %s %s/%s: %s", e.Type, e.Reason, e.Namespace, e.ObjectName, e.Message)
}

// describe summarizes one key: its item count, or the error that stopped it.
//
// The typed snapshot is read first and an untyped one second, because a source
// that failed before it could build the real element type publishes
// Snapshot[any]. Checking only the typed shape would report "pending" for a key
// that actually holds a permission error.
func describe[T any](s *store.Store, key string, sample func(T) string) reportRow {
	row := reportRow{key: key}

	if snap, ok := store.Get[model.Snapshot[T]](s, key); ok {
		switch {
		case snap.Err != nil:
			row.summary = "error: " + snap.Err.Error()
		case len(snap.Items) == 0 && snap.Note != "":
			// Not empty, not broken: still filling, and the note says why. Without
			// this the row reads "0 item(s)" and looks like a settled result.
			row.summary = "not ready: " + snap.Note
		default:
			row.summary = fmt.Sprintf("%d item(s)", len(snap.Items))
			if len(snap.Items) > 0 && sample != nil {
				row.sample = sample(snap.Items[0])
			}
		}
		return row
	}
	if snap, ok := store.Get[model.Snapshot[any]](s, key); ok && snap.Err != nil {
		row.summary = "unavailable: " + snap.Err.Error()
		return row
	}
	row.summary = "pending (no data published)"
	return row
}

// joinRow reports the Machine to Metal3Machine to BareMetalHost join, which is
// the piece most worth confirming: it is the one thing here that no other tool
// does, and the one most likely to be wrong on an unfamiliar cluster layout.
func joinRow(s *store.Store) reportRow {
	row := reportRow{key: "derived/host-join"}

	machines, mOK := store.Get[model.Snapshot[model.Machine]](s, model.KeyMgmtMachines)
	if !mOK {
		row.summary = "pending (no machines)"
		return row
	}
	m3ms, _ := store.Get[model.Snapshot[model.Metal3Machine]](s, model.KeyMgmtMetal3Machines)
	bmhs, _ := store.Get[model.Snapshot[model.BareMetalHost]](s, model.KeyMgmtBareMetalHosts)

	rows := capi.Join(machines.Items, m3ms.Items, bmhs.Items, nil)
	withHost := 0
	for _, r := range rows {
		if r.BareMetalHost != nil {
			withHost++
		}
	}
	unclaimed := capi.UnclaimedHosts(m3ms.Items, bmhs.Items)
	row.summary = fmt.Sprintf("%d machine(s), %d with a host, %d host(s) unclaimed",
		len(rows), withHost, len(unclaimed))

	if len(rows) > 0 {
		r := rows[0]
		host := r.HostName()
		if host == "" {
			host = "(none)"
		}
		row.sample = fmt.Sprintf("%s -> host %s (role=%s)", r.Machine.Name, host, r.Role)
	}
	return row
}

// StoreKeys returns the keys currently populated, sorted. Used by diagnostics to
// notice a source publishing under a key nothing reads.
func StoreKeys(s *store.Store) []string {
	keys := s.Keys()
	sort.Strings(keys)
	return keys
}
