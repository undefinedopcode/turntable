// Package excelc is the Excel (.xlsx) file connector backed by xuri/excelize.
// Each worksheet is exposed as a dataset: the first row is the header and
// subsequent rows are data. Column types are inferred from a sample of cells
// (int, float, bool, time, or string). All columns are nullable.
//
// A source may name a specific sheet via the "sheet" option. When "sheet" is
// "*" (or omitted at registration time with a wildcard), every sheet in the
// workbook is registered as its own dataset.
package excelc

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	"github.com/xuri/excelize/v2"
)

// Connector reads Excel (.xlsx) workbooks.
type Connector struct{}

// New constructs an Excel connector.
func New() *Connector { return &Connector{} }

func (Connector) Name() string { return "excel" }

func (Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) {
	return nil, nil
}

// DatasetsFor enumerates the worksheets in the workbook identified by
// ds.Source (the file path). It returns one Dataset per sheet, each carrying
// the path in Source and the sheet name in Options["sheet"].
func (Connector) DatasetsFor(ctx context.Context, ds connector.Dataset) ([]connector.Dataset, error) {
	path := ds.Source
	if path == "" {
		return nil, fmt.Errorf("excel connector requires a file path")
	}
	f, err := excelize.OpenFile(path)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()
	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return nil, fmt.Errorf("workbook %q has no sheets", path)
	}
	out := make([]connector.Dataset, 0, len(sheets))
	for _, name := range sheets {
		opts := map[string]any{}
		for k, v := range ds.Options {
			opts[k] = v
		}
		opts["sheet"] = name
		// The dataset Name is the sheet name so it can be registered as a
		// queryable source under that name; Source is the file path.
		out = append(out, connector.Dataset{Name: name, Source: path, Options: opts})
	}
	return out, nil
}

// sheetFor returns the sheet name from options, or the first sheet if unset.
func sheetFor(ds connector.Dataset, f *excelize.File) string {
	if s, ok := ds.Options["sheet"].(string); ok && s != "" {
		return s
	}
	if list := f.GetSheetList(); len(list) > 0 {
		return list[0]
	}
	return ""
}

func (Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	path := ds.Source
	f, err := excelize.OpenFile(path)
	if err != nil {
		return engine.Schema{}, fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()
	sheet := sheetFor(ds, f)
	if sheet == "" {
		return engine.Schema{}, fmt.Errorf("workbook %q has no sheets", path)
	}
	rows, err := f.Rows(sheet)
	if err != nil {
		return engine.Schema{}, fmt.Errorf("read sheet %q: %w", sheet, err)
	}
	defer rows.Close()

	// First row is the header.
	if !rows.Next() {
		return engine.Schema{}, nil // empty sheet
	}
	header, err := rows.Columns()
	if err != nil {
		return engine.Schema{}, fmt.Errorf("read header: %w", err)
	}

	// Sample up to 64 data rows to infer column types.
	types := make([]engine.Type, len(header))
	for i := range types {
		types[i] = engine.TypeString
	}
	for i := 0; i < 64; i++ {
		if !rows.Next() {
			break
		}
		cells, err := rows.Columns()
		if err != nil {
			break
		}
		for j := 0; j < len(cells) && j < len(types); j++ {
			t := inferCellType(cells[j], f, sheet)
			types[j] = widenType(types[j], t)
		}
	}
	cols := make([]engine.Column, len(header))
	for i, name := range header {
		if strings.TrimSpace(name) == "" {
			name = fmt.Sprintf("col%d", i+1)
		}
		cols[i] = engine.Column{Name: name, Type: types[i], Nullable: true}
	}
	return engine.Schema{Columns: cols}, nil
}

func (Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	path := req.Dataset.Source
	f, err := excelize.OpenFile(path)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	sheet := sheetFor(req.Dataset, f)
	if sheet == "" {
		f.Close()
		return nil, fmt.Errorf("workbook %q has no sheets", path)
	}
	rows, err := f.Rows(sheet)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("read sheet %q: %w", sheet, err)
	}
	schema, err := (Connector{}).Resolve(ctx, req.Dataset)
	if err != nil {
		rows.Close()
		f.Close()
		return nil, err
	}
	// Skip the header row.
	if !rows.Next() {
		rows.Close()
		f.Close()
		return &excelIter{closed: true}, nil
	}
	return &excelIter{f: f, rows: rows, schema: schema, sheet: sheet}, nil
}

type excelIter struct {
	f      *excelize.File
	rows   *excelize.Rows
	schema engine.Schema
	sheet  string
	closed bool
}

func (e *excelIter) Next() (engine.Row, bool, error) {
	if e.closed {
		return engine.Row{}, false, nil
	}
	if !e.rows.Next() {
		return engine.Row{}, false, nil
	}
	cells, err := e.rows.Columns()
	if err != nil {
		return engine.Row{}, false, fmt.Errorf("read row: %w", err)
	}
	vals := make([]engine.Value, len(e.schema.Columns))
	for i := 0; i < len(e.schema.Columns); i++ {
		if i >= len(cells) {
			vals[i] = engine.Null()
			continue
		}
		raw := cells[i]
		col := e.schema.Columns[i]
		vals[i] = parseCellValue(raw, col.Type)
	}
	return engine.Row{Values: vals}, true, nil
}

func (e *excelIter) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	err1 := e.rows.Close()
	err2 := e.f.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// ---- type inference / parsing -----------------------------------------------

// inferCellType guesses a column type from a raw cell string. Excel stores
// everything as text via Rows.Columns, so we parse the string the same way the
// CSV connector does, plus ISO date/time detection.
func inferCellType(s string, f *excelize.File, sheet string) engine.Type {
	s = strings.TrimSpace(s)
	if s == "" {
		return engine.TypeString
	}
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return engine.TypeInt
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return engine.TypeFloat
	}
	if strings.EqualFold(s, "true") || strings.EqualFold(s, "false") {
		return engine.TypeBool
	}
	if _, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return engine.TypeTime
	}
	if _, err := time.Parse("2006-01-02", s); err == nil {
		return engine.TypeTime
	}
	return engine.TypeString
}

// widenType reconciles a previously-inferred type with a new observation.
// TypeString is the initial "unknown" sentinel: the first non-string
// observation replaces it. int/float widen to float; any conflict with a real
// string observation collapses to string.
func widenType(prev, obs engine.Type) engine.Type {
	// Initial sentinel: adopt the first concrete type we see.
	if prev == engine.TypeString {
		return obs
	}
	if prev == obs {
		return prev
	}
	// int <-> float widens to float.
	if (prev == engine.TypeInt && obs == engine.TypeFloat) ||
		(prev == engine.TypeFloat && obs == engine.TypeInt) {
		return engine.TypeFloat
	}
	// Any other mismatch collapses to string.
	return engine.TypeString
}

func parseCellValue(raw string, t engine.Type) engine.Value {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return engine.Null()
	}
	switch t {
	case engine.TypeInt:
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return engine.IntVal(n)
		}
	case engine.TypeFloat:
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			return engine.FloatVal(f)
		}
	case engine.TypeBool:
		if strings.EqualFold(raw, "true") {
			return engine.BoolVal(true)
		}
		if strings.EqualFold(raw, "false") {
			return engine.BoolVal(false)
		}
	case engine.TypeTime:
		if tv, ok := parseExcelTime(raw); ok {
			return engine.TimeVal(tv)
		}
	}
	return engine.StringVal(raw)
}

func parseExcelTime(s string) (time.Time, bool) {
	layouts := []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05.999999",
		"2006-01-02",
		time.RFC3339,
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// statPath is a tiny helper used by callers that want to check a path exists
// before enumerating sheets; kept to avoid importing os elsewhere.
func statPath(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}