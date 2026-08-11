package eventmetrics

import (
	"sync"
	"testing"
)

func TestCounterConcurrent(t *testing.T) {
	var c Counter
	var wg sync.WaitGroup
	n := 1000
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Inc()
		}()
	}
	wg.Wait()
	if v := c.Value(); v != int64(n) {
		t.Errorf("counter value = %d, want %d", v, n)
	}
}

func TestCounterReset(t *testing.T) {
	var c Counter
	c.Inc()
	c.Inc()
	c.Reset()
	if v := c.Value(); v != 0 {
		t.Errorf("after reset, value = %d, want 0", v)
	}
}

func TestUnknownEvent(t *testing.T) {
	ResetAll() // ensure clean state

	c1 := UnknownEvent("claude", "unknown_type")
	if c1 == nil {
		t.Fatal("UnknownEvent returned nil")
	}
	c1.Inc()
	if v := c1.Value(); v != 1 {
		t.Errorf("unknown event counter = %d, want 1", v)
	}

	// Same key returns the same counter.
	c2 := UnknownEvent("claude", "unknown_type")
	if c2 != c1 {
		t.Error("UnknownEvent returned different counter for same key")
	}

	// Different key returns a different counter.
	c3 := UnknownEvent("opencode", "unknown_type")
	if c3 == c1 {
		t.Error("UnknownEvent returned same counter for different key")
	}
}

func TestUnknownEventConcurrent(t *testing.T) {
	ResetAll()

	var wg sync.WaitGroup
	keys := []string{"claude", "opencode", "omp", "miniagent"}
	for range 100 {
		for _, b := range keys {
			for _, e := range []string{"a", "b", "c"} {
				wg.Add(1)
				go func(backend, eventType string) {
					defer wg.Done()
					UnknownEvent(backend, eventType).Inc()
				}(b, e)
			}
		}
	}
	wg.Wait()

	// Each of the 12 (backend,event) combos should have 100.
	for _, b := range keys {
		for _, e := range []string{"a", "b", "c"} {
			if v := UnknownEvent(b, e).Value(); v != 100 {
				t.Errorf("UnknownEvent(%q,%q) = %d, want 100", b, e, v)
			}
		}
	}
}

func TestResetAll(t *testing.T) {
	TerminalEmitRetries.Inc()
	UnknownEvent("miniagent", "unknown").Inc()

	ResetAll()

	if v := TerminalEmitRetries.Value(); v != 0 {
		t.Errorf("TerminalEmitRetries = %d after ResetAll, want 0", v)
	}
	if v := UnknownEvent("miniagent", "unknown").Value(); v != 0 {
		t.Errorf("UnknownEvent(miniagent,unknown) = %d after ResetAll, want 0", v)
	}
}

// TestLineTruncated verifies the per-backend truncation counters are
// deduplicated per backend and isolated across backends.
func TestLineTruncated(t *testing.T) {
	ResetAll()
	c1 := LineTruncated("omp")
	if c1 == nil {
		t.Fatal("LineTruncated returned nil")
	}
	c1.Inc()
	if LineTruncated("omp") != c1 {
		t.Error("LineTruncated returned different counter for same backend")
	}
	if LineTruncated("claude") == c1 {
		t.Error("LineTruncated returned same counter for different backends")
	}
	if got := LineTruncated("omp").Value(); got != 1 {
		t.Errorf("LineTruncated(omp) = %d, want 1", got)
	}
	ResetAll()
	if got := LineTruncated("omp").Value(); got != 0 {
		t.Errorf("LineTruncated(omp) = %d after ResetAll, want 0", got)
	}
}

// TestUnknownStoreOverflow verifies the cardinality cap: once maxUnknownKeys
// distinct keys are tracked, further unseen keys fold into one shared
// overflow counter instead of growing the map without bound.
func TestUnknownStoreOverflow(t *testing.T) {
	s := newUnknownStore()

	// Fill the store up to the cap; each key gets its own counter.
	for i := range maxUnknownKeys {
		c := s.get(string(rune(i)))
		if c == s.Overflow() {
			t.Fatalf("key %d below cap folded into overflow", i)
		}
	}

	// The next unseen key must fold into the shared overflow counter.
	c := s.get("past-the-cap")
	if c != s.Overflow() {
		t.Fatal("key past cap did not fold into overflow counter")
	}
	// ...and so must any further unseen key (same shared counter).
	if s.get("another") != s.Overflow() {
		t.Fatal("second key past cap did not fold into overflow counter")
	}

	// Existing tracked keys still resolve to their own (non-overflow) counter.
	if s.get(string(rune(0))) == s.Overflow() {
		t.Fatal("existing tracked key collapsed into overflow")
	}

	// Overflow counter is counted.
	s.Overflow().Inc()
	if v := s.Overflow().Value(); v != 1 {
		t.Errorf("overflow value = %d, want 1", v)
	}
}
