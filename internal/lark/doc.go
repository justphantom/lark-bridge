// Package lark is the self-contained Feishu client used by lark-bridge. It
// covers exactly the surface the bridge consumes: tenant-access-token
// acquisition, the WebSocket long-poll for inbound events/card callbacks, and
// the three REST endpoints the bridge calls (create message, reply message,
// patch message). No third-party dependencies; only the Go standard library.
//
// Layering:
//
//   - internal/lark/websocket  RFC 6455 client transport
//   - internal/lark/ws         lark WS sub-protocol (frame codec, bootstrap,
//     reconnect, chunk reassembly, event routing)
//   - internal/lark            high-level Client (REST + WS) and shared types
//
// internal/feishu wraps this package with the bridge's business concerns
// (logger, watchdog, fallback card). Callers outside internal/feishu should
// not import the sub-packages directly.
package lark
