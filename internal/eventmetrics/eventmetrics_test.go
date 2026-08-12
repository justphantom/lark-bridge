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

func TestResetAll(t *testing.T) {
	TerminalEmitRetries.Inc()
	LineTruncated("miniagent").Inc()

	ResetAll()

	if v := TerminalEmitRetries.Value(); v != 0 {
		t.Errorf("TerminalEmitRetries = %d after ResetAll, want 0", v)
	}
	if v := LineTruncated("miniagent").Value(); v != 0 {
		t.Errorf("LineTruncated(miniagent) = %d after ResetAll, want 0", v)
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
