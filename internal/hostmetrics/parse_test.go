package hostmetrics

import "testing"

func TestParseLoadavg(t *testing.T) {
	l1, l5, l15, err := parseLoadavg([]byte("0.95 0.38 0.33 1/234 5678\n"))
	if err != nil {
		t.Fatalf("parseLoadavg: %v", err)
	}
	if l1 != 0.95 || l5 != 0.38 || l15 != 0.33 {
		t.Errorf("got %v/%v/%v", l1, l5, l15)
	}
	if _, _, _, err := parseLoadavg([]byte("0.95 0.38")); err == nil {
		t.Errorf("short input: want error")
	}
	if _, _, _, err := parseLoadavg([]byte("x 0.38 0.33 1/1 1")); err == nil {
		t.Errorf("bad float: want error")
	}
}

func TestParseMeminfo(t *testing.T) {
	in := []byte(`MemTotal:        8174956 kB
MemFree:         1234567 kB
MemAvailable:    1787654 kB
Buffers:          100000 kB
`)
	total, avail, err := parseMeminfo(in)
	if err != nil {
		t.Fatalf("parseMeminfo: %v", err)
	}
	if total != 8174956*1024 {
		t.Errorf("total = %d", total)
	}
	if avail != 1787654*1024 {
		t.Errorf("avail = %d", avail)
	}
	if _, _, err := parseMeminfo([]byte("MemFree: 1 kB\n")); err == nil {
		t.Errorf("missing fields: want error")
	}
	if _, _, err := parseMeminfo([]byte("MemTotal: x kB\nMemAvailable: 1 kB\n")); err == nil {
		t.Errorf("bad value: want error")
	}
}

func TestParseUintLine(t *testing.T) {
	v, err := parseUintLine([]byte("10240000\n"))
	if err != nil || v != 10240000 {
		t.Errorf("got (%d, %v)", v, err)
	}
	if _, err := parseUintLine([]byte("max")); err == nil {
		t.Errorf("non-numeric: want error")
	}
	if _, err := parseUintLine([]byte("")); err == nil {
		t.Errorf("empty: want error")
	}
}
