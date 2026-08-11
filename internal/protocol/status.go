package protocol

// StatusSnapshot is the JSON body returned by the frontend's GET /v1/status —
// the single source of truth for "which turns are in flight right now". A
// backend (e.g. status-monitor's /running) consumes it to render the live turn
// list instead of guessing from its own partial view. Field tags match the
// frontend's response exactly.
type StatusSnapshot struct {
	InFlight int           `json:"inflight"`
	Backends []string      `json:"backends"`
	Turns    []TurnInfo    `json:"turns"`
	Hosts    []HostStats   `json:"hosts,omitempty"`
	Services []ServiceStat `json:"services,omitempty"`
}

// TurnInfo is one in-flight turn's identity, as exposed by GET /v1/status.
// ElapsedS is wall-clock seconds since the turn started.
type TurnInfo struct {
	PromptID  string `json:"prompt_id"`
	ChatID    string `json:"chat_id"`
	BackendID string `json:"backend_id"`
	ElapsedS  int64  `json:"elapsed_s"`
}

// HostStats is one physical host's load snapshot, reported by any backend on
// that host; the frontend dedupes by IP (same-host backends overwrite with
// the latest report).
type HostStats struct {
	IP             string  `json:"ip"`
	Hostname       string  `json:"hostname"`
	MachineID      string  `json:"machineId,omitempty"` // 主机去重键（/etc/machine-id），不在卡片展示
	Load1          float64 `json:"load1"`
	Load5          float64 `json:"load5"`
	Load15         float64 `json:"load15"`
	MemTotalBytes  uint64  `json:"memTotalBytes"`
	MemAvailBytes  uint64  `json:"memAvailBytes"`
	DiskTotalBytes uint64  `json:"diskTotalBytes"`
	DiskUsedBytes  uint64  `json:"diskUsedBytes"`
	DiskPath       string  `json:"diskPath"` // state_dir mount point, fallback "/"
	ReportedAt     int64   `json:"reportedAt"`
}

// ServiceStat is one backend process's snapshot. CgroupMemBytes == 0 means
// no instance (e.g. miniagent idle) or unreadable — rendered as "—".
type ServiceStat struct {
	BackendID      string `json:"backendID"`
	IP             string `json:"ip"`
	Version        string `json:"version"` // handshake-reported; empty = "unknown"
	CgroupMemBytes uint64 `json:"cgroupMemBytes,omitempty"`
	ReportedAt     int64  `json:"reportedAt"`
}

// MetricsReport is the periodic backend → frontend report body
// (POST /v1/metrics/<backendID>). Auth reuses the shared IPC bearer token.
// Host and process data travel together; the frontend splits them into the
// conn's HostStats/ServiceStat views.
type MetricsReport struct {
	Hostname       string     `json:"hostname"`
	IP             string     `json:"ip"`
	MachineID      string     `json:"machineId,omitempty"` // 与 Host.MachineID 同值；置于顶层便于去重层直接取用
	ReportedAt     int64      `json:"reportedAt"`
	Host           HostStats  `json:"host"`
	Version        string     `json:"version"` // redundant with the handshake; lets the frontend cross-check/fall back
	CgroupMemBytes uint64     `json:"cgroupMemBytes,omitempty"`
	Turns          []TurnInfo `json:"turns,omitempty"` // running sessions snapshot for reconciliation
}
