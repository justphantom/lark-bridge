package config

import "time"

// StatusMonitorConfig is the status-monitor binary's view of the union
// config document: the shared Core plus the status_monitor section only the
// monitor owns. Its owned top-level keys are exactly what LoadStatusMonitor
// decodes — everything else in the file is skipped.
type StatusMonitorConfig struct {
	Core

	StatusMonitor StatusMonitor `json:"status_monitor,omitempty"`
}

// StatusMonitorIntervalDefault is the documented default tick (also the
// value applyMonitorDefaults fills): one overview card refresh per minute.
const StatusMonitorIntervalDefault = 60 * time.Second

// StatusMonitor holds settings for the lark-status-monitor backend, which
// periodically pushes an overview card (online backends + in-flight turns) to
// every chat bound to it. The card is PATCHed in place each tick and re-sent
// only if a user deleted it.
type StatusMonitor struct {
	// Interval is the refresh period. Go duration string ("60s", "2m").
	// 0/absent → 60s.
	Interval Duration `json:"interval,omitempty"`
}

// applyStatusMonitorDefaults fills the interval default. Shared by
// LoadStatusMonitor (the section's owning binary) and LoadMiniAgentBack
// (which reuses status_monitor.interval as its metrics-push period).
func applyStatusMonitorDefaults(s *StatusMonitor) {
	if s.Interval == 0 {
		s.Interval = Duration(StatusMonitorIntervalDefault)
	}
}

// applyMonitorDefaults fills the monitor-owned section's defaults.
func applyMonitorDefaults(cfg *StatusMonitorConfig) {
	applyStatusMonitorDefaults(&cfg.StatusMonitor)
}
