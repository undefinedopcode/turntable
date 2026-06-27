package loginfer

import (
	"regexp"
	"strings"
	"testing"

	"github.com/april/turntable/internal/sql"
)

// Every emitted pattern must parse its own sample line and yield groups matching
// the reported columns.
func assertPatternValid(t *testing.T, tpl Template) {
	t.Helper()
	re, err := regexp.Compile(tpl.Pattern)
	if err != nil {
		t.Fatalf("pattern %q: %v", tpl.Pattern, err)
	}
	m := re.FindStringSubmatch(tpl.SampleLine)
	if m == nil {
		t.Fatalf("pattern %q did not match its sample %q", tpl.Pattern, tpl.SampleLine)
	}
	names := re.SubexpNames()
	got := 0
	for _, n := range names {
		if n != "" {
			got++
		}
	}
	if got != len(tpl.Columns) {
		t.Errorf("pattern has %d groups but %d columns", got, len(tpl.Columns))
	}
}

func TestInferPacmanLike(t *testing.T) {
	lines := strings.Split(`[2026-05-25T20:44:11-0700] [ALPM] installed glib2 (2.88.1-1)
[2026-05-25T20:45:02-0700] [ALPM] installed harfbuzz (12.1.0-1)
[2026-05-25T20:45:05-0700] [ALPM] installed systemd (260.1-2)`, "\n")
	tpls := Infer(lines)
	if len(tpls) != 1 {
		t.Fatalf("templates = %d, want 1: %+v", len(tpls), tpls)
	}
	tpl := tpls[0]
	if tpl.Count != 3 {
		t.Errorf("count = %d, want 3", tpl.Count)
	}
	assertPatternValid(t, tpl)
	// time + package + version (the (...) detail). The literal "[ALPM]" and
	// "installed" are anchors, not columns.
	if len(tpl.Columns) != 3 {
		t.Fatalf("columns = %v, want 3", tpl.Columns)
	}
	if tpl.Columns[0].Name != "time" || tpl.Columns[0].Type != "time" {
		t.Errorf("col0 = %+v, want time/time", tpl.Columns[0])
	}
	// the package column captures glib2 in the sample.
	if tpl.Sample[1] != "glib2" {
		t.Errorf("sample package = %q, want glib2", tpl.Sample[1])
	}
}

func TestInferVariableLengthMessage(t *testing.T) {
	// Different-length messages must reunite into one <MSG> template.
	lines := strings.Split(`2026-05-25 20:44:11 INFO starting worker pool size=8
2026-05-25 20:44:19 WARN some other message size=4
2026-05-25 20:44:25 INFO booting the primary scheduler now size=2`, "\n")
	tpls := Infer(lines)
	if len(tpls) != 1 {
		t.Fatalf("templates = %d, want 1 (messages should merge): %+v", len(tpls), tpls)
	}
	tpl := tpls[0]
	assertPatternValid(t, tpl)
	// time, level, message, size — message absorbs the variable middle.
	names := make([]string, len(tpl.Columns))
	for i, c := range tpl.Columns {
		names[i] = c.Name
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "message") || !strings.Contains(joined, "size") {
		t.Errorf("columns = %v, want a message + size", names)
	}
}

func TestInferMultipleTemplates(t *testing.T) {
	lines := strings.Split(`worker-3 processed 42 items
worker-1 processed 7 items
config /etc/app.conf reloaded
config /etc/db.conf reloaded`, "\n")
	tpls := Infer(lines)
	if len(tpls) < 2 {
		t.Fatalf("templates = %d, want >= 2", len(tpls))
	}
	for _, tpl := range tpls {
		assertPatternValid(t, tpl)
	}
	// Ordered by coverage (most lines first); both clusters have 2 here.
	if tpls[0].Count < tpls[len(tpls)-1].Count {
		t.Errorf("templates not ordered by count: %+v", tpls)
	}
}

func TestInferNumericFirstTokenClusters(t *testing.T) {
	// worker-N (varying N) as the first token must cluster into one template,
	// with the worker id captured as a field.
	lines := strings.Split(`worker-3 processed 42 items
worker-1 processed 7 items
worker-9 processed 128 items
worker-42 processed 5 items`, "\n")
	tpls := Infer(lines)
	if len(tpls) != 1 {
		t.Fatalf("templates = %d, want 1 (worker-N should cluster): %+v", len(tpls), tpls)
	}
	tpl := tpls[0]
	if tpl.Count != 4 {
		t.Errorf("count = %d, want 4", tpl.Count)
	}
	assertPatternValid(t, tpl)
	// First column captures the (variable) worker token; there's also the <NUM>.
	if len(tpl.Columns) < 2 {
		t.Fatalf("columns = %v, want the worker + the count", tpl.Columns)
	}
	if tpl.Sample[0] != "worker-3" {
		t.Errorf("first sample = %q, want worker-3", tpl.Sample[0])
	}
}

func TestBucketStem(t *testing.T) {
	for in, want := range map[string]string{
		"worker-3": "worker-#",
		"host42":   "host#",
		"<TS>":     "<TS>",
		"config":   "config",
		"v2.88.1":  "v#.#.#",
	} {
		if got := bucketStem(in); got != want {
			t.Errorf("bucketStem(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInferAvoidsReservedNames(t *testing.T) {
	// The token after the literal "in" would be named "in" — a SQL keyword. The
	// name pass must steer it clear so it can be queried bare.
	lines := strings.Split(`task done in 5s
task done in 9s
task done in 2s`, "\n")
	tpls := Infer(lines)
	if len(tpls) != 1 {
		t.Fatalf("templates = %d, want 1: %+v", len(tpls), tpls)
	}
	for _, tpl := range tpls {
		for _, c := range tpl.Columns {
			if sql.IsKeyword(c.Name) {
				t.Errorf("column %q is a reserved word", c.Name)
			}
		}
	}
	// The collision specifically becomes "in_".
	found := false
	for _, c := range tpls[0].Columns {
		if c.Name == "in_" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an 'in_' column, got %+v", tpls[0].Columns)
	}
}

func TestSafeIdent(t *testing.T) {
	for in, want := range map[string]string{
		"in":     "in_",
		"count":  "count_",
		"order":  "order_",
		"worker": "worker", // not reserved
		"msg":    "msg",
	} {
		if got := safeIdent(in); got != want {
			t.Errorf("safeIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCommonTemplateCatchAll(t *testing.T) {
	// A regular log whose first token (file) and message vary, so the miner
	// over-splits — but all lines share <word> <TS> <NUM> <LEVEL> <word> <MSG>.
	// A single "common" catch-all should be synthesized and offered first.
	lines := strings.Split(`api.log 2017-05-16 00:00:00.008 25746 INFO nova.api started serving on 0.0.0.0
compute.log 2017-05-16 00:01:02.100 25800 WARNING nova.compute claiming resources for instance abc
scheduler.log 2017-05-16 00:02:03.200 25900 INFO nova.scheduler selected host n3 for build request
api.log 2017-05-16 00:03:04.300 25746 INFO nova.api returning 200 for GET /servers in 0.12s
compute.log 2017-05-16 00:04:05.400 25800 INFO nova.compute spawned instance def on hypervisor`, "\n")
	tpls := Infer(lines)
	if len(tpls) == 0 {
		t.Fatal("no templates")
	}
	if !tpls[0].Common {
		t.Fatalf("first template should be the common catch-all, got %+v", tpls[0])
	}
	if tpls[0].Count != 5 {
		t.Errorf("common count = %d, want 5 (all rows)", tpls[0].Count)
	}
	assertPatternValid(t, tpls[0])
	// It must capture the shared header (time, level) plus a message, and the
	// pattern must match every line.
	re := regexp.MustCompile(tpls[0].Pattern)
	for _, ln := range lines {
		if re.FindStringSubmatch(ln) == nil {
			t.Errorf("common pattern did not match: %q", ln)
		}
	}
	names := make([]string, len(tpls[0].Columns))
	for i, c := range tpls[0].Columns {
		names[i] = c.Name
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "time") || !strings.Contains(joined, "level") || !strings.Contains(joined, "message") {
		t.Errorf("common columns = %v, want time/level/message", names)
	}
}

func TestCommonTemplateSkippedWhenMinerCovers(t *testing.T) {
	// When one mined template already covers everything, no redundant catch-all.
	lines := strings.Split(`svc done in 12ms ok=true
svc done in 7ms ok=true
svc done in 40ms ok=false`, "\n")
	for _, tpl := range Infer(lines) {
		if tpl.Common {
			t.Errorf("did not expect a common catch-all here: %+v", tpl)
		}
	}
}

func TestRenameColumn(t *testing.T) {
	pat := `^(?P<time>\S+) (?P<pkg>\S+)$`
	got := RenameColumn(pat, "pkg", "package")
	if got != `^(?P<time>\S+) (?P<package>\S+)$` {
		t.Errorf("rename = %q", got)
	}
}
