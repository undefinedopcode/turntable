package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// matViewApp builds an App with an emp.json fixture and returns a `json:`
// qualified ref to it plus a helper that runs one statement and returns
// (stdout, stderr, code) on a single shared App (so session state persists).
func matViewApp(t *testing.T) (empRef string, seq func(q string) (string, string, int)) {
	t.Helper()
	dir := t.TempDir()
	emp := `[{"name":"Ann","dept":"eng","salary":100},
{"name":"Bob","dept":"eng","salary":90},
{"name":"Cy","dept":"eng","salary":150},
{"name":"Di","dept":"sales","salary":80}]`
	path := filepath.Join(dir, "emp.json")
	if err := os.WriteFile(path, []byte(emp), 0644); err != nil {
		t.Fatal(err)
	}
	app := NewApp()
	var out, errb bytes.Buffer
	app.Out = &out
	app.Err = &errb
	ctx := context.Background()
	seq = func(q string) (string, string, int) {
		out.Reset()
		errb.Reset()
		code := app.runQueryInto(ctx, q, app.replExplain, true, &out, &errb)
		return out.String(), errb.String(), code
	}
	return "json:" + path, seq
}

func TestMatViewLifecycle(t *testing.T) {
	emp, seq := matViewApp(t)

	// CREATE populates and reports the row count.
	_, e, code := seq("CREATE MATERIALIZED VIEW eng AS SELECT name, salary FROM " + emp + " WHERE dept = 'eng'")
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, e)
	}
	if !strings.Contains(e, "created (3 rows)") {
		t.Errorf("create notice = %q", e)
	}

	// The view is queryable by name with clean, unqualified columns.
	o, _, code := seq("SELECT name FROM eng ORDER BY salary DESC")
	if code != 0 {
		t.Fatalf("select exit %d", code)
	}
	if !strings.Contains(o, "Cy") || !strings.Contains(o, "Ann") || !strings.Contains(o, "Bob") {
		t.Errorf("select output:\n%s", o)
	}
	if strings.Contains(o, "Di") {
		t.Errorf("Di (sales) should not be in the eng view:\n%s", o)
	}

	// DROP unregisters it.
	if _, e, code := seq("DROP MATERIALIZED VIEW eng"); code != 0 || !strings.Contains(e, "dropped") {
		t.Fatalf("drop exit %d: %s", code, e)
	}
	if _, _, code := seq("SELECT * FROM eng"); code == 0 {
		t.Error("expected error querying a dropped view")
	}
}

func TestMatViewRefreshSnapshot(t *testing.T) {
	emp, seq := matViewApp(t)
	// A view over a filtered count; refresh re-runs the same query.
	if _, _, code := seq("CREATE MATERIALIZED VIEW c AS SELECT COUNT(*) AS n FROM " + emp); code != 0 {
		t.Fatal("create failed")
	}
	o, _, _ := seq("SELECT n FROM c")
	if !strings.Contains(o, "4") {
		t.Errorf("want count 4, got:\n%s", o)
	}
	if _, e, code := seq("REFRESH MATERIALIZED VIEW c"); code != 0 || !strings.Contains(e, "refreshed (1 rows)") {
		t.Errorf("refresh: code=%d notice=%q", code, e)
	}
}

func TestMatViewWithNoData(t *testing.T) {
	emp, seq := matViewApp(t)
	if _, e, code := seq("CREATE MATERIALIZED VIEW v AS SELECT name FROM " + emp + " WITH NO DATA"); code != 0 {
		t.Fatalf("create no data failed: %s", e)
	}
	// Unpopulated → scanning errors until refreshed.
	if _, e, code := seq("SELECT * FROM v"); code == 0 || !strings.Contains(e, "not been populated") {
		t.Errorf("expected unpopulated error, code=%d err=%q", code, e)
	}
	if _, _, code := seq("REFRESH MATERIALIZED VIEW v"); code != 0 {
		t.Fatal("refresh failed")
	}
	if o, _, code := seq("SELECT COUNT(*) AS n FROM v"); code != 0 || !strings.Contains(o, "4") {
		t.Errorf("after refresh: code=%d out=%s", code, o)
	}
}

func TestMatViewCollisionAndIfNotExists(t *testing.T) {
	emp, seq := matViewApp(t)
	if _, _, code := seq("CREATE MATERIALIZED VIEW v AS SELECT name FROM " + emp); code != 0 {
		t.Fatal("first create failed")
	}
	// A second CREATE without IF NOT EXISTS errors.
	if _, e, code := seq("CREATE MATERIALIZED VIEW v AS SELECT name FROM " + emp); code == 0 || !strings.Contains(e, "already exists") {
		t.Errorf("expected collision error, code=%d err=%q", code, e)
	}
	// IF NOT EXISTS makes it a no-op success.
	if _, e, code := seq("CREATE MATERIALIZED VIEW IF NOT EXISTS v AS SELECT name FROM " + emp); code != 0 || !strings.Contains(e, "skipping") {
		t.Errorf("if-not-exists: code=%d err=%q", code, e)
	}
}

func TestMatViewDuplicateColumnRejected(t *testing.T) {
	emp, seq := matViewApp(t)
	// Two columns both unqualifying to "name" must be rejected (Postgres rule).
	q := "CREATE MATERIALIZED VIEW v AS SELECT a.name, b.name FROM " + emp + " AS a JOIN " + emp + " AS b ON a.dept = b.dept"
	if _, e, code := seq(q); code == 0 || !strings.Contains(e, "specified more than once") {
		t.Errorf("expected duplicate-column error, code=%d err=%q", code, e)
	}
}

func TestMatViewDropIfExists(t *testing.T) {
	_, seq := matViewApp(t)
	// DROP IF EXISTS on a missing view is a no-op success.
	if _, e, code := seq("DROP MATERIALIZED VIEW IF EXISTS ghost"); code != 0 || !strings.Contains(e, "skipping") {
		t.Errorf("drop-if-exists: code=%d err=%q", code, e)
	}
	// Plain DROP on a missing view errors.
	if _, _, code := seq("DROP MATERIALIZED VIEW ghost"); code == 0 {
		t.Error("expected error dropping a missing view")
	}
}
