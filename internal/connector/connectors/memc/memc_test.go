package memc

import (
	"context"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

func sampleTable(populated bool) *Table {
	return &Table{
		Schema:    engine.Schema{Columns: []engine.Column{{Name: "n", Type: engine.TypeInt}}},
		Rows:      []engine.Row{{Values: []engine.Value{engine.IntVal(1)}}, {Values: []engine.Value{engine.IntVal(2)}}},
		Populated: populated,
	}
}

func TestMemcPutScanDrop(t *testing.T) {
	c := New()
	c.Put("v", sampleTable(true))
	if !c.Has("v") {
		t.Fatal("Has(v) = false after Put")
	}
	it, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: connector.Dataset{Name: "v"}})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := engine.Materialize(context.Background(), it)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("scanned %d rows, want 2", len(rows))
	}
	if !c.Drop("v") {
		t.Error("Drop(v) = false, want true")
	}
	if c.Drop("v") {
		t.Error("second Drop(v) = true, want false")
	}
}

func TestMemcUnpopulatedErrors(t *testing.T) {
	c := New()
	c.Put("v", sampleTable(false))
	if _, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: connector.Dataset{Name: "v"}}); err == nil {
		t.Error("expected error scanning an unpopulated view")
	}
	// Resolve still works (schema is known even WITH NO DATA).
	if _, err := c.Resolve(context.Background(), connector.Dataset{Name: "v"}); err != nil {
		t.Errorf("Resolve on unpopulated view: %v", err)
	}
}

func TestMemcMissing(t *testing.T) {
	c := New()
	if _, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: connector.Dataset{Name: "nope"}}); err == nil {
		t.Error("expected error scanning a missing table")
	}
}
