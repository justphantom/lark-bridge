package opencodeserve

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// messageId generation, ported from opencode's MessageID.create()
// (packages/opencode/src/id/id.ts). Output format:
//
//	msg_<12 hex><14 base62>
//
// The 12 hex chars encode the low 48 bits of `now = (ms mod 2^36) << 12 | seq`
// as 6 big-endian bytes; the 14 trailing chars are random base62. Because the
// hex segment is monotonic in wall-clock time, ids sort after every existing
// id minted earlier — which is what opencode's agent loop requires (it only
// processes the lexicographically-largest user message in a session).
const (
	msgPrefix      = "msg"
	msgRandLen     = 14
	msgTimeShift   = 12 // 0x1000
	msgTsMask36    = (uint64(1) << 36) - 1
	base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

type msgIDState struct {
	mu      sync.Mutex
	lastMs  int64
	counter uint64
}

var defaultMsgIDState = &msgIDState{}

// generateMessageID returns a new id using the current wall clock.
func generateMessageID() (string, error) {
	return generateMessageIDAt(time.Now().UnixMilli())
}

// generateMessageIDAt mints an id for a specific millisecond. The per-ms
// counter is shared across all callers via defaultMsgIDState, so within one
// millisecond successive calls return strictly increasing ids.
//
// ms is clamped to max(ms, lastMs) so a wall-clock rollback (NTP step)
// cannot produce an id whose hex segment sorts below a previously minted
// id — opencode's agent loop only processes the lexicographically-largest
// user message, so a rollback-induced regression would silently drop the
// prompt.
func generateMessageIDAt(ms int64) (string, error) {
	defaultMsgIDState.mu.Lock()
	if ms <= defaultMsgIDState.lastMs {
		ms = defaultMsgIDState.lastMs
	} else {
		defaultMsgIDState.lastMs = ms
	}
	defaultMsgIDState.counter++
	seq := defaultMsgIDState.counter
	defaultMsgIDState.mu.Unlock()

	// now = (ms mod 2^36) << 12 | seq, exactly 48 bits — equivalent to the
	// low 48 bits of create()'s `ts * 0x1000 + counter`.
	now := (uint64(ms) & msgTsMask36) << msgTimeShift //nolint:gosec // G115: ms is clamped non-negative ms; cast is intentional
	now |= seq & 0xFFF
	var b [6]byte
	for i := range 6 {
		b[i] = byte(now >> (40 - 8*i)) //nolint:gosec // G115: high-byte extraction, value always 0..255
	}
	rand, err := base62Rand()
	if err != nil {
		return "", err
	}
	return msgPrefix + "_" + hex.EncodeToString(b[:]) + rand, nil
}

// base62Rand returns msgRandLen base62 characters from crypto/rand.
func base62Rand() (string, error) {
	var buf [msgRandLen]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	var out [msgRandLen]byte
	for i, c := range buf {
		out[i] = base62Alphabet[int(c)%62]
	}
	return string(out[:]), nil
}
