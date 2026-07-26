//go:build linux || darwin

// Package claude wraps the Claude Code CLI as a standalone SDK.
//
// The SDK shells out to the `claude` binary in print/stream-json mode
// per turn and consumes a stream of events from stdout. A Run returns a
// channel of parsed Events terminated by a result or error event.
//
// Minimal example:
//
//	c := claude.New(claude.Options{})
//	ch, err := c.Run(ctx, claude.RunOptions{Prompt: "hello"})
//	if err != nil {
//		// handle
//	}
//	for ev := range ch {
//		// consume ev.Type / ev.Text / ev.Result
//	}
package claude
