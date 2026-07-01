package azkql

import (
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/sql"
)

// parseWhere parses "SELECT * FROM t WHERE <expr>" and returns the predicate.
func parseWhere(t *testing.T, expr string) sql.Expr {
	t.Helper()
	stmt, err := sql.Parse("SELECT * FROM t WHERE " + expr)
	if err != nil {
		t.Fatalf("parse %q: %v", expr, err)
	}
	return stmt.(*sql.SelectStmt).Where
}

func TestBuildPredicates(t *testing.T) {
	cases := []struct {
		where string
		want  string // the rendered "| where …" body, or "" if nothing pushes
	}{
		{"type = 'microsoft.compute/virtualmachines'", `type == "microsoft.compute/virtualmachines"`},
		{"name <> 'x'", `name != "x"`},
		{"num > 5", "num > 5"},
		{"type = 'vm' AND location = 'eastus'", `type == "vm" and location == "eastus"`},
		{"type = 'vm' OR type = 'disk'", `(type == "vm") or (type == "disk")`},
		{"location IN ('eastus', 'westus')", `location in ("eastus", "westus")`},
		{"location NOT IN ('eastus')", `location !in ("eastus")`},
		{"name LIKE '%prod%'", `name contains "prod"`},
		{"name IS NULL", "isnull(name)"},
		{"name IS NOT NULL", "isnotnull(name)"},
		// Partial: only the translatable conjunct is pushed; the engine re-applies all.
		{"type = 'vm' AND tags.env = 'prod'", `type == "vm"`},
		// Nested-only / untranslatable -> nothing pushes.
		{"tags.env = 'prod'", ""},
		{"name LIKE 'pre%fix'", ""},
	}
	for _, c := range cases {
		got := Build(Query{Table: "Resources", Predicate: parseWhere(t, c.where)})
		want := "Resources"
		if c.want != "" {
			want += " | where " + c.want
		}
		if got != want {
			t.Errorf("WHERE %s\n  got:  %q\n  want: %q", c.where, got, want)
		}
	}
}

func TestBuildOrderLimitCap(t *testing.T) {
	lim := 10
	cases := []struct {
		q    Query
		want string
	}{
		{Query{Table: "Resources", Limit: &lim}, "Resources | take 10"},
		{Query{Table: "Resources", Cap: 5000}, "Resources | take 5000"},
		{Query{Table: "Resources", Limit: &lim, Cap: 5000}, "Resources | take 10"}, // explicit limit wins
		{Query{Table: "Resources", OrderBy: []connector.OrderTerm{{Column: "name", Desc: true}}}, "Resources | order by name desc"},
		{Query{Table: "Resources", OrderBy: []connector.OrderTerm{{Column: "name"}, {Column: "type", Desc: true}}, Cap: 100},
			"Resources | order by name asc, type desc | take 100"},
		// unsafe identifier in order by is skipped
		{Query{Table: "Resources", OrderBy: []connector.OrderTerm{{Column: "tags.env"}}}, "Resources"},
	}
	for _, c := range cases {
		if got := Build(c.q); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
}

func TestBuildFullPipeline(t *testing.T) {
	lim := 3
	got := Build(Query{
		Table:     "Resources",
		Predicate: parseWhere(t, "type = 'vm'"),
		OrderBy:   []connector.OrderTerm{{Column: "name"}},
		Limit:     &lim,
	})
	want := `Resources | where type == "vm" | order by name asc | take 3`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStringEscaping(t *testing.T) {
	// A value with a quote/backslash must not break out of the literal.
	got := Build(Query{Table: "Resources", Predicate: parseWhere(t, `name = 'a"b\c'`)})
	want := `Resources | where name == "a\"b\\c"`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
