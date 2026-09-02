package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
)

// This file carries error values across the wire, which encoding/json cannot
// do on its own.
//
// Several snapshot types hold an `Err error`: [model.Snapshot] has one, so
// does every subsystem state, and some sit inside slices —
// ovn.State.Statuses and openstack.State.Services each carry their own.
// error is an interface, and encoding/json can neither marshal one usefully
// (a *errors.errorString has only unexported fields, so it becomes {}) nor
// unmarshal into one at all: it cannot know which concrete type to allocate,
// so it records an UnmarshalTypeError and skips the field.
//
// Left alone that loses the one field the dashboard must never lose. Decoding
// returns an error, the caller drops the entry, and the client keeps whatever
// it had before — so a cluster whose API server has gone away goes on
// rendering its last good snapshot. "We could not read the nodes" arrives
// looking like "the nodes are fine", which is the distinction NodesKnown and
// WorkloadProblem exist to protect.
//
// So the errors travel beside the data rather than inside it. collect walks a
// value and reports every non-nil error by path; strip removes those members
// from the JSON so the rest decodes cleanly; apply puts them back on the far
// side. Paths are dotted, with slice indices as segments: "Err",
// "Statuses.0.Err", "Services.2.Err".
//
// The walk is guided by the target type rather than by the data, and it
// descends only into types that can hold an error. A snapshot of two hundred
// Machines costs one memoized type lookup, not two hundred visits.

var errorType = reflect.TypeFor[error]()

// holdsErrCache memoizes holdsError, which is pure and called per entry.
var holdsErrCache sync.Map // reflect.Type -> bool

// holdsError reports whether t can contain an error field at any depth.
//
// It is the gate on every walk below: a type that cannot hold an error is
// never descended into, which is what keeps this off the hot path for the
// large item slices that make up most of a snapshot.
func holdsError(t reflect.Type) bool {
	if v, ok := holdsErrCache.Load(t); ok {
		return v.(bool)
	}
	res := holdsErrorSeen(t, map[reflect.Type]bool{})
	holdsErrCache.Store(t, res)
	return res
}

func holdsErrorSeen(t reflect.Type, seen map[reflect.Type]bool) bool {
	if t == errorType {
		return true
	}
	// A recursive type would otherwise walk forever. Reporting false on the
	// second visit is correct: if the type holds an error, some other branch
	// of the first visit finds it.
	if seen[t] {
		return false
	}
	seen[t] = true

	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		return holdsErrorSeen(t.Elem(), seen)
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" {
				continue // unexported: encoding/json cannot see it either
			}
			if holdsErrorSeen(f.Type, seen) {
				return true
			}
		}
	}
	return false
}

// path joins a parent path and one segment.
func path(base, seg string) string {
	if base == "" {
		return seg
	}
	return base + "." + seg
}

// collect records every non-nil error reachable from v, keyed by path.
//
// It reads and never writes: v is a live store snapshot shared with whatever
// else is reading the store, so copying or clearing it here would be a data
// race. The error fields stay in the value and are stripped from the JSON
// instead.
func collect(v reflect.Value, at string, out map[string]string) {
	if !v.IsValid() || !holdsError(v.Type()) {
		return
	}
	switch v.Kind() {
	case reflect.Interface:
		if v.Type() == errorType && !v.IsNil() {
			if err, ok := v.Interface().(error); ok && err != nil {
				out[at] = err.Error()
			}
		}
	case reflect.Pointer:
		if !v.IsNil() {
			collect(v.Elem(), at, out)
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			if t.Field(i).PkgPath != "" {
				continue
			}
			collect(v.Field(i), path(at, t.Field(i).Name), out)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			collect(v.Index(i), path(at, strconv.Itoa(i)), out)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			collect(v.MapIndex(k), path(at, fmt.Sprint(k.Interface())), out)
		}
	}
}

// apply sets error fields on v from messages collected by [collect].
//
// v must be addressable. Paths that name no field are ignored rather than
// reported: a newer server may describe a field this client's types do not
// have, and dropping the unknown one is the same forward-compatibility rule
// Load applies to unknown keys.
func apply(v reflect.Value, at string, errs map[string]string) {
	if !v.IsValid() || !holdsError(v.Type()) {
		return
	}
	switch v.Kind() {
	case reflect.Interface:
		if v.Type() == errorType && v.CanSet() {
			if msg, ok := errs[at]; ok {
				v.Set(reflect.ValueOf(errors.New(msg)))
			}
		}
	case reflect.Pointer:
		if !v.IsNil() {
			apply(v.Elem(), at, errs)
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			if t.Field(i).PkgPath != "" {
				continue
			}
			apply(v.Field(i), path(at, t.Field(i).Name), errs)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			apply(v.Index(i), path(at, strconv.Itoa(i)), errs)
		}
	case reflect.Map:
		// Map values are not addressable, so each one is copied out, written
		// to, and put back.
		for _, k := range v.MapKeys() {
			ev := reflect.New(v.Type().Elem()).Elem()
			ev.Set(v.MapIndex(k))
			apply(ev, path(at, fmt.Sprint(k.Interface())), errs)
			v.SetMapIndex(k, ev)
		}
	}
}

// setTopErr sets a struct's own top-level Err field, if it has one.
//
// Used when a value could not be decoded: the reason belongs in the field the
// panes already read, so the failure renders as "unavailable, and here is
// why" rather than as an absent key.
func setTopErr(v reflect.Value, err error) bool {
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return false
	}
	f := v.FieldByName("Err")
	if !f.IsValid() || f.Type() != errorType || !f.CanSet() {
		return false
	}
	f.Set(reflect.ValueOf(err))
	return true
}

// strip removes error-typed members from raw JSON, guided by the target type,
// and reports how many it removed.
//
// This is what lets the rest of the value decode: with the error members gone,
// encoding/json has nothing it cannot handle, so a decode failure afterwards
// is a real one rather than the interface field every snapshot carries.
func strip(data []byte, t reflect.Type) (out []byte, removed int, err error) {
	if !holdsError(t) {
		return data, 0, nil
	}
	var x any
	if err := json.Unmarshal(data, &x); err != nil {
		return nil, 0, err
	}
	x, removed = stripValue(x, t)
	out, err = json.Marshal(x)
	if err != nil {
		return nil, 0, err
	}
	return out, removed, nil
}

func stripValue(x any, t reflect.Type) (any, int) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if x == nil || !holdsError(t) {
		return x, 0
	}
	var removed int
	switch t.Kind() {
	case reflect.Struct:
		m, ok := x.(map[string]any)
		if !ok {
			return x, 0
		}
		for key, val := range m {
			f, ok := fieldFor(t, key)
			if !ok {
				continue
			}
			if f.Type == errorType {
				// A nil error marshals to null, which decodes fine and says
				// nothing. Only a real one needs removing, and only that one
				// is counted.
				if val != nil {
					removed++
				}
				delete(m, key)
				continue
			}
			sub, n := stripValue(val, f.Type)
			m[key] = sub
			removed += n
		}
		return m, removed
	case reflect.Slice, reflect.Array:
		a, ok := x.([]any)
		if !ok {
			return x, 0
		}
		for i := range a {
			sub, n := stripValue(a[i], t.Elem())
			a[i] = sub
			removed += n
		}
		return a, removed
	case reflect.Map:
		m, ok := x.(map[string]any)
		if !ok {
			return x, 0
		}
		for k, val := range m {
			sub, n := stripValue(val, t.Elem())
			m[k] = sub
			removed += n
		}
		return m, removed
	}
	return x, 0
}

// fieldFor resolves a JSON member name to a struct field the way
// encoding/json does: the json tag name if there is one, matched exactly and
// then case-insensitively.
func fieldFor(t reflect.Type, key string) (reflect.StructField, bool) {
	var fold *reflect.StructField
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		name := jsonName(f)
		if name == key {
			return f, true
		}
		if fold == nil && strings.EqualFold(name, key) {
			g := f
			fold = &g
		}
	}
	if fold != nil {
		return *fold, true
	}
	return reflect.StructField{}, false
}

func jsonName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" || tag == "-" {
		return f.Name
	}
	if i := strings.Index(tag, ","); i >= 0 {
		tag = tag[:i]
	}
	if tag == "" {
		return f.Name
	}
	return tag
}
