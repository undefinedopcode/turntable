// Package matviewstore persists a materialized view's snapshot to a single,
// self-contained Parquet file and reads it back.
//
// Parquet is a natural fit — columnar, compressed, typed, and turntable already
// reads it (the parquetc connector) — but its native type system is looser than
// turntable's (no distinct duration/bytes/structured, and timestamp precision is
// a storage choice). So rather than trust Parquet's inferred logical types on
// reload, each file carries a **sidecar** in its footer key-value metadata:
//
//   - turntable.schema     the exact engine.Schema (column names + types, in
//                          order) as JSON — the source of truth on read
//   - turntable.query      the defining SQL text, so REFRESH works after reload
//   - turntable.created_at RFC3339 timestamp of when the snapshot was taken
//   - turntable.populated  "true"/"false" (a WITH NO DATA view persists empty)
//
// On read we decode each Parquet cell guided by the sidecar type and reconcile
// columns by name, so fidelity survives even where Parquet's own types would
// blur (a duration column is a plain INT64 in the file but restores to
// TypeDuration). The file is still a portable artifact any Parquet reader
// (DuckDB, Polars, pandas) can open — it just sees the widened native types.
package matviewstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/april/turntable/internal/engine"
)

// Footer metadata keys.
const (
	keySchema    = "turntable.schema"
	keyQuery     = "turntable.query"
	keyCreatedAt = "turntable.created_at"
	keyPopulated = "turntable.populated"
)

// Meta is the non-row state persisted alongside a snapshot.
type Meta struct {
	Query     string    // defining SQL, for REFRESH after reload
	CreatedAt time.Time // when the snapshot was taken
	Populated bool      // false for a WITH NO DATA view
}

// colJSON is the on-disk form of one schema column (the sidecar).
type colJSON struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// Write serializes schema+rows+meta to a Parquet file at path, atomically
// (written to a temp file then renamed). An unpopulated snapshot writes the
// schema and zero rows.
func Write(path string, schema engine.Schema, rows []engine.Row, meta Meta) error {
	pschema, err := parquetSchema(schema)
	if err != nil {
		return err
	}
	sidecar, err := json.Marshal(columnsJSON(schema))
	if err != nil {
		return fmt.Errorf("encode schema: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".matview-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// On any failure below, don't leave the temp file behind.
	defer func() { _ = os.Remove(tmpName) }()

	w := parquet.NewWriter(tmp, pschema,
		parquet.KeyValueMetadata(keySchema, string(sidecar)),
		parquet.KeyValueMetadata(keyQuery, meta.Query),
		parquet.KeyValueMetadata(keyCreatedAt, meta.CreatedAt.UTC().Format(time.RFC3339Nano)),
		parquet.KeyValueMetadata(keyPopulated, fmt.Sprintf("%t", meta.Populated)),
	)

	leafIndex := leafIndexByName(pschema)
	prows := make([]parquet.Row, len(rows))
	for i, r := range rows {
		prows[i] = encodeRow(schema, r, leafIndex)
	}
	if len(prows) > 0 {
		if _, err := w.WriteRows(prows); err != nil {
			tmp.Close()
			return fmt.Errorf("write rows: %w", err)
		}
	}
	if err := w.Close(); err != nil {
		tmp.Close()
		return fmt.Errorf("finalize parquet: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Read loads a snapshot written by Write, returning the schema (from the
// sidecar, so types are exact), its rows, and the metadata.
func Read(path string) (engine.Schema, []engine.Row, Meta, error) {
	f, err := os.Open(path)
	if err != nil {
		return engine.Schema{}, nil, Meta{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return engine.Schema{}, nil, Meta{}, err
	}
	pf, err := parquet.OpenFile(f, info.Size())
	if err != nil {
		return engine.Schema{}, nil, Meta{}, fmt.Errorf("open parquet: %w", err)
	}

	sidecar, ok := pf.Lookup(keySchema)
	if !ok {
		return engine.Schema{}, nil, Meta{}, fmt.Errorf("%s: not a turntable materialized view (missing %s)", path, keySchema)
	}
	var cols []colJSON
	if err := json.Unmarshal([]byte(sidecar), &cols); err != nil {
		return engine.Schema{}, nil, Meta{}, fmt.Errorf("decode schema: %w", err)
	}
	schema, err := schemaFromJSON(cols)
	if err != nil {
		return engine.Schema{}, nil, Meta{}, err
	}

	meta := Meta{}
	meta.Query, _ = pf.Lookup(keyQuery)
	if s, ok := pf.Lookup(keyCreatedAt); ok {
		meta.CreatedAt, _ = time.Parse(time.RFC3339Nano, s)
	}
	if s, ok := pf.Lookup(keyPopulated); ok {
		meta.Populated = s == "true"
	} else {
		meta.Populated = true // pre-populated-flag files
	}

	rows, err := readRows(pf, schema)
	if err != nil {
		return engine.Schema{}, nil, Meta{}, err
	}
	return schema, rows, meta, nil
}

// --- schema mapping ----------------------------------------------------------

// parquetSchema builds the Parquet schema for an engine schema. Every column is
// Optional (engine columns are nullable). The physical/logical type is the
// widest faithful Parquet carrier; exact engine types are recovered from the
// sidecar on read, so e.g. duration and int both ride as INT64 here.
func parquetSchema(schema engine.Schema) (*parquet.Schema, error) {
	g := parquet.Group{}
	for _, c := range schema.Columns {
		node, err := parquetNode(c.Type)
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", c.Name, err)
		}
		g[c.Name] = parquet.Optional(node)
	}
	return parquet.NewSchema("matview", g), nil
}

func parquetNode(t engine.Type) (parquet.Node, error) {
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
		return nil, fmt.Errorf("cannot persist type %s", t)
	}
}

// leafIndexByName maps each column name to its leaf index in the (name-sorted)
// Parquet schema, so encodeRow can place values at the right column position.
func leafIndexByName(s *parquet.Schema) map[string]int {
	cols := s.Columns()
	idx := make(map[string]int, len(cols))
	for i, path := range cols {
		idx[path[len(path)-1]] = i
	}
	return idx
}

func columnsJSON(schema engine.Schema) []colJSON {
	out := make([]colJSON, len(schema.Columns))
	for i, c := range schema.Columns {
		out[i] = colJSON{Name: c.Name, Type: c.Type.String()}
	}
	return out
}

func schemaFromJSON(cols []colJSON) (engine.Schema, error) {
	out := make([]engine.Column, len(cols))
	for i, c := range cols {
		t, ok := typeByName(c.Type)
		if !ok {
			return engine.Schema{}, fmt.Errorf("unknown column type %q", c.Type)
		}
		out[i] = engine.Column{Name: c.Name, Type: t, Nullable: true}
	}
	return engine.Schema{Columns: out}, nil
}

func typeByName(s string) (engine.Type, bool) {
	switch s {
	case "int":
		return engine.TypeInt, true
	case "float":
		return engine.TypeFloat, true
	case "string":
		return engine.TypeString, true
	case "bool":
		return engine.TypeBool, true
	case "time":
		return engine.TypeTime, true
	case "duration":
		return engine.TypeDuration, true
	case "bytes":
		return engine.TypeBytes, true
	case "any":
		return engine.TypeAny, true
	case "null":
		return engine.TypeNull, true
	}
	return engine.TypeInvalid, false
}

// --- row encode / decode -----------------------------------------------------

// encodeRow turns an engine row into a Parquet row: one value per column placed
// at its leaf index, with definition level 1 (present) or 0 (null) since every
// column is Optional.
func encodeRow(schema engine.Schema, r engine.Row, leafIndex map[string]int) parquet.Row {
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

// readRows reads every Parquet row and maps cells back to engine values by
// column name (via the file's leaf order) and the sidecar schema's types.
func readRows(pf *parquet.File, schema engine.Schema) ([]engine.Row, error) {
	// leaf index -> position in the (sidecar) engine schema, by name.
	leafToEngine := make([]int, 0)
	for _, path := range pf.Schema().Columns() {
		leafToEngine = append(leafToEngine, schema.Index(path[len(path)-1]))
	}

	var out []engine.Row
	buf := make([]parquet.Row, 64)
	for _, g := range pf.RowGroups() {
		rows := g.Rows()
		for {
			n, err := rows.ReadRows(buf)
			for i := 0; i < n; i++ {
				out = append(out, decodeRow(buf[i], schema, leafToEngine))
			}
			if err != nil {
				rows.Close()
				if errors.Is(err, io.EOF) {
					break
				}
				return nil, fmt.Errorf("read rows: %w", err)
			}
			if n == 0 {
				rows.Close()
				break
			}
		}
	}
	return out, nil
}

func decodeRow(prow parquet.Row, schema engine.Schema, leafToEngine []int) engine.Row {
	vals := make([]engine.Value, len(schema.Columns))
	for i := range vals {
		vals[i] = engine.Null()
	}
	for _, pv := range prow {
		leaf := pv.Column()
		if leaf < 0 || leaf >= len(leafToEngine) {
			continue
		}
		ei := leafToEngine[leaf]
		if ei < 0 {
			continue // a column in the file not in the sidecar schema
		}
		vals[ei] = decodeValue(pv, schema.Columns[ei].Type)
	}
	return engine.Row{Values: vals}
}

func decodeValue(pv parquet.Value, t engine.Type) engine.Value {
	if pv.IsNull() {
		return engine.Null()
	}
	switch t {
	case engine.TypeInt:
		return engine.IntVal(pv.Int64())
	case engine.TypeDuration:
		return engine.Value{Type: engine.TypeDuration, V: time.Duration(pv.Int64())}
	case engine.TypeFloat:
		return engine.FloatVal(pv.Double())
	case engine.TypeBool:
		return engine.BoolVal(pv.Boolean())
	case engine.TypeString:
		return engine.StringVal(string(pv.ByteArray()))
	case engine.TypeBytes:
		b := pv.ByteArray()
		cp := make([]byte, len(b))
		copy(cp, b)
		return engine.Value{Type: engine.TypeBytes, V: cp}
	case engine.TypeTime:
		return engine.TimeVal(time.UnixMicro(pv.Int64()).UTC())
	case engine.TypeAny:
		var v any
		if err := json.Unmarshal(pv.ByteArray(), &v); err != nil {
			return engine.Null()
		}
		return engine.AnyVal(v)
	default:
		return engine.Null()
	}
}
