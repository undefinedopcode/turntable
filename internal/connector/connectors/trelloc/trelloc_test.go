package trelloc

import (
	"context"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

// fakeTrello serves canned arrays keyed by request path and records the last
// path requested (so board substitution can be asserted).
type fakeTrello struct {
	data     map[string][]map[string]any
	lastPath string
}

func (f *fakeTrello) get(ctx context.Context, path string) ([]map[string]any, error) {
	f.lastPath = path
	return f.data[path], nil
}

func drain(t *testing.T, it engine.RowIterator) []engine.Row {
	t.Helper()
	rows, err := engine.Materialize(context.Background(), it)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return rows
}

func TestResolveSchema(t *testing.T) {
	c := New()
	sc, err := c.Resolve(context.Background(), connector.Dataset{Source: "cards"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"id", "name", "desc", "closed", "id_board", "id_list", "due", "due_complete", "url", "date_last_activity", "pos"}
	if len(sc.Columns) != len(want) {
		t.Fatalf("cols = %d, want %d", len(sc.Columns), len(want))
	}
	for i, n := range want {
		if sc.Columns[i].Name != n {
			t.Errorf("col %d = %q, want %q", i, sc.Columns[i].Name, n)
		}
	}
}

func TestUnknownDataset(t *testing.T) {
	if _, err := New().Resolve(context.Background(), connector.Dataset{Source: "widgets"}); err == nil {
		t.Fatal("expected error for unknown dataset")
	}
}

func TestScanBoardsNoBoardNeeded(t *testing.T) {
	fake := &fakeTrello{data: map[string][]map[string]any{
		"/members/me/boards": {
			{"id": "b1", "name": "Roadmap", "closed": false, "dateLastActivity": "2024-01-02T03:04:05.000Z"},
			{"id": "b2", "name": "Archive", "closed": true},
		},
	}}
	c := newWithClient(fake)
	ds := connector.Dataset{Source: "boards", Options: map[string]any{"key": "k", "token": "t"}}
	it, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds})
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, it)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// cols: id(0) name(1) desc(2) closed(3) url(4) id_organization(5) date_last_activity(6)
	if rows[0].Values[1].V != "Roadmap" {
		t.Errorf("name = %v", rows[0].Values[1].V)
	}
	if b, _ := rows[1].Values[3].AsBool(); !b {
		t.Errorf("board 2 closed should be true")
	}
	if rows[0].Values[6].Type != engine.TypeTime {
		t.Errorf("dateLastActivity should coerce to time, got %v", rows[0].Values[6].Type)
	}
	// desc missing on both -> NULL.
	if !rows[0].Values[2].IsNull() {
		t.Errorf("missing desc should be NULL")
	}
}

func TestScanCardsRequiresBoardAndBuildsPath(t *testing.T) {
	fake := &fakeTrello{data: map[string][]map[string]any{
		"/boards/B1/cards": {
			{"id": "c1", "name": "Task", "idBoard": "B1", "idList": "L1", "pos": 16384.0, "dueComplete": false},
		},
	}}
	c := newWithClient(fake)

	// Missing board -> error.
	noBoard := connector.Dataset{Source: "cards", Options: map[string]any{"key": "k", "token": "t"}}
	if _, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: noBoard}); err == nil {
		t.Fatal("expected error when board option is missing")
	}

	ds := connector.Dataset{Source: "cards", Options: map[string]any{"key": "k", "token": "t", "board": "B1"}}
	it, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds})
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, it)
	if fake.lastPath != "/boards/B1/cards" {
		t.Errorf("path = %q, want /boards/B1/cards", fake.lastPath)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	// pos is a float column.
	if f, _ := rows[0].Values[10].AsFloat(); f != 16384.0 {
		t.Errorf("pos = %v, want 16384", rows[0].Values[10].V)
	}
}

func TestScanLimitNoPredicate(t *testing.T) {
	fake := &fakeTrello{data: map[string][]map[string]any{
		"/members/me/boards": {{"id": "1"}, {"id": "2"}, {"id": "3"}},
	}}
	c := newWithClient(fake)
	ds := connector.Dataset{Source: "boards", Options: map[string]any{"key": "k", "token": "t"}}
	one := 1
	it, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds, Limit: &one})
	if err != nil {
		t.Fatal(err)
	}
	if rows := drain(t, it); len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (limit)", len(rows))
	}
}

func TestMissingAuth(t *testing.T) {
	c := New() // no injected client; real client build requires key+token
	ds := connector.Dataset{Source: "boards", Options: map[string]any{}}
	if _, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds}); err == nil {
		t.Fatal("expected error when key/token are missing")
	}
}
