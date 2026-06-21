package sqlc

import (
	"context"
	"database/sql"
	"testing"

	"github.com/april/octoparser/internal/connector"
	_ "modernc.org/sqlite"
)

func TestDatasetsForSQLite(t *testing.T) {
	ctx := context.Background()
	dsn := "file:datasets_test?mode=memory&cache=shared"

	// Seed an in-memory SQLite DB with several user tables (plus we verify
	// sqlite_* internal tables are excluded).
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	for _, ddl := range []string{
		`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE events (id INTEGER PRIMARY KEY, user_id INTEGER, ts TEXT)`,
		`CREATE TABLE metrics (id INTEGER PRIMARY KEY, value REAL)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("exec %q: %v", ddl, err)
		}
	}

	c := Connector{}
	ds := connector.Dataset{
		Name:    "",
		Source:  "",
		Options: map[string]any{"driver": "sqlite", "dsn": dsn},
	}
	got, err := c.DatasetsFor(ctx, ds)
	if err != nil {
		t.Fatalf("DatasetsFor error: %v", err)
	}
	names := make([]string, len(got))
	for i, d := range got {
		names[i] = d.Name
	}
	want := []string{"events", "metrics", "users"} // sorted by listTables
	if len(names) != 3 {
		t.Fatalf("got %v, want 3 tables (no sqlite_* internal tables)", names)
	}
	for _, w := range want {
		found := false
		for _, n := range names {
			if n == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing table %q in %v", w, names)
		}
	}
	// Each dataset must carry the connection options so it can be scanned.
	if got[0].Options["driver"] != "sqlite" || got[0].Options["dsn"] != dsn {
		t.Errorf("dataset options lost: %v", got[0].Options)
	}
}