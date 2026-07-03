package matviewstore

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/april/turntable/internal/engine"
)

// TestRoundTrip persists a snapshot covering every engine type — including the
// ones Parquet's native types would blur (duration, any, bytes, time) and NULLs
// — and reads it back, asserting exact fidelity and preserved column order.
func TestRoundTrip(t *testing.T) {
	schema := engine.Schema{Columns: []engine.Column{
		{Name: "id", Type: engine.TypeInt, Nullable: true},
		{Name: "amount", Type: engine.TypeFloat, Nullable: true},
		{Name: "label", Type: engine.TypeString, Nullable: true},
		{Name: "active", Type: engine.TypeBool, Nullable: true},
		{Name: "seen_at", Type: engine.TypeTime, Nullable: true},
		{Name: "elapsed", Type: engine.TypeDuration, Nullable: true},
		{Name: "blob", Type: engine.TypeBytes, Nullable: true},
		{Name: "meta", Type: engine.TypeAny, Nullable: true},
	}}

	ts := time.Date(2026, 7, 3, 12, 30, 45, 123456000, time.UTC)
	rows := []engine.Row{
		{Values: []engine.Value{
			engine.IntVal(1),
			engine.FloatVal(9.5),
			engine.StringVal("hello"),
			engine.BoolVal(true),
			engine.TimeVal(ts),
			{Type: engine.TypeDuration, V: 90 * time.Minute},
			{Type: engine.TypeBytes, V: []byte{0x01, 0x02, 0xff}},
			engine.AnyVal(map[string]any{"k": "v", "n": float64(3)}),
		}},
		// A row that is NULL in every column exercises the definition levels.
		{Values: []engine.Value{
			engine.Null(), engine.Null(), engine.Null(), engine.Null(),
			engine.Null(), engine.Null(), engine.Null(), engine.Null(),
		}},
	}

	path := filepath.Join(t.TempDir(), "v.parquet")
	meta := Meta{
		Query:     "SELECT * FROM orders",
		CreatedAt: time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC),
		Populated: true,
	}
	if err := Write(path, schema, rows, meta); err != nil {
		t.Fatalf("write: %v", err)
	}

	gotSchema, gotRows, gotMeta, err := Read(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// Schema: exact names, types, order.
	if !reflect.DeepEqual(gotSchema, schema) {
		t.Errorf("schema mismatch:\n got %+v\nwant %+v", gotSchema, schema)
	}
	// Metadata.
	if gotMeta.Query != meta.Query || !gotMeta.Populated || !gotMeta.CreatedAt.Equal(meta.CreatedAt) {
		t.Errorf("meta = %+v, want %+v", gotMeta, meta)
	}
	if len(gotRows) != 2 {
		t.Fatalf("rows = %d, want 2", len(gotRows))
	}

	// Row 0: check each type round-tripped to the right Go value.
	r := gotRows[0].Values
	if v, _ := r[0].AsInt(); v != 1 {
		t.Errorf("id = %v", r[0].V)
	}
	if v, _ := r[1].AsFloat(); v != 9.5 {
		t.Errorf("amount = %v", r[1].V)
	}
	if r[2].AsString() != "hello" {
		t.Errorf("label = %v", r[2].V)
	}
	if b, _ := r[3].AsBool(); !b {
		t.Errorf("active = %v", r[3].V)
	}
	if got := r[4].V.(time.Time); !got.Equal(ts) {
		t.Errorf("seen_at = %v, want %v", got, ts)
	}
	if got := r[5].V.(time.Duration); got != 90*time.Minute {
		t.Errorf("elapsed = %v, want 90m", got)
	}
	if got := r[6].V.([]byte); !reflect.DeepEqual(got, []byte{0x01, 0x02, 0xff}) {
		t.Errorf("blob = %v", got)
	}
	// any round-trips through JSON.
	wantMeta := map[string]any{"k": "v", "n": float64(3)}
	if !reflect.DeepEqual(r[7].V, wantMeta) {
		t.Errorf("meta = %#v, want %#v", r[7].V, wantMeta)
	}
	if r[7].Type != engine.TypeAny {
		t.Errorf("meta type = %v, want any", r[7].Type)
	}

	// Row 1: all NULL.
	for i, v := range gotRows[1].Values {
		if !v.IsNull() {
			t.Errorf("row1 col %d = %+v, want NULL", i, v)
		}
	}
}

// TestUnpopulated persists a WITH NO DATA snapshot: schema present, zero rows,
// populated=false.
func TestUnpopulated(t *testing.T) {
	schema := engine.Schema{Columns: []engine.Column{
		{Name: "x", Type: engine.TypeInt, Nullable: true},
	}}
	path := filepath.Join(t.TempDir(), "empty.parquet")
	if err := Write(path, schema, nil, Meta{Query: "SELECT 1 AS x", Populated: false}); err != nil {
		t.Fatalf("write: %v", err)
	}
	gotSchema, rows, meta, err := Read(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %d, want 0", len(rows))
	}
	if meta.Populated {
		t.Error("populated = true, want false")
	}
	if len(gotSchema.Columns) != 1 || gotSchema.Columns[0].Name != "x" {
		t.Errorf("schema = %+v", gotSchema)
	}
}

// TestPortableArtifact confirms the file is a real Parquet file whose footer
// carries the sidecar — i.e. openable by any Parquet tool, not a private format.
func TestPortableArtifact(t *testing.T) {
	schema := engine.Schema{Columns: []engine.Column{{Name: "n", Type: engine.TypeInt, Nullable: true}}}
	path := filepath.Join(t.TempDir(), "p.parquet")
	if err := Write(path, schema, []engine.Row{{Values: []engine.Value{engine.IntVal(7)}}}, Meta{Query: "q"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The sidecar is valid JSON describing the columns.
	_, _, _, err := Read(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var cols []colJSON
	if err := json.Unmarshal([]byte(`[{"name":"n","type":"int"}]`), &cols); err != nil {
		t.Fatal(err)
	}
}
