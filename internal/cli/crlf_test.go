package cli

import (
	"bytes"
	"testing"
)

func TestCrlfWriterTranslatesBareLF(t *testing.T) {
	var buf bytes.Buffer
	w := &crlfWriter{w: &buf}
	w.Write([]byte("hello\nworld\n"))
	got := buf.String()
	want := "hello\r\nworld\r\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCrlfWriterPreservesExistingCRLF(t *testing.T) {
	var buf bytes.Buffer
	w := &crlfWriter{w: &buf}
	// Already-CRLF newlines must not be doubled to \r\r\n.
	w.Write([]byte("line1\r\nline2\r\n"))
	got := buf.String()
	want := "line1\r\nline2\r\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCrlfWriterAcrossWrites(t *testing.T) {
	var buf bytes.Buffer
	w := &crlfWriter{w: &buf}
	// A '\r' at the end of one write followed by '\n' at the start of the next
	// must not produce an extra '\r'.
	w.Write([]byte("abc\r"))
	w.Write([]byte("\n"))
	got := buf.String()
	want := "abc\r\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCrlfWriterNoNewline(t *testing.T) {
	var buf bytes.Buffer
	w := &crlfWriter{w: &buf}
	w.Write([]byte("no newlines here"))
	got := buf.String()
	want := "no newlines here"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}