package azdevopsc

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	tsql "github.com/april/turntable/internal/sql"
)

// fakeDevops simulates the Azure DevOps API over a canned set of work items
// (each map has an "id" plus fields). It honors the System.Id watermark in the
// WIQL so the connector's forward paging can be exercised, and records the
// WIQL queries / fields it was asked for.
type fakeDevops struct {
	items    []map[string]any
	wiqls    []string // every queryIDs query, in order
	lastTop  int
	lastFlds []string
}

func (f *fakeDevops) itemID(m map[string]any) int { return int(m["id"].(float64)) }

func (f *fakeDevops) queryIDs(ctx context.Context, wiql string, top int) ([]int, error) {
	f.wiqls = append(f.wiqls, wiql)
	f.lastTop = top
	watermark := parseWatermark(wiql) // -1 if the wiql has no "[System.Id] > N"
	var ids []int
	for _, m := range f.items {
		if id := f.itemID(m); watermark < 0 || id > watermark {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	if len(ids) > top {
		ids = ids[:top]
	}
	return ids, nil
}

func (f *fakeDevops) workItems(ctx context.Context, ids []int, flds []string) ([]map[string]any, error) {
	f.lastFlds = flds
	byID := map[int]map[string]any{}
	for _, m := range f.items {
		byID[f.itemID(m)] = m
	}
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		if m, ok := byID[id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// parseWatermark extracts N from a "[System.Id] > N" clause, or -1 if absent.
func parseWatermark(wiql string) int {
	const marker = "[System.Id] > "
	i := strings.Index(wiql, marker)
	if i < 0 {
		return -1
	}
	rest := wiql[i+len(marker):]
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	n, err := strconv.Atoi(rest[:j])
	if err != nil {
		return -1
	}
	return n
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
	want := []string{"id", "title", "work_item_type", "state", "assigned_to", "assigned_to_email", "area_path", "iteration_path", "tags", "priority", "created_date", "changed_date"}
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
	// cols: id(0) title(1) type(2) state(3) assigned_to(4) assigned_to_email(5)
	//       area(6) iter(7) tags(8) priority(9) created(10) changed(11)
	if n, _ := rows[0].Values[0].AsInt(); n != 42 {
		t.Errorf("id = %v, want 42", rows[0].Values[0].V)
	}
	if rows[0].Values[4].V != "Ada Lovelace" {
		t.Errorf("assigned_to = %v, want Ada Lovelace (nested displayName)", rows[0].Values[4].V)
	}
	if p, _ := rows[0].Values[9].AsInt(); p != 2 {
		t.Errorf("priority = %v, want 2", rows[0].Values[9].V)
	}
	if rows[0].Values[11].Type != engine.TypeTime {
		t.Errorf("changed_date should coerce to time, got %v", rows[0].Values[11].Type)
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
	// The default query filters by type, pages by ascending System.Id, and
	// starts from watermark 0.
	want := "SELECT [System.Id] FROM workitems WHERE [System.TeamProject] = @project AND [System.WorkItemType] = 'Bug' AND [System.Id] > 0 ORDER BY [System.Id] ASC"
	if fake.wiqls[0] != want {
		t.Errorf("wiql:\n got  %s\n want %s", fake.wiqls[0], want)
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
	// A custom query is run once, verbatim (no watermark injected).
	if len(fake.wiqls) != 1 || fake.wiqls[0] != custom {
		t.Errorf("queries = %v, want one verbatim %q", fake.wiqls, custom)
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
	// The pushed LIMIT becomes the page $top, so only 2 ids are requested.
	if fake.lastTop != 2 {
		t.Errorf("$top = %d, want 2", fake.lastTop)
	}
}

func TestTranslateWIQL(t *testing.T) {
	cases := []struct {
		expr string
		want string
		ok   bool
	}{
		{`assigned_to_email = 'me@x.com'`, "[System.AssignedTo] = 'me@x.com'", true},
		{`state = 'Active'`, "[System.State] = 'Active'", true},
		{`state = 'Active' AND priority <= 2`, "([System.State] = 'Active' AND [Microsoft.VSTS.Common.Priority] <= 2)", true},
		{`state IN ('Active', 'New')`, "[System.State] IN ('Active', 'New')", true},
		// AND with an untranslatable conjunct: the translatable side is kept
		// (still a superset, since the engine re-applies the full predicate).
		{`state = 'Active' AND LOWER(title) = 'x'`, "[System.State] = 'Active'", true},
		// OR is all-or-nothing; an untranslatable side voids the whole OR.
		{`state = 'Active' OR LOWER(title) = 'x'`, "", false},
		{`LOWER(title) = 'x'`, "", false}, // function not pushable
		{`unknown_col = 'x'`, "", false},  // unmapped column
		{`title LIKE 'a%'`, "", false},    // LIKE not pushed
	}
	for _, tc := range cases {
		e, err := tsql.ParseExpr(tc.expr)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.expr, err)
		}
		got, ok := translateWIQL(e)
		if ok != tc.ok {
			t.Errorf("translate %q: ok=%v, want %v", tc.expr, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("translate %q:\n got  %s\n want %s", tc.expr, got, tc.want)
		}
	}
}

// TestScanPushesPredicate verifies the SQL WHERE is injected into the WIQL so
// Azure filters server-side (the fix for hitting the 20000 cap on a filtered
// query). This is the user's reported case.
func TestScanPushesPredicate(t *testing.T) {
	fake := &fakeDevops{}
	c := newWithClient(fake)
	pred, err := tsql.ParseExpr(`assigned_to_email = 'me@company.com'`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Scan(context.Background(), connector.ScanRequest{Dataset: ds(nil), Predicate: pred})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fake.wiqls[0], "[System.AssignedTo] = 'me@company.com'") {
		t.Errorf("predicate not pushed into WIQL:\n%s", fake.wiqls[0])
	}
}

func TestScanPushesOrderBy(t *testing.T) {
	fake := &fakeDevops{}
	c := newWithClient(fake)
	// A fully-column ORDER BY hint -> a single ordered WIQL query (no System.Id
	// paging), with the SQL WHERE also pushed.
	pred, _ := tsql.ParseExpr(`state = 'Active'`)
	_, err := c.Scan(context.Background(), connector.ScanRequest{
		Dataset:   ds(nil),
		Predicate: pred,
		OrderBy:   []connector.OrderTerm{{Column: "changed_date", Desc: true}, {Column: "priority"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	q := fake.wiqls[0]
	if !strings.Contains(q, "ORDER BY [System.ChangedDate] DESC, [Microsoft.VSTS.Common.Priority] ASC") {
		t.Errorf("ORDER BY not pushed into WIQL:\n%s", q)
	}
	if strings.Contains(q, "[System.Id] >") {
		t.Errorf("ordered query should not page by System.Id:\n%s", q)
	}
	if !strings.Contains(q, "[System.State] = 'Active'") {
		t.Errorf("predicate should still be pushed:\n%s", q)
	}
}

func TestScanUnpushableOrderByFallsBackToPaging(t *testing.T) {
	fake := &fakeDevops{items: []map[string]any{{"id": float64(1)}, {"id": float64(2)}}}
	c := newWithClient(fake)
	// area_path is mappable, but an unmapped column voids the whole order hint;
	// simulate that with an unknown column -> connector falls back to id paging.
	_, err := c.Scan(context.Background(), connector.ScanRequest{
		Dataset: ds(nil),
		OrderBy: []connector.OrderTerm{{Column: "not_a_field"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fake.wiqls[0], "[System.Id] > 0 ORDER BY [System.Id] ASC") {
		t.Errorf("expected fallback to System.Id paging, got:\n%s", fake.wiqls[0])
	}
}

func TestPagingByWatermark(t *testing.T) {
	// Shrink the page size so a handful of items forces multiple pages.
	old := wiqlPageSize
	wiqlPageSize = 2
	defer func() { wiqlPageSize = old }()

	var items []map[string]any
	for _, id := range []int{10, 20, 30, 40, 50} {
		items = append(items, map[string]any{"id": float64(id), "System.Title": fmt.Sprintf("wi-%d", id)})
	}
	fake := &fakeDevops{items: items}
	c := newWithClient(fake)

	it, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds(nil)})
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, it)
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5 (paged across pages of 2)", len(rows))
	}
	// 5 items in pages of 2 -> queries at watermark 0, 20, 40 (last page short,
	// loop stops): at least 3 paged queries with ascending watermarks.
	if len(fake.wiqls) < 3 {
		t.Fatalf("queries = %d, want >= 3 (paging)", len(fake.wiqls))
	}
	if parseWatermark(fake.wiqls[0]) != 0 || parseWatermark(fake.wiqls[1]) != 20 {
		t.Errorf("watermarks = [%d, %d, ...], want [0, 20, ...]",
			parseWatermark(fake.wiqls[0]), parseWatermark(fake.wiqls[1]))
	}
	// Titles come back for all five (rows ordered by ascending id).
	if rows[0].Values[1].V != "wi-10" || rows[4].Values[1].V != "wi-50" {
		t.Errorf("row titles wrong: %v .. %v", rows[0].Values[1].V, rows[4].Values[1].V)
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

// TestWIQLTopCap verifies each WIQL request carries $top: the page size by
// default, and the engine's LIMIT when that is smaller (so a small LIMIT fetches
// just one short page). Paging — not $top — is what keeps under the 20000 match
// cap; this only checks $top is sent and sized correctly.
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

// TestRetriesOn429 verifies a throttled response is retried (honoring
// Retry-After) rather than surfaced as an error. The server 429s the first two
// requests with Retry-After: 0 (no real wait), then succeeds.
func TestRetriesOn429(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"workItems":[]}`))
	}))
	defer srv.Close()

	opts := map[string]any{"organization": "org", "project": "proj", "pat": "x", "url": srv.URL}
	it, err := New().Scan(context.Background(), connector.ScanRequest{Dataset: connector.Dataset{Source: "work_items", Options: opts}})
	if err != nil {
		t.Fatalf("scan should have retried through the 429s: %v", err)
	}
	it.Close()
	if calls != 3 {
		t.Errorf("server saw %d requests, want 3 (two throttled + one success)", calls)
	}
}

// TestRetriesExhausted verifies that a server which never stops throttling
// eventually surfaces the 429 rather than looping forever.
func TestRetriesExhausted(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	opts := map[string]any{"organization": "org", "project": "proj", "pat": "x", "url": srv.URL}
	_, err := New().Scan(context.Background(), connector.ScanRequest{Dataset: connector.Dataset{Source: "work_items", Options: opts}})
	if err == nil {
		t.Fatal("expected an error once retries are exhausted")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error = %v, want it to mention the 429 status", err)
	}
	if calls != maxRetries+1 {
		t.Errorf("server saw %d requests, want %d (initial + %d retries)", calls, maxRetries+1, maxRetries)
	}
}

func TestRetryDelay(t *testing.T) {
	mk := func(h string) *http.Response {
		r := &http.Response{Header: http.Header{}}
		if h != "" {
			r.Header.Set("Retry-After", h)
		}
		return r
	}
	// Delta-seconds Retry-After is honored.
	if got := retryDelay(mk("2"), 0); got != 2*time.Second {
		t.Errorf("Retry-After 2s = %v, want 2s", got)
	}
	// A value beyond the cap is clamped, not obeyed literally.
	if got := retryDelay(mk("9999"), 0); got != maxRetryDelay {
		t.Errorf("Retry-After 9999s = %v, want clamp to %v", got, maxRetryDelay)
	}
	// No header -> exponential backoff from a 2s base.
	if got := retryDelay(mk(""), 0); got != 2*time.Second {
		t.Errorf("backoff attempt 0 = %v, want 2s", got)
	}
	if got := retryDelay(mk(""), 2); got != 8*time.Second {
		t.Errorf("backoff attempt 2 = %v, want 8s", got)
	}
}

func TestMissingAuth(t *testing.T) {
	c := New() // no injected client; real build requires org/project/pat
	if _, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds(map[string]any{"organization": "acme"})}); err == nil {
		t.Fatal("expected error when project/pat are missing")
	}
}
