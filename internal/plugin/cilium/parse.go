package cilium

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Parsing `cilium status -o json`.
//
// # Why each section is decoded separately
//
// The schema is unversioned and changes shape between releases. The obvious
// approach — one struct for the whole document — has a failure mode that took a
// real cluster to expose: `json.Unmarshal` aborts on the *first* type mismatch
// anywhere in the tree, so a single field of an unexpected type discards
// everything. On one cluster `hubble.observer.uptime` was a duration string
// rather than a number, and that lost the version, the kube-proxy mode, IPAM and
// the controller counts along with it.
//
// So the envelope holds each section as raw JSON and every section is decoded on
// its own. A section we cannot read becomes zero and is named in
// [Status.Unreadable]; the rest still land. That is what "degrades one cell"
// actually requires.

// rawEnvelope keeps each section raw so one bad section cannot take the others
// down with it.
type rawEnvelope struct {
	Cilium               json.RawMessage `json:"cilium"`
	KubeProxyReplacement json.RawMessage `json:"kube-proxy-replacement"`
	Encryption           json.RawMessage `json:"encryption"`
	IPAM                 json.RawMessage `json:"ipam"`
	Hubble               json.RawMessage `json:"hubble"`
	Controllers          json.RawMessage `json:"controllers"`
	ClusterMesh          json.RawMessage `json:"cluster-mesh"`
}

type rawCilium struct {
	State   string `json:"state"`
	Version string `json:"version"`
	// Msg carries the version on releases that have no version field at all.
	// Observed on 1.19: {"state":"Ok","msg":"1.19.6 (v1.19.6-9a898243)"}.
	Msg string `json:"msg"`
}

type rawMode struct {
	Mode string `json:"mode"`
}

// rawIPAM keeps ipv4 raw because its shape varies. See ipamFrom.
type rawIPAM struct {
	IPv4 json.RawMessage `json:"ipv4"`
	// Some releases summarize at the top of the ipam block instead.
	Used      int `json:"used"`
	Available int `json:"available"`
	// Older releases emit a flat list of allocated addresses.
	IPv4Allocations json.RawMessage `json:"ipv4-allocations"`
	// Status is a human-readable summary, e.g.
	// "IPv4: 124/254 allocated from 172.18.7.0/24, ". Observed on 1.19, where it
	// is the *only* place the pool total appears — every other field in that
	// release's block lists individual addresses.
	Status string `json:"status"`
}

type rawIPAMObject struct {
	Used      int                `json:"used"`
	Available int                `json:"available"`
	Allocated []rawIPAMAllocated `json:"allocated"`
}

type rawIPAMAllocated struct {
	Pool      string `json:"pool"`
	Used      int    `json:"used"`
	Available int    `json:"available"`
}

// rawHubble deliberately declares only what is used.
//
// `uptime` is absent from this struct even though the JSON carries it, because it
// is a number on some releases and a duration string on others — and the flow
// rate is derived between polls rather than from uptime, so the field has no
// consumer. A field that is not read cannot break the parse.
type rawHubble struct {
	State    string            `json:"state"`
	Observer *rawHubbleObserve `json:"observer"`
}

type rawHubbleObserve struct {
	SeenFlows int64 `json:"seen-flows"`
}

type rawController struct {
	Status struct {
		SuccessCount     int `json:"success-count"`
		ConsecutiveFails int `json:"consecutive-failure-count"`
	} `json:"status"`
}

type rawClusterMesh struct {
	NumGlobalServices int `json:"num-global-services"`
	Clusters          []struct {
		Name  string `json:"name"`
		Ready bool   `json:"ready"`
	} `json:"clusters"`
}

// ParseStatus decodes `cilium status -o json`.
//
// It returns an error only when the payload is not JSON at all. A section that
// cannot be decoded is skipped and named in [Status.Unreadable], because losing
// one cell is a far better outcome than losing the pane.
func ParseStatus(payload []byte) (Status, error) {
	var env rawEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return Status{}, fmt.Errorf("cilium status json: %w", err)
	}

	out := Status{}
	unreadable := map[string]bool{}

	// section decodes one raw block, recording rather than propagating a failure.
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

	var cil rawCilium
	section("cilium", env.Cilium, &cil, func() {
		out.State = cil.State
		// The version field is absent on some releases, which put it in msg
		// instead: {"state":"Ok","msg":"1.19.6 (v1.19.6-9a898243)"}.
		out.Version = stripVersionExtras(firstNonEmpty(cil.Version, cil.Msg))
	})

	var kpr rawMode
	section("kube-proxy-replacement", env.KubeProxyReplacement, &kpr, func() {
		out.KubeProxyReplacement = kpr.Mode
	})

	var enc rawMode
	section("encryption", env.Encryption, &enc, func() {
		out.EncryptionMode = enc.Mode
	})

	var ipam rawIPAM
	section("ipam", env.IPAM, &ipam, func() {
		out.IPAM = ipamFrom(ipam)
	})

	var hub rawHubble
	section("hubble", env.Hubble, &hub, func() {
		out.Hubble = Hubble{
			State: hub.State,
			// An empty state means the field is absent, not that Hubble is off,
			// so neither is reported as enabled.
			Enabled: hub.State != "" && !strings.EqualFold(hub.State, "Disabled"),
		}
		if hub.Observer != nil {
			out.Hubble.SeenFlows = hub.Observer.SeenFlows
		}
	})

	var ctrls []rawController
	section("controllers", env.Controllers, &ctrls, func() {
		for _, c := range ctrls {
			out.Controllers.Total++
			if c.Status.ConsecutiveFails > 0 {
				out.Controllers.Failing++
			}
		}
	})

	var mesh rawClusterMesh
	section("cluster-mesh", env.ClusterMesh, &mesh, func() {
		out.ClusterMesh.GlobalServices = mesh.NumGlobalServices
		for _, c := range mesh.Clusters {
			out.ClusterMesh.Peers = append(out.ClusterMesh.Peers, MeshPeer{Name: c.Name, Ready: c.Ready})
		}
	})

	out.Unreadable = sortedKeys(unreadable)
	return out, nil
}

// ipamStatusRE extracts the counts from the human-readable IPAM summary, e.g.
// "IPv4: 124/254 allocated from 172.18.7.0/24, ".
var ipamStatusRE = regexp.MustCompile(`IPv4:\s*(\d+)\s*/\s*(\d+)\s+allocated`)

// ipamFrom extracts IPv4 pod-address usage from whichever shape this release
// emits, trying each in turn:
//
//  1. `ipam.status`, a human summary like "IPv4: 124/254 allocated from …".
//     Parsing prose is unappealing, but on 1.19 it is the *only* place the pool
//     total appears — every structured field there lists individual addresses —
//     and a used count without a total cannot answer "are we about to run out".
//  2. `ipam.used` / `ipam.available` summarized at the top of the block.
//  3. `ipam.ipv4` as an object with used/available, or with an `allocated`
//     list of per-pool entries to sum.
//  4. `ipam.ipv4` as a bare array of allocation entries, with no wrapper.
//  5. `ipam.ipv4` as a bare array of address *strings*, where only the count is
//     knowable.
//  6. `ipam.ipv4-allocations` as a flat list of addresses, likewise count-only.
//
// Every path returns a zero value on mismatch rather than an error. Five shapes
// have now been observed across six releases, so this will change again and must
// never be able to cost more than itself.
func ipamFrom(r rawIPAM) IPAM {
	if m := ipamStatusRE.FindStringSubmatch(r.Status); m != nil {
		used, uErr := strconv.Atoi(m[1])
		total, tErr := strconv.Atoi(m[2])
		if uErr == nil && tErr == nil && total >= used {
			return IPAM{Used: used, Available: total - used}
		}
	}

	if r.Used > 0 || r.Available > 0 {
		return IPAM{Used: r.Used, Available: r.Available}
	}

	if len(r.IPv4) > 0 {
		var obj rawIPAMObject
		if err := json.Unmarshal(r.IPv4, &obj); err == nil {
			if obj.Used > 0 || obj.Available > 0 {
				return IPAM{Used: obj.Used, Available: obj.Available}
			}
			if sum := sumAllocated(obj.Allocated); sum.Used > 0 || sum.Available > 0 {
				return sum
			}
		}
		var arr []rawIPAMAllocated
		if err := json.Unmarshal(r.IPv4, &arr); err == nil {
			if sum := sumAllocated(arr); sum.Used > 0 || sum.Available > 0 {
				return sum
			}
		}
		// A bare array of address strings: the count is all that is knowable.
		var addrs []string
		if err := json.Unmarshal(r.IPv4, &addrs); err == nil && len(addrs) > 0 {
			return IPAM{Used: len(addrs)}
		}
	}

	if len(r.IPv4Allocations) > 0 {
		var addrs []string
		if err := json.Unmarshal(r.IPv4Allocations, &addrs); err == nil && len(addrs) > 0 {
			// No remaining count exists in this shape, so Available stays zero and
			// Total equals Used. The pane must not present that as a full pool,
			// which is what ExhaustionKnown is for.
			return IPAM{Used: len(addrs)}
		}
	}
	return IPAM{}
}

func sumAllocated(entries []rawIPAMAllocated) IPAM {
	var out IPAM
	for _, a := range entries {
		out.Used += a.Used
		out.Available += a.Available
	}
	return out
}

// stripVersionExtras reduces a reported version to its number.
//
// Cilium appends build metadata — "1.16.5 abcdef1 2025-01-01T00:00:00+00:00
// go version go1.23" — which is noise in a status cell.
func stripVersionExtras(v string) string {
	if v == "" {
		return ""
	}
	if i := strings.IndexAny(v, " \t"); i > 0 {
		return v[:i]
	}
	return v
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
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
