package cli

import (
	"strings"
	"testing"
)

func TestReplCompleter(t *testing.T) {
	c := &replCompleter{cands: []string{
		".tables", ".schema", ".help", ".quit", "customers", "orders",
	}}
	// "cus" should complete to "customers".
	matches, offset := c.Do([]rune("cus"), 3)
	if offset != 0 {
		t.Errorf("offset = %d, want 0", offset)
	}
	if len(matches) != 1 || string(matches[0]) != "customers" {
		t.Errorf("matches = %v, want [customers]", matches)
	}
}

func TestReplCompleterWordBoundary(t *testing.T) {
	c := &replCompleter{cands: []string{"customers", "orders"}}
	// "FROM cus" — the word starts after the space; offset should point at it.
	line := []rune("FROM cus")
	matches, offset := c.Do(line, len(line))
	if offset != 5 { // index of 'c' in "cus"
		t.Errorf("offset = %d, want 5", offset)
	}
	if len(matches) != 1 || string(matches[0]) != "customers" {
		t.Errorf("matches = %v, want [customers]", matches)
	}
}

func TestReplCompleterNoMatch(t *testing.T) {
	c := &replCompleter{cands: []string{".tables", "orders"}}
	matches, _ := c.Do([]rune("xyz"), 3)
	if matches != nil {
		t.Errorf("matches = %v, want nil", matches)
	}
}

func TestReplCompleterEmptyPrefix(t *testing.T) {
	c := &replCompleter{cands: []string{".tables"}}
	matches, _ := c.Do([]rune(""), 0)
	if matches != nil {
		t.Errorf("matches = %v, want nil for empty prefix", matches)
	}
}

func TestIsWordBreak(t *testing.T) {
	breaks := " \t,();"
	for _, r := range breaks {
		if !isWordBreak(r) {
			t.Errorf("isWordBreak(%q) = false, want true", r)
		}
	}
	if isWordBreak('a') {
		t.Error("isWordBreak('a') = true, want false")
	}
}

func TestReplBatch(t *testing.T) {
	// Drive the non-interactive REPL path with piped input.
	app := NewApp()
	app.Out = &strings.Builder{}
	app.Err = &strings.Builder{}
	in := strings.NewReader(".tables\nSELECT 1+1 AS two;\n.quit\n")
	code := app.replBatch(nil, in)
	if code != 0 {
		t.Errorf("replBatch code = %d, want 0", code)
	}
	out := app.Out.(*strings.Builder).String()
	if !strings.Contains(out, "no sources") {
		t.Errorf("expected 'no sources' in output, got: %s", out)
	}
	if !strings.Contains(out, "2") {
		t.Errorf("expected query result 2 in output, got: %s", out)
	}
}

func TestReplBatchMultiline(t *testing.T) {
	// A SQL statement split across multiple lines, terminated by ';'.
	app := NewApp()
	app.Out = &strings.Builder{}
	app.Err = &strings.Builder{}
	in := strings.NewReader("SELECT 1+1\n  AS two;\n.quit\n")
	code := app.replBatch(nil, in)
	if code != 0 {
		t.Errorf("replBatch code = %d, want 0", code)
	}
	out := app.Out.(*strings.Builder).String()
	if !strings.Contains(out, "2") {
		t.Errorf("expected query result 2 in output, got: %s", out)
	}
}