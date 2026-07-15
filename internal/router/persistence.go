package router

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/hu/lark-bridge/internal/atomicwrite"
	"github.com/hu/lark-bridge/internal/log"
)

// routerVersion is the on-disk bindings format version.
const routerVersion = 5

// filePerm is the permission for the router persist file.
const filePerm = 0o600

// load reads the persisted bindings from disk. The on-disk format is v5:
//
//	{"bindings": {chatID: Binding}, "version": 5}
//
// where key is the Feishu chatID. Unknown JSON fields (e.g. a claude-written
// file read by opencode, or vice versa) are silently ignored by
// json.Unmarshal, so v5 files remain forward- and backward-compatible across
// the two backends.
//
// A malformed file or an unsupported version is a HARD error, not a silent
// drop: returning nil would reset bindings to empty and the next save would
// overwrite the file, permanently losing every session. Failing loudly lets
// the operator back up or repair the file before the bridge starts empty.
func (r *Router) load() error {
	return log.LogOperation(r.logger, "router_state_load", func() error {
		data, err := os.ReadFile(r.persistPath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("router: load %s: %w", r.persistPath, err)
		}
		var raw struct {
			Bindings map[string]json.RawMessage `json:"bindings"`
			Version  int                        `json:"version"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("router: %s: parse bindings: %w; back up or remove the file",
				r.persistPath, err)
		}
		if raw.Version != routerVersion {
			return fmt.Errorf("router: %s: unsupported version %d (expected %d); back up or remove the file",
				r.persistPath, raw.Version, routerVersion)
		}
		// A missing "bindings" key or an explicit "bindings":null unmarshals
		// into a nil map. Treat both as "no bindings".
		bindings := make(map[string]Binding)
		for chatID, b := range raw.Bindings {
			var binding Binding
			if err := json.Unmarshal(b, &binding); err != nil {
				// A single corrupt binding is dropped, and the next save will
				// persist that drop — permanently losing this chat's session.
				// Log at Error with a backup hint so the operator can repair
				// before the next save overwrites the file.
				r.logger.Error("persist load skip binding; back up the file before the next save overwrites it",
					log.FieldPath, r.persistPath,
					log.FieldChatID, chatID,
					log.FieldError, err)
				continue
			}
			bindings[chatID] = binding
		}
		r.bindings = bindings
		r.logger.Info("router state loaded",
			"bindings_count", len(bindings),
			log.FieldPath, r.persistPath)
		return nil
	})
}

// save writes the current bindings to disk atomically (tmp + fsync + rename
// + dir fsync via atomicwrite.Write). A crash mid-write leaves either the
// previous contents or the new contents, never a truncated file.
//
// An empty persistPath is a no-op (in-memory mode).
func (r *Router) save() error {
	if r.persistPath == "" {
		return nil
	}
	return log.LogOperation(r.logger, "router_state_save", func() error {
		r.saveMu.Lock()
		defer r.saveMu.Unlock()

		r.mu.RLock()
		payload := struct {
			Bindings map[string]Binding `json:"bindings"`
			Version  int                `json:"version"`
		}{
			Bindings: make(map[string]Binding, len(r.bindings)),
			Version:  routerVersion,
		}
		for k, v := range r.bindings {
			payload.Bindings[k] = v
		}
		r.mu.RUnlock()

		data, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return err
		}

		if err := atomicwrite.Write(r.persistPath, data, filePerm); err != nil {
			r.logger.Error("router state save failed",
				log.FieldPath, r.persistPath,
				log.FieldError, err)
			return fmt.Errorf("router: save %s: %w", r.persistPath, err)
		}
		return nil
	})
}

// saveAsync schedules a save on the worker goroutine started by New.
// Coalesces: if multiple mutations call saveAsync before the worker has
// drained the previous signal, only one save runs (the latest snapshot of
// r.bindings is what hits disk).
func (r *Router) saveAsync() {
	if r.persistPath == "" {
		return
	}
	select {
	case r.saveCh <- struct{}{}:
	default:
		// A save is already pending; the worker will pick up the latest
		// snapshot when it next wakes, so this mutation is not lost.
	}
}

// saveLoop drains saveCh and writes the bindings to disk. Started by New on a
// dedicated goroutine; stopped by Close (which closes saveDone on exit so
// Close can wait before doing the final save).
func (r *Router) saveLoop() {
	defer close(r.saveDone)
	for {
		select {
		case <-r.saveCh:
			if err := r.save(); err != nil {
				r.logger.Error("router state save failed in loop",
					log.FieldPath, r.persistPath,
					log.FieldError, err)
			}
		case <-r.saveStop:
			return
		}
	}
}
