package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/april/octoparser/internal/engine"
)

func testSchema() engine.Schema {
	return engine.Schema{Columns: []engine.Column{
		{Name: "id", Type: engine.TypeInt},
		{Name: "name", Type: engine.TypeString},
	}}
}

func TestStreamCSV(t *testing.T) {
	schema := testSchema()
	rows := []engine.Row{
		{Values: []engine.Value{engine.IntVal(1), engine.StringVal("alice")}},
		{Values: []engine.Value{engine.IntVal(2), engine.StringVal("bob")}},
	}
	sr, err := NewStream(FormatCSV)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	n, err := sr.RenderStream(&buf, schema, engine.NewSliceIter(rows))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("row count = %d, want 2", n)
	}
	want := "id,name\n1,alice\n2,bob\n"
	if buf.String() != want {
		t.Errorf("csv stream = %q, want %q", buf.String(), want)
	}
}

func TestStreamJSON(t *testing.T) {
	schema := testSchema()
	rows := []engine.Row{
		{Values: []engine.Value{engine.IntVal(1), engine.StringVal("alice")}},
	}
	sr, err := NewStream(FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := sr.RenderStream(&buf, schema, engine.NewSliceIter(rows)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"id":1`) || !strings.Contains(buf.String(), `"name":"alice"`) {
		t.Errorf("json stream = %q", buf.String())
	}
	if !strings.HasPrefix(buf.String(), "[\n") || !strings.HasSuffix(buf.String(), "]\n") {
		t.Errorf("json stream not bracketed: %q", buf.String())
	}
}

func TestStreamNDJSON(t *testing.T) {
	schema := testSchema()
	rows := []engine.Row{
		{Values: []engine.Value{engine.IntVal(1), engine.StringVal("alice")}},
		{Values: []engine.Value{engine.IntVal(2), engine.StringVal("bob")}},
	}
	sr, err := NewStream(FormatNDJSON)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	n, err := sr.RenderStream(&buf, schema, engine.NewSliceIter(rows))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("row count = %d, want 2", n)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Errorf("ndjson lines = %d, want 2", len(lines))
	}
}

func TestStreamRaw(t *testing.T) {
	schema := testSchema()
	rows := []engine.Row{
		{Values: []engine.Value{engine.IntVal(1), engine.StringVal("alice")}},
	}
	sr, err := NewStream(FormatRaw)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := sr.RenderStream(&buf, schema, engine.NewSliceIter(rows)); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "1\talice\n" {
		t.Errorf("raw stream = %q", buf.String())
	}
}

func TestStreamTableUnsupported(t *testing.T) {
	if _, err := NewStream(FormatTable); err == nil {
		t.Error("expected error for streaming table format")
	}
}

func TestStreamYAML(t *testing.T) {
	schema := testSchema()
	rows := []engine.Row{
		{Values: []engine.Value{engine.IntVal(1), engine.StringVal("alice")}},
	}
	sr, err := NewStream(FormatYAML)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := sr.RenderStream(&buf, schema, engine.NewSliceIter(rows)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "id: 1") || !strings.Contains(buf.String(), "name: alice") {
		t.Errorf("yaml stream = %q", buf.String())
	}
}
