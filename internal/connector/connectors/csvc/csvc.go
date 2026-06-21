// Package csvc is the CSV file connector. It streams a CSV file as rows using
// the first record as the header. Column types are inferred from a sample of
// values: int, float, bool, or string (fallback). All columns are nullable.
package csvc

import (
	"bufio"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

type Connector struct{}

func New() *Connector { return &Connector{} }

func (Connector) Name() string { return "csv" }

func (Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) { return nil, nil }

func (Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	path := ds.Source
	sep := ','
	if d, ok := ds.Options["delimiter"].(string); ok && len(d) > 0 {
		sep = rune(d[0])
	}
	f, err := os.Open(path)
	if err != nil {
		return engine.Schema{}, fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.Comma = sep
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return engine.Schema{}, fmt.Errorf("read header: %w", err)
	}
	// Sample up to 64 rows to infer types.
	types := make([]engine.Type, len(header))
	for i := range types {
		types[i] = engine.TypeString
	}
	for i := 0; i < 64; i++ {
		rec, err := r.Read()
		if err != nil {
			break
		}
		for j := 0; j < len(rec) && j < len(types); j++ {
			t := inferCSVType(rec[j])
			if t != engine.TypeString && types[j] == engine.TypeString {
				types[j] = t
			} else if t != types[j] {
				// widen int->float, mixed->string
				if types[j] == engine.TypeInt && t == engine.TypeFloat {
					types[j] = engine.TypeFloat
				} else if t == engine.TypeString {
					types[j] = engine.TypeString
				}
			}
		}
	}
	cols := make([]engine.Column, len(header))
	for i, name := range header {
		cols[i] = engine.Column{Name: name, Type: types[i], Nullable: true}
	}
	return engine.Schema{Columns: cols}, nil
}

func (Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	path := req.Dataset.Source
	sep := ','
	if d, ok := req.Dataset.Options["delimiter"].(string); ok && len(d) > 0 {
		sep = rune(d[0])
	}
	schema, err := (Connector{}).Resolve(ctx, req.Dataset)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	r := csv.NewReader(bufio.NewReader(f))
	r.Comma = sep
	r.FieldsPerRecord = -1
	// skip header
	if _, err := r.Read(); err != nil {
		f.Close()
		return nil, fmt.Errorf("read header: %w", err)
	}
	return &csvIter{f: f, r: r, schema: schema}, nil
}

type csvIter struct {
	f      *os.File
	r      *csv.Reader
	schema engine.Schema
	closed bool
}

func (c *csvIter) Next() (engine.Row, bool, error) {
	rec, err := c.r.Read()
	if err != nil {
		if err == io.EOF || isParseEOF(err) {
			return engine.Row{}, false, nil
		}
		return engine.Row{}, false, err
	}
	vals := make([]engine.Value, len(c.schema.Columns))
	for i := 0; i < len(c.schema.Columns); i++ {
		if i >= len(rec) {
			vals[i] = engine.Null()
			continue
		}
		raw := rec[i]
		col := c.schema.Columns[i]
		vals[i] = parseCSVValue(raw, col.Type)
	}
	return engine.Row{Values: vals}, true, nil
}

func (c *csvIter) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	return c.f.Close()
}

func parseCSVValue(raw string, t engine.Type) engine.Value {
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
	}
	return engine.StringVal(raw)
}

func inferCSVType(s string) engine.Type {
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
	return engine.TypeString
}

func isParseEOF(err error) bool {
	return err == io.EOF
}