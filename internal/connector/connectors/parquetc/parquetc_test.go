package parquetc

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	"github.com/parquet-go/parquet-go"
)

type sampleRow struct {
	ID     int64   `parquet:"id"`
	Name   string  `parquet:"name"`
	Score  float64 `parquet:"score"`
	Active bool    `parquet:"active"`
}

func writeSample(t *testing.T) (string, []sampleRow) {
	t.Helper()
	rows := []sampleRow{
		{ID: 1, Name: "alice", Score: 1.5, Active: true},
		{ID: 2, Name: "bob", Score: 2.25, Active: false},
		{ID: 3, Name: "carol", Score: -0.5, Active: true},
	}
	path := filepath.Join(t.TempDir(), "sample.parquet")
	if err := parquet.WriteFile(path, rows); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path, rows
}

func TestResolveSchema(t *testing.T) {
	path, _ := writeSample(t)
	c := New()
	ds := connector.Dataset{Name: path, Source: path}

	schema, err := c.Resolve(context.Background(), ds)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	want := []engine.Column{
		{Name: "id", Type: engine.TypeInt, Nullable: true},
		{Name: "name", Type: engine.TypeString, Nullable: true},
		{Name: "score", Type: engine.TypeFloat, Nullable: true},
		{Name: "active", Type: engine.TypeBool, Nullable: true},
	}
	if len(schema.Columns) != len(want) {
		t.Fatalf("got %d columns, want %d: %+v", len(schema.Columns), len(want), schema.Columns)
	}
	for i, w := range want {
		got := schema.Columns[i]
		if got.Name != w.Name || got.Type != w.Type || got.Nullable != w.Nullable {
			t.Errorf("column %d = %+v, want %+v", i, got, w)
		}
	}
}

func TestScanRows(t *testing.T) {
	path, rows := writeSample(t)
	c := New()
	ds := connector.Dataset{Name: path, Source: path}

	it, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	defer it.Close()

	var got []engine.Row
	for {
		row, ok, err := it.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		got = append(got, row)
	}

	if len(got) != len(rows) {
		t.Fatalf("got %d rows, want %d", len(got), len(rows))
	}
	for i, want := range rows {
		vals := got[i].Values
		if len(vals) != 4 {
			t.Fatalf("row %d: got %d values, want 4", i, len(vals))
		}
		if vals[0].Type != engine.TypeInt || vals[0].V.(int64) != want.ID {
			t.Errorf("row %d id = %+v, want %d", i, vals[0], want.ID)
		}
		if vals[1].Type != engine.TypeString || vals[1].V.(string) != want.Name {
			t.Errorf("row %d name = %+v, want %q", i, vals[1], want.Name)
		}
		if vals[2].Type != engine.TypeFloat || vals[2].V.(float64) != want.Score {
			t.Errorf("row %d score = %+v, want %v", i, vals[2], want.Score)
		}
		if vals[3].Type != engine.TypeBool || vals[3].V.(bool) != want.Active {
			t.Errorf("row %d active = %+v, want %v", i, vals[3], want.Active)
		}
	}
}

func TestScanLimitNoPredicate(t *testing.T) {
	path, _ := writeSample(t)
	c := New()
	ds := connector.Dataset{Name: path, Source: path}

	limit := 2
	it, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds, Limit: &limit})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	defer it.Close()

	n := 0
	for {
		_, ok, err := it.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		n++
	}
	if n != 2 {
		t.Fatalf("limit honored: got %d rows, want 2", n)
	}
}

func TestPathOptionFallback(t *testing.T) {
	path, _ := writeSample(t)
	c := New()
	// Source empty; path comes from options.
	ds := connector.Dataset{Name: path, Options: map[string]any{"path": path}}

	schema, err := c.Resolve(context.Background(), ds)
	if err != nil {
		t.Fatalf("Resolve via path option: %v", err)
	}
	if len(schema.Columns) != 4 {
		t.Fatalf("got %d columns, want 4", len(schema.Columns))
	}
}
