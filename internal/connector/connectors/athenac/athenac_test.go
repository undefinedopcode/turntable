package athenac

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	atypes "github.com/aws/aws-sdk-go-v2/service/athena/types"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	osql "github.com/april/turntable/internal/sql"
)

// fakeAthena is an in-memory athenaAPI: it records the submitted query, reports
// the query as SUCCEEDED, and serves canned metadata + result pages.
type fakeAthena struct {
	cols      []atypes.Column
	tables    []string
	pages     []*athena.GetQueryResultsOutput
	lastQuery string
	lastStart *athena.StartQueryExecutionInput
	pageIdx   int
}

func (f *fakeAthena) StartQueryExecution(_ context.Context, in *athena.StartQueryExecutionInput, _ ...func(*athena.Options)) (*athena.StartQueryExecutionOutput, error) {
	f.lastQuery = aws.ToString(in.QueryString)
	f.lastStart = in
	return &athena.StartQueryExecutionOutput{QueryExecutionId: aws.String("q1")}, nil
}

func (f *fakeAthena) GetQueryExecution(_ context.Context, _ *athena.GetQueryExecutionInput, _ ...func(*athena.Options)) (*athena.GetQueryExecutionOutput, error) {
	return &athena.GetQueryExecutionOutput{QueryExecution: &atypes.QueryExecution{
		Status: &atypes.QueryExecutionStatus{State: atypes.QueryExecutionStateSucceeded},
	}}, nil
}

func (f *fakeAthena) GetQueryResults(_ context.Context, _ *athena.GetQueryResultsInput, _ ...func(*athena.Options)) (*athena.GetQueryResultsOutput, error) {
	out := f.pages[f.pageIdx]
	f.pageIdx++
	return out, nil
}

func (f *fakeAthena) GetTableMetadata(_ context.Context, _ *athena.GetTableMetadataInput, _ ...func(*athena.Options)) (*athena.GetTableMetadataOutput, error) {
	return &athena.GetTableMetadataOutput{TableMetadata: &atypes.TableMetadata{
		Name: aws.String("inventory"), Columns: f.cols,
	}}, nil
}

func (f *fakeAthena) ListTableMetadata(_ context.Context, _ *athena.ListTableMetadataInput, _ ...func(*athena.Options)) (*athena.ListTableMetadataOutput, error) {
	var list []atypes.TableMetadata
	for _, t := range f.tables {
		list = append(list, atypes.TableMetadata{Name: aws.String(t)})
	}
	return &athena.ListTableMetadataOutput{TableMetadataList: list}, nil
}

func col(name, typ string) atypes.Column {
	return atypes.Column{Name: aws.String(name), Type: aws.String(typ)}
}

func row(vals ...*string) atypes.Row {
	data := make([]atypes.Datum, len(vals))
	for i, v := range vals {
		data[i] = atypes.Datum{VarCharValue: v}
	}
	return atypes.Row{Data: data}
}

func s(v string) *string { return &v }

func invCols() []atypes.Column {
	return []atypes.Column{
		col("id", "integer"), col("item", "varchar"), col("qty", "bigint"), col("price", "double"),
	}
}

func TestResolveSchema(t *testing.T) {
	c := newWithClient(&fakeAthena{cols: invCols()})
	schema, err := c.Resolve(context.Background(), connector.Dataset{Name: "inventory"})
	if err != nil {
		t.Fatal(err)
	}
	want := []engine.Type{engine.TypeInt, engine.TypeString, engine.TypeInt, engine.TypeFloat}
	if len(schema.Columns) != 4 {
		t.Fatalf("columns = %d, want 4", len(schema.Columns))
	}
	for i, w := range want {
		if schema.Columns[i].Type != w {
			t.Errorf("col %s type = %v, want %v", schema.Columns[i].Name, schema.Columns[i].Type, w)
		}
	}
}

func TestScanPushdownAndParse(t *testing.T) {
	fake := &fakeAthena{
		cols: invCols(),
		pages: []*athena.GetQueryResultsOutput{
			// Page 1: header row + one data row, with a NextToken for page 2.
			{
				ResultSet: &atypes.ResultSet{Rows: []atypes.Row{
					row(s("id"), s("item"), s("qty"), s("price")), // header (skipped)
					row(s("2"), s("nails"), s("1000"), s("0.05")),
				}},
				NextToken: aws.String("tok"),
			},
			// Page 2: one data row, no header, no NextToken.
			{
				ResultSet: &atypes.ResultSet{Rows: []atypes.Row{
					row(s("1"), s("hammer"), s("10"), nil), // null price
				}},
			},
		},
	}
	c := newWithClient(fake)
	ctx := context.Background()
	pred, _ := osql.ParseExpr(`qty > 5`)
	limit := 2
	ds := connector.Dataset{Name: "inventory", Options: map[string]any{
		"database": "default", "output_location": "s3://bucket/out", "workgroup": "wg",
	}}
	it, err := c.Scan(ctx, connector.ScanRequest{
		Dataset: ds, Columns: []string{"id", "item", "qty", "price"},
		Predicate: pred, Limit: &limit,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The submitted SQL must carry the Presto-quoted projection, pushed WHERE,
	// qualified table, and trailing LIMIT.
	q := fake.lastQuery
	for _, want := range []string{
		`SELECT "id", "item", "qty", "price"`,
		`FROM "default"."inventory"`,
		`WHERE ("qty" > 5)`,
		`LIMIT 2`,
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query missing %q:\n  %s", want, q)
		}
	}
	// Query context carried the database + output location + workgroup.
	if aws.ToString(fake.lastStart.QueryExecutionContext.Database) != "default" {
		t.Errorf("database = %q", aws.ToString(fake.lastStart.QueryExecutionContext.Database))
	}
	if fake.lastStart.ResultConfiguration == nil ||
		aws.ToString(fake.lastStart.ResultConfiguration.OutputLocation) != "s3://bucket/out" {
		t.Error("output location not set on the query")
	}
	if aws.ToString(fake.lastStart.WorkGroup) != "wg" {
		t.Error("workgroup not set")
	}

	rows, err := engine.Materialize(ctx, it)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (header skipped, 2 pages)", len(rows))
	}
	// Row 0: id=2 (int), item=nails (string), qty=1000 (int), price=0.05 (float).
	if n, _ := rows[0].Values[0].AsInt(); n != 2 {
		t.Errorf("row0 id = %v, want 2", rows[0].Values[0])
	}
	if rows[0].Values[1].AsString() != "nails" {
		t.Errorf("row0 item = %v", rows[0].Values[1])
	}
	if f, _ := rows[0].Values[3].AsFloat(); f != 0.05 {
		t.Errorf("row0 price = %v, want 0.05", rows[0].Values[3])
	}
	// Row 1 (page 2): null price.
	if !rows[1].Values[3].IsNull() {
		t.Errorf("row1 price should be NULL, got %v", rows[1].Values[3])
	}
}

func TestBuildQueryNoPushdownWhenUntranslatable(t *testing.T) {
	// An ILIKE can't push to Presto, so the WHERE is omitted and the limit is
	// withheld (the engine filters + limits).
	pred, _ := osql.ParseExpr(`item ILIKE 'h%'`)
	limit := 5
	q := buildQuery(connector.ScanRequest{Predicate: pred, Limit: &limit}, "db", "t")
	if strings.Contains(q, "WHERE") {
		t.Errorf("ILIKE should not push a WHERE: %s", q)
	}
	if strings.Contains(q, "LIMIT") {
		t.Errorf("limit must be withheld when predicate is unpushed: %s", q)
	}
	if q != `SELECT * FROM "db"."t"` {
		t.Errorf("query = %q", q)
	}
}

func TestTranslateExpr(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{`qty > 5`, `("qty" > 5)`, true},
		{`id IN (1, 2)`, `"id" IN (1, 2)`, true},
		{`name LIKE 'a%'`, `"name" LIKE 'a%'`, true},
		{`name ILIKE 'a%'`, "", false}, // Presto has no ILIKE
		{`qty BETWEEN 1 AND 9`, `"qty" BETWEEN 1 AND 9`, true},
	}
	for _, c := range cases {
		e, err := osql.ParseExpr(c.in)
		if err != nil {
			t.Fatalf("parse %q: %v", c.in, err)
		}
		got, ok := translateExpr(e)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("translateExpr(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestDatasetsFor(t *testing.T) {
	c := newWithClient(&fakeAthena{tables: []string{"orders", "customers"}})
	ds, err := c.DatasetsFor(context.Background(), connector.Dataset{Options: map[string]any{"database": "shop"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 2 || ds[0].Name != "orders" {
		t.Fatalf("datasets = %+v", ds)
	}
	if ds[0].Options["table"] != "orders" {
		t.Errorf("table option not set on enumerated dataset: %+v", ds[0].Options)
	}
}
