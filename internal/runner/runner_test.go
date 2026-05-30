package runner

import (
	"strings"
	"testing"
)

func TestCappedWriterUnderLimit(t *testing.T) {
	w := &cappedWriter{limit: 100}
	data := []byte("hello")
	n, err := w.Write(data)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(data) {
		t.Errorf("Write returned %d, want %d", n, len(data))
	}
	if string(w.Bytes()) != "hello" {
		t.Errorf("got %q, want %q", string(w.Bytes()), "hello")
	}
}

func TestCappedWriterExceedsLimit(t *testing.T) {
	w := &cappedWriter{limit: 5}
	w.Write([]byte("hello world"))
	got := string(w.Bytes())
	if !strings.HasPrefix(got, "hello") {
		t.Errorf("want prefix %q in %q", "hello", got)
	}
	if !strings.Contains(got, "[output truncated]") {
		t.Errorf("want truncation marker in %q", got)
	}
}

func TestCappedWriterDropsAfterCap(t *testing.T) {
	w := &cappedWriter{limit: 5}
	w.Write([]byte("hello world"))
	sizeAfterCap := len(w.Bytes())
	w.Write([]byte("more data that should be dropped"))
	if len(w.Bytes()) != sizeAfterCap {
		t.Errorf("buffer grew after cap: %d → %d bytes", sizeAfterCap, len(w.Bytes()))
	}
}
