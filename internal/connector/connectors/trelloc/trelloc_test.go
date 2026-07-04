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

// idx returns the position of a column by name (schemas are looked up by name so
// tests survive added columns), failing if absent.
func idx(t *testing.T, sc engine.Schema, name string) int {
	t.Helper()
	if i := sc.Index(name); i >= 0 {
		return i
	}
	t.Fatalf("column %q not in schema", name)
	return -1
}

func schemaFor(t *testing.T, source string) engine.Schema {
	t.Helper()
	sc, err := New().Resolve(context.Background(), connector.Dataset{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func TestResolveSchema(t *testing.T) {
	sc := schemaFor(t, "cards")
	// The core columns plus the richer ones added from the full card object.
	for _, n := range []string{
		"id", "name", "desc", "closed", "id_board", "id_list", "due", "due_complete",
		"url", "date_last_activity", "pos",
		"id_members", "id_labels", "labels", "start", "short_url", "short_link", "badges",
	} {
		if sc.Index(n) < 0 {
			t.Errorf("cards schema missing column %q", n)
		}
	}
	// Array/object fields are carried as `any`.
	for _, n := range []string{"id_members", "labels", "badges"} {
		if sc.Columns[idx(t, sc, n)].Type != engine.TypeAny {
			t.Errorf("%q should be TypeAny", n)
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
	sc := schemaFor(t, "boards")
	if rows[0].Values[idx(t, sc, "name")].V != "Roadmap" {
		t.Errorf("name = %v", rows[0].Values[idx(t, sc, "name")].V)
	}
	if b, _ := rows[1].Values[idx(t, sc, "closed")].AsBool(); !b {
		t.Errorf("board 2 closed should be true")
	}
	if rows[0].Values[idx(t, sc, "date_last_activity")].Type != engine.TypeTime {
		t.Errorf("dateLastActivity should coerce to time")
	}
	// desc missing on both -> NULL.
	if !rows[0].Values[idx(t, sc, "desc")].IsNull() {
		t.Errorf("missing desc should be NULL")
	}
}

func TestScanCardsRequiresBoardAndBuildsPath(t *testing.T) {
	fake := &fakeTrello{data: map[string][]map[string]any{
		"/boards/B1/cards": {
			{
				"id": "c1", "name": "Task", "idBoard": "B1", "idList": "L1",
				"pos": 16384.0, "dueComplete": false, "idShort": 7.0,
				"idMembers": []any{"m1", "m2"},
				"labels":    []any{map[string]any{"name": "bug", "color": "red"}},
				"badges":    map[string]any{"comments": 3.0, "attachments": 1.0},
			},
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
	sc := schemaFor(t, "cards")
	if f, _ := rows[0].Values[idx(t, sc, "pos")].AsFloat(); f != 16384.0 {
		t.Errorf("pos = %v, want 16384", rows[0].Values[idx(t, sc, "pos")].V)
	}
	if n, _ := rows[0].Values[idx(t, sc, "id_short")].AsInt(); n != 7 {
		t.Errorf("id_short = %v, want 7", rows[0].Values[idx(t, sc, "id_short")].V)
	}
	// Array/object fields survive as `any` (slice / map).
	if _, ok := rows[0].Values[idx(t, sc, "id_members")].V.([]any); !ok {
		t.Errorf("id_members = %T, want []any", rows[0].Values[idx(t, sc, "id_members")].V)
	}
	if _, ok := rows[0].Values[idx(t, sc, "badges")].V.(map[string]any); !ok {
		t.Errorf("badges = %T, want map", rows[0].Values[idx(t, sc, "badges")].V)
	}
}

func TestMembersRequestsExtraFields(t *testing.T) {
	fake := &fakeTrello{data: map[string][]map[string]any{
		"/boards/B1/members?fields=id,fullName,username,initials,avatarUrl,confirmed": {
			{"id": "m1", "fullName": "Ada", "username": "ada", "initials": "A", "confirmed": true},
		},
	}}
	ds := connector.Dataset{Source: "members", Options: map[string]any{"key": "k", "token": "t", "board": "B1"}}
	it, err := newWithClient(fake).Scan(context.Background(), connector.ScanRequest{Dataset: ds})
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, it)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (path was %q)", len(rows), fake.lastPath)
	}
	sc := schemaFor(t, "members")
	if b, _ := rows[0].Values[idx(t, sc, "confirmed")].AsBool(); !b {
		t.Errorf("confirmed should be true")
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
