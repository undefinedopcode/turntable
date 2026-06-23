// Package parquetc is the Parquet file connector. It streams a Parquet file as
// rows using the file's embedded schema. Leaf column types are mapped from the
// Parquet physical/logical types to engine types. All columns are nullable.
//
// It uses github.com/parquet-go/parquet-go with the reflection-free, generic
// row API: parquet.OpenFile to read the footer/schema, and a streaming
// parquet.Reader.ReadRows loop to yield rows one at a time (bounded memory).
package parquetc

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	"github.com/parquet-go/parquet-go"
)

type Connector struct{}

func New() *Connector { return &Connector{} }

func (Connector) Name() string { return "parquet" }

func (Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) { return nil, nil }

// path returns the file path for a dataset: the Source, falling back to a
// "path" option.
func datasetPath(ds connector.Dataset) string {
	if ds.Source != "" {
		return ds.Source
	}
	return stringOpt(ds.Options, "path")
}

// stringOpt extracts a string option by key, returning "" when absent or not a
// string. Copied from the sqlc connector since it is not exported.
func stringOpt(opts map[string]any, key string) string {
	v, ok := opts[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// openFile opens the parquet file at path and returns the parsed *parquet.File
// plus the underlying *os.File (so the caller can close it).
func openFile(path string) (*os.File, *parquet.File, error) {
	if path == "" {
		return nil, nil, fmt.Errorf("parquet connector requires a file path")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %q: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("stat %q: %w", path, err)
	}
	pf, err := parquet.OpenFile(f, info.Size())
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("open parquet %q: %w", path, err)
	}
	return f, pf, nil
}

// tsUnit is the time unit a TIMESTAMP-logical INT64 column is stored in. For
// non-time columns it is tsNone.
type tsUnit int

const (
	tsNone tsUnit = iota
	tsMillis
	tsMicros
	tsNanos
)

func (Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	f, pf, err := openFile(datasetPath(ds))
	if err != nil {
		return engine.Schema{}, err
	}
	defer f.Close()
	schema, _ := schemaOf(pf.Schema())
	return schema, nil
}

// schemaOf builds an engine.Schema from a parquet schema's top-level fields,
// plus a parallel slice of per-column timestamp units. Leaf fields map to a
// typed column; nested groups (lists/maps/structs) map to TypeAny. Columns are
// emitted in field order, which matches the leaf column order in a row for flat
// schemas.
func schemaOf(s *parquet.Schema) (engine.Schema, []tsUnit) {
	fields := s.Fields()
	cols := make([]engine.Column, len(fields))
	units := make([]tsUnit, len(fields))
	for i, fld := range fields {
		cols[i] = engine.Column{
			Name:     fld.Name(),
			Type:     leafEngineType(fld),
			Nullable: true,
		}
		units[i] = timestampUnit(fld)
	}
	return engine.Schema{Columns: cols}, units
}

// timestampUnit returns the storage unit for an INT64 TIMESTAMP column, or
// tsNone if the field is not a timestamp.
func timestampUnit(node parquet.Node) tsUnit {
	if !node.Leaf() {
		return tsNone
	}
	lt := node.Type().LogicalType()
	if lt == nil || lt.Timestamp == nil {
		return tsNone
	}
	switch {
	case lt.Timestamp.Unit.Nanos != nil:
		return tsNanos
	case lt.Timestamp.Unit.Millis != nil:
		return tsMillis
	default:
		return tsMicros
	}
}

// leafEngineType maps a parquet field node to an engine.Type.
//
//	INT32 / INT64                 -> TypeInt
//	INT64 w/ TIMESTAMP logical    -> TypeTime
//	FLOAT / DOUBLE                -> TypeFloat
//	BOOLEAN                       -> TypeBool
//	BYTE_ARRAY w/ STRING(UTF8)    -> TypeString
//	BYTE_ARRAY (raw)              -> TypeBytes
//	FIXED_LEN_BYTE_ARRAY          -> TypeBytes
//	anything nested (list/map/..) -> TypeAny
func leafEngineType(node parquet.Node) engine.Type {
	if !node.Leaf() {
		return engine.TypeAny
	}
	typ := node.Type()
	logical := typ.LogicalType()
	switch typ.Kind() {
	case parquet.Boolean:
		return engine.TypeBool
	case parquet.Int32, parquet.Int64:
		if logical != nil && logical.Timestamp != nil {
			return engine.TypeTime
		}
		return engine.TypeInt
	case parquet.Float, parquet.Double:
		return engine.TypeFloat
	case parquet.ByteArray, parquet.FixedLenByteArray:
		if logical != nil && (logical.UTF8 != nil || logical.Json != nil || logical.Enum != nil) {
			return engine.TypeString
		}
		return engine.TypeBytes
	case parquet.Int96:
		// INT96 is the legacy timestamp encoding; its (Julian day + nanos)
		// layout is non-trivial to decode portably, so surface it as Any.
		return engine.TypeAny
	}
	return engine.TypeAny
}

func (Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	f, pf, err := openFile(datasetPath(req.Dataset))
	if err != nil {
		return nil, err
	}
	schema, units := schemaOf(pf.Schema())
	reader := parquet.NewReader(pf)

	// Honor Limit only when there is no predicate to push residual filtering to
	// the engine; with a predicate present, return all rows so the engine can
	// apply WHERE before LIMIT.
	var limit *int
	if req.Predicate == nil {
		limit = req.Limit
	}

	return &parquetIter{
		f:      f,
		reader: reader,
		schema: schema,
		units:  units,
		limit:  limit,
	}, nil
}

type parquetIter struct {
	f      *os.File
	reader *parquet.Reader
	schema engine.Schema
	units  []tsUnit

	buf    []parquet.Row // batch buffer
	bi     int           // index into buf
	eof    bool
	closed bool

	limit   *int
	emitted int
}

func (it *parquetIter) Next() (engine.Row, bool, error) {
	if it.closed {
		return engine.Row{}, false, nil
	}
	if it.limit != nil && it.emitted >= *it.limit {
		return engine.Row{}, false, nil
	}

	// Refill the batch buffer if exhausted.
	if it.bi >= len(it.buf) {
		if it.eof {
			return engine.Row{}, false, nil
		}
		if it.buf == nil {
			it.buf = make([]parquet.Row, 64)
		}
		n, err := it.reader.ReadRows(it.buf)
		it.buf = it.buf[:n]
		it.bi = 0
		if err != nil {
			if err == io.EOF {
				it.eof = true
			} else {
				return engine.Row{}, false, err
			}
		}
		if n == 0 {
			return engine.Row{}, false, nil
		}
	}

	prow := it.buf[it.bi]
	it.bi++
	it.emitted++
	return engine.Row{Values: it.convert(prow)}, true, nil
}

// convert maps a parquet row to engine values aligned to the schema column
// order. Each parquet value carries its leaf column index via Column(); we use
// that to place it. Columns with no value present are left null.
func (it *parquetIter) convert(prow parquet.Row) []engine.Value {
	vals := make([]engine.Value, len(it.schema.Columns))
	for i := range vals {
		vals[i] = engine.Null()
	}
	for _, pv := range prow {
		col := pv.Column()
		if col < 0 || col >= len(vals) {
			continue
		}
		vals[col] = toEngineValue(pv, it.schema.Columns[col].Type, it.units[col])
	}
	return vals
}

// toEngineValue converts a single parquet.Value to an engine.Value, guided by
// the column's resolved engine type (for INT/TIME disambiguation) and, for
// timestamps, the storage unit.
func toEngineValue(pv parquet.Value, t engine.Type, unit tsUnit) engine.Value {
	if pv.IsNull() {
		return engine.Null()
	}
	switch pv.Kind() {
	case parquet.Boolean:
		return engine.BoolVal(pv.Boolean())
	case parquet.Int32:
		return engine.IntVal(int64(pv.Int32()))
	case parquet.Int64:
		if t == engine.TypeTime {
			// TIMESTAMP logical types store an integer count since the unix
			// epoch in the column's declared unit.
			return engine.TimeVal(timestampFrom(pv.Int64(), unit))
		}
		return engine.IntVal(pv.Int64())
	case parquet.Float:
		return engine.FloatVal(float64(pv.Float()))
	case parquet.Double:
		return engine.FloatVal(pv.Double())
	case parquet.ByteArray, parquet.FixedLenByteArray:
		b := pv.ByteArray()
		if t == engine.TypeString {
			return engine.StringVal(string(b))
		}
		// Copy: the underlying buffer may be reused across reads.
		cp := make([]byte, len(b))
		copy(cp, b)
		return engine.Value{Type: engine.TypeBytes, V: cp}
	case parquet.Int96:
		return engine.AnyVal(pv.Int96().String())
	}
	return engine.AnyVal(pv.String())
}

// timestampFrom converts an integer timestamp in the given unit to a UTC time.
func timestampFrom(v int64, unit tsUnit) time.Time {
	switch unit {
	case tsMillis:
		return time.UnixMilli(v).UTC()
	case tsNanos:
		return time.Unix(0, v).UTC()
	default: // tsMicros or unspecified
		return time.UnixMicro(v).UTC()
	}
}

func (it *parquetIter) Close() error {
	if it.closed {
		return nil
	}
	it.closed = true
	err1 := it.reader.Close()
	err2 := it.f.Close()
	if err1 != nil {
		return err1
	}
	return err2
}
