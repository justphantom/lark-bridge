// Package ws implements the lark WebSocket sub-protocol: the protobuf Frame
// wire format, bootstrap handshake, control/data frame types, and the chunk
// reassembler. It sits one layer below the high-level lark.Client and above
// the generic internal/lark/websocket transport.
package ws

// Method values carried in Frame.Method.
const (
	MethodControl = 0 // ping/pong handshake frames
	MethodData    = 1 // event/card callback frames
)

// Header keys used by the lark WS frame protocol. The headers field is an
// array of {Key,Value} pairs (not a map); lookups are linear.
const (
	HeaderType      = "type"       // event | card | ping | pong
	HeaderMessageID = "message_id" // chunk-group id, inherited after split
	HeaderSum       = "sum"        // chunk count (1 = unsplit)
	HeaderSeq       = "seq"        // chunk ordinal (0 = unsplit)
	HeaderBizRt     = "biz_rt"     // business processing time, ms
)

// FrameType (the "type" header) for data frames.
const (
	TypeEvent = "event" // im.message.receive_v1, card.action.trigger, …
	TypePing  = "ping"  // control frame payload type
	TypePong  = "pong"  // control frame payload type
)

// EndpointResponse codes from POST /callback/ws/endpoint.
const (
	codeOK            = 0
	codeSystemBusy    = 1
	codeAuthFailed    = 514
	codeInternalError = 1000040343
)

// BootstrapEndpoint is the HTTP path that returns the WS URL + client config.
const BootstrapEndpoint = "/callback/ws/endpoint"

// Query params attached to the WS URL by the bootstrap response; serviceID
// seeds the Service field of every ping frame.
const (
	queryDeviceID  = "device_id"
	queryServiceID = "service_id"
)
