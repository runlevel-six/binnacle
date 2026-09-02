package store

import (
	"sync"
	"testing"
	"time"
)

func TestPutGetTyped(t *testing.T) {
	s := New()
	s.Put("k", []string{"a", "b"})

	got, ok := Get[[]string](s, "k")
	if !ok {
		t.Fatal("expected ok")
	}
	if len(got) != 2 || got[0] != "a" {
		t.Errorf("got %v", got)
	}
}

func TestGetTyped_WrongType(t *testing.T) {
	s := New()
	s.Put("k", "string-value")
	if _, ok := Get[int](s, "k"); ok {
		t.Error("expected wrong-type to return false")
	}
}

func TestGetTyped_Missing(t *testing.T) {
	s := New()
	if _, ok := Get[int](s, "nope"); ok {
		t.Error("expected missing to return false")
	}
}

func TestRaw(t *testing.T) {
	s := New()
	s.Put("k", 42)

	v, ok := s.Raw("k")
	if !ok {
		t.Fatal("expected ok")
	}
	if v.(int) != 42 {
		t.Errorf("Raw: got %v want 42", v)
	}
	if _, ok := s.Raw("absent"); ok {
		t.Error("expected absent key to return false")
	}
}

func TestKeys(t *testing.T) {
	s := New()
	s.Put("a", 1)
	s.Put("b", 2)
	s.Put("a", 3) // overwrite must not duplicate the key

	got := s.Keys()
	if len(got) != 2 {
		t.Fatalf("Keys: got %v want 2 entries", got)
	}
	seen := map[string]bool{}
	for _, k := range got {
		seen[k] = true
	}
	if !seen["a"] || !seen["b"] {
		t.Errorf("Keys: got %v want a and b", got)
	}
}

func TestSubscribeReceivesTick(t *testing.T) {
	s := New()
	ch := s.Subscribe()
	s.Put("k", 1)
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("no tick received")
	}
}

func TestSubscribe_LossyOnBackpressure(t *testing.T) {
	// A subscriber that never drains must not block Put.
	s := New()
	_ = s.Subscribe() // never read
	done := make(chan struct{})
	go func() {
		for range 100 {
			s.Put("k", 1)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Put blocked on slow subscriber")
	}
}

func TestUnsubscribe_StopsTicks(t *testing.T) {
	s := New()
	ch := s.Subscribe()
	s.Unsubscribe(ch)

	s.Put("k", 1)
	select {
	case <-ch:
		t.Fatal("received a tick after Unsubscribe")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestUnsubscribe_LeavesOtherSubscribers(t *testing.T) {
	s := New()
	keep := s.Subscribe()
	drop := s.Subscribe()
	s.Unsubscribe(drop)

	s.Put("k", 1)
	select {
	case <-keep:
	case <-time.After(time.Second):
		t.Fatal("surviving subscriber got no tick")
	}
}

func TestUnsubscribe_UnknownChannelIsNoOp(t *testing.T) {
	s := New()
	ch := s.Subscribe()
	other := make(chan struct{}, 1)

	s.Unsubscribe(other) // must not remove ch

	s.Put("k", 1)
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("Unsubscribe of an unknown channel removed the wrong subscriber")
	}
}

func TestConcurrentPuts(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Put("k", i)
		}()
	}
	wg.Wait()
	if _, ok := Get[int](s, "k"); !ok {
		t.Error("expected key to be set after concurrent puts")
	}
}

// TestConcurrentSubscribeUnsubscribeDuringPut is a race-detector target: it
// exercises the subscriber slice being mutated while Put walks a copy of it.
func TestConcurrentSubscribeUnsubscribeDuringPut(t *testing.T) {
	s := New()
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 200 {
			s.Put("k", 1)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 200 {
			ch := s.Subscribe()
			s.Unsubscribe(ch)
		}
	}()

	wg.Wait()
}
