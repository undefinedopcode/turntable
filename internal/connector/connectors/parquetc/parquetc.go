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
	"strings"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/sql"
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

	// Row-group pruning: use the footer's per-chunk min/max statistics to skip
	// row groups that provably cannot contain a matching row. It is a pure
	// superset optimization — the engine re-applies the full WHERE regardless —
	// but on a time-partitioned sensor file a `WHERE ts > …` skips most of the
	// file without reading it.
	groups := pf.RowGroups()
	if req.Predicate != nil {
		if bounds := extractBounds(req.Predicate, schema); len(bounds) > 0 {
			groups = pruneRowGroups(groups, bounds, schema, units)
		}
	}

	// Honor Limit only when there is no predicate to push residual filtering to
	// the engine; with a predicate present, return all (surviving) rows so the
	// engine can apply WHERE before LIMIT.
	var limit *int
	if req.Predicate == nil {
		limit = req.Limit
	}

	return &parquetIter{
		f:      f,
		groups: groups,
		schema: schema,
		units:  units,
		limit:  limit,
	}, nil
}

type parquetIter struct {
	f      *os.File
	groups []parquet.RowGroup // row groups to read (post-pruning)
	gi     int                // next group to open
	rows   parquet.Rows       // reader over the current group (nil = open next)
	schema engine.Schema
	units  []tsUnit

	buf    []parquet.Row // batch buffer
	bi     int           // index into buf
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

	// Refill the batch buffer, advancing across row groups as each drains.
	for it.bi >= len(it.buf) {
		if it.rows == nil {
			if it.gi >= len(it.groups) {
				return engine.Row{}, false, nil
			}
			it.rows = it.groups[it.gi].Rows()
			it.gi++
		}
		if it.buf == nil {
			it.buf = make([]parquet.Row, 64)
		}
		it.buf = it.buf[:cap(it.buf)]
		n, err := it.rows.ReadRows(it.buf)
		it.buf = it.buf[:n]
		it.bi = 0
		if err != nil && err != io.EOF {
			return engine.Row{}, false, err
		}
		if err == io.EOF || n == 0 {
			// This group is drained; close it and move to the next.
			it.rows.Close()
			it.rows = nil
			if n == 0 {
				continue
			}
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
	var err1 error
	if it.rows != nil {
		err1 = it.rows.Close()
		it.rows = nil
	}
	err2 := it.f.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// ---- row-group pruning ---------------------------------------------------------

// bound is one provable per-column condition extracted from the pushed WHERE:
// `column OP literal` with OP in =, <, <=, >, >=.
type bound struct {
	col int // schema column index
	op  string
	val engine.Value
}

// extractBounds pulls prunable conditions out of the predicate's top-level AND
// conjuncts: column-vs-literal comparisons and BETWEEN. Anything else (OR, <>,
// LIKE, expressions, …) is simply skipped — it cannot help pruning, but the
// remaining conjuncts still can, and the engine applies the full WHERE anyway.
func extractBounds(pred connector.Expr, schema engine.Schema) []bound {
	var out []bound
	var walk func(e connector.Expr)
	walk = func(e connector.Expr) {
		switch ex := e.(type) {
		case *sql.BinaryOp:
			if ex.Op == "AND" {
				walk(ex.Left)
				walk(ex.Right)
				return
			}
			switch ex.Op {
			case "=", "<", "<=", ">", ">=":
			default:
				return
			}
			if col, val, ok := colLit(ex.Left, ex.Right, schema); ok {
				out = append(out, bound{col: col, op: ex.Op, val: val})
			} else if col, val, ok := colLit(ex.Right, ex.Left, schema); ok {
				out = append(out, bound{col: col, op: flipCmp(ex.Op), val: val})
			}
		case *sql.BetweenExpr:
			if ex.Negate {
				return
			}
			if col, lo, ok := colLit(ex.Expr, ex.Low, schema); ok {
				out = append(out, bound{col: col, op: ">=", val: lo})
				if _, hi, ok := colLit(ex.Expr, ex.High, schema); ok {
					out = append(out, bound{col: col, op: "<=", val: hi})
				}
			}
		}
	}
	walk(pred)
	return out
}

func flipCmp(op string) string {
	switch op {
	case "<":
		return ">"
	case "<=":
		return ">="
	case ">":
		return "<"
	case ">=":
		return "<="
	}
	return op
}

// colLit resolves (column-expr, literal-expr) to a schema column index and an
// engine literal value comparable against that column's chunk statistics. A
// string literal against a time column is parsed to a time.
func colLit(colExpr, litExpr connector.Expr, schema engine.Schema) (int, engine.Value, bool) {
	cr, ok := colExpr.(*sql.ColRef)
	if !ok {
		return 0, engine.Value{}, false
	}
	col := -1
	for i, c := range schema.Columns {
		if strings.EqualFold(c.Name, cr.Name) {
			col = i
			break
		}
	}
	if col < 0 {
		return 0, engine.Value{}, false
	}
	var v engine.Value
	switch lit := litExpr.(type) {
	case *sql.LitInt:
		v = engine.IntVal(lit.V)
	case *sql.LitFloat:
		v = engine.FloatVal(lit.V)
	case *sql.LitString:
		v = engine.StringVal(lit.V)
		if schema.Columns[col].Type == engine.TypeTime {
			t, err := parseTimeLit(lit.V)
			if err != nil {
				return 0, engine.Value{}, false
			}
			v = engine.TimeVal(t)
		}
	default:
		return 0, engine.Value{}, false
	}
	return col, v, true
}

func parseTimeLit(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable time %q", s)
}

// pruneRowGroups keeps only the groups whose chunk statistics could satisfy
// every bound. A chunk with no usable statistics never causes pruning.
func pruneRowGroups(groups []parquet.RowGroup, bounds []bound, schema engine.Schema, units []tsUnit) []parquet.RowGroup {
	kept := groups[:0:0]
	for _, rg := range groups {
		if groupMayMatch(rg, bounds, schema, units) {
			kept = append(kept, rg)
		}
	}
	return kept
}

// groupMayMatch reports whether a row group could contain a row satisfying
// every bound, judged by per-chunk [min, max]. Rows where the column is NULL
// fail a comparison predicate anyway, so nulls in a chunk never block pruning.
func groupMayMatch(rg parquet.RowGroup, bounds []bound, schema engine.Schema, units []tsUnit) bool {
	chunks := rg.ColumnChunks()
	for _, b := range bounds {
		if b.col >= len(chunks) || b.col >= len(units) {
			continue
		}
		lo, hi, ok := chunkBounds(chunks[b.col], schema.Columns[b.col].Type, units[b.col])
		if !ok {
			continue // no statistics: cannot prune on this column
		}
		match := true
		switch b.op {
		case "=":
			match = engine.Compare(b.val, lo) >= 0 && engine.Compare(b.val, hi) <= 0
		case "<":
			match = engine.Compare(lo, b.val) < 0
		case "<=":
			match = engine.Compare(lo, b.val) <= 0
		case ">":
			match = engine.Compare(hi, b.val) > 0
		case ">=":
			match = engine.Compare(hi, b.val) >= 0
		}
		if !match {
			return false
		}
	}
	return true
}

// chunkBounds returns the [min, max] of a column chunk from its page index,
// converted to engine values. ok=false when the index is absent or every page
// is null-only.
func chunkBounds(chunk parquet.ColumnChunk, typ engine.Type, unit tsUnit) (lo, hi engine.Value, ok bool) {
	idx, err := chunk.ColumnIndex()
	if err != nil || idx == nil {
		return engine.Value{}, engine.Value{}, false
	}
	for i := 0; i < idx.NumPages(); i++ {
		if idx.NullPage(i) {
			continue
		}
		mn, mx := idx.MinValue(i), idx.MaxValue(i)
		if mn.IsNull() || mx.IsNull() {
			continue
		}
		lov := toEngineValue(mn, typ, unit)
		hiv := toEngineValue(mx, typ, unit)
		if !ok {
			lo, hi, ok = lov, hiv, true
			continue
		}
		if engine.Compare(lov, lo) < 0 {
			lo = lov
		}
		if engine.Compare(hiv, hi) > 0 {
			hi = hiv
		}
	}
	return lo, hi, ok
}
