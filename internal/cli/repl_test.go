package cli

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
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

func TestCmdUseShorthand(t *testing.T) {
	app := NewApp()
	if _, err := app.cmdUse("prod", []string{"yaml:./examples/data/products.yaml"}); err != nil {
		t.Fatalf("cmdUse shorthand error: %v", err)
	}
	s, ok := app.Reg.Resolve("prod")
	if !ok {
		t.Fatal("source 'prod' not registered")
	}
	if s.Conn.Name() != "yaml" {
		t.Errorf("connector = %q, want yaml", s.Conn.Name())
	}
}

func TestCmdUseExplicitKV(t *testing.T) {
	app := NewApp()
	if _, err := app.cmdUse("ord", []string{"csv", "path=./examples/data/orders.csv", "delimiter=,"}); err != nil {
		t.Fatalf("cmdUse kv error: %v", err)
	}
	s, ok := app.Reg.Resolve("ord")
	if !ok {
		t.Fatal("source 'ord' not registered")
	}
	if s.Conn.Name() != "csv" {
		t.Errorf("connector = %q, want csv", s.Conn.Name())
	}
}

func TestCmdUseSQL(t *testing.T) {
	app := NewApp()
	if _, err := app.cmdUse("inv", []string{"sql", "driver=sqlite", "dsn=./examples/data/inventory.db", "table=inventory"}); err != nil {
		t.Fatalf("cmdUse sql error: %v", err)
	}
	s, ok := app.Reg.Resolve("inv")
	if !ok {
		t.Fatal("source 'inv' not registered")
	}
	if s.Conn.Name() != "sql" {
		t.Errorf("connector = %q, want sql", s.Conn.Name())
	}
}

func TestCmdUseSQLAllTables(t *testing.T) {
	// A wildcard table= "*" expands a SQL source to every user table in the DB.
	// Use an in-memory shared SQLite DB seeded with two tables so the test
	// is self-contained (no dependency on example file paths).
	dsn := "file:cmduse_alltables?mode=memory&cache=shared"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	for _, ddl := range []string{
		`CREATE TABLE alpha (id INTEGER PRIMARY KEY, v TEXT)`,
		`CREATE TABLE beta (id INTEGER PRIMARY KEY, n INTEGER)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("exec %q: %v", ddl, err)
		}
	}

	app := NewApp()
	names, err := app.cmdUse("db", []string{"sql", "driver=sqlite", "dsn=" + dsn, "table=*"})
	if err != nil {
		t.Fatalf("cmdUse sql-all error: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("registered %v, want 2 tables", names)
	}
	// Each table should be registered under its own name.
	for _, want := range []string{"alpha", "beta"} {
		s, ok := app.Reg.Resolve(want)
		if !ok {
			t.Errorf("table %q not registered; got %v", want, names)
			continue
		}
		if s.Conn.Name() != "sql" {
			t.Errorf("%s connector = %q, want sql", want, s.Conn.Name())
		}
	}
}

func TestCmdUseErrors(t *testing.T) {
	app := NewApp()
	cases := []struct {
		name string
		args []string
	}{
		{"missing-spec", []string{}},
		{"bad-shorthand", []string{"noseparator"}},
		{"unknown-connector", []string{"x", "frobnicate:./data.csv"}},
		{"missing-path", []string{"x", "csv", "delimiter=,"}},
		{"sql-missing-dsn", []string{"x", "sql", "driver=sqlite"}},
		{"bad-option", []string{"x", "csv", "noequals"}},
	}
	for _, c := range cases {
		if _, err := app.cmdUse(c.name, c.args); err == nil {
			t.Errorf("cmdUse(%q, %v) expected error, got nil", c.name, c.args)
		}
	}
}

func TestCmdUseDuplicate(t *testing.T) {
	app := NewApp()
	if _, err := app.cmdUse("dup", []string{"yaml:./examples/data/products.yaml"}); err != nil {
		t.Fatalf("first cmdUse error: %v", err)
	}
	if _, err := app.cmdUse("dup", []string{"yaml:./examples/data/products.yaml"}); err == nil {
		t.Error("second cmdUse expected duplicate error, got nil")
	}
}