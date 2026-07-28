package hostmetrics

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// parseLoadavg parses /proc/loadavg's first three fields:
//
//	0.95 0.38 0.33 1/234 5678
func parseLoadavg(b []byte) (l1, l5, l15 float64, err error) {
	fields := strings.Fields(string(b))
	if len(fields) < 3 {
		return 0, 0, 0, fmt.Errorf("loadavg: want ≥3 fields, got %d", len(fields))
	}
	vals := make([]float64, 3)
	for i := range vals {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("loadavg field %d: %w", i, err)
		}
		vals[i] = v
	}
	return vals[0], vals[1], vals[2], nil
}

// parseMeminfo parses /proc/meminfo and returns MemTotal and MemAvailable in
// bytes. Both fields are mandatory in any kernel this project runs on; a
// missing one is a parse error, not a silent zero.
func parseMeminfo(b []byte) (total, avail uint64, err error) {
	var gotTotal, gotAvail bool
	for _, line := range bytes.Split(b, []byte("\n")) {
		key, val, found := bytes.Cut(line, []byte(":"))
		if !found {
			continue
		}
		k := string(key)
		if k != "MemTotal" && k != "MemAvailable" {
			continue
		}
		// Values are "<n> kB"; Fields drops the unit and any whitespace.
		f := strings.Fields(string(val))
		if len(f) == 0 {
			return 0, 0, fmt.Errorf("meminfo %s: empty value", k)
		}
		n, perr := strconv.ParseUint(f[0], 10, 64)
		if perr != nil {
			return 0, 0, fmt.Errorf("meminfo %s: %w", k, perr)
		}
		switch k {
		case "MemTotal":
			total, gotTotal = n*1024, true
		case "MemAvailable":
			avail, gotAvail = n*1024, true
		}
	}
	if !gotTotal || !gotAvail {
		return 0, 0, fmt.Errorf("meminfo: MemTotal/MemAvailable missing")
	}
	return total, avail, nil
}

// parseUintLine parses a file whose content is a single unsigned integer
// (e.g. cgroup v2 memory.current), tolerating a trailing newline.
func parseUintLine(b []byte) (uint64, error) {
	s := strings.TrimSpace(string(b))
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("uint line %q: %w", s, err)
	}
	return v, nil
}
