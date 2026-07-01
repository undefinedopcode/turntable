package awsconfigc

import (
	"context"
	"strings"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/sql"
)

// fakeConfig serves canned result pages and records the last expression.
type fakeConfig struct {
	pages    [][]string // successive result pages (JSON strings)
	lastExpr string
	lastAgg  string
	calls    int
}

func (f *fakeConfig) query(ctx context.Context, expression, aggregator string, limit int32, nextToken string) ([]string, string, error) {
	f.lastExpr = expression
	f.lastAgg = aggregator
	i := f.calls
	f.calls++
	if i >= len(f.pages) {
		return nil, "", nil
	}
	next := ""
	if i+1 < len(f.pages) {
		next = "page" // non-empty -> more pages
	}
	return f.pages[i], next, nil
}

func drain(t *testing.T, it engine.RowIterator) []engine.Row {
	t.Helper()
	rows, err := engine.Materialize(context.Background(), it)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return rows
}

func predicate(t *testing.T, where string) sql.Expr {
	t.Helper()
	stmt, err := sql.Parse("SELECT * FROM t WHERE " + where)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return stmt.(*sql.SelectStmt).Where
}

func TestTableSchemaFixed(t *testing.T) {
	c := New()
	sc, err := c.Resolve(context.Background(), connector.Dataset{Source: "resources"})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]engine.Type{}
	for _, col := range sc.Columns {
		names[col.Name] = col.Type
	}
	if names["resourceType"] != engine.TypeString || names["configuration"] != engine.TypeAny || names["resourceCreationTime"] != engine.TypeTime {
		t.Errorf("unexpected fixed schema: %+v", names)
	}
}

func TestScanTableModePushesWhereAndMaps(t *testing.T) {
	f := &fakeConfig{pages: [][]string{{
		`{"resourceId":"i-1","resourceType":"AWS::EC2::Instance","awsRegion":"us-east-1","configuration":{"instanceType":"m5.large"},"tags":[{"key":"env","value":"prod"}]}`,
		`{"resourceId":"i-2","resourceType":"AWS::EC2::Instance","awsRegion":"us-west-2","configuration":{"instanceType":"t3.micro"}}`,
	}}}
	c := newWithClient(f)
	ds := connector.Dataset{Source: "resources"}
	rows := drain(t, mustScan(t, c, connector.ScanRequest{
		Dataset:   ds,
		Predicate: predicate(t, "resourceType = 'AWS::EC2::Instance' AND awsRegion IN ('us-east-1','us-west-2')"),
	}))

	// The pushed Config SELECT includes the fixed field list and the translated WHERE.
	if !strings.HasPrefix(f.lastExpr, "SELECT resourceId, resourceType,") {
		t.Errorf("expr missing select list: %q", f.lastExpr)
	}
	if !strings.Contains(f.lastExpr, "resourceType = 'AWS::EC2::Instance'") ||
		!strings.Contains(f.lastExpr, "awsRegion IN ('us-east-1', 'us-west-2')") {
		t.Errorf("expr WHERE not translated: %q", f.lastExpr)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// resourceType is column index 1; configuration (any) index 9.
	if rows[0].Values[1].V != "AWS::EC2::Instance" {
		t.Errorf("resourceType = %v", rows[0].Values[1].V)
	}
	if rows[0].Values[9].Type != engine.TypeAny {
		t.Errorf("configuration should be any/object, got %v", rows[0].Values[9].Type)
	}
}

func TestNestedPredicateNotPushed(t *testing.T) {
	f := &fakeConfig{pages: [][]string{{`{"resourceId":"i-1","resourceType":"AWS::EC2::Instance"}`}}}
	c := newWithClient(f)
	ds := connector.Dataset{Source: "resources"}
	// A predicate on a non-top-level column (or unsupported op) must not appear in
	// the pushed WHERE; the engine re-applies it.
	_, err := c.Scan(context.Background(), connector.ScanRequest{
		Dataset:   ds,
		Predicate: predicate(t, "resourceName <> 'x'"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.lastExpr, "WHERE") {
		t.Errorf("no pushable conjunct -> no WHERE, got %q", f.lastExpr)
	}
}

func TestScanPaginatesToCap(t *testing.T) {
	f := &fakeConfig{pages: [][]string{
		{`{"resourceId":"a"}`, `{"resourceId":"b"}`},
		{`{"resourceId":"c"}`},
	}}
	c := newWithClient(f)
	rows := drain(t, mustScan(t, c, connector.ScanRequest{Dataset: connector.Dataset{Source: "resources"}}))
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (paginated)", len(rows))
	}
	if f.calls != 2 {
		t.Errorf("expected 2 pages fetched, got %d", f.calls)
	}
}

func TestRawQueryModeInfersSchema(t *testing.T) {
	f := &fakeConfig{pages: [][]string{{
		`{"resourceId":"i-1","instanceType":"m5.large"}`,
		`{"resourceId":"i-2","instanceType":"t3.micro"}`,
	}}}
	c := newWithClient(f)
	raw := "SELECT resourceId, configuration.instanceType WHERE resourceType = 'AWS::EC2::Instance'"
	ds := connector.Dataset{Options: map[string]any{"query": raw}}

	sc, err := c.Resolve(context.Background(), ds)
	if err != nil {
		t.Fatal(err)
	}
	// Inferred, sorted columns.
	if len(sc.Columns) != 2 || sc.Columns[0].Name != "instanceType" || sc.Columns[1].Name != "resourceId" {
		t.Fatalf("inferred cols = %+v", sc.Columns)
	}
	_, err = c.Scan(context.Background(), connector.ScanRequest{Dataset: ds, Predicate: predicate(t, "resourceId = 'x'")})
	if err != nil {
		t.Fatal(err)
	}
	// Raw mode: the expression is verbatim (predicate not pushed).
	if f.lastExpr != raw {
		t.Errorf("raw expr = %q, want verbatim", f.lastExpr)
	}
}

func TestAggregatorThreadedThrough(t *testing.T) {
	f := &fakeConfig{pages: [][]string{{`{"resourceId":"a"}`}}}
	c := newWithClient(f)
	ds := connector.Dataset{Source: "resources", Options: map[string]any{"aggregator": "org-agg"}}
	_, _ = c.Scan(context.Background(), connector.ScanRequest{Dataset: ds})
	if f.lastAgg != "org-agg" {
		t.Errorf("aggregator = %q, want org-agg", f.lastAgg)
	}
}

func mustScan(t *testing.T, c *Connector, req connector.ScanRequest) engine.RowIterator {
	t.Helper()
	it, err := c.Scan(context.Background(), req)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return it
}
