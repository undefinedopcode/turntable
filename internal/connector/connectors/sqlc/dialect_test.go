package sqlc

import (
	"testing"

	"github.com/april/turntable/internal/sql"
)

func TestDialectQuoteIdent(t *testing.T) {
	cases := []struct {
		driver string
		in     string
		want   string
	}{
		{"sqlite", "col", `"col"`},
		{"postgres", "col", `"col"`},
		{"", "col", `"col"`}, // default == sqlite
		{"mysql", "col", "`col`"},
		// Embedded quote characters are doubled per dialect.
		{"postgres", `we"ird`, `"we""ird"`},
		{"mysql", "we`ird", "`we``ird`"},
	}
	for _, tc := range cases {
		if got := dialectFor(tc.driver).quoteIdent(tc.in); got != tc.want {
			t.Errorf("dialect %q quoteIdent(%q) = %s, want %s", tc.driver, tc.in, got, tc.want)
		}
	}
}

func TestDialectPlaceholder(t *testing.T) {
	cases := []struct {
		driver string
		n      int
		want   string
	}{
		{"postgres", 1, "$1"},
		{"postgres", 3, "$3"},
		{"pgx", 2, "$2"},
		{"mysql", 1, "?"},
		{"sqlite", 1, "?"},
		{"", 1, "?"},
	}
	for _, tc := range cases {
		if got := dialectFor(tc.driver).placeholder(tc.n); got != tc.want {
			t.Errorf("dialect %q placeholder(%d) = %s, want %s", tc.driver, tc.n, got, tc.want)
		}
	}
}

// TestTranslateExprMySQLBackticks verifies that pushed-down predicates quote
// columns with backticks for MySQL — double quotes would be read as a string
// literal there and silently match nothing.
func TestTranslateExprMySQLBackticks(t *testing.T) {
	e, err := sql.ParseExpr(`qty > 5 AND name LIKE 'a%'`)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := translateExpr(e, dialectFor("mysql"))
	if !ok {
		t.Fatal("expected expression to translate")
	}
	want := "((`qty` > 5) AND `name` LIKE 'a%')"
	if got != want {
		t.Errorf("mysql translate:\n got  %s\n want %s", got, want)
	}
}

func TestTranslateExprPostgresDoubleQuotes(t *testing.T) {
	e, err := sql.ParseExpr(`id IN (1, 2)`)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := translateExpr(e, dialectFor("postgres"))
	if !ok {
		t.Fatal("expected expression to translate")
	}
	if want := `"id" IN (1, 2)`; got != want {
		t.Errorf("postgres translate:\n got  %s\n want %s", got, want)
	}
}

// TestTableRefQuotedDialect checks schema-qualified table names quote per
// dialect.
func TestTableRefQuotedDialect(t *testing.T) {
	tr := parseTableName("public.events")
	if got := tr.quoted(dialectFor("postgres")); got != `"public"."events"` {
		t.Errorf("postgres quoted = %s", got)
	}
	if got := tr.quoted(dialectFor("mysql")); got != "`public`.`events`" {
		t.Errorf("mysql quoted = %s", got)
	}
}
