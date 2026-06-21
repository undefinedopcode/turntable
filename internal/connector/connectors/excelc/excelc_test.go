package excelc

import (
	"context"
	"testing"

	"github.com/april/octoparser/internal/connector"
	"github.com/april/octoparser/internal/engine"
	"github.com/xuri/excelize/v2"
)

// makeTestXLSX writes a workbook with two sheets to a temp path and returns it.
// "sales": id(int), item(string), qty(int), price(float)
// "notes": a single string column with a bool and a date for inference.
func makeTestXLSX(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/test.xlsx"
	f := excelize.NewFile()
	defer f.Close()

	// Sheet 1 (the default "Sheet1") renamed to "sales".
	sales := f.GetSheetName(0)
	f.SetSheetName(sales, "sales")
	f.SetCellValue("sales", "A1", "id")
	f.SetCellValue("sales", "B1", "item")
	f.SetCellValue("sales", "C1", "qty")
	f.SetCellValue("sales", "D1", "price")
	rows := [][]any{
		{1, "hammer", 10, 12.99},
		{2, "nails", 1000, 0.05},
		{3, "saw", 5, 24.50},
	}
	for i, r := range rows {
		f.SetCellValue("sales", cell(0, i+2), r[0])
		f.SetCellValue("sales", cell(1, i+2), r[1])
		f.SetCellValue("sales", cell(2, i+2), r[2])
		f.SetCellValue("sales", cell(3, i+2), r[3])
	}

	// Sheet 2: "notes" with mixed types to exercise inference.
	idx, _ := f.NewSheet("notes")
	f.SetActiveSheet(idx)
	f.SetCellValue("notes", "A1", "flag")
	f.SetCellValue("notes", "B1", "when")
	f.SetCellValue("notes", "A2", true)
	f.SetCellValue("notes", "B2", "2024-03-01")
	f.SetCellValue("notes", "A3", false)
	f.SetCellValue("notes", "B3", "2024-03-02")

	if err := f.SaveAs(path); err != nil {
		t.Fatalf("save xlsx: %v", err)
	}
	return path
}

// cell returns an Excel cell ref for column index col (0-based) and row (1-based).
func cell(col, row int) string {
	name, _ := excelize.CoordinatesToCellName(col+1, row)
	return name
}

func TestResolveAndScan(t *testing.T) {
	ctx := context.Background()
	path := makeTestXLSX(t)
	c := Connector{}
	ds := connector.Dataset{
		Name:    "sales",
		Source:  path,
		Options: map[string]any{"sheet": "sales"},
	}
	schema, err := c.Resolve(ctx, ds)
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	wantCols := []string{"id", "item", "qty", "price"}
	if len(schema.Columns) != len(wantCols) {
		t.Fatalf("schema cols = %d, want %d (%v)", len(schema.Columns), len(wantCols), schema.Columns)
	}
	for i, w := range wantCols {
		if schema.Columns[i].Name != w {
			t.Errorf("col[%d] = %q, want %q", i, schema.Columns[i].Name, w)
		}
	}
	// id and qty should be int; price float; item string.
	if schema.Columns[0].Type != engine.TypeInt {
		t.Errorf("id type = %s, want int", schema.Columns[0].Type)
	}
	if schema.Columns[3].Type != engine.TypeFloat {
		t.Errorf("price type = %s, want float", schema.Columns[3].Type)
	}

	it, err := c.Scan(ctx, connector.ScanRequest{Dataset: ds})
	if err != nil {
		t.Fatalf("Scan error: %v", err)
	}
	defer it.Close()
	rows, err := engine.Materialize(ctx, it)
	if err != nil {
		t.Fatalf("Materialize error: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	if rows[0].Values[1].AsString() != "hammer" {
		t.Errorf("row0 item = %q, want hammer", rows[0].Values[1].AsString())
	}
}

func TestDatasetsForAllSheets(t *testing.T) {
	ctx := context.Background()
	path := makeTestXLSX(t)
	c := Connector{}
	ds := connector.Dataset{Source: path}
	got, err := c.DatasetsFor(ctx, ds)
	if err != nil {
		t.Fatalf("DatasetsFor error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("datasets = %d, want 2", len(got))
	}
	names := []string{got[0].Name, got[1].Name}
	want := map[string]bool{"sales": false, "notes": false}
	for _, n := range names {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for n, found := range want {
		if !found {
			t.Errorf("missing sheet %q in %v", n, names)
		}
	}
	// Each dataset must carry the sheet name in options.
	for _, d := range got {
		if d.Options["sheet"] == nil {
			t.Errorf("dataset %q missing sheet option", d.Name)
		}
	}
}

func TestDefaultSheet(t *testing.T) {
	// No "sheet" option: the first sheet is used.
	ctx := context.Background()
	path := makeTestXLSX(t)
	c := Connector{}
	ds := connector.Dataset{Source: path}
	schema, err := c.Resolve(ctx, ds)
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if len(schema.Columns) != 4 {
		t.Errorf("default sheet cols = %d, want 4 (sales)", len(schema.Columns))
	}
}