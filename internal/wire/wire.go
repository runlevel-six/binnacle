// Package wire serializes a [store.Store] for HTTP transport.
//
// The store holds map[string]any where each value is a concrete type
// (model.Snapshot[model.Machine], ceph.State, etc.). Go's encoding/json can
// marshal these via reflection, but it cannot round-trip the error interface
// fields several of them carry: an error marshals to {} and will not unmarshal
// back at all. Those fields travel beside the data instead, in [Entry.Errs] —
// see errfields.go for why that matters more than it looks.
//
// This package owns that contract: Dump marshals each store key's value to
// JSON, Load decodes it back into the correct concrete type and puts it into a
// store. Both sides share the same key→type table.
package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/runlevel-six/binnacle/pkg/model"
	"github.com/runlevel-six/binnacle/pkg/store"
	"github.com/runlevel-six/binnacle/pkg/subsystem/ceph"
	"github.com/runlevel-six/binnacle/pkg/subsystem/cilium"
	"github.com/runlevel-six/binnacle/pkg/subsystem/metallb"
	"github.com/runlevel-six/binnacle/pkg/subsystem/openstack"
	"github.com/runlevel-six/binnacle/pkg/subsystem/ovn"
)

// errNoErrText stands in when a value arrived carrying an error the sender did
// not describe — an older server, which marshaled the field to {} and sent no
// Errs beside it. The text is gone, but the fact of the failure is not, and a
// pane that says "unavailable" is right where one that says "healthy" is not.
var errNoErrText = errors.New("the server reported a failure but sent no detail (older server?)")

// Entry is one store key and its JSON-serialized value.
type Entry struct {
	Key  string          `json:"key"`
	Data json.RawMessage `json:"data"`
	// Errs carries the error fields Data cannot, keyed by path within the
	// value ("Err", "Statuses.0.Err"). Omitted when nothing failed, which is
	// the common case and keeps a healthy fleet's payload unchanged.
	Errs map[string]string `json:"errs,omitempty"`
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
		e := Entry{Key: key, Data: data}
		if v := reflect.ValueOf(val); v.IsValid() && holdsError(v.Type()) {
			errs := map[string]string{}
			collect(v, "", errs)
			if len(errs) > 0 {
				e.Errs = errs
			}
		}
		entries = append(entries, e)
	}
	return entries
}

// decoder is a function that unmarshals JSON into the concrete type
// stored under a given key, returning the value as any for store.Put.
type decoder func(json.RawMessage, map[string]string) (any, error)

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

func decodeSnapshot[T any](data json.RawMessage, errs map[string]string) (any, error) {
	return decodeInto[model.Snapshot[T]](data, errs)
}

func decodeType[T any](data json.RawMessage, errs map[string]string) (any, error) {
	return decodeInto[T](data, errs)
}

// decodeInto decodes one entry into T and restores its error fields.
//
// The common case — nothing failed on the far side — is a plain Unmarshal,
// because a nil error marshals to null and decodes back to nil without help.
// Only an entry that actually carries an error pays for the strip pass.
func decodeInto[T any](data json.RawMessage, errs map[string]string) (any, error) {
	var v T

	if len(errs) == 0 {
		if err := json.Unmarshal(data, &v); err == nil {
			return v, nil
		}
		// Fall through. Either the payload is malformed, which strip will
		// also find, or it came from a server old enough to send an error
		// object with no Errs beside it.
		v = *new(T)
	}

	clean, removed, err := strip(data, reflect.TypeFor[T]())
	if err != nil {
		return undecodable[T](err)
	}
	if err := json.Unmarshal(clean, &v); err != nil {
		return undecodable[T](err)
	}

	switch {
	case len(errs) > 0:
		apply(reflect.ValueOf(&v).Elem(), "", errs)
	case removed > 0:
		setTopErr(reflect.ValueOf(&v).Elem(), errNoErrText)
	}
	return v, nil
}

// undecodable returns a zero T carrying the reason it could not be decoded.
//
// Returning the right type with its Err set, rather than nothing, is what
// keeps a failed decode from reading as an absent subsystem: every type in the
// table above has a top-level Err, so the pane's existing "unavailable" path
// renders the reason. The error is returned as well, so Load can fall back for
// a type that has no such field.
func undecodable[T any](cause error) (any, error) {
	var v T
	if setTopErr(reflect.ValueOf(&v).Elem(), cause) {
		return v, nil
	}
	return nil, cause
}

// Load decodes wire entries and puts them into a store.
//
// Unknown keys are skipped: a server running a newer version may publish keys
// the client does not recognize, and the correct behavior is to ignore them
// rather than fail.
//
// A key the client *does* know but cannot decode is a different case, and it
// must not be skipped. Leaving the previous value in place would let a pane go
// on rendering the last good snapshot as though it were current — the failure
// this whole package exists to prevent. The reason is published under the key
// instead, in the shape the panes already look for.
func Load(entries []Entry, s *store.Store) {
	for _, e := range entries {
		dec, ok := decoders[e.Key]
		if !ok {
			continue
		}
		val, err := dec(e.Data, e.Errs)
		if err != nil {
			s.Put(e.Key, model.ErrorSnapshot(fmt.Errorf("decoding %s from the server: %w", e.Key, err)))
			continue
		}
		s.Put(e.Key, val)
	}
}
