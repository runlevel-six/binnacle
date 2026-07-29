package ovn

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Parsing `ovn-appctl cluster/status`.
//
// This is the only non-JSON parser in the tool: ovn-appctl has no structured
// output mode, so the text is all there is. It is line-oriented and stable in
// practice, but a line we do not recognize is skipped rather than treated as an
// error — losing one field beats losing the pane, and this format has no version
// to check against.
//
// The output looks like:
//
//	8469
//	Name: OVN_Northbound
//	Cluster ID: e715 (e715f8c2-0b7e-4dbd-ae62-7b8f871bd527)
//	Server ID: 8469 (84692da6-7258-4c5f-b510-1a95e92d7f41)
//	Address: tcp:ovn-ovsdb-nb-0...:6643
//	Status: cluster member
//	Role: follower
//	Term: 317
//	Leader: cc1b
//	Vote: cc1b
//
//	Last Election started 162981437 ms ago, reason: leadership_transfer
//	Election timer: 1000
//	Log: [917631, 923915]
//	Entries not yet committed: 0
//	Entries not yet applied: 0
//	Connections: ->cc1b ->7ed9 <-cc1b <-7ed9
//	Disconnections: 20
//	Servers:
//	    cc1b (cc1b at tcp:...:6643) last msg 38 ms ago
//	    8469 (8469 at tcp:...:6643) (self)
//	    7ed9 (7ed9 at tcp:...:6643) last msg 7965698 ms ago
//
// The status above is a *follower's*, and its "last msg" figures for the other
// members are not a health signal — see [ClusterStatus.Stale]. A leader's Servers
// block carries replication indices as well:
//
//	Servers:
//	    cc1b (cc1b at tcp:...:6643) (self) next_index=923207 match_index=924822
//	    8469 (8469 at tcp:...:6643) next_index=924823 match_index=924822 last msg 46 ms ago
//	    7ed9 (7ed9 at tcp:...:6643) next_index=924823 match_index=924822 last msg 46 ms ago

var (
	fieldRE = regexp.MustCompile(`^([A-Za-z][A-Za-z ]*):\s*(.*)$`)
	idRE    = regexp.MustCompile(`^([0-9a-f]+)`)
	// Captures the member's ID and its address:
	//   "    7ed9 (7ed9 at tcp:ovn-ovsdb-nb-2.ovn-ovsdb-nb.openstack…:6643) last msg …"
	serverRE   = regexp.MustCompile(`^\s+([0-9a-f]+)\s+\([0-9a-f]+\s+at\s+(\S+?)\)`)
	lastMsgRE  = regexp.MustCompile(`last msg (\d+) ms ago`)
	logRangeRE = regexp.MustCompile(`\[(\d+),\s*(\d+)\]`)
	// Only a leader reports these, and match_index is the authoritative measure of
	// how far a follower has actually replicated — independent of message timing.
	matchIndexRE = regexp.MustCompile(`match_index=(\d+)`)
)

// ParseClusterStatus reads one database's Raft status.
func ParseClusterStatus(out string) (ClusterStatus, error) {
	st := ClusterStatus{}
	var sawAnything bool

	inServers := false
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}

		// The Servers block is indented and continues to the end.
		if inServers {
			if srv, ok := parseServer(line); ok {
				st.Servers = append(st.Servers, srv)
				continue
			}
			// A non-matching line ends the block rather than aborting the parse.
			inServers = false
		}

		m := fieldRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key, value := m[1], strings.TrimSpace(m[2])
		sawAnything = true

		switch key {
		case "Name":
			st.Database = value
		case "Cluster ID":
			st.ClusterID = shortID(value)
		case "Server ID":
			st.ServerID = shortID(value)
		case "Address":
			// The queried member's own address, used to name it when its entry in
			// the Servers block is the "(self)" form.
			st.Address = value
		case "Status":
			st.Status = value
		case "Role":
			st.Role = strings.ToLower(value)
		case "Term":
			st.Term, _ = strconv.ParseInt(value, 10, 64)
		case "Leader":
			// "unknown" appears while an election is in progress.
			if !strings.EqualFold(value, "unknown") {
				st.Leader = value
			}
		case "Disconnections":
			st.Disconnections, _ = strconv.Atoi(value)
		case "Log":
			if lm := logRangeRE.FindStringSubmatch(value); lm != nil {
				st.LogLow, _ = strconv.ParseInt(lm[1], 10, 64)
				st.LogHigh, _ = strconv.ParseInt(lm[2], 10, 64)
			}
		case "Entries not yet committed":
			st.Uncommitted, _ = strconv.Atoi(value)
		case "Entries not yet applied":
			st.Unapplied, _ = strconv.Atoi(value)
		case "Servers":
			inServers = true
		}
	}

	if !sawAnything {
		return ClusterStatus{}, fmt.Errorf("no recognizable fields in cluster/status output")
	}
	return st, nil
}

// parseServer reads one line of the Servers block.
//
// The self entry carries "(self)" instead of a last-message age, which is not a
// missing value: a server does not message itself. That distinction matters,
// because treating it as "never heard from" would report every node as stale.
func parseServer(line string) (Server, bool) {
	m := serverRE.FindStringSubmatch(line)
	if m == nil {
		return Server{}, false
	}
	srv := Server{ID: m[1], Address: m[2], Name: PodNameFrom(m[2])}

	// Present only in a leader's output, and on the self line too, so it is read
	// before the self shortcut below.
	if mi := matchIndexRE.FindStringSubmatch(line); mi != nil {
		if n, err := strconv.ParseInt(mi[1], 10, 64); err == nil {
			srv.MatchIndex = n
			srv.MatchIndexKnown = true
		}
	}

	if strings.Contains(line, "(self)") {
		srv.Self = true
		return srv, true
	}
	if lm := lastMsgRE.FindStringSubmatch(line); lm != nil {
		ms, err := strconv.ParseInt(lm[1], 10, 64)
		if err == nil {
			srv.LastMsg = time.Duration(ms) * time.Millisecond
			srv.LastMsgKnown = true
		}
	}
	return srv, true
}

// shortID takes the abbreviated form from "e715 (e715f8c2-…)".
//
// The short ID is what every other line refers to — Leader, Vote, Connections and
// the Servers block all use it — so it is the useful identifier, and the full UUID
// would only crowd the pane.
func shortID(value string) string {
	if m := idRE.FindStringSubmatch(value); m != nil {
		return m[1]
	}
	return value
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
