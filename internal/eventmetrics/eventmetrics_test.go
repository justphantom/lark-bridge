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
	ClaudeResultLenientHit.Inc()
	ClaudeResultParseFail.Inc()
	OMPTextEndFallback.Inc()
	OMPAutoRetryLimit.Inc()
	UnknownEvent("omp", "unknown").Inc()

	ResetAll()

	if v := ClaudeResultLenientHit.Value(); v != 0 {
		t.Errorf("ClaudeResultLenientHit = %d after ResetAll, want 0", v)
	}
	if v := UnknownEvent("omp", "unknown").Value(); v != 0 {
		t.Errorf("UnknownEvent(omp,unknown) = %d after ResetAll, want 0", v)
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
