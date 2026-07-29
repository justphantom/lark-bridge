package backendrpc

import (
	"context"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// ControlSender is the subset of *Client the deploymonitor / statusmonitor /
// miniagent handlers need to POST a Control. Lifted from per-package private
// copies so a test fake in one binary can be shared across all three.
// *Client satisfies this interface via its SendControl method.
type ControlSender interface {
	SendControl(ctx context.Context, ctrl *protocol.Control) error
}

// StatusQuerier is the subset of *Client the deploymonitor / statusmonitor
// handlers need to read the frontend's in-flight turn snapshot via
// GET /v1/status. *Client satisfies this interface via its Status method.
type StatusQuerier interface {
	Status(ctx context.Context) (*protocol.StatusSnapshot, error)
}

// compile-time assertions that *Client satisfies both interfaces.
var (
	_ ControlSender = (*Client)(nil)
	_ StatusQuerier = (*Client)(nil)
)
