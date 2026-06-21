package cli

import (
	"strings"
	"testing"
)

func TestLineEditorCompletion(t *testing.T) {
	e := newLineEditor(nil, &strings.Builder{}, []string{
		".tables", ".schema", ".help", ".quit", "customers", "orders",
	})
	// Type "cus" then complete -> should expand to "customers".
	e.buf = []rune("cus")
	e.pos = 3
	e.complete()
	if got := string(e.buf); got != "customers" {
		t.Errorf("complete(cus) = %q, want customers", got)
	}
	if e.pos != len("customers") {
		t.Errorf("complete pos = %d, want %d", e.pos, len("customers"))
	}
}

func TestLineEditorCompletionCommonPrefix(t *testing.T) {
	e := newLineEditor(nil, &strings.Builder{}, []string{
		".tables", ".schema", ".help",
	})
	// Type "." then complete -> ambiguous; extend to longest common prefix ".".
	e.buf = []rune(".")
	e.pos = 1
	e.complete()
	// All candidates share "." prefix only; no further extension, but no
	// error either. The buffer should remain "." (hint printed to out).
	if got := string(e.buf); got != "." {
		t.Errorf("complete(.) = %q, want .", got)
	}
}

func TestLineEditorCompletionWordBoundary(t *testing.T) {
	e := newLineEditor(nil, &strings.Builder{}, []string{"customers", "orders"})
	// "FROM cus" should complete the word "cus" -> "customers".
	e.buf = []rune("FROM cus")
	e.pos = len("FROM cus")
	e.complete()
	if got := string(e.buf); got != "FROM customers" {
		t.Errorf("complete(FROM cus) = %q, want 'FROM customers'", got)
	}
}

func TestCommonPrefix(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"abc", "abd", "ab"},
		{"hello", "help", "hel"},
		{"x", "y", ""},
		{"abc", "abc", "abc"},
	}
	for _, c := range cases {
		if got := commonPrefix(c.a, c.b); got != c.want {
			t.Errorf("commonPrefix(%q,%q) = %q, want %q", c.a, c.b, got, c.want)
		}
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
	// Register a trivial source so .tables has output.
	// (No config loaded; .tables should report no sources gracefully.)
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
