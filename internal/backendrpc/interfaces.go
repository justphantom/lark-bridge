package backendrpc

import "github.com/justphantom/lark-bridge/internal/protocol"

// ControlSender / StatusQuerier moved to internal/protocol (the contract
// package): the interfaces reference only context + protocol types, and
// defining them in backendrpc forced every consumer of the seam (miniagent, statusmonitor) to import the transport package. backendrpc now
// only asserts conformance.

// compile-time assertions that *Client satisfies the protocol seam
// interfaces.
var (
	_ protocol.ControlSender = (*Client)(nil)
	_ protocol.StatusQuerier = (*Client)(nil)
)
