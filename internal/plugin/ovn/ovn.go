// Package ovn reports the Raft health of OVN's northbound and southbound
// databases.
//
// This is the pane that catches a failure mode nothing else shows. Both database
// StatefulSets can report every replica Ready while a Raft member has not been
// heard from in hours — it is running, it is passing its probes, and it is not
// participating. Kubernetes has no opinion about that; only `cluster/status` does.
//
// # Tested against
//
// The parser is exercised against captured output from OVN 24.03 as shipped by a
// production OpenStack deployment (see testdata), including a genuinely stale
// member. `ovn-appctl` has no structured output mode, so text is all there is —
// which is why an unrecognized line is skipped rather than fatal.
package ovn

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/runlevel-six/binnacle/internal/plugin/kube"
	"github.com/runlevel-six/binnacle/pkg/health"
	"github.com/runlevel-six/binnacle/pkg/store"
	ovnstate "github.com/runlevel-six/binnacle/pkg/subsystem/ovn"
)

// Name is the plugin's registration name.
const Name = "ovn"

// KeyState holds a State.
const KeyState = ovnstate.KeyState

// pollInterval is how often the databases are queried. Two execs per poll, so
// this is unhurried; Raft state does not change on a shorter timescale than an
// operator would notice.
const pollInterval = 20 * time.Second

// The OVN state types live in pkg/subsystem/ovn so a consumer outside this
// module can read them. Aliased here so this package's own code is unchanged.
type (
	// Server is an alias for [ovnstate.Server].
	Server = ovnstate.Server
	// ClusterStatus is an alias for [ovnstate.ClusterStatus].
	ClusterStatus = ovnstate.ClusterStatus
	// Database is an alias for [ovnstate.Database].
	Database = ovnstate.Database
	// State is an alias for [ovnstate.State].
	State = ovnstate.State
)

// StaleThreshold is how long since a member's last message counts as stale.
const StaleThreshold = ovnstate.StaleThreshold

// PodNameFrom extracts a member's pod name from its OVSDB address.
var PodNameFrom = ovnstate.PodNameFrom

// Databases are the OVSDB databases this plugin reads, in upgrade order.
var Databases = ovnstate.Databases

// Settings is the plugin's profile configuration.
type Settings struct {
	// Namespace pins where OVN runs. Empty derives it from the StatefulSets.
	Namespace string
	// Container is the container to exec into.
	Container string
}

// Defaults leave the namespace to discovery.
//
// Every hardcoded namespace in this codebase has been wrong on the first real
// cluster it met, so this one is derived. The container name is a constant because
// it comes from OVN's own chart rather than from a deployment's naming.
func Defaults() Settings {
	return Settings{Container: "ovsdb"}
}

// SettingsFrom reads a profile's plugin block over the defaults.
func SettingsFrom(raw map[string]any) Settings {
	s := Defaults()
	if v, ok := raw["namespace"].(string); ok && v != "" {
		s.Namespace = v
	}
	if v, ok := raw["container"].(string); ok && v != "" {
		s.Container = v
	}
	return s
}

// Plugin observes OVN's databases.
type Plugin struct {
	client   *kube.Client
	settings Settings

	namespace  string
	tier       kube.Tier
	tierReason string
	// pods maps a database's StatefulSet to the pod discovered for it.
	pods map[string]string
}

// New builds the plugin.
func New(c *kube.Client, settings Settings) *Plugin {
	return &Plugin{client: c, settings: settings, pods: map[string]string{}}
}

// Name implements plugin.Plugin.
func (p *Plugin) Name() string { return Name }

// Detect finds OVN and decides the tier.
//
// The namespace is discovered by looking for the database StatefulSets rather than
// assumed, and the pod to query is discovered too — a StatefulSet's pods are
// ordinal-suffixed, so the name is predictable in principle but the *ready* one is
// not.
func (p *Plugin) Detect(ctx context.Context) (bool, error) {
	ns, err := p.findNamespace(ctx)
	if err != nil || ns == "" {
		p.tier = kube.TierAbsent
		return false, err
	}
	p.namespace = ns

	pod, err := p.anyDatabasePod(ctx)
	if err != nil {
		// The StatefulSets exist but no pod is ready. OVN is present and worth
		// reporting as degraded rather than absent.
		p.tier = kube.TierInformer
		p.tierReason = "no ready ovsdb pod to query"
		// Present, not reachable — reported, and still active. See plugin.Source.
		return true, err
	}
	ok, forbidden := p.client.ExecProbe(ctx, ns, pod, p.settings.Container)
	switch {
	case ok:
		p.tier = kube.TierFull
	case forbidden:
		p.tier = kube.TierInformer
		p.tierReason = "no pods/exec permission on " + ns + " — use --server for full detail"
	default:
		// Not a verdict — poll re-derives this every time.
		p.tier = kube.TierInformer
		p.tierReason = "ovsdb pod " + pod + " did not answer"
	}
	return true, nil
}

// findNamespace locates the namespace holding OVN's database StatefulSets.
func (p *Plugin) findNamespace(ctx context.Context) (string, error) {
	if p.settings.Namespace != "" {
		return p.settings.Namespace, nil
	}
	list, err := p.client.Typed.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list statefulsets: %w", err)
	}

	seen := map[string]bool{}
	for _, sts := range list.Items {
		for _, db := range Databases {
			if strings.HasSuffix(sts.Name, db.StatefulSet) {
				seen[sts.Namespace] = true
			}
		}
	}
	switch len(seen) {
	case 0:
		return "", nil
	case 1:
		for ns := range seen {
			return ns, nil
		}
	}
	namespaces := make([]string, 0, len(seen))
	for ns := range seen {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)
	return "", fmt.Errorf(
		"OVN database StatefulSets span %d namespaces (%s); pin one with the namespace setting",
		len(seen), strings.Join(namespaces, ", "))
}

// anyDatabasePod returns a pod from either database, for the exec capability
// probe.
func (p *Plugin) anyDatabasePod(ctx context.Context) (string, error) {
	for _, db := range Databases {
		if pod, err := p.podFor(ctx, db); err == nil {
			return pod, nil
		}
	}
	return "", fmt.Errorf("no ready ovsdb pod in namespace %s", p.namespace)
}

// podsFor finds the pods of a database's StatefulSet worth querying, best first.
//
// Matching on the StatefulSet name as a prefix of the pod name rather than a
// label, because the pod's ordinal suffix makes the relationship structural and
// the labels vary by chart. Readiness and terminating-pod filtering come from
// [kube.Client.PodCandidates]: during a rolling upgrade one member of a
// three-member database is always the one that just went away, and it keeps phase
// Running for minutes after it stops answering.
func (p *Plugin) podsFor(ctx context.Context, db Database) ([]string, error) {
	return p.client.PodCandidates(ctx, p.namespace, "", func(pod *corev1.Pod) bool {
		return strings.Contains(pod.Name, db.StatefulSet)
	})
}

// podFor returns the best pod of a database's StatefulSet.
func (p *Plugin) podFor(ctx context.Context, db Database) (string, error) {
	pods, err := p.podsFor(ctx, db)
	if err != nil {
		return "", err
	}
	return pods[0], nil
}

// Run polls until ctx is canceled.
func (p *Plugin) Run(ctx context.Context, s *store.Store) error {
	publish := func() { s.Put(KeyState, p.poll(ctx)) }
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
	state := State{Tier: p.tier, TierReason: p.tierReason, UpdatedAt: time.Now()}
	// Independent of the Raft queries below and of the tier they depend on: this
	// needs no exec, so a plugin stuck at informer-only still reports it.
	state.Components = p.pollComponents(ctx)
	// Attempted every poll, whatever happened last time — including a permission
	// denial, which can be repaired mid-session. See [kube.Forbidden].
	for _, db := range Databases {
		state.Statuses = append(state.Statuses, p.queryDatabase(ctx, db))
	}

	// A database that answered means whatever detection concluded is out of date.
	// One broken database does not undo the other's detail.
	denied := false
	for _, st := range state.Statuses {
		switch {
		case st.Err == nil:
			p.tier, p.tierReason = kube.TierFull, ""
			state.Tier, state.TierReason = kube.TierFull, ""
			return state
		case kube.Forbidden(st.Err):
			denied = true
		}
	}
	state.Tier = kube.TierInformer
	if denied {
		state.TierReason = "no pods/exec permission on " + p.namespace + " — use --server for full detail"
	} else {
		state.TierReason = "no ovsdb pod answered"
	}
	return state
}

// queryDatabase reads one database's Raft status, from the leader where possible.
//
// Which pod answers decides what the status *means*. Every member reports the same
// cluster, but only the leader's view of the other members is a health signal — see
// [ClusterStatus.Stale]. So a follower's answer is used to find the leader, and the
// leader is then asked directly.
//
// That costs a second exec on a cluster whose first ready pod is not the leader,
// which is two thirds of the time for a three-member database. Worth it: the
// alternative is a pane that reports healthy followers as silent, which is what
// this replaced.
//
// A failure is recorded on that database's own entry rather than failing the poll,
// so a broken northbound database does not hide a healthy southbound one.
func (p *Plugin) queryDatabase(ctx context.Context, db Database) ClusterStatus {
	pods, err := p.podsFor(ctx, db)
	if err != nil {
		return ClusterStatus{Database: db.Name, Err: err}
	}

	// Try each member until one answers. A rolling upgrade guarantees that one of
	// the three is unreachable at any moment, and asking only the first is how a
	// healthy database comes to report no detail at all.
	var status ClusterStatus
	var pod string
	for _, candidate := range pods {
		status, err = p.statusFrom(ctx, db, candidate)
		if err == nil {
			pod = candidate
			break
		}
		if kube.Forbidden(err) {
			// No point asking the other members: permission is not per-pod.
			break
		}
	}
	if err != nil {
		return ClusterStatus{Database: db.Name, Pod: pods[0], Err: err}
	}
	if status.IsLeader() {
		return status
	}

	// Ask the leader instead. Its pod name comes from its address in the Servers
	// block, which is the only place the Raft ID to pod mapping appears.
	leaderPod := status.LeaderName()
	if leaderPod == "" || leaderPod == pod || leaderPod == status.Leader {
		// No leader (an election is in progress, which the pane reports), or its
		// address was not resolvable to a pod. The follower's own view still
		// carries the term, the log and who it thinks leads.
		return status
	}
	leaderStatus, err := p.statusFrom(ctx, db, leaderPod)
	if err != nil || !leaderStatus.IsLeader() {
		// The leader moved, or its pod is unreachable from here. Keeping the
		// follower's status is right — it is honest about being a follower's view,
		// and StaleServers returns nothing from it rather than a false alarm.
		return status
	}
	return leaderStatus
}

// statusFrom execs cluster/status in one pod and parses it.
func (p *Plugin) statusFrom(ctx context.Context, db Database, pod string) (ClusterStatus, error) {
	out, err := p.client.Exec(ctx, p.namespace, pod, p.settings.Container,
		[]string{"ovn-appctl", "-t", db.Socket, "cluster/status", db.Name})
	if err != nil {
		return ClusterStatus{}, err
	}
	status, err := ParseClusterStatus(out)
	if err != nil {
		return ClusterStatus{}, err
	}
	status.Pod = pod
	if status.Database == "" {
		status.Database = db.Name
	}
	return status, nil
}

// Cells implements plugin.BannerProvider.
func (p *Plugin) Cells(s *store.Store) []health.Cell {
	state, ok := store.Get[State](s, KeyState)
	if !ok {
		return nil
	}

	cell := health.Cell{Name: "OVN"}
	switch {
	case state.Tier != kube.TierFull:
		cell.Status = health.StatusLoading
		cell.Detail = "no detail"
	case len(state.Statuses) == 0:
		cell.Status = health.StatusLoading
	default:
		var problems []string
		for _, st := range state.Statuses {
			label := databaseLabel(st.Database)
			switch {
			case st.Err != nil:
				problems = append(problems, label+" unreadable")
			case !st.HasLeader():
				// No leader means writes are failing right now.
				problems = append(problems, label+" no leader")
			case len(st.StaleServers()) > 0:
				problems = append(problems, fmt.Sprintf("%s %d stale", label, len(st.StaleServers())))
			}
		}
		if len(problems) == 0 {
			cell.Status = health.StatusOK
			// Nothing is known to be wrong, but say when the members could not be
			// checked — a leader that cannot be reached leaves a gap in what this
			// cell is asserting, and the gap should be visible rather than read as
			// a clean bill of health.
			var unchecked []string
			for _, st := range state.Statuses {
				if st.Err == nil && st.HasLeader() && !st.MemberViewTrusted() {
					unchecked = append(unchecked, databaseLabel(st.Database))
				}
			}
			if len(unchecked) > 0 {
				cell.Detail = strings.Join(unchecked, ", ") + " members unchecked"
			}
			break
		}
		// A missing leader is an outage; a stale member is a degraded quorum.
		cell.Status = health.StatusWarn
		for _, pr := range problems {
			if strings.Contains(pr, "no leader") {
				cell.Status = health.StatusErr
			}
		}
		cell.Detail = strings.Join(problems, ", ")
	}

	// Version drift rides on the same cell rather than claiming one of its own.
	// The strip is a row of subsystems, not of questions about them, and a second
	// OVN cell would read as a second OVN.
	if pending := PendingComponents(state.Components); len(pending) > 0 {
		var stale int32
		manual := false
		for _, c := range pending {
			stale += c.Stale()
			manual = manual || c.Manual
		}
		note := fmt.Sprintf("%d pod(s) behind", stale)
		if manual {
			// The drift that will still be there next week unless somebody acts,
			// so it is worth amber on a cell that is otherwise green.
			note += " (manual)"
			cell.Status = cell.Status.Worse(health.StatusWarn)
		}
		if cell.Detail != "" {
			note = cell.Detail + ", " + note
		}
		cell.Detail = note
	}
	return []health.Cell{cell}
}

// databaseLabel shortens a database name for a cell.
func databaseLabel(name string) string {
	for _, db := range Databases {
		if db.Name == name {
			return db.Label
		}
	}
	return name
}
