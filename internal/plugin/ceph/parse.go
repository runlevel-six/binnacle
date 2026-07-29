package ceph

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Parsing `ceph -s --format=json`.
//
// Sections are decoded independently, for the reason the Cilium parser documents
// at length: one struct for the whole document means `json.Unmarshal` aborts on
// the first type mismatch anywhere and discards everything. A section we cannot
// read becomes zero and is named in [Status.Unreadable].

type rawEnvelope struct {
	FSID     string          `json:"fsid"`
	Health   json.RawMessage `json:"health"`
	MonMap   json.RawMessage `json:"monmap"`
	MgrMap   json.RawMessage `json:"mgrmap"`
	OSDMap   json.RawMessage `json:"osdmap"`
	PGMap    json.RawMessage `json:"pgmap"`
	FSMap    json.RawMessage `json:"fsmap"`
	QuorumNm json.RawMessage `json:"quorum_names"`
}

type rawHealth struct {
	Status string `json:"status"`
	// Checks is keyed by check name, e.g. "OSD_NEARFULL". It is an empty object
	// on a healthy cluster rather than absent.
	Checks map[string]struct {
		Severity string `json:"severity"`
		Summary  struct {
			Message string `json:"message"`
		} `json:"summary"`
	} `json:"checks"`
	Mutes []json.RawMessage `json:"mutes"`
}

// rawMonMap covers both shapes: older releases emit a `mons` array, newer ones a
// `num_mons` summary.
type rawMonMap struct {
	NumMons int `json:"num_mons"`
	Mons    []struct {
		Name string `json:"name"`
	} `json:"mons"`
}

// rawMgrMap has no active_name on current releases — see mgrFrom.
type rawMgrMap struct {
	Available   bool     `json:"available"`
	NumStandbys int      `json:"num_standbys"`
	ActiveName  string   `json:"active_name"`
	Modules     []string `json:"modules"`
}

type rawOSDMap struct {
	Epoch         int64 `json:"epoch"`
	NumOSDs       int   `json:"num_osds"`
	NumUpOSDs     int   `json:"num_up_osds"`
	NumInOSDs     int   `json:"num_in_osds"`
	NumRemappedPG int   `json:"num_remapped_pgs"`
}

type rawPGMap struct {
	PGsByState []struct {
		StateName string `json:"state_name"`
		Count     int64  `json:"count"`
	} `json:"pgs_by_state"`
	NumPGs     int64 `json:"num_pgs"`
	NumPools   int64 `json:"num_pools"`
	NumObjects int64 `json:"num_objects"`
	DataBytes  int64 `json:"data_bytes"`
	BytesUsed  int64 `json:"bytes_used"`
	BytesAvail int64 `json:"bytes_avail"`
	BytesTotal int64 `json:"bytes_total"`
	ReadBytes  int64 `json:"read_bytes_sec"`
	WriteBytes int64 `json:"write_bytes_sec"`
	ReadOps    int64 `json:"read_op_per_sec"`
	WriteOps   int64 `json:"write_op_per_sec"`
}

// ParseStatus decodes `ceph -s --format=json`.
func ParseStatus(payload []byte) (Status, error) {
	var env rawEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return Status{}, fmt.Errorf("ceph status json: %w", err)
	}

	out := Status{FSID: env.FSID}
	unreadable := map[string]bool{}

	section := func(name string, raw json.RawMessage, into any, apply func()) {
		if len(raw) == 0 || string(raw) == "null" {
			return
		}
		if err := json.Unmarshal(raw, into); err != nil {
			unreadable[name] = true
			return
		}
		apply()
	}

	var health rawHealth
	section("health", env.Health, &health, func() {
		out.Health = health.Status
		for name, check := range health.Checks {
			out.Checks = append(out.Checks, Check{
				Name:     name,
				Severity: check.Severity,
				Message:  check.Summary.Message,
			})
		}
		// Sorted so the pane does not reshuffle between renders; map iteration
		// order is not stable.
		sort.Slice(out.Checks, func(i, j int) bool { return out.Checks[i].Name < out.Checks[j].Name })
		out.MutedChecks = len(health.Mutes)
	})

	var mons rawMonMap
	section("monmap", env.MonMap, &mons, func() {
		out.Mons.Total = mons.NumMons
		if out.Mons.Total == 0 {
			out.Mons.Total = len(mons.Mons)
		}
	})
	var quorum []string
	section("quorum_names", env.QuorumNm, &quorum, func() {
		out.Mons.InQuorum = len(quorum)
	})

	var mgr rawMgrMap
	section("mgrmap", env.MgrMap, &mgr, func() { out.Mgr = mgrFrom(mgr) })

	var osds rawOSDMap
	section("osdmap", env.OSDMap, &osds, func() {
		out.OSDs = OSDs{
			Total:      osds.NumOSDs,
			Up:         osds.NumUpOSDs,
			In:         osds.NumInOSDs,
			RemappedPG: osds.NumRemappedPG,
			Epoch:      osds.Epoch,
		}
	})

	var pgs rawPGMap
	section("pgmap", env.PGMap, &pgs, func() {
		out.PGs = PGs{
			Total:      pgs.NumPGs,
			Pools:      pgs.NumPools,
			Objects:    pgs.NumObjects,
			DataBytes:  pgs.DataBytes,
			UsedBytes:  pgs.BytesUsed,
			AvailBytes: pgs.BytesAvail,
			TotalBytes: pgs.BytesTotal,
		}
		for _, s := range pgs.PGsByState {
			out.PGs.ByState = append(out.PGs.ByState, PGState{Name: s.StateName, Count: s.Count})
		}
		sort.Slice(out.PGs.ByState, func(i, j int) bool {
			return out.PGs.ByState[i].Count > out.PGs.ByState[j].Count
		})
		out.IO = IO{
			ReadBytesPerSec:  pgs.ReadBytes,
			WriteBytesPerSec: pgs.WriteBytes,
			ReadOpsPerSec:    pgs.ReadOps,
			WriteOpsPerSec:   pgs.WriteOps,
		}
	})

	out.Unreadable = sortedKeys(unreadable)
	return out, nil
}

// mgrFrom projects the manager summary.
//
// Current releases emit a summary-only mgrmap: `available` and `num_standbys` are
// there, but `active_name` is not. That is why [Status.ActiveMgrUnknown] exists and
// why the plugin makes a second exec to fill it in — reporting "no active manager"
// from a missing name would be a false alarm on a perfectly healthy cluster, and
// the manager being down is a real condition that must stay distinguishable.
func mgrFrom(m rawMgrMap) Mgr {
	return Mgr{
		Available: m.Available,
		Standbys:  m.NumStandbys,
		Active:    m.ActiveName,
		Modules:   len(m.Modules),
	}
}

// ParseMgrStat decodes `ceph mgr stat -f json`, the small endpoint that does
// report the active manager's name.
func ParseMgrStat(payload []byte) (string, error) {
	var v struct {
		ActiveName string `json:"active_name"`
	}
	if err := json.Unmarshal(payload, &v); err != nil {
		return "", fmt.Errorf("ceph mgr stat json: %w", err)
	}
	return v.ActiveName, nil
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
