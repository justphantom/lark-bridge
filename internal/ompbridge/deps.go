// Package ompbridge glues Feishu events to the Oh My Pi (omp) CLI agent
// backend.
//
// One Handler per process owns the router (chatID -> omp session binding),
// the omp CLI client, and the backendrpc client used to emit Control
// messages. Like the opencode/claude bridges, this bridge is stream-driven:
// one `omp -p --mode json` subprocess per turn, whose NDJSON stream IS the
// response.
package ompbridge

import (
	"context"

	"github.com/justphantom/lark-bridge/internal/omp"
)

// ompAPI is the omp backend capability the bridge needs. The production
// implementation is *omp.Client; the interface exists so handler tests can
// substitute a fake that replays canned event streams.
//
// Only Run is required: omp's `models --json` is too slow to drive a dynamic
// /model picker (§A.4 observed 30s+ timeout), and session list/delete are not
// supported in v1. Pickers fall back to static config option lists.
type ompAPI interface {
	// Run starts one agent turn and returns the event stream. The caller
	// drains the channel until it is closed; a terminal event (agent_end /
	// error) precedes close.
	Run(ctx context.Context, opts omp.RunOptions) (<-chan omp.Event, error)
}
