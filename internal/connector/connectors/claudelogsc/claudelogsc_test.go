package claudelogsc

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

// sample lines mixing message events with bookkeeping events (which must be
// skipped) and both string- and array-shaped content.
const sample = `{"type":"user","uuid":"u1","timestamp":"2026-05-23T20:15:38.928Z","sessionId":"s1","cwd":"/home/x","gitBranch":"main","message":{"role":"user","content":"hello there"}}
{"type":"ai-title","aiTitle":"chat","sessionId":"s1"}
{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-05-23T20:15:40.000Z","sessionId":"s1","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"hi!"},{"type":"tool_use","name":"Bash"},{"type":"text","text":"running it"}]}}
{"type":"mode","permissionMode":"default","sessionId":"s1"}
{"type":"user","uuid":"u2","parentUuid":"a1","timestamp":"2026-05-23T20:15:42.000Z","sessionId":"s1","message":{"role":"user","content":[{"type":"tool_result","content":"command output"}]}}
`

func writeSession(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func drain(t *testing.T, it engine.RowIterator) []engine.Row {
	t.Helper()
	rows, err := engine.Materialize(context.Background(), it)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return rows
}

func scanPath(t *testing.T, path string) []engine.Row {
	t.Helper()
	c := New()
	it, err := c.Scan(context.Background(), connector.ScanRequest{
		Dataset: connector.Dataset{Options: map[string]any{"path": path}},
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return drain(t, it)
}

func TestSchema(t *testing.T) {
	sc, _ := New().Resolve(context.Background(), connector.Dataset{})
	want := []string{"session_id", "session_file", "uuid", "parent_uuid", "type", "role", "model", "timestamp", "text", "tool_uses", "cwd", "git_branch"}
	if len(sc.Columns) != len(want) {
		t.Fatalf("cols = %d, want %d", len(sc.Columns), len(want))
	}
	for i, n := range want {
		if sc.Columns[i].Name != n {
			t.Errorf("col %d = %q, want %q", i, sc.Columns[i].Name, n)
		}
	}
}

func TestScanFileFiltersAndFlattens(t *testing.T) {
	dir := t.TempDir()
	p := writeSession(t, dir, "s1.jsonl", sample)
	rows := scanPath(t, p)

	// 3 message events (user, assistant, user); the ai-title and mode events
	// are skipped.
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (bookkeeping events skipped)", len(rows))
	}
	// cols: session_id(0) session_file(1) uuid(2) parent(3) type(4) role(5)
	//       model(6) timestamp(7) text(8) tool_uses(9) cwd(10) git_branch(11)
	r0 := rows[0].Values
	if r0[4].V != "user" || r0[8].V != "hello there" {
		t.Errorf("row0 type/text = %v / %v", r0[4].V, r0[8].V)
	}
	if r0[1].V != "s1" {
		t.Errorf("session_file = %v, want s1", r0[1].V)
	}
	if r0[7].Type != engine.TypeTime {
		t.Errorf("timestamp not coerced to time: %v", r0[7].Type)
	}

	// Assistant row: array content -> text blocks joined, tool_use counted.
	a := rows[1].Values
	if a[6].V != "claude-opus-4-8" {
		t.Errorf("model = %v", a[6].V)
	}
	if a[8].V != "hi!\nrunning it" {
		t.Errorf("assistant text = %q, want joined text blocks", a[8].V)
	}
	if n, _ := a[9].AsInt(); n != 1 {
		t.Errorf("tool_uses = %v, want 1", a[9].V)
	}

	// Tool-result user row: nested content extracted.
	if rows[2].Values[8].V != "command output" {
		t.Errorf("tool_result text = %v", rows[2].Values[8].V)
	}
}

func TestScanDirectoryAcrossSessions(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "s1.jsonl", sample)
	writeSession(t, dir, "s2.jsonl",
		`{"type":"user","uuid":"x","timestamp":"2026-05-24T00:00:00Z","sessionId":"s2","message":{"role":"user","content":"second session"}}
`)
	// A non-jsonl file must be ignored.
	writeSession(t, dir, "notes.txt", "ignore me")

	rows := scanPath(t, dir)
	if len(rows) != 4 { // 3 from s1 + 1 from s2
		t.Fatalf("rows = %d, want 4 across two sessions", len(rows))
	}
	sessions := map[string]int{}
	for _, r := range rows {
		sessions[r.Values[1].V.(string)]++
	}
	if sessions["s1"] != 3 || sessions["s2"] != 1 {
		t.Errorf("session counts = %v, want s1:3 s2:1", sessions)
	}
}

func TestScanToolsKind(t *testing.T) {
	dir := t.TempDir()
	// An assistant message with two tool_use blocks + text; the user/tool_result
	// message has no tool_use blocks (yields no tool rows).
	p := writeSession(t, dir, "s1.jsonl", sample)
	c := New()
	it, err := c.Scan(context.Background(), connector.ScanRequest{
		Dataset: connector.Dataset{Options: map[string]any{"path": p, "kind": "tools"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, it)
	// sample has one tool_use (Bash) in the assistant message.
	if len(rows) != 1 {
		t.Fatalf("tool rows = %d, want 1", len(rows))
	}
	// cols: session_id(0) session_file(1) message_uuid(2) parent(3) timestamp(4)
	//       tool_name(5) tool_id(6) tool_input(7)
	r := rows[0].Values
	if r[5].V != "Bash" {
		t.Errorf("tool_name = %v, want Bash", r[5].V)
	}
	if r[2].V != "a1" {
		t.Errorf("message_uuid = %v, want a1 (the assistant message)", r[2].V)
	}
}

func TestToolsKindWithInputs(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "s.jsonl",
		`{"type":"assistant","uuid":"a","timestamp":"2026-05-24T00:00:00Z","sessionId":"s","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls -la","description":"list"}},{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"/x"}}]}}
`)
	c := New()
	it, _ := c.Scan(context.Background(), connector.ScanRequest{
		Dataset: connector.Dataset{Options: map[string]any{"path": dir, "kind": "tools"}},
	})
	rows := drain(t, it)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (one per tool_use block)", len(rows))
	}
	// Inputs are JSON-encoded and queryable as strings.
	if s, _ := rows[0].Values[7].V.(string); s != `{"command":"ls -la","description":"list"}` {
		t.Errorf("tool_input[0] = %q", rows[0].Values[7].V)
	}
	if rows[1].Values[5].V != "Read" || rows[1].Values[6].V != "t2" {
		t.Errorf("row1 = name %v id %v", rows[1].Values[5].V, rows[1].Values[6].V)
	}
}

func TestUnknownKindErrors(t *testing.T) {
	c := New()
	_, err := c.Resolve(context.Background(), connector.Dataset{Options: map[string]any{"kind": "bogus"}})
	if err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

func TestDefaultProjectDir(t *testing.T) {
	// With base set and no path/project, it derives the slug from cwd.
	base := t.TempDir()
	cwd, _ := os.Getwd()
	projectDir := filepath.Join(base, slugify(cwd))
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSession(t, projectDir, "cur.jsonl",
		`{"type":"user","uuid":"d","timestamp":"2026-05-24T00:00:00Z","sessionId":"d","message":{"role":"user","content":"current project"}}
`)

	c := New()
	it, err := c.Scan(context.Background(), connector.ScanRequest{
		Dataset: connector.Dataset{Options: map[string]any{"base": base}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, it)
	if len(rows) != 1 || rows[0].Values[8].V != "current project" {
		t.Fatalf("default project dir resolution failed: %+v", rows)
	}
}

func TestMissingPathErrors(t *testing.T) {
	c := New()
	_, err := c.Scan(context.Background(), connector.ScanRequest{
		Dataset: connector.Dataset{Options: map[string]any{"path": "/no/such/path.jsonl"}},
	})
	if err == nil {
		t.Fatal("expected error for a nonexistent path")
	}
}
