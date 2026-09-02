// Package store is the central state holder that data sources write into and
// that UI panes read from.
//
// The design is deliberately simple:
//
//   - One Store, many string keys (e.g. "mgmt/machines", "workload/nodes").
//   - Sources call Put(key, snapshot). Snapshots are immutable from the store's
//     point of view — a source hands over a freshly-built value rather than
//     mutating the previous one in place.
//   - Panes call Get[T](s, key) to retrieve a typed snapshot.
//   - Anyone can Subscribe to receive a tick on every Put.
//
// The store does not know what types live under what keys. That contract is
// established by whoever owns the key — conventionally, the package that
// declares the key constant also declares the snapshot type.
//
// A Store is safe for concurrent use by any number of goroutines.
package store

import "sync"

// Store is a thread-safe map of opaque snapshots keyed by string.
//
// The zero value is not usable; call New.
type Store struct {
	mu        sync.RWMutex
	snapshots map[string]any
	subs      []chan struct{}
}

// New returns an empty Store.
func New() *Store {
	return &Store{snapshots: make(map[string]any)}
}

// Put stores snapshot at key and notifies every subscriber.
//
// Notifications are non-blocking sends: if a subscriber's buffer is already
// full it misses this tick and catches up on the next one. That is the right
// behavior for a redraw trigger, where only the latest state matters.
func (s *Store) Put(key string, snapshot any) {
	s.mu.Lock()
	s.snapshots[key] = snapshot
	subs := append([]chan struct{}(nil), s.subs...)
	s.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Raw returns the untyped snapshot at key.
//
// Prefer the generic [Get], which does the type assertion for you. Raw exists
// for callers that genuinely need to inspect an unknown value, such as a
// debug dump.
func (s *Store) Raw(key string) (any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.snapshots[key]
	return v, ok
}

// Keys returns every key currently present, in unspecified order. Intended
// for diagnostics.
func (s *Store) Keys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.snapshots))
	for k := range s.snapshots {
		out = append(out, k)
	}
	return out
}

// Subscribe returns a channel that receives a tick whenever any key changes.
//
// The channel is buffered (size 1) with lossy semantics — see [Store.Put].
// Callers that stop reading should call [Store.Unsubscribe] to release it;
// a subscriber that is never unsubscribed is retained for the life of the
// Store.
func (s *Store) Subscribe() <-chan struct{} {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs = append(s.subs, ch)
	s.mu.Unlock()
	return ch
}

// Unsubscribe releases a channel previously returned by [Store.Subscribe].
// Unsubscribing a channel that is not subscribed is a no-op. The channel is
// not closed, so a concurrent reader blocked on it will not observe a
// spurious tick.
func (s *Store) Unsubscribe(ch <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, sub := range s.subs {
		if (<-chan struct{})(sub) == ch {
			s.subs = append(s.subs[:i], s.subs[i+1:]...)
			return
		}
	}
}

// Get is the typed accessor. It reports ok=false if the key is missing or if
// the stored value is not a T — the latter usually meaning a source and a
// pane disagree about the type behind a key.
func Get[T any](s *Store, key string) (T, bool) {
	var zero T
	v, ok := s.Raw(key)
	if !ok {
		return zero, false
	}
	t, ok := v.(T)
	if !ok {
		return zero, false
	}
	return t, true
}
