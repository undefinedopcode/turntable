package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestSlugify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Station Overview", "station-overview"},
		{"  North -- District!  ", "north-district"},
		{"UPPER", "upper"},
		{"日本語", "dashboard"},
		{"", "dashboard"},
		{"a", "a"},
		{"trailing---", "trailing"},
	}
	for _, c := range cases {
		if got := slugify(c.in); got != c.want {
			t.Errorf("slugify(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func dashApp(t *testing.T) *App {
	t.Helper()
	a := NewApp()
	a.dashDir = t.TempDir()
	return a
}

func postDashboard(t *testing.T, a *App, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/dashboards", strings.NewReader(body))
	rec := httptest.NewRecorder()
	a.handleDashboards(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body: %s)", err, rec.Body.String())
	}
	return resp
}

func TestDashboardRoundTrip(t *testing.T) {
	a := dashApp(t)

	resp := postDashboard(t, a, `{
		"name": "Station Overview",
		"description": "flow analysis",
		"variables": {"station": {"default": "N-04", "options_query": "SELECT DISTINCT s FROM x"}},
		"panels": [
			{"kind": "markdown", "text": "## Hello"},
			{"kind": "chart", "title": "Flow", "query": "SELECT 1 AS n",
			 "view": {"chart": {"type": "line", "x": "t", "y": ["flow"]}}, "width": "half"}
		]
	}`)
	if resp["error"] != nil {
		t.Fatalf("save error: %v", resp["error"])
	}
	if resp["slug"] != "station-overview" {
		t.Fatalf("slug = %v, want station-overview", resp["slug"])
	}

	// The YAML on disk is readable and carries the fields.
	data, err := os.ReadFile(a.dashPath("station-overview"))
	if err != nil {
		t.Fatalf("read yaml: %v", err)
	}
	for _, want := range []string{"name: Station Overview", "kind: markdown", "options_query:"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("yaml missing %q:\n%s", want, data)
		}
	}

	// GET one.
	req := httptest.NewRequest(http.MethodGet, "/api/dashboards/station-overview", nil)
	rec := httptest.NewRecorder()
	a.handleDashboardItem(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d (%s)", rec.Code, rec.Body.String())
	}
	var got struct {
		Slug string `json:"slug"`
		Dashboard
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if got.Slug != "station-overview" || got.Name != "Station Overview" || len(got.Panels) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got.Panels[1].View == nil || got.Panels[1].View["chart"] == nil {
		t.Errorf("panel view lost in round-trip: %+v", got.Panels[1].View)
	}
	if got.Variables["station"].Default != "N-04" {
		t.Errorf("variables lost: %+v", got.Variables)
	}

	// List.
	req = httptest.NewRequest(http.MethodGet, "/api/dashboards", nil)
	rec = httptest.NewRecorder()
	a.handleDashboards(rec, req)
	var list []dashboardSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v (%s)", err, rec.Body.String())
	}
	if len(list) != 1 || list[0].Slug != "station-overview" || list[0].Panels != 2 {
		t.Fatalf("list = %+v", list)
	}

	// Update by slug (append a panel).
	got.Panels = append(got.Panels, DashboardPanel{Kind: "table", Query: "SELECT 2"})
	body, _ := json.Marshal(struct {
		Slug string `json:"slug"`
		Dashboard
	}{got.Slug, got.Dashboard})
	resp = postDashboard(t, a, string(body))
	if resp["error"] != nil {
		t.Fatalf("update error: %v", resp["error"])
	}
	d, err := a.loadDashboard("station-overview")
	if err != nil || len(d.Panels) != 3 {
		t.Fatalf("after update: %v panels=%d", err, len(d.Panels))
	}

	// DELETE.
	req = httptest.NewRequest(http.MethodDelete, "/api/dashboards/station-overview", nil)
	rec = httptest.NewRecorder()
	a.handleDashboardItem(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d", rec.Code)
	}
	if _, err := os.Stat(a.dashPath("station-overview")); !os.IsNotExist(err) {
		t.Fatalf("file still exists after delete")
	}
}

func TestDashboardValidation(t *testing.T) {
	a := dashApp(t)
	cases := []struct{ body, wantErr string }{
		{`{"name":"","panels":[]}`, "name is required"},
		{`{"name":"x","panels":[{"kind":"bogus"}]}`, "unknown kind"},
		{`{"name":"x","panels":[{"kind":"chart"}]}`, "needs a query"},
		{`{"name":"x","panels":[{"kind":"markdown"}]}`, "needs text"},
		{`{"name":"x","panels":[{"kind":"table","query":"SELECT 1","width":"third"}]}`, "width"},
		{`{"slug":"../evil","name":"x","panels":[]}`, "bad dashboard slug"},
	}
	for _, c := range cases {
		resp := postDashboard(t, a, c.body)
		errMsg, _ := resp["error"].(string)
		if !strings.Contains(errMsg, c.wantErr) {
			t.Errorf("body %s: error = %q, want contains %q", c.body, errMsg, c.wantErr)
		}
	}
}

func TestDashboardSlugTraversal(t *testing.T) {
	a := dashApp(t)
	for _, path := range []string{
		"/api/dashboards/../secret",
		"/api/dashboards/a.b",
		"/api/dashboards/A",
		"/api/dashboards/",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		a.handleDashboardItem(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want 400", path, rec.Code)
		}
	}
	// Unknown-but-valid slug is a 404, not a 400.
	req := httptest.NewRequest(http.MethodGet, "/api/dashboards/nope", nil)
	rec := httptest.NewRecorder()
	a.handleDashboardItem(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET unknown status = %d, want 404", rec.Code)
	}
}

func TestDashboardListEmptyAndBadFile(t *testing.T) {
	a := dashApp(t)
	// Missing dir (fresh App pointing at a non-existent path) lists empty.
	a.dashDir = a.dashDir + "/does-not-exist"
	list, err := a.listDashboards()
	if err != nil || len(list) != 0 {
		t.Fatalf("empty list: %v %v", list, err)
	}
	// A malformed YAML file is listed with its error, not hidden.
	a = dashApp(t)
	if err := os.WriteFile(a.dashPath("broken"), []byte("::not yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	list, err = a.listDashboards()
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, err %v", list, err)
	}
	if list[0].Slug != "broken" || list[0].Error == "" {
		t.Errorf("broken entry = %+v, want error set", list[0])
	}
}
