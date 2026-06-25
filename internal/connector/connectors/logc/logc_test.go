package logc

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

func writeLog(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "test.log")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func scan(t *testing.T, path string, opts map[string]any) (engine.Schema, []engine.Row) {
	t.Helper()
	ds := connector.Dataset{Source: path, Options: opts}
	sc, err := New().Resolve(context.Background(), ds)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	it, err := New().Scan(context.Background(), connector.ScanRequest{Dataset: ds})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	rows, err := engine.Materialize(context.Background(), it)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return sc, rows
}

func colNames(s engine.Schema) []string {
	out := make([]string, len(s.Columns))
	for i, c := range s.Columns {
		out[i] = c.Name
	}
	return out
}

func col(s engine.Schema, name string) (engine.Column, bool) {
	for _, c := range s.Columns {
		if c.Name == name {
			return c, true
		}
	}
	return engine.Column{}, false
}

func TestDetectCombined(t *testing.T) {
	p := writeLog(t, `127.0.0.1 - frank [10/Oct/2023:13:55:36 -0700] "GET /a HTTP/1.1" 200 2326 "http://e/" "UA/1"
10.0.0.2 - - [10/Oct/2023:13:55:37 -0700] "POST /b HTTP/1.1" 401 12 "-" "curl/8"
`)
	sc, rows := scan(t, p, nil)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// cols: host ident user time method path protocol status bytes referer user_agent
	if got := colNames(sc); len(got) != 11 || got[4] != "method" || got[7] != "status" {
		t.Fatalf("cols = %v", got)
	}
	if c, _ := col(sc, "status"); c.Type != engine.TypeInt {
		t.Errorf("status type = %v, want int", c.Type)
	}
	if c, _ := col(sc, "time"); c.Type != engine.TypeTime {
		t.Errorf("time type = %v, want time", c.Type)
	}
	r0 := rows[0].Values
	if r0[4].V != "GET" || r0[5].V != "/a" {
		t.Errorf("row0 method/path = %v/%v", r0[4].V, r0[5].V)
	}
	if n, _ := r0[7].AsInt(); n != 200 {
		t.Errorf("status = %v, want 200", r0[7].V)
	}
	// missing user ("-") is NULL.
	if !rows[1].Values[2].IsNull() {
		t.Errorf("row1 user = %v, want NULL", rows[1].Values[2].V)
	}
}

func TestDetectJSONLines(t *testing.T) {
	p := writeLog(t, `{"level":"info","msg":"a","port":8080}
{"level":"error","msg":"b","port":9090}
`)
	sc, rows := scan(t, p, nil)
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	// keys sorted: level msg port
	if got := colNames(sc); len(got) != 3 || got[0] != "level" {
		t.Fatalf("cols = %v", got)
	}
	if n, _ := rows[0].Values[2].AsInt(); n != 8080 {
		t.Errorf("port = %v, want 8080 (number preserved)", rows[0].Values[2].V)
	}
}

func TestDetectLogfmt(t *testing.T) {
	p := writeLog(t, `level=info msg="all good" status=200 dur=1.5
level=warn msg="slow" status=200 dur=9.2
`)
	sc, rows := scan(t, p, nil)
	if got := colNames(sc); len(got) != 4 {
		t.Fatalf("cols = %v, want 4", got)
	}
	// numeric coercion: status int, dur float; quoted msg keeps spaces.
	if n, _ := rows[0].Values[indexOf(colNames(sc), "status")].AsInt(); n != 200 {
		t.Errorf("status not coerced to int: %v", rows[0].Values[indexOf(colNames(sc), "status")].V)
	}
	if rows[0].Values[indexOf(colNames(sc), "msg")].V != "all good" {
		t.Errorf("quoted msg = %v", rows[0].Values[indexOf(colNames(sc), "msg")].V)
	}
}

func TestDetectSyslog(t *testing.T) {
	p := writeLog(t, `Oct 10 13:55:36 myhost sshd[1234]: Accepted password for alice
Oct 10 13:55:40 myhost cron: session opened
`)
	sc, rows := scan(t, p, nil)
	if got := colNames(sc); got[1] != "host" || got[2] != "tag" || got[3] != "pid" {
		t.Fatalf("cols = %v", got)
	}
	if rows[0].Values[2].V != "sshd" {
		t.Errorf("tag = %v, want sshd", rows[0].Values[2].V)
	}
	if n, _ := rows[0].Values[3].AsInt(); n != 1234 {
		t.Errorf("pid = %v, want 1234", rows[0].Values[3].V)
	}
	// second line has no pid -> NULL.
	if !rows[1].Values[3].IsNull() {
		t.Errorf("row1 pid = %v, want NULL", rows[1].Values[3].V)
	}
}

func TestDetectLeveled(t *testing.T) {
	p := writeLog(t, `2023-10-10 13:55:36 INFO starting up
2023-10-10 13:55:37 ERROR boom
2023-10-10T13:55:38Z message with no level
`)
	sc, rows := scan(t, p, nil)
	if got := colNames(sc); got[0] != "time" || got[1] != "level" || got[2] != "message" {
		t.Fatalf("cols = %v", got)
	}
	if rows[0].Values[1].V != "INFO" {
		t.Errorf("level = %v, want INFO", rows[0].Values[1].V)
	}
	if rows[0].Values[2].V != "starting up" {
		t.Errorf("message = %v", rows[0].Values[2].V)
	}
	// no-level line: level NULL, whole remainder is the message.
	if !rows[2].Values[1].IsNull() {
		t.Errorf("row2 level = %v, want NULL", rows[2].Values[1].V)
	}
}

func TestDetectBracketed(t *testing.T) {
	// pacman/ALPM style: [timestamp] [component] message.
	p := writeLog(t, `[2026-05-25T20:44:11-0700] [ALPM] installed glib2 (2.88.1-1)
[2026-05-25T20:44:11-0700] [ALPM] warning: foo installed as foo.pacnew
[2026-05-25T20:44:11-0700] [ALPM-SCRIPTLET] Initializing machine ID.
`)
	sc, rows := scan(t, p, nil)
	if got := colNames(sc); len(got) != 3 || got[0] != "time" || got[1] != "component" || got[2] != "message" {
		t.Fatalf("cols = %v", got)
	}
	if c, _ := col(sc, "time"); c.Type != engine.TypeTime {
		t.Errorf("time type = %v, want time", c.Type)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	// the -0700 numeric offset parses (UTC hour is 03:44 next... just check non-null).
	if rows[0].Values[0].IsNull() {
		t.Error("time did not parse")
	}
	if rows[0].Values[1].V != "ALPM" || rows[0].Values[2].V != "installed glib2 (2.88.1-1)" {
		t.Errorf("row0 = %v / %v", rows[0].Values[1].V, rows[0].Values[2].V)
	}
	if rows[2].Values[1].V != "ALPM-SCRIPTLET" {
		t.Errorf("row2 component = %v", rows[2].Values[1].V)
	}
}

func TestBracketedNeedsTimestamp(t *testing.T) {
	// A bracketed line whose first field is NOT a timestamp must not be detected
	// as the bracketed format (falls back to raw).
	p := writeLog(t, `[module] [submodule] some text here
[other] [thing] more text
`)
	sc, _ := scan(t, p, nil)
	if got := colNames(sc); got[0] != "line" {
		t.Fatalf("expected raw fallback, got cols %v", got)
	}
}

func TestRawFallback(t *testing.T) {
	// Free-form lines that match no structured format fall back to raw.
	p := writeLog(t, `this is just some text
another unstructured line
`)
	sc, rows := scan(t, p, nil)
	if got := colNames(sc); got[0] != "line" {
		t.Fatalf("cols = %v, want line first", got)
	}
	if rows[0].Values[0].V != "this is just some text" {
		t.Errorf("line = %v", rows[0].Values[0].V)
	}
}

func TestForcedFormat(t *testing.T) {
	// A logfmt line, but forced to raw: one "line" column with the whole text.
	p := writeLog(t, "level=info msg=hi status=200 dur=1.5\n")
	sc, rows := scan(t, p, map[string]any{"format": "raw"})
	if len(sc.Columns) != 3 || sc.Columns[0].Name != "line" {
		t.Fatalf("forced raw cols = %v", colNames(sc))
	}
	if rows[0].Values[0].V != "level=info msg=hi status=200 dur=1.5" {
		t.Errorf("line = %v", rows[0].Values[0].V)
	}
}

func TestUnknownFormatErrors(t *testing.T) {
	p := writeLog(t, "x\n")
	if _, err := New().Resolve(context.Background(), connector.Dataset{Source: p, Options: map[string]any{"format": "bogus"}}); err == nil {
		t.Fatal("expected error for unknown format")
	}
}

func TestCustomPattern(t *testing.T) {
	p := writeLog(t, `[2023-10-10 13:55:36] worker-3 did 42 things
[2023-10-10 13:55:37] worker-1 did 7 things
`)
	pat := `^\[(?P<time>[^\]]+)\] (?P<worker>\S+) did (?P<n>\d+) things$`
	sc, rows := scan(t, p, map[string]any{"pattern": pat})
	if got := colNames(sc); len(got) != 3 || got[0] != "time" || got[1] != "worker" || got[2] != "n" {
		t.Fatalf("pattern cols = %v", got)
	}
	if c, _ := col(sc, "time"); c.Type != engine.TypeTime {
		t.Errorf("time type = %v, want time", c.Type)
	}
	if n, _ := rows[0].Values[2].AsInt(); n != 42 {
		t.Errorf("n = %v, want 42 (coerced int)", rows[0].Values[2].V)
	}
	if rows[0].Values[1].V != "worker-3" {
		t.Errorf("worker = %v", rows[0].Values[1].V)
	}
}

func TestPatternNoGroupsErrors(t *testing.T) {
	p := writeLog(t, "x\n")
	_, err := New().Resolve(context.Background(), connector.Dataset{Source: p, Options: map[string]any{"pattern": `^\d+$`}})
	if err == nil {
		t.Fatal("expected error: pattern has no named groups")
	}
}

func TestCoerceScalar(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{"200", int64(200)},
		{"1.5", 1.5},
		{"true", true},
		{"hello", "hello"},
	}
	for _, c := range cases {
		if got := coerceScalar(c.in).V; got != c.want {
			t.Errorf("coerceScalar(%q) = %v (%T), want %v", c.in, got, got, c.want)
		}
	}
}
