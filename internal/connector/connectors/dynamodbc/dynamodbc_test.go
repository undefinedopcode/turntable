package dynamodbc

import (
	"context"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

// fakeDynamo is an in-memory dynamoAPI. It paginates a fixed item list (capping
// each page at pageSize, like DynamoDB returning a partial page + a continuation
// key) and serves a fixed table list.
type fakeDynamo struct {
	items     []map[string]types.AttributeValue
	tables    []string
	pageSize  int // 0 = one page
	scanCalls int
}

const offKey = "__off"

func (f *fakeDynamo) Scan(ctx context.Context, in *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	f.scanCalls++
	start := 0
	if av, ok := in.ExclusiveStartKey[offKey]; ok {
		if n, ok := av.(*types.AttributeValueMemberN); ok {
			start, _ = strconv.Atoi(n.Value)
		}
	}
	n := len(f.items) - start
	if in.Limit != nil && int(*in.Limit) < n {
		n = int(*in.Limit)
	}
	if f.pageSize > 0 && f.pageSize < n {
		n = f.pageSize
	}
	end := start + n
	out := &dynamodb.ScanOutput{Items: f.items[start:end]}
	if end < len(f.items) {
		out.LastEvaluatedKey = map[string]types.AttributeValue{
			offKey: &types.AttributeValueMemberN{Value: strconv.Itoa(end)},
		}
	}
	return out, nil
}

func (f *fakeDynamo) ListTables(ctx context.Context, in *dynamodb.ListTablesInput, _ ...func(*dynamodb.Options)) (*dynamodb.ListTablesOutput, error) {
	return &dynamodb.ListTablesOutput{TableNames: f.tables}, nil
}

func s(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }
func num(v string) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: v}
}

func ds(opts map[string]any) connector.Dataset {
	if opts == nil {
		opts = map[string]any{"table": "t"}
	}
	return connector.Dataset{Name: "t", Source: "t", Options: opts}
}

func drain(t *testing.T, it engine.RowIterator) []engine.Row {
	t.Helper()
	rows, err := engine.Materialize(context.Background(), it)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return rows
}

func TestResolveInfersUnionSchema(t *testing.T) {
	fake := &fakeDynamo{items: []map[string]types.AttributeValue{
		{"id": num("1"), "name": s("ada")},
		{"id": num("2"), "name": s("bea"), "region": s("emea")}, // extra key
	}}
	c := newWithClient(fake)
	schema, err := c.Resolve(context.Background(), ds(nil))
	if err != nil {
		t.Fatal(err)
	}
	// Union of keys, sorted: id, name, region.
	want := []string{"id", "name", "region"}
	if len(schema.Columns) != len(want) {
		t.Fatalf("cols = %d, want %d (%+v)", len(schema.Columns), len(want), schema.Columns)
	}
	for i, n := range want {
		if schema.Columns[i].Name != n {
			t.Errorf("col %d = %q, want %q", i, schema.Columns[i].Name, n)
		}
		if schema.Columns[i].Type != engine.TypeAny {
			t.Errorf("col %q type = %v, want any", n, schema.Columns[i].Type)
		}
	}
}

func TestScanAlignsRowsAndNullsMissing(t *testing.T) {
	fake := &fakeDynamo{items: []map[string]types.AttributeValue{
		{"id": num("1"), "name": s("ada"), "region": s("emea")},
		{"id": num("2"), "name": s("bea")}, // missing region -> NULL
	}}
	c := newWithClient(fake)
	it, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds(nil)})
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, it)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// Columns sorted: id(0), name(1), region(2).
	if rows[0].Values[1].V != "ada" {
		t.Errorf("row0 name = %v", rows[0].Values[1].V)
	}
	if !rows[1].Values[2].IsNull() {
		t.Errorf("row1 region = %+v, want NULL", rows[1].Values[2])
	}
}

func TestScanPaginates(t *testing.T) {
	var items []map[string]types.AttributeValue
	for i := 0; i < 5; i++ {
		items = append(items, map[string]types.AttributeValue{"id": num(strconv.Itoa(i))})
	}
	fake := &fakeDynamo{items: items, pageSize: 2} // forces 3 pages
	c := newWithClient(fake)
	it, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if rows := drain(t, it); len(rows) != 5 {
		t.Fatalf("rows = %d, want 5 (across pages)", len(rows))
	}
	// 1 sample scan (Resolve) + 3 data pages.
	if fake.scanCalls < 4 {
		t.Errorf("scanCalls = %d, expected pagination (>=4)", fake.scanCalls)
	}
}

func TestScanLimitWithoutPredicate(t *testing.T) {
	var items []map[string]types.AttributeValue
	for i := 0; i < 5; i++ {
		items = append(items, map[string]types.AttributeValue{"id": num(strconv.Itoa(i))})
	}
	fake := &fakeDynamo{items: items, pageSize: 2}
	c := newWithClient(fake)
	three := 3
	it, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds(nil), Limit: &three})
	if err != nil {
		t.Fatal(err)
	}
	if rows := drain(t, it); len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (limit honored)", len(rows))
	}
}

func TestDatasetsForEnumerates(t *testing.T) {
	fake := &fakeDynamo{tables: []string{"users", "events", "orders"}}
	c := newWithClient(fake)
	got, err := c.DatasetsFor(context.Background(), connector.Dataset{Options: map[string]any{"region": "us-east-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("datasets = %d, want 3", len(got))
	}
	// Each dataset carries the table name as its own table option.
	for _, d := range got {
		if d.Options["table"] != d.Name {
			t.Errorf("dataset %q has table option %v", d.Name, d.Options["table"])
		}
		if d.Options["region"] != "us-east-1" {
			t.Errorf("dataset %q lost region option", d.Name)
		}
	}
}

func TestMissingTable(t *testing.T) {
	c := newWithClient(&fakeDynamo{})
	if _, err := c.Resolve(context.Background(), connector.Dataset{}); err == nil {
		t.Fatal("expected error when no table is given")
	}
}
