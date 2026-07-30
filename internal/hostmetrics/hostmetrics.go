// Package hostmetrics collects point-in-time host and process load snapshots
// (load average, memory, state-disk usage, cgroup memory) for the
// status-monitor overview card. Pure functions, no state, Linux only — the
// same precondition as the rest of the project.
//
// Parsers (parse.go) are separated from readers so the /proc and /sys text
// formats are unit-testable without injecting filesystem paths.
package hostmetrics

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

const (
	loadavgPath = "/proc/loadavg"
	meminfoPath = "/proc/meminfo"
	cgroupRoot  = "/sys/fs/cgroup/system.slice"

	// machineID 的候选路径：systemd 机器写 /etc/machine-id，dbus 旧式写
	// /var/lib/dbus/machine-id；二者内容一致，任一存在即可。都缺失返回空
	// （非 systemd/容器精简镜像），由去重层回退到 (IP, Hostname)。
	machineIDPrimaryPath = "/etc/machine-id"
	machineIDDBusPath    = "/var/lib/dbus/machine-id"
)

// CollectHost reads one host load snapshot. diskPath is the state_dir whose
// mount point the operator cares about; when statfs on it fails (directory
// missing, etc.) it falls back to "/" so a misconfigured state_dir never
// blanks the whole row.
func CollectHost(diskPath string, now time.Time) (protocol.HostStats, error) {
	h := protocol.HostStats{ReportedAt: now.Unix()}
	if hn, err := os.Hostname(); err == nil {
		h.Hostname = hn
	}
	h.MachineID = MachineID()

	lb, err := os.ReadFile(loadavgPath)
	if err != nil {
		return h, fmt.Errorf("read loadavg: %w", err)
	}
	l1, l5, l15, err := parseLoadavg(lb)
	if err != nil {
		return h, fmt.Errorf("parse loadavg: %w", err)
	}
	h.Load1, h.Load5, h.Load15 = l1, l5, l15

	mb, err := os.ReadFile(meminfoPath)
	if err != nil {
		return h, fmt.Errorf("read meminfo: %w", err)
	}
	total, avail, err := parseMeminfo(mb)
	if err != nil {
		return h, fmt.Errorf("parse meminfo: %w", err)
	}
	h.MemTotalBytes, h.MemAvailBytes = total, avail

	// Disk usage of the mount hosting diskPath, falling back to "/".
	h.DiskPath = diskPath
	var st syscall.Statfs_t
	if err := syscall.Statfs(diskPath, &st); err != nil {
		h.DiskPath = "/"
		if err2 := syscall.Statfs("/", &st); err2 != nil {
			return h, fmt.Errorf("statfs: %w", err2)
		}
	}
	h.DiskTotalBytes = st.Blocks * uint64(st.Bsize)             //nolint:gosec // G115: bsize 为正
	h.DiskUsedBytes = (st.Blocks - st.Bfree) * uint64(st.Bsize) //nolint:gosec // G115: bsize 为正
	return h, nil
}

// SelfCgroupMem reads this process's cgroup v2 memory.current for a systemd
// unit such as "lark-claude-back.service". A missing file (non-systemd
// environment, unit not registered) is NOT an error: it returns ok=false so
// the caller renders "—". A present-but-unreadable file is a real error.
func SelfCgroupMem(unitName string) (bytes uint64, ok bool, err error) {
	if unitName == "" {
		return 0, false, nil
	}
	b, err := os.ReadFile(filepath.Join(cgroupRoot, unitName, "memory.current"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("read cgroup mem: %w", err)
	}
	v, err := parseUintLine(b)
	if err != nil {
		return 0, false, fmt.Errorf("parse cgroup mem: %w", err)
	}
	return v, true, nil
}

// MachineID returns this host's stable machine identifier (systemd
// /etc/machine-id, falling back to the dbus /var/lib/dbus/machine-id).
// The value is generated at install time, stable across reboots, and identical
// for every process on the same host — the property the status-monitor dedup
// keys on. A missing file (non-systemd, stripped container image) is NOT an
// error: it returns ("", nil) so the caller's dedup falls back to (IP, Hostname).
// A present-but-unreadable file is a real error. Trailing whitespace (the file
// holds a trailing newline) is trimmed.
func MachineID() string {
	for _, p := range []string{machineIDPrimaryPath, machineIDDBusPath} {
		b, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "" // unreadable: best-effort, let dedup fall back
		}
		return strings.TrimSpace(string(b))
	}
	return ""
}

// OutboundIP probes which local address a connection to frontendAddr would
// originate from, i.e. the IP the frontend actually sees on a multi-homed
// host. The UDP dial performs only a route lookup — no packets are sent —
// so it is cheap and side-effect free. Call once at startup and cache the
// result; re-probe on process restart.
func OutboundIP(frontendAddr string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp", frontendAddr)
	if err != nil {
		return "", fmt.Errorf("probe outbound ip: %w", err)
	}
	defer func() { _ = conn.Close() }()
	la, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return "", fmt.Errorf("probe outbound ip: unexpected local addr %T", conn.LocalAddr())
	}
	return la.IP.String(), nil
}

// PrimaryIPv4 returns the first non-loopback IPv4 address of the host, for
// callers (feishu-front self-report) that cannot probe via a remote peer.
// Returns "127.0.0.1" when no routable interface exists.
func PrimaryIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		if v4 := ipNet.IP.To4(); v4 != nil {
			return v4.String()
		}
	}
	return "127.0.0.1"
}
