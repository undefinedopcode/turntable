package azdevopsc

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

// fakeDevops records the WIQL/fields/max it was asked for and returns canned
// flattened work item maps.
type fakeDevops struct {
	items    []map[string]any
	lastWIQL string
	lastMax  int
	lastFlds []string
}

func (f *fakeDevops) queryWorkItems(ctx context.Context, wiql string, flds []string, max int) ([]map[string]any, error) {
	f.lastWIQL, f.lastFlds, f.lastMax = wiql, flds, max
	items := f.items
	if max > 0 && max < len(items) {
		items = items[:max]
	}
	return items, nil
}

func drain(t *testing.T, it engine.RowIterator) []engine.Row {
	t.Helper()
	rows, err := engine.Materialize(context.Background(), it)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return rows
}

func ds(opts map[string]any) connector.Dataset {
	if opts == nil {
		opts = map[string]any{}
	}
	return connector.Dataset{Name: "work_items", Source: "work_items", Options: opts}
}

func TestResolveSchema(t *testing.T) {
	sc, err := New().Resolve(context.Background(), ds(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"id", "title", "work_item_type", "state", "assigned_to", "area_path", "iteration_path", "tags", "priority", "created_date", "changed_date"}
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
	if _, err := New().Resolve(context.Background(), connector.Dataset{Source: "epics"}); err == nil {
		t.Fatal("expected error for unknown dataset")
	}
}

func TestScanFlattensFields(t *testing.T) {
	fake := &fakeDevops{items: []map[string]any{
		{
			"id":                             float64(42),
			"System.Title":                   "Fix the thing",
			"System.WorkItemType":            "Bug",
			"System.State":                   "Active",
			"System.AssignedTo":              map[string]any{"displayName": "Ada Lovelace", "uniqueName": "ada@x"},
			"Microsoft.VSTS.Common.Priority": float64(2),
			"System.ChangedDate":             "2024-03-04T05:06:07Z",
		},
		{
			"id":           float64(43),
			"System.Title": "Unassigned item",
			"System.State": "New",
			// no AssignedTo -> NULL
		},
	}}
	c := newWithClient(fake)
	it, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds(nil)})
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, it)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// cols: id(0) title(1) type(2) state(3) assigned_to(4) area(5) iter(6) tags(7) priority(8) created(9) changed(10)
	if n, _ := rows[0].Values[0].AsInt(); n != 42 {
		t.Errorf("id = %v, want 42", rows[0].Values[0].V)
	}
	if rows[0].Values[4].V != "Ada Lovelace" {
		t.Errorf("assigned_to = %v, want Ada Lovelace (nested displayName)", rows[0].Values[4].V)
	}
	if p, _ := rows[0].Values[8].AsInt(); p != 2 {
		t.Errorf("priority = %v, want 2", rows[0].Values[8].V)
	}
	if rows[0].Values[10].Type != engine.TypeTime {
		t.Errorf("changed_date should coerce to time, got %v", rows[0].Values[10].Type)
	}
	// Row 1: missing AssignedTo -> NULL.
	if !rows[1].Values[4].IsNull() {
		t.Errorf("row1 assigned_to = %+v, want NULL", rows[1].Values[4])
	}
}

func TestDefaultWIQLAndTypeFilter(t *testing.T) {
	fake := &fakeDevops{}
	c := newWithClient(fake)
	_, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds(map[string]any{"type": "Bug"})})
	if err != nil {
		t.Fatal(err)
	}
	if want := "SELECT [System.Id] FROM workitems WHERE [System.TeamProject] = @project AND [System.WorkItemType] = 'Bug' ORDER BY [System.ChangedDate] DESC"; fake.lastWIQL != want {
		t.Errorf("wiql:\n got  %s\n want %s", fake.lastWIQL, want)
	}
	// Requested fields should include the namespaced keys but not "id".
	for _, f := range fake.lastFlds {
		if f == "id" {
			t.Error("requested fields should not include synthetic id")
		}
	}
}

func TestWIQLOverride(t *testing.T) {
	fake := &fakeDevops{}
	c := newWithClient(fake)
	custom := "SELECT [System.Id] FROM workitems WHERE [System.State] = 'Done'"
	if _, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds(map[string]any{"wiql": custom})}); err != nil {
		t.Fatal(err)
	}
	if fake.lastWIQL != custom {
		t.Errorf("wiql = %q, want override %q", fake.lastWIQL, custom)
	}
}

func TestScanLimitNoPredicate(t *testing.T) {
	fake := &fakeDevops{items: []map[string]any{
		{"id": float64(1)}, {"id": float64(2)}, {"id": float64(3)},
	}}
	c := newWithClient(fake)
	two := 2
	it, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds(nil), Limit: &two})
	if err != nil {
		t.Fatal(err)
	}
	if rows := drain(t, it); len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if fake.lastMax != 2 {
		t.Errorf("max passed to API = %d, want 2", fake.lastMax)
	}
}

func TestNormalizeOrg(t *testing.T) {
	cases := map[string]string{
		"myorg":                          "myorg",
		"https://dev.azure.com/myorg":    "myorg",
		"https://dev.azure.com/myorg/":   "myorg",
		"dev.azure.com/myorg":            "myorg",
		"http://dev.azure.com/myorg/sub": "myorg",
		"  myorg  ":                      "myorg",
	}
	for in, want := range cases {
		if got := normalizeOrg(in); got != want {
			t.Errorf("normalizeOrg(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestURLEscaping guards against the "dangerous Request.Path" 400: a URL-valued
// org must not leak ":" into the path, and a project name with a space must be
// percent-encoded.
func TestURLEscaping(t *testing.T) {
	h := &httpClient{
		base:    "https://dev.azure.com",
		org:     normalizeOrg("https://dev.azure.com/contoso"),
		project: "My Project",
	}
	wiql := fmt.Sprintf("%s/%s/%s/_apis/wit/wiql?api-version=%s",
		h.base, h.orgPath(), h.projectPath(), apiVersion)
	if strings.Contains(wiql[len("https://"):], ":") {
		t.Errorf("path still contains a colon: %s", wiql)
	}
	if !strings.Contains(wiql, "/contoso/") {
		t.Errorf("org not normalized into path: %s", wiql)
	}
	if !strings.Contains(wiql, "My%20Project") {
		t.Errorf("project space not escaped: %s", wiql)
	}
}

// TestRealClientPathNoColon drives the actual HTTP client (not the injected
// fake) against a local server, with a URL-valued organization — the config
// that produced the reported "dangerous Request.Path (:)" 400. It asserts the
// request path Azure would have seen contains no colon and the org/project are
// correct.
func TestRealClientPathNoColon(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path // decoded path, as Azure's front end would validate
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"workItems":[]}`)) // empty -> no batch fetch
	}))
	defer srv.Close()

	c := New()
	ds := connector.Dataset{Source: "work_items", Options: map[string]any{
		"organization": "https://dev.azure.com/contoso", // a URL, not a slug
		"project":      "My Project",
		"pat":          "x",
		"url":          srv.URL,
	}}
	it, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	it.Close()

	if strings.Contains(gotPath, ":") {
		t.Errorf("request path contains a colon (would 400): %q", gotPath)
	}
	if gotPath != "/contoso/My Project/_apis/wit/wiql" {
		t.Errorf("path = %q, want /contoso/My Project/_apis/wit/wiql", gotPath)
	}
}

// TestWIQLTopCap verifies the WIQL request always carries $top: capped at the
// hard limit by default (so a >20000-item project doesn't fail the query), and
// lowered to the engine's LIMIT when that can be pushed.
func TestWIQLTopCap(t *testing.T) {
	var gotTop string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTop = r.URL.Query().Get("$top")
		w.Write([]byte(`{"workItems":[]}`))
	}))
	defer srv.Close()

	opts := map[string]any{"organization": "org", "project": "proj", "pat": "x", "url": srv.URL}
	ds := connector.Dataset{Source: "work_items", Options: opts}

	// No engine limit -> capped at the WIQL hard max.
	it, err := New().Scan(context.Background(), connector.ScanRequest{Dataset: ds})
	if err != nil {
		t.Fatal(err)
	}
	it.Close()
	if gotTop != "20000" {
		t.Errorf("default $top = %q, want 20000", gotTop)
	}

	// A pushed LIMIT lowers $top so only that many ids are requested.
	five := 5
	it, err = New().Scan(context.Background(), connector.ScanRequest{Dataset: ds, Limit: &five})
	if err != nil {
		t.Fatal(err)
	}
	it.Close()
	if gotTop != "5" {
		t.Errorf("$top with LIMIT 5 = %q, want 5", gotTop)
	}
}

func TestMissingAuth(t *testing.T) {
	c := New() // no injected client; real build requires org/project/pat
	if _, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds(map[string]any{"organization": "acme"})}); err == nil {
		t.Fatal("expected error when project/pat are missing")
	}
}
