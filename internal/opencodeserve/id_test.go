package opencodeserve

import (
	"strings"
	"sync"
	"testing"
)

// TestGenerateMessageIDFormat verifies the id format and that
// successive ids within the same millisecond are strictly increasing.
func TestGenerateMessageIDFormat(t *testing.T) {
	id1, err := generateMessageID()
	if err != nil {
		t.Fatalf("generateMessageID: %v", err)
	}
	if !strings.HasPrefix(id1, "msg_") {
		t.Fatalf("missing msg_ prefix: %q", id1)
	}
	// msg_ (4) + 12 hex + 14 base62 = 30 chars
	if len(id1) != 30 {
		t.Fatalf("unexpected id length: got %d want 30 (%q)", len(id1), id1)
	}

	id2, err := generateMessageID()
	if err != nil {
		t.Fatalf("second generateMessageID: %v", err)
	}
	// Within the same millisecond the counter ensures id2 > id1.
	// Across milliseconds the ms segment ensures monotonicity.
	if id1 >= id2 {
		t.Fatalf("ids not strictly increasing: %q >= %q", id1, id2)
	}
}

// TestGenerateMessageIDMonotonicAcrossRollback verifies that after
// minting an id at ms=1000, minting one at ms=999 (simulated NTP
// rollback) still produces an id whose hex segment is >= the first.
func TestGenerateMessageIDMonotonicAcrossRollback(t *testing.T) {
	saved := defaultMsgIDState
	t.Cleanup(func() { defaultMsgIDState = saved })
	defaultMsgIDState = &msgIDState{}

	id1, err := generateMessageIDAt(1000)
	if err != nil {
		t.Fatalf("generateMessageIDAt(1000): %v", err)
	}
	id2, err := generateMessageIDAt(999)
	if err != nil {
		t.Fatalf("generateMessageIDAt(999): %v", err)
	}
	hex1 := id1[4:16]
	hex2 := id2[4:16]
	if hex1 > hex2 {
		t.Fatalf("hex segment regressed after rollback: %q > %q", hex1, hex2)
	}
	if id1 >= id2 {
		t.Fatalf("id not strictly increasing after rollback: %q >= %q", id1, id2)
	}
}

// TestGenerateMessageIDConcurrentUnique verifies that 100 goroutines
// each minting 1000 ids produce no duplicates.
func TestGenerateMessageIDConcurrentUnique(t *testing.T) {
	saved := defaultMsgIDState
	t.Cleanup(func() { defaultMsgIDState = saved })
	defaultMsgIDState = &msgIDState{}

	const goroutines = 100
	const perG = 1000

	var mu sync.Mutex
	seen := make(map[string]struct{}, goroutines*perG)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range perG {
				id, err := generateMessageIDAt(2000)
				if err != nil {
					t.Errorf("generateMessageIDAt: %v", err)
					return
				}
				mu.Lock()
				if _, dup := seen[id]; dup {
					t.Errorf("duplicate id: %q", id)
				}
				seen[id] = struct{}{}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seen) != goroutines*perG {
		t.Fatalf("expected %d unique ids, got %d", goroutines*perG, len(seen))
	}
}
