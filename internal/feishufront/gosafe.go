package feishufront

// goSafe runs fn in a new goroutine, recovering from any panic so a stray
// bug in a long-lived frontend goroutine cannot crash the bot process.
//
// The frontend keeps its own recover-and-swallow variant rather than reuse
// miniagent's taskrunner / a backend-side helper: backendrpc's tests use
// feishufront as a fixture, so feishufront must not sit above backendrpc in
// the import graph, and a frontend-local goroutine with no single reachable
// logger cannot mirror the backend's log-on-panic GoSafe. The existing
// dedupSet.StartPrune / Layer1Router.StartPrune inline the same
// defer-recover; centralising it here keeps the pattern uniform.
func goSafe(fn func()) {
	go func() {
		defer func() { _ = recover() }()
		fn()
	}()
}
