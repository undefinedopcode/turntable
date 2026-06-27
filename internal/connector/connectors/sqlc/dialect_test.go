package sqlc

import (
	dbsql "database/sql"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/sql"
)

func intPtr(n int) *int { return &n }

// TestSQLServerDriverRegistered guards the blank import in sqlc.go: the
// microsoft/go-mssqldb driver must register the "sqlserver" name.
func TestSQLServerDriverRegistered(t *testing.T) {
	for _, d := range dbsql.Drivers() {
		if d == "sqlserver" {
			return
		}
	}
	t.Errorf("sqlserver driver not registered; drivers = %v", dbsql.Drivers())
}

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
		{"sqlserver", "col", "[col]"},
		{"mssql", "col", "[col]"},
		// Embedded quote characters are doubled per dialect.
		{"postgres", `we"ird`, `"we""ird"`},
		{"mysql", "we`ird", "`we``ird`"},
		{"sqlserver", "we]ird", "[we]]ird]"},
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
		{"sqlserver", 1, "@p1"},
		{"sqlserver", 4, "@p4"},
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

// TestBuildScanQuery covers the per-dialect SQL shape, especially SQL Server's
// leading TOP (n) vs the trailing LIMIT n used elsewhere.
func TestBuildScanQuery(t *testing.T) {
	pred, err := sql.ParseExpr(`qty > 5`)
	if err != nil {
		t.Fatal(err)
	}
	base := func(driver string) connector.ScanRequest {
		return connector.ScanRequest{
			Dataset:   connector.Dataset{Name: "inv", Options: map[string]any{"driver": driver}},
			Columns:   []string{"id", "qty"},
			Predicate: pred,
			Limit:     intPtr(3),
		}
	}
	cases := []struct {
		driver string
		want   string
	}{
		{"sqlite", `SELECT "id", "qty" FROM "inv" WHERE ("qty" > 5) LIMIT 3`},
		{"postgres", `SELECT "id", "qty" FROM "inv" WHERE ("qty" > 5) LIMIT 3`},
		{"mysql", "SELECT `id`, `qty` FROM `inv` WHERE (`qty` > 5) LIMIT 3"},
		{"sqlserver", `SELECT TOP (3) [id], [qty] FROM [inv] WHERE ([qty] > 5)`},
	}
	for _, tc := range cases {
		got := buildScanQuery(base(tc.driver), parseTableName("inv"), dialectFor(tc.driver))
		if got != tc.want {
			t.Errorf("buildScanQuery[%s]:\n got  %s\n want %s", tc.driver, got, tc.want)
		}
	}
}

// TestSQLServerLikeNotPushed verifies a LIKE predicate is kept in the engine for
// SQL Server (collation-dependent case-sensitivity makes pushdown unsafe), so
// the limit is withheld and no WHERE is emitted.
func TestSQLServerLikeNotPushed(t *testing.T) {
	pred, err := sql.ParseExpr(`name LIKE 'a%'`)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := translateExpr(pred, dialectFor("sqlserver")); ok {
		t.Error("expected LIKE not to translate for sqlserver")
	}
	req := connector.ScanRequest{Predicate: pred, Limit: intPtr(3)}
	got := buildScanQuery(req, parseTableName("t"), dialectFor("sqlserver"))
	if want := `SELECT * FROM [t]`; got != want {
		t.Errorf("buildScanQuery = %q, want %q (no WHERE, no TOP)", got, want)
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
