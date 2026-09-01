package fleet

import (
	"strings"

	"github.com/runlevel-six/sextant/pkg/model"
)

// eventQuiet is the rank at or above which an event group folds away. See
// eventRank.
const eventQuiet = 1

// ownedNames is the set of management object names this cluster answers for.
//
// The management namespace holds every cluster in the datacenter, so a name is
// the only thing that ties an event there to one of them. Collected from what
// the page has already read rather than re-listed: the cluster itself, its node
// pools, its machines and the provider machines behind them, and the hosts
// already scoped to it by hostsFor.
//
// Cluster names are no help as a prefix — a cluster called tenant-01-cluster
// owns a control plane called tenant-01-kcp, and neither is a prefix of the
// other — which is why this is a set of names and not a rule about them.
func (d *ClusterDetail) ownedNames() map[string]bool {
	owned := make(map[string]bool, 32)
	add := func(name string) {
		if name != "" {
			owned[name] = true
		}
	}

	add(d.Name)
	for _, p := range d.Pools {
		add(p.Name)
	}
	for _, m := range d.Machines.All() {
		add(m.Name)
		add(m.InfraName)
	}
	for _, h := range d.Hosts.All() {
		add(h.Name)
	}
	return owned
}

// eventsFor keeps the management events that concern this cluster.
//
// The management event snapshot is the whole namespace, which on a
// multi-cluster management cluster means every cluster's Cluster API events
// arrive together. Unscoped, a control plane paused on one cluster is reported
// on every cluster's page — indistinguishable from that cluster's own control
// plane being paused, which is the most alarming thing the page can say.
//
// Matching is by name against ownedNames, generously: Cluster API names the
// objects in a pool as extensions of each other, so a MachineSet is a prefix of
// its Machines and a KubeadmConfig extends the Machine it belongs to, and
// neither is in the set. A segment either side of a hyphen counts as a match,
// which keeps those rows on the page. The same rule as hostsFor, for the same
// reason: an event wrongly dropped is worse than one wrongly kept, because the
// reader cannot tell it is missing.
//
// Returns the kept events and how many were left, because a pane that quietly
// drops half a namespace has answered a different question than the one it
// appears to answer. Workload events are not passed through here: they come
// from the cluster's own API server, so all of them are already its own.
func eventsFor(events []model.Event, owned map[string]bool) (mine []model.Event, elsewhere int) {
	for _, e := range events {
		if ownsName(owned, e.ObjectName) {
			mine = append(mine, e)
			continue
		}
		elsewhere++
	}
	return mine, elsewhere
}

// ownsName reports whether an object name belongs to one of the owned names,
// either exactly or as a hyphen-separated extension in either direction.
//
// The boundary matters: without it a short owned name would match unrelated
// objects that merely start with the same letters — tenant-1 would claim
// everything tenant-10 owns.
func ownsName(owned map[string]bool, name string) bool {
	if name == "" {
		return false
	}
	if owned[name] {
		return true
	}
	for o := range owned {
		if strings.HasPrefix(name, o+"-") || strings.HasPrefix(o, name+"-") {
			return true
		}
	}
	return false
}

// eventRank orders event groups by whether anybody needs to read them.
//
// Warnings are the pane; Normal events are the audit trail. A production
// cluster produces a steady stream of the second — an etcd backup CronJob alone
// fills forty rows an hour with pulled images and created pods — and folded
// away they stop burying the handful of warnings above them.
func eventRank(g EventGroup) int {
	if g.Type == "Warning" {
		return 0
	}
	return eventQuiet
}

func splitEvents(groups []EventGroup) Split[EventGroup] {
	// Already ordered worst-first by GroupEvents, and split's sort is stable, so
	// the tie-break only has to preserve that order.
	return split(groups, eventRank, func(a, b EventGroup) bool { return false }, eventQuiet)
}
