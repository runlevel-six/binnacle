package fleet

import (
	"sort"

	"github.com/runlevel-six/sextant/pkg/model"
)

// collapseAfter is how many quiet rows a table tolerates before folding them
// away.
//
// Below it a reader is better served seeing the whole table: on a six-node
// development cluster, hiding four healthy nodes behind a disclosure costs a
// click and saves nothing. Above it the quiet rows are the page. A production
// undercloud runs forty to seventy nodes against a datacenter's worth of hosts,
// and the two rows worth reading are otherwise scattered among two hundred that
// are not.
const collapseAfter = 10

// Ranks. Lower is worse, and a row at or above the type's quiet rank is one
// nobody needs to see unless they ask. Kept as named constants because the
// rank functions and the split share them, and a table folded at the wrong
// rank hides exactly the rows it should not.
const (
	nodeQuiet    = 3
	machineQuiet = 3
	hostQuiet    = 2
)

// Split is a table divided into the rows worth reading now and the rows a
// reader can ask for.
//
// Quiet is empty when the table is short enough to render whole, so a template
// always renders Shown and only offers a disclosure when something is behind
// it. The decision lives here rather than in each pane, because three panes
// making it separately is three chances to make it differently.
type Split[T any] struct {
	Shown []T
	Quiet []T
}

// Total is every row, folded or not: what a pane heading counts.
func (s Split[T]) Total() int { return len(s.Shown) + len(s.Quiet) }

// All is every row, folded or not.
//
// For callers that need the whole set rather than the reading order — matching
// hosts to machines, say, where using Shown alone would silently drop every
// machine that was healthy enough to be folded.
func (s Split[T]) All() []T {
	if len(s.Quiet) == 0 {
		return s.Shown
	}
	out := make([]T, 0, len(s.Shown)+len(s.Quiet))
	return append(append(out, s.Shown...), s.Quiet...)
}

// split orders rows worst-first and folds away the quiet tail.
//
// rank gives a row's severity and less breaks ties within one rank. Ordering
// and folding are one operation because they answer the same question — which
// rows matter — and splitting them apart invites a table folded in an order
// nobody chose.
func split[T any](rows []T, rank func(T) int, less func(a, b T) bool, quietRank int) Split[T] {
	// Copied before sorting. These slices come straight out of the store, which
	// hands the same backing array to every reader, and a pane that reorders it
	// would reorder it for all of them. Doing it here rather than at each call
	// site means a new caller cannot forget.
	rows = append([]T(nil), rows...)

	sort.SliceStable(rows, func(i, j int) bool {
		ri, rj := rank(rows[i]), rank(rows[j])
		if ri != rj {
			return ri < rj
		}
		return less(rows[i], rows[j])
	})

	// Sorted worst-first, the quiet rows are a suffix: the cut is the first of
	// them.
	cut := len(rows)
	for i := range rows {
		if rank(rows[i]) >= quietRank {
			cut = i
			break
		}
	}
	if len(rows)-cut < collapseAfter {
		return Split[T]{Shown: rows}
	}
	return Split[T]{Shown: rows[:cut], Quiet: rows[cut:]}
}

// nodeRank orders nodes by how much they need looking at.
//
// Commitment is deliberately not a rank: a control plane sitting at 87% CPU
// committed is ordinary, and ranking on it would pull most of a production
// cluster out of the quiet set and defeat the fold.
func nodeRank(n NodeRow) int {
	switch {
	case !n.Ready():
		return 0
	case n.Pressure():
		return 1
	case n.Cordoned:
		return 2
	}
	return nodeQuiet
}

// machineRank orders machines by phase. Running is the only quiet phase;
// everything else is either a rollout in progress or a rollout stuck, and the
// two look the same until someone reads the row.
func machineRank(m model.Machine) int {
	switch m.Phase {
	case "Failed", "Unknown", "":
		return 0
	case "Deleting":
		return 1
	case "Running":
		return machineQuiet
	}
	// Pending, Provisioning, Provisioned: mid-rollout and worth watching.
	return 2
}

// hostRank orders hosts by whether the hardware is complaining.
//
// A host consumed by another cluster is not ranked differently from one of
// ours: the snapshot is datacenter-wide, and an errored host is worth seeing
// whoever is using it.
func hostRank(h model.BareMetalHost) int {
	switch {
	case h.ErrorMessage != "":
		return 0
	case h.OperationalStatus != "" && h.OperationalStatus != "OK":
		return 1
	}
	return hostQuiet
}

func splitNodes(rows []NodeRow) Split[NodeRow] {
	return split(rows, nodeRank, func(a, b NodeRow) bool {
		// Role then name within a rank, the same ordering the pools table uses,
		// so a reader moving between the two is not re-learning it.
		if a.Role != b.Role {
			return a.Role < b.Role
		}
		return a.Name < b.Name
	}, nodeQuiet)
}

func splitMachines(rows []model.Machine) Split[model.Machine] {
	return split(rows, machineRank, func(a, b model.Machine) bool { return a.Name < b.Name }, machineQuiet)
}

func splitHosts(rows []model.BareMetalHost) Split[model.BareMetalHost] {
	return split(rows, hostRank, func(a, b model.BareMetalHost) bool { return a.Name < b.Name }, hostQuiet)
}

// hostsFor keeps the hosts this cluster is actually running on.
//
// The BareMetalHost snapshot is datacenter-wide — one namespace on the
// management cluster holding every machine in the building. On a four-cluster
// datacenter three quarters of it describes hardware this page is not about,
// and the Ceph nodes in it belong to no undercloud at all.
//
// The link is the host's consumerRef, which points at the provider machine
// (a Metal3Machine) rather than at the CAPI Machine. Machine.InfraName carries
// exactly that reference, so it is what this matches on; the machine's own name
// is accepted too, because providers commonly name the pair alike and a host
// wrongly dropped from this pane is worse than one wrongly kept.
//
// Returns the kept hosts and how many were left in the datacenter, because a
// pane that quietly shows six of two hundred and fifty rows is a pane that has
// answered a different question than the one it appears to answer.
func hostsFor(hosts []model.BareMetalHost, machines []model.Machine) (mine []model.BareMetalHost, elsewhere int) {
	claimed := make(map[string]bool, len(machines)*2)
	for _, m := range machines {
		if m.InfraName != "" {
			claimed[m.Namespace+"/"+m.InfraName] = true
		}
		claimed[m.Namespace+"/"+m.Name] = true
	}

	for _, h := range hosts {
		// A host with no consumer is unclaimed hardware: a datacenter spare, or
		// a Ceph node. Neither is this cluster's.
		if h.ConsumerName != "" && claimed[h.ConsumerNamespace+"/"+h.ConsumerName] {
			mine = append(mine, h)
			continue
		}
		elsewhere++
	}
	return mine, elsewhere
}
