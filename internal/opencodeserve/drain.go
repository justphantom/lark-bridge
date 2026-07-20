package opencodeserve

import "io"

// drainLimit caps how many bytes drainAndClose pulls from a body.
// 1 MiB is well above any legitimate error response and bounds the
// goroutine time / memory when a malicious or stuck upstream tries
// to stream forever (e.g. an unending SSE error page).
const drainLimit int64 = 1 << 20 // 1 MiB

// drainAndClose reads at most drainLimit bytes from body and closes
// it. The read+close pair is mandatory on Go 1.22+ when the body is
// not fully consumed, otherwise the underlying connection cannot be
// reused.
//
// The read is bounded so a malicious or stuck upstream (a never-EOF
// SSE, a multi-GB error page) cannot pin a goroutine or grow memory
// indefinitely. Total-time limits remain the caller's responsibility
// (via http.Client.Timeout / ctx); we do not layer a timer here.
//
// Errors are deliberately swallowed: draining is best-effort cleanup.
func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, drainLimit))
	_ = body.Close()
}
