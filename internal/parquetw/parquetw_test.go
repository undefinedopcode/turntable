package parquetw

import (
	"bytes"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/april/turntable/internal/engine"
)

// TestWriteReadback encodes rows covering every engine type (plus NULLs) and
// reads the Parquet back, confirming it is a valid, standard file with the right
// shape. Exact engine-type recovery is matviewstore's concern (via its sidecar);
// here we just check the encoder produces sound Parquet.
func TestWriteReadback(t *testing.T) {
	schema := engine.Schema{Columns: []engine.Column{
		{Name: "i", Type: engine.TypeInt},
		{Name: "f", Type: engine.TypeFloat},
		{Name: "s", Type: engine.TypeString},
		{Name: "b", Type: engine.TypeBool},
		{Name: "t", Type: engine.TypeTime},
		{Name: "d", Type: engine.TypeDuration},
		{Name: "raw", Type: engine.TypeBytes},
		{Name: "j", Type: engine.TypeAny},
	}}
	rows := []engine.Row{
		{Values: []engine.Value{
			engine.IntVal(7),
			engine.FloatVal(2.5),
			engine.StringVal("hi"),
			engine.BoolVal(true),
			engine.TimeVal(time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)),
			{Type: engine.TypeDuration, V: 3 * time.Second},
			{Type: engine.TypeBytes, V: []byte{9, 8, 7}},
			engine.AnyVal(map[string]any{"k": "v"}),
		}},
		{Values: []engine.Value{
			engine.Null(), engine.Null(), engine.Null(), engine.Null(),
			engine.Null(), engine.Null(), engine.Null(), engine.Null(),
		}},
	}

	var buf bytes.Buffer
	if err := Write(&buf, schema, rows); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := buf.Bytes(); len(got) < 4 || string(got[:4]) != "PAR1" {
		t.Fatal("output is not a parquet file")
	}

	pf, err := parquet.OpenFile(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if pf.NumRows() != 2 {
		t.Errorf("rows = %d, want 2", pf.NumRows())
	}
	if n := len(pf.Schema().Columns()); n != 8 {
		t.Errorf("columns = %d, want 8", n)
	}
}

func TestWriteEmpty(t *testing.T) {
	schema := engine.Schema{Columns: []engine.Column{{Name: "x", Type: engine.TypeInt}}}
	var buf bytes.Buffer
	if err := Write(&buf, schema, nil); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	pf, err := parquet.OpenFile(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if pf.NumRows() != 0 {
		t.Errorf("rows = %d, want 0", pf.NumRows())
	}
}
