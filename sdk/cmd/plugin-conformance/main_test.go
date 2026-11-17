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
		if !strings.HasPrefix(piece, pluginPrefix) || !strings.HasSuffix(piece, "\n") {
			t.Fatalf("piece %.40q... is not a prefixed line", piece)
		}
		if body := len(piece) - len(pluginPrefix) - 1; body > lineLimit {
			t.Fatalf("piece holds %d bytes, want at most %d", body, lineLimit)
		}
		got.WriteString(strings.TrimSuffix(strings.TrimPrefix(piece, pluginPrefix), "\n"))
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
	if want := "plugin: done\nplugin: giving up\n"; out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
}
