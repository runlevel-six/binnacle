// Package wire serializes a [store.Store] for HTTP transport.
//
// The store holds map[string]any where each value is a concrete type
// (model.Snapshot[model.Machine], ceph.State, etc.). Go's encoding/json
// can marshal these via reflection, but the error interface fields on
// several types are problematic: json.Marshal on *errors.errorString
// produces {} (unexported fields are skipped), which is lossy but does
// not fail. On the decode side, {} into an error field yields nil.
//
// This package owns that contract: Dump marshals each store key's value
// to JSON, Load decodes it back into the correct concrete type and puts
// it into a store. Both sides share the same key→type table.
package wire

import (
	"encoding/json"
	"sort"

	"github.com/runlevel-six/binnacle/pkg/model"
	"github.com/runlevel-six/binnacle/pkg/store"
	"github.com/runlevel-six/binnacle/pkg/subsystem/ceph"
	"github.com/runlevel-six/binnacle/pkg/subsystem/cilium"
	"github.com/runlevel-six/binnacle/pkg/subsystem/metallb"
	"github.com/runlevel-six/binnacle/pkg/subsystem/openstack"
	"github.com/runlevel-six/binnacle/pkg/subsystem/ovn"
)

// Entry is one store key and its JSON-serialized value.
type Entry struct {
	Key  string          `json:"key"`
	Data json.RawMessage `json:"data"`
}

// Dump serializes a store's contents to a slice of wire entries, sorted
// by key for deterministic output.
func Dump(s *store.Store) []Entry {
	keys := s.Keys()
	sort.Strings(keys)
	var entries []Entry
	for _, key := range keys {
		val, ok := s.Raw(key)
		if !ok {
			continue
		}
		data, err := json.Marshal(val)
		if err != nil {
			continue
		}
		entries = append(entries, Entry{Key: key, Data: data})
	}
	return entries
}

// decoder is a function that unmarshals JSON into the concrete type
// stored under a given key, returning the value as any for store.Put.
type decoder func(json.RawMessage) (any, error)

// decoders maps each store key to its concrete type's decoder.
var decoders = map[string]decoder{
	model.KeyMgmtClusters:           decodeSnapshot[model.Cluster],
	model.KeyMgmtKCPs:               decodeSnapshot[model.KubeadmControlPlane],
	model.KeyMgmtMachineDeployments: decodeSnapshot[model.MachineDeployment],
	model.KeyMgmtMachines:           decodeSnapshot[model.Machine],
	model.KeyMgmtEvents:             decodeSnapshot[model.Event],
	model.KeyMgmtMetal3Clusters:     decodeSnapshot[model.Metal3Cluster],
	model.KeyMgmtMetal3Machines:     decodeSnapshot[model.Metal3Machine],
	model.KeyMgmtBareMetalHosts:     decodeSnapshot[model.BareMetalHost],
	model.KeyWorkloadNodes:          decodeSnapshot[model.Node],
	model.KeyWorkloadPods:           decodeSnapshot[model.Pod],
	model.KeyWorkloadEvents:         decodeSnapshot[model.Event],
	model.KeyWorkloadWorkloads:      decodeSnapshot[model.Workload],

	metallb.KeyState:        decodeType[metallb.State],
	cilium.KeyState:         decodeType[cilium.State],
	ceph.KeyState:           decodeType[ceph.State],
	ovn.KeyState:            decodeType[ovn.State],
	openstack.KeyState:      decodeType[openstack.State],
	openstack.KeyMigrations: decodeType[openstack.Migrations],
	openstack.KeyInventory:  decodeType[openstack.Inventory],
}

func decodeSnapshot[T any](data json.RawMessage) (any, error) {
	var v model.Snapshot[T]
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return v, nil
}

func decodeType[T any](data json.RawMessage) (any, error) {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// Load decodes wire entries and puts them into a store. Unknown keys
// are skipped: a server running a newer version may publish keys the
// client does not recognize, and the correct behavior is to ignore them
// rather than fail.
func Load(entries []Entry, s *store.Store) {
	for _, e := range entries {
		dec, ok := decoders[e.Key]
		if !ok {
			continue
		}
		val, err := dec(e.Data)
		if err != nil {
			continue
		}
		s.Put(e.Key, val)
	}
}
