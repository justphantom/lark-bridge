package feishufront

// goSafe runs fn in a new goroutine, recovering from any panic so a stray
// bug in a long-lived frontend goroutine cannot crash the bot process.
//
// This is the feishufront-local equivalent of bridgebase.GoSafe, duplicated
// to keep the frontend free of backend-side helper packages (backendrpc's
// tests use feishufront as a fixture, so feishufront must not sit above
// backendrpc in the import graph; bridgebase no longer imports backendrpc —
// its seam is protocol.ControlSender now — but the frontend still has no
// reason to reach into a backend-side package). The existing
// dedupSet.StartPrune / Layer1Router.StartPrune
// inline the same defer-recover; centralising it here keeps the pattern
// uniform. Unlike bridgebase.GoSafe this variant does not log (the frontend
// has no single logger reachable from every call site); panics are swallowed
// silently, matching the established dedup/router behavior.
func goSafe(fn func()) {
	go func() {
		defer func() { _ = recover() }()
		fn()
	}()
}
