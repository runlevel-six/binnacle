package wire

import (
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/runlevel-six/binnacle/pkg/model"
	"github.com/runlevel-six/binnacle/pkg/store"
	"github.com/runlevel-six/binnacle/pkg/subsystem/ceph"
	"github.com/runlevel-six/binnacle/pkg/subsystem/ovn"
)

// The bug these cover: an error field made the whole entry undecodable, Load
// skipped it, and the client kept the value it already had. A cluster whose
// API server had gone away went on rendering its last good snapshot.

// TestLoad_AnErroredSnapshotReplacesTheGoodOne is the regression test. It is
// the one to keep if any of the others go: it asserts the failure mode
// directly, with a destination store that already holds good data.
func TestLoad_AnErroredSnapshotReplacesTheGoodOne(t *testing.T) {
	dst := store.New()
	dst.Put(model.KeyWorkloadNodes, model.Snapshot[model.Node]{
		Items:     []model.Node{{Name: "node-1", Status: "Ready"}},
		UpdatedAt: time.Now(),
	})

	src := store.New()
	src.Put(model.KeyWorkloadNodes, model.Snapshot[model.Node]{
		Err:       errors.New(`nodes is forbidden: User "system:serviceaccount:binnacle:binnacle" cannot list resource "nodes"`),
		UpdatedAt: time.Now(),
	})

	Load(Dump(src), dst)

	got, ok := store.Get[model.Snapshot[model.Node]](dst, model.KeyWorkloadNodes)
	if !ok {
		t.Fatal("the nodes key was dropped entirely")
	}
	if got.Err == nil {
		t.Fatal("the error did not survive: the pane would render this as healthy")
	}
	if want := "nodes is forbidden"; !strings.Contains(got.Err.Error(), want) {
		t.Errorf("error text lost: got %q, want it to contain %q", got.Err, want)
	}
	if len(got.Items) != 0 {
		t.Errorf("stale items survived an errored snapshot: %v", got.Items)
	}
}

// TestRoundTrip_EveryRegisteredKeyKeepsItsErrors walks the decoder table
// itself, so a type that grows a new error field is covered without anyone
// remembering to add a case here. It seeds every error position it can reach,
// including one element inside each slice that can hold one.
func TestRoundTrip_EveryRegisteredKeyKeepsItsErrors(t *testing.T) {
	for key, dec := range decoders {
		t.Run(key, func(t *testing.T) {
			// The decoder is the only thing that knows the concrete type, so
			// decoding an empty object is how the test gets a zero value of it.
			zero, err := dec([]byte(`{}`), nil)
			if err != nil {
				t.Fatalf("decoding {} into the type for %q: %v", key, err)
			}
			v := reflect.New(reflect.TypeOf(zero)).Elem()
			v.Set(reflect.ValueOf(zero))

			want := map[string]string{}
			seedErrors(v, "", want, 0)
			if len(want) == 0 {
				t.Fatalf("no error field reachable in the type for %q, but the "+
					"wire contract assumes every registered type carries one", key)
			}

			src := store.New()
			src.Put(key, v.Interface())

			dst := store.New()
			Load(Dump(src), dst)

			raw, ok := dst.Raw(key)
			if !ok {
				t.Fatalf("%q was dropped by Load", key)
			}
			got := map[string]string{}
			collect(reflect.ValueOf(raw), "", got)

			if !maps.Equal(got, want) {
				t.Errorf("errors did not survive the round trip\ngot:  %v\nwant: %v", got, want)
			}
		})
	}
}

// TestRoundTrip_NestedErrorsInsideSlices names the case the generic test
// covers by construction, because it is the one a hand-written mirror type
// would most likely miss.
func TestRoundTrip_NestedErrorsInsideSlices(t *testing.T) {
	src := store.New()
	src.Put(ovn.KeyState, ovn.State{
		Statuses: []ovn.ClusterStatus{
			{Err: errors.New("nb: no ovsdb pod answered")},
			{Err: errors.New("sb: no pods/exec permission on openstack")},
		},
		Err: errors.New("ovn is degraded"),
	})

	dst := store.New()
	Load(Dump(src), dst)

	got, ok := store.Get[ovn.State](dst, ovn.KeyState)
	if !ok {
		t.Fatal("ovn state was dropped")
	}
	if got.Err == nil {
		t.Error("the state's own error was lost")
	}
	if len(got.Statuses) != 2 {
		t.Fatalf("expected 2 statuses, got %d", len(got.Statuses))
	}
	for i, st := range got.Statuses {
		if st.Err == nil {
			t.Errorf("Statuses[%d] lost its error", i)
		}
	}
	if !strings.Contains(got.Statuses[1].Err.Error(), "no pods/exec permission") {
		t.Errorf("Statuses[1] error text wrong: %q", got.Statuses[1].Err)
	}
}

// TestDump_HealthyPayloadCarriesNoErrs keeps the common case free: a fleet
// with nothing wrong sends exactly the bytes it sent before this change.
func TestDump_HealthyPayloadCarriesNoErrs(t *testing.T) {
	src := store.New()
	src.Put(model.KeyMgmtMachines, model.Snapshot[model.Machine]{
		Items: []model.Machine{{Name: "machine-1", Phase: "Running"}},
	})
	src.Put(ceph.KeyState, ceph.State{Status: ceph.Status{Health: "HEALTH_OK"}})

	for _, e := range Dump(src) {
		if e.Errs != nil {
			t.Errorf("%q carried an errs map with nothing wrong: %v", e.Key, e.Errs)
		}
		var probe struct {
			Errs map[string]string `json:"errs"`
		}
		blob, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(blob, &probe); err != nil {
			t.Fatal(err)
		}
		if probe.Errs != nil {
			t.Errorf("%q serialized an errs member: %s", e.Key, blob)
		}
	}
}

// TestLoad_ErrorObjectWithoutErrsMap covers an older server: it marshaled the
// error to {} and sent no errs beside it. The text is unrecoverable, but the
// fact of the failure must not be, or the pane reads as healthy.
func TestLoad_ErrorObjectWithoutErrsMap(t *testing.T) {
	dst := store.New()
	Load([]Entry{{
		Key:  model.KeyWorkloadPods,
		Data: json.RawMessage(`{"Items":null,"UpdatedAt":"2026-09-02T00:00:00Z","Err":{},"Note":""}`),
	}}, dst)

	got, ok := store.Get[model.Snapshot[model.Pod]](dst, model.KeyWorkloadPods)
	if !ok {
		t.Fatal("the pods key was dropped")
	}
	if got.Err == nil {
		t.Fatal("an error object with no errs map decoded as healthy")
	}
	if !errors.Is(got.Err, errNoErrText) {
		t.Errorf("expected the no-detail sentinel, got %q", got.Err)
	}
}

// TestLoad_UndecodableEntryPublishesTheReason: a key the client knows but
// cannot parse must not leave the previous value standing either.
func TestLoad_UndecodableEntryPublishesTheReason(t *testing.T) {
	dst := store.New()
	dst.Put(ceph.KeyState, ceph.State{Status: ceph.Status{Health: "HEALTH_OK"}})

	Load([]Entry{{
		Key:  ceph.KeyState,
		Data: json.RawMessage(`{"Status":{"Health":42}}`), // Health is a string
	}}, dst)

	got, ok := store.Get[ceph.State](dst, ceph.KeyState)
	if !ok {
		t.Fatal("ceph state was dropped")
	}
	if got.Err == nil {
		t.Fatal("an undecodable entry left the stale value standing")
	}
	if got.Status.Health == "HEALTH_OK" {
		t.Error("the stale HEALTH_OK survived a decode failure")
	}
}

// seedErrors fills every error field reachable from v with a distinct message,
// growing one element into any empty slice whose elements can hold an error so
// the nested cases are exercised. It records what it set, keyed by the paths
// collect uses.
func seedErrors(v reflect.Value, at string, out map[string]string, depth int) {
	const maxDepth = 8 // insurance against a self-referential type
	if depth > maxDepth || !v.IsValid() || !holdsError(v.Type()) {
		return
	}
	switch v.Kind() {
	case reflect.Interface:
		if v.Type() == errorType && v.CanSet() {
			msg := "seeded failure at " + at
			v.Set(reflect.ValueOf(errors.New(msg)))
			out[at] = msg
		}
	case reflect.Pointer:
		if v.IsNil() && v.CanSet() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		if !v.IsNil() {
			seedErrors(v.Elem(), at, out, depth+1)
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			if t.Field(i).PkgPath != "" {
				continue
			}
			seedErrors(v.Field(i), path(at, t.Field(i).Name), out, depth+1)
		}
	case reflect.Slice:
		if v.Len() == 0 && v.CanSet() {
			v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		}
		for i := 0; i < v.Len(); i++ {
			seedErrors(v.Index(i), path(at, strconv.Itoa(i)), out, depth+1)
		}
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			seedErrors(v.Index(i), path(at, strconv.Itoa(i)), out, depth+1)
		}
	}
	// Maps are left alone: no wire type holds an error behind one, and
	// inventing a key would test the test rather than the code.
}
