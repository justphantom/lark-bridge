package opencodeservebridge

import (
	"context"
	"path/filepath"

	oc "github.com/justphantom/opencode-go-sdk-lite"
)

// streamFor returns the event stream for loc's directory, creating it on
// first use. The v1 event bus is isolated by directory: a stream only
// receives events for sessions under its own directory, so each working
// directory needs a dedicated stream. nil loc (empty directory) maps to the
// server-default-directory stream.
func (a *Agent) streamFor(loc *oc.LocationRef) *oc.GlobalEventStream {
	key := ""
	if loc != nil {
		key = filepath.Clean(loc.Directory)
	}
	a.streamsMu.Lock()
	defer a.streamsMu.Unlock()
	if s, ok := a.streams[key]; ok {
		return s
	}
	s, _ := a.client.NewGlobalEventStream(context.Background(), loc)
	a.streams[key] = s
	return s
}
