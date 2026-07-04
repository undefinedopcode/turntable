// Package parquetw encodes turntable engine rows to Parquet. It is the shared
// write half used both by the materialized-view snapshot store
// (internal/matviewstore, which adds footer metadata on top) and by the web
// UI's Parquet export.
//
// Every column is written as an Optional leaf; the physical/logical type is the
// widest faithful Parquet carrier for the engine type (duration and int both
// ride as INT64, time as a µs TIMESTAMP, any as JSON). Callers that need exact
// engine-type recovery on read (matviewstore) persist the engine schema
// separately; a plain export just produces standard, portable Parquet.
package parquetw

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/april/turntable/internal/engine"
)

// Schema builds the Parquet schema for an engine schema. Every column is
// Optional (engine columns are nullable).
func Schema(schema engine.Schema) (*parquet.Schema, error) {
	g := parquet.Group{}
	for _, c := range schema.Columns {
		node, err := node(c.Type)
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", c.Name, err)
		}
		g[c.Name] = parquet.Optional(node)
	}
	return parquet.NewSchema("turntable", g), nil
}

func node(t engine.Type) (parquet.Node, error) {
	switch t {
	case engine.TypeInt, engine.TypeDuration:
		return parquet.Leaf(parquet.Int64Type), nil
	case engine.TypeFloat:
		return parquet.Leaf(parquet.DoubleType), nil
	case engine.TypeBool:
		return parquet.Leaf(parquet.BooleanType), nil
	case engine.TypeString:
		return parquet.String(), nil
	case engine.TypeBytes:
		return parquet.Leaf(parquet.ByteArrayType), nil
	case engine.TypeTime:
		// Microseconds: range-safe (±292k years) and enough precision for every
		// source turntable reads; sub-µs is effectively never present.
		return parquet.Timestamp(parquet.Microsecond), nil
	case engine.TypeAny:
		return parquet.JSON(), nil
	default:
		return nil, fmt.Errorf("cannot encode type %s", t)
	}
}

// LeafIndexByName maps each column name to its leaf index in the (name-sorted)
// Parquet schema, so EncodeRow can place values at the right column position.
func LeafIndexByName(s *parquet.Schema) map[string]int {
	cols := s.Columns()
	idx := make(map[string]int, len(cols))
	for i, path := range cols {
		idx[path[len(path)-1]] = i
	}
	return idx
}

// EncodeRows encodes engine rows to Parquet rows for the given Parquet schema.
func EncodeRows(schema engine.Schema, rows []engine.Row, ps *parquet.Schema) []parquet.Row {
	leaf := LeafIndexByName(ps)
	out := make([]parquet.Row, len(rows))
	for i, r := range rows {
		out[i] = EncodeRow(schema, r, leaf)
	}
	return out
}

// EncodeRow turns an engine row into a Parquet row: one value per column placed
// at its leaf index, with definition level 1 (present) or 0 (null).
func EncodeRow(schema engine.Schema, r engine.Row, leafIndex map[string]int) parquet.Row {
	prow := make(parquet.Row, len(schema.Columns))
	for i, c := range schema.Columns {
		col := leafIndex[c.Name]
		var v engine.Value
		if i < len(r.Values) {
			v = r.Values[i]
		}
		prow[col] = encodeValue(c.Type, v).Level(0, definitionLevel(v), col)
	}
	return prow
}

func definitionLevel(v engine.Value) int {
	if v.IsNull() || v.V == nil {
		return 0
	}
	return 1
}

func encodeValue(t engine.Type, v engine.Value) parquet.Value {
	if v.IsNull() || v.V == nil {
		return parquet.NullValue()
	}
	switch t {
	case engine.TypeInt:
		n, _ := v.AsInt()
		return parquet.Int64Value(n)
	case engine.TypeDuration:
		if d, ok := v.V.(time.Duration); ok {
			return parquet.Int64Value(int64(d))
		}
		return parquet.NullValue()
	case engine.TypeFloat:
		f, _ := v.AsFloat()
		return parquet.DoubleValue(f)
	case engine.TypeBool:
		b, _ := v.AsBool()
		return parquet.BooleanValue(b)
	case engine.TypeString:
		return parquet.ByteArrayValue([]byte(v.AsString()))
	case engine.TypeBytes:
		if b, ok := v.V.([]byte); ok {
			return parquet.ByteArrayValue(b)
		}
		return parquet.NullValue()
	case engine.TypeTime:
		if ts, ok := v.V.(time.Time); ok {
			return parquet.Int64Value(ts.UnixMicro())
		}
		return parquet.NullValue()
	case engine.TypeAny:
		b, err := json.Marshal(v.V)
		if err != nil {
			return parquet.NullValue()
		}
		return parquet.ByteArrayValue(b)
	default:
		return parquet.NullValue()
	}
}

// Write encodes schema+rows as a standalone Parquet file to w.
func Write(w io.Writer, schema engine.Schema, rows []engine.Row) error {
	ps, err := Schema(schema)
	if err != nil {
		return err
	}
	pw := parquet.NewWriter(w, ps)
	if len(rows) > 0 {
		if _, err := pw.WriteRows(EncodeRows(schema, rows, ps)); err != nil {
			return fmt.Errorf("write rows: %w", err)
		}
	}
	return pw.Close()
}
