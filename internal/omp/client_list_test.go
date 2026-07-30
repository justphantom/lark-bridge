//go:build linux || darwin

package omp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

// TestModelsJSON_ParsesSelector verifies the modelsJSON struct mirrors omp's
// `models --json` payload and the selector field is the one consumed. Uses a
// trimmed sample of the real ~137s output captured on omp/17.1.8.
func TestModelsJSON_ParsesSelector(t *testing.T) {
	raw := `{"models":[
		{"provider":"autoapi","id":"agnes-2.0-flash","selector":"autoapi/agnes-2.0-flash","name":"agnes-2.0-flash"},
		{"provider":"nvidia","id":"z-ai/glm5","selector":"nvidia/z-ai/glm5","name":"glm5"}
	]}`
	var parsed modelsJSON
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed.Models) != 2 {
		t.Fatalf("got %d models, want 2", len(parsed.Models))
	}
	want := []string{"autoapi/agnes-2.0-flash", "nvidia/z-ai/glm5"}
	got := make([]string, 0, len(parsed.Models))
	for _, m := range parsed.Models {
		got = append(got, m.Selector)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("selectors = %v, want %v", got, want)
	}
}

// TestCachedList_HitAndMiss verifies cachedList serves from cache within TTL
// and re-invokes fetch once the entry expires. fetch invocation is counted so
// the test does not depend on wall-clock fork behaviour.
func TestCachedList_HitAndMiss(t *testing.T) {
	calls := 0
	fetch := func(context.Context) ([]string, error) {
		calls++
		return []string{"a", "b"}, nil
	}
	c := &Client{listTTL: 10 * time.Minute}

	// Miss → fetch.
	cache := (*listCache)(nil)
	got, err := c.cachedList(context.Background(), &cache, fetch)
	if err != nil {
		t.Fatalf("cachedList miss: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("miss result = %v, want [a b]", got)
	}
	if calls != 1 {
		t.Errorf("fetch calls after miss = %d, want 1", calls)
	}

	// Hit → no fetch.
	got, err = c.cachedList(context.Background(), &cache, fetch)
	if err != nil {
		t.Fatalf("cachedList hit: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("hit result = %v, want [a b]", got)
	}
	if calls != 1 {
		t.Errorf("fetch calls after hit = %d, want 1 (cache should serve)", calls)
	}

	// Hit-path defensive copy: mutating the cached return must not corrupt
	// the cache for a later caller within the TTL.
	got[0] = "MUTATED"
	again, err := c.cachedList(context.Background(), &cache, fetch)
	if err != nil {
		t.Fatalf("cachedList post-mutation: %v", err)
	}
	if calls != 1 {
		t.Errorf("fetch calls after mutation = %d, want 1 (still cached)", calls)
	}
	if again[0] == "MUTATED" {
		t.Error("hit-path return shares the cached slice; mutation leaked into cache")
	}

	// Force expiry → fetch again. The returned slice is intentionally
	// discarded: this branch asserts the fetch was re-invoked (calls below),
	// not its value (already checked on the miss above).
	cache.fetchedAt = time.Now().Add(-time.Hour)
	_, err = c.cachedList(context.Background(), &cache, fetch)
	if err != nil {
		t.Fatalf("cachedList expiry: %v", err)
	}
	if calls != 2 {
		t.Errorf("fetch calls after expiry = %d, want 2", calls)
	}
}

// TestCachedList_Disabled verifies listTTL <= 0 bypasses the cache (every call
// forks), matching the documented contract.
func TestCachedList_Disabled(t *testing.T) {
	calls := 0
	fetch := func(context.Context) ([]string, error) {
		calls++
		return []string{"x"}, nil
	}
	c := &Client{listTTL: 0} // disabled
	cache := (*listCache)(nil)
	for i := range 3 {
		if _, err := c.cachedList(context.Background(), &cache, fetch); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if calls != 3 {
		t.Errorf("fetch calls = %d, want 3 (caching disabled)", calls)
	}
}

// TestCachedList_FetchErrorNotCached verifies a failed fetch is not cached, so
// the next call retries instead of serving a stale error.
func TestCachedList_FetchErrorNotCached(t *testing.T) {
	fetchErr := errors.New("boom")
	fetch := func(context.Context) ([]string, error) { return nil, fetchErr }
	c := &Client{listTTL: 10 * time.Minute}
	cache := (*listCache)(nil)
	if _, err := c.cachedList(context.Background(), &cache, fetch); !errors.Is(err, fetchErr) {
		t.Fatalf("got err = %v, want %v", err, fetchErr)
	}
	if cache != nil {
		t.Errorf("failed fetch was cached: %+v", cache)
	}
}
