// Package ovn holds the state sextant's OVN plugin publishes.
//
// The types are here, separate from the plugin that fills them, so a consumer
// outside this module can read a cluster's OVN state without importing the
// machinery that produced it — and, more to the point, without reimplementing
// the judgement inside it. Only the Raft leader hears from followers, so a
// follower's view of another member's silence measures the age of the last
// election and nothing else; [ClusterStatus.IsLeader] is what gates that.
package ovn

import (
	"net"
	"strings"
	"time"

	"github.com/runlevel-six/sextant/pkg/subsystem"
)

// KeyState holds a [State].
const KeyState = "ovn/state"

// StaleThreshold is how long since a member's last message counts as stale.
//
// Raft election timers here are on the order of a second, so a member silent for
// a minute is not slow, it is gone. The threshold is deliberately far above the
// timer so a brief pause is never reported as a failure.
const StaleThreshold = time.Minute

// Server is one Raft member as its peers see it.
type Server struct {
	// ID is the four-hex-digit identifier every other line refers to.
	ID string
	// Address is the member's OVSDB address.
	Address string
	// Name is the pod serving this member, derived from Address. This is what an
	// operator actually needs: the ID identifies a member, but only the pod name
	// says which thing to go and look at.
	Name string
	// Self marks the member being queried. It carries no last-message age
	// because a server does not message itself — which is not the same as never
	// having been heard from.
	Self bool
	// LastMsg is how long since this member was last heard from, *by the member
	// that produced the status*. Only the leader's figure means anything; see
	// [ClusterStatus.Stale].
	LastMsg time.Duration
	// LastMsgKnown distinguishes "no age reported" from "zero milliseconds ago".
	LastMsgKnown bool
	// MatchIndex is the highest log entry known to be replicated to this member.
	// Reported by the leader only.
	MatchIndex int64
	// MatchIndexKnown distinguishes an unreported index from entry zero.
	MatchIndexKnown bool
}

// quiet reports whether this member's last message is older than
// [StaleThreshold], as a fact about the number and nothing more.
//
// Unexported because the number alone does not mean the member is unwell: whether
// it is depends entirely on who reported it. Go through [ClusterStatus.Stale].
func (s Server) quiet() bool {
	return !s.Self && s.LastMsgKnown && s.LastMsg > StaleThreshold
}

// ClusterStatus is one database's Raft state.
type ClusterStatus struct {
	Database  string
	ClusterID string
	ServerID  string
	Status    string
	Role      string
	Term      int64
	// Leader is the leader's ID, or "self" when this member leads.
	Leader string
	// Address is this member's own OVSDB address, used to name it when its entry
	// in the Servers block carries no address.
	Address string
	Servers []Server

	LogLow, LogHigh int64
	Uncommitted     int
	Unapplied       int
	Disconnections  int

	// Pod names where this was read, so a per-member figure can be attributed.
	Pod string
	Err error
}

// HasLeader reports whether this member knows who the leader is. No leader means
// an election is in progress, during which writes fail.
func (c ClusterStatus) HasLeader() bool { return c.Leader != "" }

// IsLeader reports whether the queried member is itself the leader.
func (c ClusterStatus) IsLeader() bool { return c.Role == "leader" }

// Self returns the queried member, if it appears in the Servers block.
func (c ClusterStatus) Self() (Server, bool) {
	for _, s := range c.Servers {
		if s.Self {
			return s, true
		}
	}
	return Server{}, false
}

// LeaderName returns the leader's pod name.
//
// The status reports the leader by ID, or as the literal "self" when this member
// leads, so both need resolving against the Servers block. An ID with no matching
// member falls back to the ID rather than to an empty cell — a member can appear
// as leader before its own entry does.
func (c ClusterStatus) LeaderName() string {
	if c.Leader == "" {
		return ""
	}
	if c.Leader == "self" || c.IsLeader() {
		if self, ok := c.Self(); ok && self.Name != "" {
			return self.Name
		}
		if name := PodNameFrom(c.Address); name != "" {
			return name
		}
	}
	for _, s := range c.Servers {
		if s.ID == c.Leader && s.Name != "" {
			return s.Name
		}
	}
	return c.Leader
}

// DisplayName returns the best label for a member: its pod name when known, its
// ID otherwise.
func (s Server) DisplayName() string {
	if s.Name != "" {
		return s.Name
	}
	return s.ID
}

// Stale reports whether a member has genuinely gone quiet, as judged from this
// status.
//
// Only the leader's view counts, and this is the whole reason the plugin goes
// looking for the leader. In Raft the leader heartbeats its followers and they
// answer the leader; two followers have no reason to exchange anything between
// elections. So a follower's "last msg" for another follower grows without bound
// on a perfectly healthy cluster — measured on a live one, a follower reported its
// peer as 4.9 hours quiet, which was almost exactly the age of the last election,
// the last moment two followers had anything to say to each other. The leader, at
// the same instant, had heard from both of them 46ms earlier and had them fully
// replicated.
//
// Reading that as a silent member is worse than reporting nothing: it fires on
// every healthy cluster, and an alarm that is always on is one nobody reads.
func (c ClusterStatus) Stale(s Server) bool {
	return c.IsLeader() && s.quiet()
}

// StaleServers returns the members that have genuinely gone quiet, which is
// knowable only from the leader — see [ClusterStatus.Stale]. A follower's status
// returns none rather than guessing.
func (c ClusterStatus) StaleServers() []Server {
	if !c.IsLeader() {
		return nil
	}
	var out []Server
	for _, s := range c.Servers {
		if s.quiet() {
			out = append(out, s)
		}
	}
	return out
}

// Behind reports how many log entries a member is short of the leader's, and
// whether that is knowable.
//
// This is the signal that does not depend on message timing at all: match_index is
// what the leader has confirmed replicated. In steady state a follower sits one
// entry behind the leader's log end, so a small number here is normal and only the
// magnitude is interesting — which is why it is reported alongside a member already
// known to be quiet rather than judged against a threshold of its own.
func (c ClusterStatus) Behind(s Server) (int64, bool) {
	if !c.IsLeader() || s.Self || !s.MatchIndexKnown || c.LogHigh == 0 {
		return 0, false
	}
	if n := c.LogHigh - s.MatchIndex; n > 0 {
		return n, true
	}
	return 0, true
}

// MemberViewTrusted reports whether this status can speak to the other members'
// health. Only the leader can; a follower knows the leader is alive and nothing
// more.
func (c ClusterStatus) MemberViewTrusted() bool { return c.IsLeader() }

// Healthy reports whether this database's Raft cluster is fully in order.
func (c ClusterStatus) Healthy() bool {
	return c.Err == nil && c.HasLeader() && len(c.StaleServers()) == 0 &&
		strings.Contains(strings.ToLower(c.Status), "cluster member")
}

// Database identifies one of OVN's two databases.
type Database struct {
	// Name is the OVSDB database name passed to cluster/status.
	Name string
	// Label is how it is shown, short enough for a table cell.
	Label string
	// StatefulSet is the workload serving it.
	StatefulSet string
	// Socket is the control socket to query.
	Socket string
}

// Databases are OVN's northbound and southbound databases.
//
// The socket paths and database names are OVN's own conventions rather than a
// deployment's, so they are constants; the StatefulSet names are only a prefix to
// discover by, since a deployment may prefix them.
var Databases = []Database{
	{Name: "OVN_Northbound", Label: "nb", StatefulSet: "ovn-ovsdb-nb", Socket: "/var/run/ovn/ovnnb_db.ctl"},
	{Name: "OVN_Southbound", Label: "sb", StatefulSet: "ovn-ovsdb-sb", Socket: "/var/run/ovn/ovnsb_db.ctl"},
}

// State is everything the plugin publishes.
type State struct {
	Tier       subsystem.Tier
	TierReason string
	// Statuses is one entry per database, in Databases order.
	Statuses []ClusterStatus
	// Components is the rollout state of the OVN and Open vSwitch workloads, in
	// upgrade order. Read from workload status alone, so it survives a denied
	// pods/exec — which is exactly when Statuses above it goes empty.
	Components []Component
	UpdatedAt  time.Time
	Err        error
}

// Healthy reports whether every database is in order.
func (s State) Healthy() bool {
	if len(s.Statuses) == 0 {
		return false
	}
	for _, st := range s.Statuses {
		if !st.Healthy() {
			return false
		}
	}
	return true
}

// PodNameFrom extracts a member's pod name from its OVSDB address.
//
// A member's address is its headless-Service DNS name, whose first label is the
// pod: "tcp:ovn-ovsdb-nb-2.ovn-ovsdb-nb.openstack.svc.cluster.local:6643" is
// ovn-ovsdb-nb-2. That is the only place the mapping appears — every other line
// refers to members by a four-hex-digit ID, which is useless for finding the pod
// to look at.
//
// An address that is an IP rather than a DNS name is returned as the IP, since
// half a name would be worse than none.
func PodNameFrom(addr string) string {
	host := addr

	// Strip the transport scheme.
	if i := strings.Index(host, ":"); i > 0 {
		switch host[:i] {
		case "tcp", "ssl", "unix", "punix", "ptcp", "pssl":
			host = host[i+1:]
		}
	}

	// Strip the port. A bracketed IPv6 literal is handled first, since its
	// colons would otherwise be mistaken for the port separator.
	switch {
	case strings.HasPrefix(host, "["):
		if end := strings.Index(host, "]"); end > 0 {
			return host[1:end]
		}
	default:
		if i := strings.LastIndex(host, ":"); i > 0 {
			host = host[:i]
		}
	}

	// An IP literal is returned whole. Taking the first dot-separated label would
	// turn 10.0.0.5 into "10", which is worse than useless — it looks like a name.
	if net.ParseIP(host) != nil {
		return host
	}

	// The first DNS label of a headless-Service name is the pod.
	if i := strings.Index(host, "."); i > 0 {
		return host[:i]
	}
	return host
}

// Component is one OVN or Open vSwitch workload family's rollout state.
//
// Named families rather than workloads, because the switching layer is not one
// workload per job. Open vSwitch in particular is split into a DaemonSet per
// distinct node configuration, hash-suffixed, and an operator asking "is OVS up to
// date" wants one number rather than four — two of which are usually empty
// leftovers from nodes that have since been relabeled.
type Component struct {
	// Name is the family, e.g. "ovn-northd", "ovn-controller", "openvswitch".
	Name string
	subsystem.Rollout
}
