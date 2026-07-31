package feishufront

// goSafe runs fn in a new goroutine, recovering from any panic so a stray
// bug in a long-lived frontend goroutine cannot crash the bot process.
//
// This is the feishufront-local equivalent of bridgebase.GoSafe, duplicated
// because feishufront must not import bridgebase (that would create a cycle:
// backendrpc's tests use feishufront as a fixture, and bridgebase imports
// backendrpc). The existing dedupSet.StartPrune / Layer1Router.StartPrune
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
