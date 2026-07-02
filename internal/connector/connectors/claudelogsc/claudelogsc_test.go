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
	want := []string{"session_id", "session_file", "uuid", "parent_uuid", "type", "role", "model", "timestamp", "text", "tool_uses", "input_tokens", "output_tokens", "cache_write_tokens", "cache_read_tokens", "cost_usd", "cwd", "git_branch"}
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
	//       model(6) timestamp(7) text(8) tool_uses(9) input_tokens(10)
	//       output_tokens(11) cache_write_tokens(12) cache_read_tokens(13)
	//       cost_usd(14) cwd(15) git_branch(16)
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

func TestScanToolResultsKind(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "s.jsonl",
		`{"type":"assistant","uuid":"a","timestamp":"2026-05-24T00:00:00Z","sessionId":"s","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}
{"type":"user","uuid":"u","timestamp":"2026-05-24T00:00:01Z","sessionId":"s","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"boom"},{"type":"tool_result","tool_use_id":"t2","content":[{"type":"text","text":"ok output"}]}]}}
`)
	c := New()
	it, err := c.Scan(context.Background(), connector.ScanRequest{
		Dataset: connector.Dataset{Options: map[string]any{"path": dir, "kind": "tool_results"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, it)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// cols: session_id(0) session_file(1) message_uuid(2) timestamp(3)
	//       tool_use_id(4) is_error(5) text(6)
	if rows[0].Values[4].V != "t1" {
		t.Errorf("tool_use_id = %v, want t1", rows[0].Values[4].V)
	}
	if b, _ := rows[0].Values[5].AsBool(); !b {
		t.Errorf("is_error = %v, want true", rows[0].Values[5].V)
	}
	if rows[0].Values[6].V != "boom" {
		t.Errorf("text = %v, want boom", rows[0].Values[6].V)
	}
	// Array content extracted; is_error defaults to false when absent.
	if b, _ := rows[1].Values[5].AsBool(); b {
		t.Errorf("row1 is_error = %v, want false (absent)", rows[1].Values[5].V)
	}
	if rows[1].Values[6].V != "ok output" {
		t.Errorf("row1 text = %v", rows[1].Values[6].V)
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

// costSample: assistant messages with usage — flat totals, the per-TTL cache
// breakdown, Sonnet 5 intro pricing, and an unpriceable synthetic model.
const costSample = `{"type":"user","uuid":"u1","timestamp":"2026-07-01T10:00:00Z","sessionId":"s1","message":{"role":"user","content":"q"}}
{"type":"assistant","uuid":"a1","timestamp":"2026-07-01T10:00:05Z","sessionId":"s1","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":100,"output_tokens":200,"cache_creation_input_tokens":1000,"cache_read_input_tokens":5000}}}
{"type":"assistant","uuid":"a2","timestamp":"2026-07-01T10:00:10Z","sessionId":"s1","message":{"role":"assistant","model":"claude-opus-4-8","content":[],"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":1000,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":400,"ephemeral_1h_input_tokens":600}}}}
{"type":"assistant","uuid":"a3","timestamp":"2026-07-01T10:00:15Z","sessionId":"s1","message":{"role":"assistant","model":"claude-sonnet-5","content":[],"usage":{"input_tokens":1000000,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}
{"type":"assistant","uuid":"a4","timestamp":"2026-10-01T10:00:15Z","sessionId":"s1","message":{"role":"assistant","model":"claude-sonnet-5","content":[],"usage":{"input_tokens":1000000,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}
{"type":"assistant","uuid":"a5","timestamp":"2026-07-01T10:00:20Z","sessionId":"s1","message":{"role":"assistant","model":"<synthetic>","content":[],"usage":{"input_tokens":10,"output_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}
`

func TestUsageAndCost(t *testing.T) {
	dir := t.TempDir()
	p := writeSession(t, dir, "s.jsonl", costSample)
	rows := scanPath(t, p)
	if len(rows) != 6 {
		t.Fatalf("rows = %d, want 6", len(rows))
	}
	const (
		colIn, colOut, colWrite, colRead, colCost = 10, 11, 12, 13, 14
	)
	approx := func(v engine.Value, want float64) bool {
		f, ok := v.AsFloat()
		return ok && f > want-1e-9 && f < want+1e-9
	}

	// User message: no usage -> all NULL.
	for _, c := range []int{colIn, colOut, colWrite, colRead, colCost} {
		if !rows[0].Values[c].IsNull() {
			t.Errorf("user row col %d = %v, want NULL", c, rows[0].Values[c].V)
		}
	}

	// Opus 4.8 flat usage: (100·5 + 200·25 + 1000·5·1.25 + 5000·5·0.1)/1e6.
	a1 := rows[1].Values
	if n, _ := a1[colIn].AsInt(); n != 100 {
		t.Errorf("input_tokens = %v", a1[colIn].V)
	}
	if n, _ := a1[colWrite].AsInt(); n != 1000 {
		t.Errorf("cache_write_tokens = %v", a1[colWrite].V)
	}
	if !approx(a1[colCost], 0.01425) {
		t.Errorf("opus cost = %v, want 0.01425", a1[colCost].V)
	}

	// Per-TTL breakdown: 400·5·1.25 + 600·5·2 = 2500 + 6000 = 0.0085.
	if !approx(rows[2].Values[colCost], 0.0085) {
		t.Errorf("ttl-split cost = %v, want 0.0085", rows[2].Values[colCost].V)
	}

	// Sonnet 5: intro pricing ($2/MTok in) before 2026-09-01, list ($3) after.
	if !approx(rows[3].Values[colCost], 2.0) {
		t.Errorf("sonnet5 intro cost = %v, want 2.0", rows[3].Values[colCost].V)
	}
	if !approx(rows[4].Values[colCost], 3.0) {
		t.Errorf("sonnet5 list cost = %v, want 3.0", rows[4].Values[colCost].V)
	}

	// Unknown model: tokens surface, cost is honest NULL.
	a5 := rows[5].Values
	if n, _ := a5[colIn].AsInt(); n != 10 {
		t.Errorf("synthetic input_tokens = %v", a5[colIn].V)
	}
	if !a5[colCost].IsNull() {
		t.Errorf("synthetic cost = %v, want NULL", a5[colCost].V)
	}
}
