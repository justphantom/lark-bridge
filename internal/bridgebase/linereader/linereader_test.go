package linereader

import (
	"io"
	"strings"
	"testing"
)

func TestReadLine_NormalLines(t *testing.T) {
	r := New(strings.NewReader("hello\nworld\n"), 1024)
	line, truncated, err := r.ReadLine()
	if line != "hello" || truncated || err != nil {
		t.Fatalf("first line: %q truncated=%v err=%v", line, truncated, err)
	}
	line, truncated, err = r.ReadLine()
	if line != "world" || truncated || err != nil {
		t.Fatalf("second line: %q truncated=%v err=%v", line, truncated, err)
	}
	line, truncated, err = r.ReadLine()
	if line != "" || truncated || err != io.EOF {
		t.Fatalf("EOF: %q truncated=%v err=%v", line, truncated, err)
	}
}

func TestReadLine_CRLF(t *testing.T) {
	r := New(strings.NewReader("hello\r\nworld\r\n"), 1024)
	line, _, err := r.ReadLine()
	if line != "hello" || err != nil {
		t.Fatalf("first line: %q err=%v", line, err)
	}
	line, _, err = r.ReadLine()
	if line != "world" || err != nil {
		t.Fatalf("second line: %q err=%v", line, err)
	}
}

func TestReadLine_EmptyLines(t *testing.T) {
	r := New(strings.NewReader("\n\nhello\n"), 1024)
	line, _, err := r.ReadLine()
	if line != "" || err != nil {
		t.Fatalf("first empty: %q err=%v", line, err)
	}
	line, _, err = r.ReadLine()
	if line != "" || err != nil {
		t.Fatalf("second empty: %q err=%v", line, err)
	}
	line, _, err = r.ReadLine()
	if line != "hello" || err != nil {
		t.Fatalf("hello: %q err=%v", line, err)
	}
}

func TestReadLine_NoTrailingNewline(t *testing.T) {
	r := New(strings.NewReader("hello\nworld"), 1024)
	line, _, err := r.ReadLine()
	if line != "hello" || err != nil {
		t.Fatalf("first: %q err=%v", line, err)
	}
	line, _, err = r.ReadLine()
	if line != "world" || err != nil {
		t.Fatalf("second (no newline): %q err=%v", line, err)
	}
	line, _, err = r.ReadLine()
	if err != io.EOF {
		t.Fatalf("expected EOF, got %q err=%v", line, err)
	}
}

func TestReadLine_Truncated(t *testing.T) {
	// Line longer than max=10 should be truncated
	longLine := "abcdefghijklmnopqrstuvwxyz"
	r := New(strings.NewReader(longLine+"\n"+"short\n"), 10)
	line, truncated, err := r.ReadLine()
	if !truncated {
		t.Fatal("expected truncated=true")
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// strutil.Truncate("abcdefghijklmnopqrstuvwxyz", 10) = "abcdefghij..."
	if line != "abcdefghij..." {
		t.Errorf("truncated line = %q, want %q", line, "abcdefghij...")
	}

	// Next line should be the short one (reader was aligned)
	line, truncated, err = r.ReadLine()
	if line != "short" || truncated || err != nil {
		t.Fatalf("next line: %q truncated=%v err=%v", line, truncated, err)
	}
}

func TestReadLine_TruncatedNoTrailingNewline(t *testing.T) {
	// Line longer than max, no trailing newline (last line of a stream)
	longLine := "abcdefghijklmnopqrstuvwxyz"
	r := New(strings.NewReader(longLine), 10)
	line, truncated, err := r.ReadLine()
	if !truncated {
		t.Fatal("expected truncated=true")
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if line != "abcdefghij..." {
		t.Errorf("truncated line = %q, want %q", line, "abcdefghij...")
	}
	// Should be EOF
	_, _, err = r.ReadLine()
	if err != io.EOF {
		t.Fatalf("expected EOF after truncated last line, got %v", err)
	}
}

func TestReadLine_TruncatedExact(t *testing.T) {
	// Line exactly max bytes should NOT be truncated
	exactLine := "abcdefghij" // 10 bytes
	r := New(strings.NewReader(exactLine+"\n"), 10)
	line, truncated, err := r.ReadLine()
	if truncated {
		t.Fatal("expected truncated=false for exact-length line")
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if line != exactLine {
		t.Errorf("line = %q, want %q", line, exactLine)
	}
}

func TestReadLine_MultipleTruncatedLines(t *testing.T) {
	input := "aaaa\n" + // short
		"bbbbbbbbbbbbbbbbbbbb\n" + // long (20 bytes, max=10)
		"cccc\n" + // short
		"dddddddddddddddddddddddddddddddddddddddddddddddddddd\n" + // very long
		"eeee\n" // short
	r := New(strings.NewReader(input), 10)

	lines := make([]string, 0, 5)
	truncs := make([]bool, 0, 5)
	for {
		line, truncated, err := r.ReadLine()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		lines = append(lines, line)
		truncs = append(truncs, truncated)
	}

	if len(lines) != 5 {
		t.Fatalf("expected 5 lines, got %d: %v", len(lines), lines)
	}
	if lines[0] != "aaaa" || truncs[0] {
		t.Errorf("line 0: %q truncated=%v", lines[0], truncs[0])
	}
	if !truncs[1] || lines[1] != "bbbbbbbbbb..." {
		t.Errorf("line 1: %q truncated=%v", lines[1], truncs[1])
	}
	if lines[2] != "cccc" || truncs[2] {
		t.Errorf("line 2: %q truncated=%v", lines[2], truncs[2])
	}
	if !truncs[3] {
		t.Errorf("line 3: expected truncated, got %q", lines[3])
	}
	if lines[4] != "eeee" || truncs[4] {
		t.Errorf("line 4: %q truncated=%v", lines[4], truncs[4])
	}
}

func TestReadLine_UTF8Boundary(t *testing.T) {
	// Chinese characters are 3 bytes each. A line of "你好世界" (12 bytes)
	// truncated to max=5 should back off to the start of "你" (3 bytes) + "..."
	r := New(strings.NewReader("你好世界\n"), 5)
	line, truncated, err := r.ReadLine()
	if !truncated {
		t.Fatal("expected truncated=true")
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// strutil.Truncate("你好世界", 5): 5 is mid-2nd-rune → back to 3 → "你..."
	if line != "你..." {
		t.Errorf("utf8 truncated = %q, want %q", line, "你...")
	}
}

func TestReadLine_EOFOnEmpty(t *testing.T) {
	r := New(strings.NewReader(""), 1024)
	_, _, err := r.ReadLine()
	if err != io.EOF {
		t.Fatalf("expected EOF on empty reader, got %v", err)
	}
}

func TestReadLine_MaxBytesZero(t *testing.T) {
	r := New(strings.NewReader("hello\n"), 0)
	_, _, err := r.ReadLine()
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("expected ErrUnexpectedEOF for max=0, got %v", err)
	}
}

// TestReadLine_LongLineSpanningBuffers exercises the ErrBufferFull path: a
// line longer than bufio's 4 KiB buffer arrives in multiple chunks and must
// be reassembled in full when it fits within max.
func TestReadLine_LongLineSpanningBuffers(t *testing.T) {
	long := strings.Repeat("x", 10000) // > 4 KiB bufio buffer, < max
	r := New(strings.NewReader(long+"\ntail\n"), 20000)
	line, truncated, err := r.ReadLine()
	if err != nil || truncated {
		t.Fatalf("truncated=%v err=%v", truncated, err)
	}
	if line != long {
		t.Errorf("line len = %d, want %d", len(line), len(long))
	}
	line, truncated, err = r.ReadLine()
	if line != "tail" || truncated || err != nil {
		t.Fatalf("next line: %q truncated=%v err=%v", line, truncated, err)
	}
}

// TestReadLine_TruncationAcrossBuffers covers truncation decided mid-line
// (accumulated chunks cross max before the newline): the rest of the line
// must be discarded without buffering and the reader must stay aligned.
func TestReadLine_TruncationAcrossBuffers(t *testing.T) {
	long := strings.Repeat("y", 10000) // > 4 KiB buffer and > max
	r := New(strings.NewReader(long+"\ntail\n"), 5000)
	line, truncated, err := r.ReadLine()
	if !truncated || err != nil {
		t.Fatalf("truncated=%v err=%v", truncated, err)
	}
	if !strings.HasPrefix(line, strings.Repeat("y", 4997)) || !strings.HasSuffix(line, "...") {
		t.Errorf("truncated line head/tail wrong, len=%d", len(line))
	}
	line, truncated, err = r.ReadLine()
	if line != "tail" || truncated || err != nil {
		t.Fatalf("next line after cross-buffer truncation: %q truncated=%v err=%v", line, truncated, err)
	}
}

// TestReadLine_HugeLineDoesNotEscape is a smoke test for the memory bound:
// a 1 MiB single line against max=1 KiB must yield a ~1 KiB result and stay
// aligned. (Peak memory is asserted implicitly — a ReadString-based
// implementation would buffer the full MiB per chunk; here the returned
// value is the only large allocation.)
func TestReadLine_HugeLineDoesNotEscape(t *testing.T) {
	huge := strings.Repeat("z", 1<<20)
	r := New(strings.NewReader(huge+"\ndone"), 1024)
	line, truncated, err := r.ReadLine()
	if !truncated || err != nil {
		t.Fatalf("truncated=%v err=%v", truncated, err)
	}
	if len(line) > 1024+3 {
		t.Errorf("truncated line len = %d, want <= %d", len(line), 1024+3)
	}
	line, _, err = r.ReadLine()
	if line != "done" || err != nil {
		t.Fatalf("final line: %q err=%v", line, err)
	}
}
