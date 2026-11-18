package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

func TestPrefixedSplitsLongLines(t *testing.T) {
	var out bytes.Buffer
	p := &prefixed{out: &out}
	line := strings.Repeat("x", 1<<20)
	for chunk := range slices.Chunk([]byte(line), 32<<10) {
		if _, err := p.Write(chunk); err != nil {
			t.Fatal(err)
		}
		if len(p.line) > lineLimit {
			t.Fatalf("pending line holds %d bytes, want at most %d", len(p.line), lineLimit)
		}
	}
	if err := p.flush(); err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	for piece := range strings.Lines(out.String()) {
		if !strings.HasPrefix(piece, monitorPrefix) || !strings.HasSuffix(piece, "\n") {
			t.Fatalf("piece %.40q... is not a prefixed line", piece)
		}
		if body := len(piece) - len(monitorPrefix) - 1; body > lineLimit {
			t.Fatalf("piece holds %d bytes, want at most %d", body, lineLimit)
		}
		got.WriteString(strings.TrimSuffix(strings.TrimPrefix(piece, monitorPrefix), "\n"))
	}
	if got.String() != line {
		t.Fatalf("pieces hold %d bytes, want the %d bytes written", got.Len(), len(line))
	}
}

func TestPrefixedFlushWritesTheLastLine(t *testing.T) {
	var out bytes.Buffer
	p := &prefixed{out: &out}
	if _, err := p.Write([]byte("done\ngiving up")); err != nil {
		t.Fatal(err)
	}
	if err := p.flush(); err != nil {
		t.Fatal(err)
	}
	if err := p.flush(); err != nil {
		t.Fatal(err)
	}
	if want := "monitor: done\nmonitor: giving up\n"; out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
}

func TestEmptyMonitorDirIsAUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"-monitor-dir", "", "--", "/nonexistent/monitor"}, &stdout, &stderr)
	if code != exitUsage || stdout.Len() != 0 {
		t.Fatalf("exit code = %d, stdout = %q, want %d and empty", code, stdout.String(), exitUsage)
	}
	if want := "monitor-conformance: -monitor-dir:  is not a directory\n"; stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestHelpNamesTheMonitorDirArgument(t *testing.T) {
	var stdout, stderr bytes.Buffer
	run(t.Context(), []string{"-h", "--", "/nonexistent/monitor"}, &stdout, &stderr)
	if want := "  -monitor-dir DIR\n"; !strings.Contains(stderr.String(), want) {
		t.Fatalf("help = %q, want it to hold %q", stderr.String(), want)
	}
}
