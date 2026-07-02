package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/connector/connectors/csvc"
)

// newMCPSession connects an in-process MCP client to the App's server over an
// in-memory transport, with a small CSV source registered as "widgets".
func newMCPSession(t *testing.T) *mcp.ClientSession {
	t.Helper()
	a := NewApp()
	dir := t.TempDir()
	p := filepath.Join(dir, "widgets.csv")
	if err := os.WriteFile(p, []byte("region,amount\nemea,10\namer,20\nemea,30\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Reg.RegisterSource("widgets", csvc.New(), connector.Dataset{Name: "widgets", Source: p}); err != nil {
		t.Fatal(err)
	}
	a.dashDir = filepath.Join(dir, "dashboards") // keep dashboard tools off the real store

	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := a.newMCPServer().Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// callTool invokes a tool and decodes the JSON text content into out (left
// zero for IsError results, whose text is the error message).
func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	if !res.IsError && out != nil {
		text := textContent(t, res)
		if err := json.Unmarshal([]byte(text), out); err != nil {
			t.Fatalf("decode %s result: %v (text: %s)", name, err, text)
		}
	}
	return res
}

func regexpFind(s, pattern string) string {
	return regexp.MustCompile(pattern).FindString(s)
}

func textContent(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("result has no content")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want TextContent", res.Content[0])
	}
	return tc.Text
}

func TestMCPQuery(t *testing.T) {
	cs := newMCPSession(t)
	var out mcpQueryOut
	res := callTool(t, cs, "query", map[string]any{
		"sql": "SELECT region, SUM(amount) AS total FROM widgets GROUP BY region ORDER BY region",
	}, &out)
	if res.IsError {
		t.Fatalf("unexpected error: %s", textContent(t, res))
	}
	if len(out.Columns) != 2 || out.Columns[0].Name != "region" || out.Columns[1].Name != "total" {
		t.Fatalf("columns = %+v", out.Columns)
	}
	if out.Count != 2 || len(out.Rows) != 2 {
		t.Fatalf("count = %d, rows = %d", out.Count, len(out.Rows))
	}
	if out.Rows[1][0] != "emea" || out.Rows[1][1] != float64(40) {
		t.Errorf("rows = %v, want emea total 40", out.Rows)
	}
	if out.Truncated {
		t.Error("small result reported truncated")
	}
}

func TestMCPQueryParseError(t *testing.T) {
	cs := newMCPSession(t)
	res := callTool(t, cs, "query", map[string]any{"sql": "SELECT FROM WHERE"}, nil)
	if !res.IsError {
		t.Fatal("expected IsError for malformed SQL")
	}
	if text := textContent(t, res); !strings.Contains(text, "parse") {
		t.Errorf("error = %q, want a parse error", text)
	}
}

func TestMCPQueryTruncation(t *testing.T) {
	cs := newMCPSession(t)
	var out mcpQueryOut
	res := callTool(t, cs, "query", map[string]any{
		"sql":      "SELECT * FROM widgets",
		"max_rows": 2,
	}, &out)
	if res.IsError {
		t.Fatalf("unexpected error: %s", textContent(t, res))
	}
	if out.Count != 2 || !out.Truncated {
		t.Fatalf("count = %d truncated = %v, want 2 rows truncated", out.Count, out.Truncated)
	}
	if out.Hint == "" {
		t.Error("truncated result should carry a hint")
	}
}

func TestMCPQueryExplain(t *testing.T) {
	cs := newMCPSession(t)
	var out mcpQueryOut
	res := callTool(t, cs, "query", map[string]any{
		"sql":     "SELECT * FROM widgets WHERE amount > 15",
		"explain": true,
	}, &out)
	if res.IsError {
		t.Fatalf("unexpected error: %s", textContent(t, res))
	}
	if !strings.Contains(out.Explain, "Scan") {
		t.Errorf("explain = %q, want a plan mentioning Scan", out.Explain)
	}
	if len(out.Rows) != 0 {
		t.Error("explain should not return rows")
	}
}

func TestMCPQuerySessionStmt(t *testing.T) {
	cs := newMCPSession(t)
	var out mcpQueryOut
	res := callTool(t, cs, "query", map[string]any{
		"sql": "CREATE VIEW emea AS SELECT * FROM widgets WHERE region = 'emea'",
	}, &out)
	if res.IsError {
		t.Fatalf("create view error: %s", textContent(t, res))
	}
	if out.Notice == "" {
		t.Fatal("expected a notice from CREATE VIEW")
	}

	var q mcpQueryOut
	res = callTool(t, cs, "query", map[string]any{"sql": "SELECT COUNT(*) AS n FROM emea"}, &q)
	if res.IsError {
		t.Fatalf("query view error: %s", textContent(t, res))
	}
	if q.Count != 1 || q.Rows[0][0] != float64(2) {
		t.Fatalf("view rows = %v, want one row with n=2", q.Rows)
	}

	var sources mcpSourcesOut
	callTool(t, cs, "list_sources", nil, &sources)
	found := false
	for _, s := range sources.Sources {
		if s.Name == "emea" && s.Connector == "view" {
			found = true
		}
	}
	if !found {
		t.Errorf("list_sources = %+v, want emea tagged view", sources.Sources)
	}
}

func TestMCPListSources(t *testing.T) {
	cs := newMCPSession(t)
	var out mcpSourcesOut
	res := callTool(t, cs, "list_sources", nil, &out)
	if res.IsError {
		t.Fatalf("unexpected error: %s", textContent(t, res))
	}
	found := false
	for _, s := range out.Sources {
		if s.Name == "widgets" && s.Connector == "csv" {
			found = true
		}
	}
	if !found {
		t.Errorf("sources = %+v, want widgets/csv", out.Sources)
	}
}

func TestMCPDescribeSource(t *testing.T) {
	cs := newMCPSession(t)
	var out mcpDescribeOut
	res := callTool(t, cs, "describe_source", map[string]any{"name": "widgets"}, &out)
	if res.IsError {
		t.Fatalf("unexpected error: %s", textContent(t, res))
	}
	if out.Source != "widgets" || len(out.Columns) != 2 {
		t.Fatalf("describe = %+v", out)
	}
	if out.Columns[0].Name != "region" || out.Columns[1].Name != "amount" {
		t.Errorf("columns = %+v", out.Columns)
	}
	if out.Path == "" || out.Size == 0 || out.Modified == "" {
		t.Errorf("file source should report path/modified/size, got %+v", out)
	}
}

func TestMCPDescribeSourceUnknown(t *testing.T) {
	cs := newMCPSession(t)
	res := callTool(t, cs, "describe_source", map[string]any{"name": "nope"}, nil)
	if !res.IsError {
		t.Fatal("expected IsError for unknown source")
	}
	if text := textContent(t, res); !strings.Contains(text, "unknown source") {
		t.Errorf("error = %q, want unknown source", text)
	}
}

func TestMCPListConnectors(t *testing.T) {
	cs := newMCPSession(t)
	var out mcpConnectorsOut
	res := callTool(t, cs, "list_connectors", nil, &out)
	if res.IsError {
		t.Fatalf("unexpected error: %s", textContent(t, res))
	}
	if len(out.Connectors) < 20 {
		t.Fatalf("connectors = %d, want the full spec table", len(out.Connectors))
	}
	var hasCSV, hasPlugin bool
	for _, c := range out.Connectors {
		if c.Name == "csv" && c.File {
			hasCSV = true
		}
		if c.Name == "plugin" {
			hasPlugin = true
		}
		if c.Name == "sql" {
			var dsn *FieldSpec
			for i := range c.Fields {
				if c.Fields[i].Key == "dsn" {
					dsn = &c.Fields[i]
				}
			}
			if dsn == nil || !dsn.Sensitive {
				t.Error("sql dsn field must be marked sensitive")
			}
		}
	}
	if !hasCSV {
		t.Error("csv must be listed as a file connector")
	}
	if hasPlugin {
		t.Error("plugin must not be offered for runtime adds")
	}
}

func TestMCPAddSourceAndQuery(t *testing.T) {
	cs := newMCPSession(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "extra.csv")
	if err := os.WriteFile(p, []byte("k,v\na,1\nb,2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out mcpAddSourceOut
	res := callTool(t, cs, "add_source", map[string]any{
		"name": "extra", "connector": "csv", "fields": map[string]any{"path": p},
	}, &out)
	if res.IsError {
		t.Fatalf("add_source error: %s", textContent(t, res))
	}
	if len(out.Registered) != 1 || out.Registered[0] != "extra" {
		t.Fatalf("registered = %v", out.Registered)
	}

	var q mcpQueryOut
	res = callTool(t, cs, "query", map[string]any{"sql": "SELECT COUNT(*) AS n FROM extra"}, &q)
	if res.IsError {
		t.Fatalf("query error: %s", textContent(t, res))
	}
	if q.Rows[0][0] != float64(2) {
		t.Fatalf("rows = %v, want n=2", q.Rows)
	}

	var rm mcpRemoveSourceOut
	res = callTool(t, cs, "remove_source", map[string]any{"name": "extra"}, &rm)
	if res.IsError {
		t.Fatalf("remove_source error: %s", textContent(t, res))
	}
	res = callTool(t, cs, "query", map[string]any{"sql": "SELECT * FROM extra"}, nil)
	if !res.IsError {
		t.Fatal("query after remove_source should fail")
	}
}

func TestMCPAddSourceRejectsPlugin(t *testing.T) {
	cs := newMCPSession(t)
	res := callTool(t, cs, "add_source", map[string]any{
		"name": "evil", "connector": "plugin", "fields": map[string]any{"command": "rm -rf /"},
	}, nil)
	if !res.IsError {
		t.Fatal("plugin connector must be rejected")
	}
	if text := textContent(t, res); !strings.Contains(text, "plugin") {
		t.Errorf("error = %q, want a plugin explanation", text)
	}
}

func TestMCPAddSourceRejectsLiteralSecret(t *testing.T) {
	cs := newMCPSession(t)
	res := callTool(t, cs, "add_source", map[string]any{
		"name": "db", "connector": "sql",
		"fields": map[string]any{"driver": "postgres", "dsn": "postgres://user:hunter2@host/db"},
	}, nil)
	if !res.IsError {
		t.Fatal("literal credential must be rejected")
	}
	if text := textContent(t, res); !strings.Contains(text, "${") {
		t.Errorf("error = %q, want a ${ENV_VAR} hint", text)
	}
}

func TestMCPRemoveSourceUnknown(t *testing.T) {
	cs := newMCPSession(t)
	res := callTool(t, cs, "remove_source", map[string]any{"name": "nope"}, nil)
	if !res.IsError {
		t.Fatal("expected IsError for unknown source")
	}
}

func TestMCPDashboardLifecycle(t *testing.T) {
	cs := newMCPSession(t)

	// Save: markdown + table + chart panels, a variable, slug derived from name.
	var saved mcpSaveDashboardOut
	res := callTool(t, cs, "save_dashboard", map[string]any{
		"name":        "Widget Report",
		"description": "regional totals",
		"variables":   map[string]any{"region": map[string]any{"default": "emea"}},
		"panels": []map[string]any{
			{"kind": "markdown", "text": "# Widgets\nTotals by *region*."},
			{"kind": "table", "title": "In {{region}}", "query": "SELECT * FROM widgets WHERE region = {{region}}"},
			{"kind": "chart", "title": "Totals", "query": "SELECT region, SUM(amount) AS total FROM widgets GROUP BY region",
				"view": map[string]any{"chart": map[string]any{"type": "bar", "x": "region", "y": []string{"total"}}}},
		},
	}, &saved)
	if res.IsError {
		t.Fatalf("save error: %s", textContent(t, res))
	}
	if saved.Slug != "widget-report" || saved.Saved == "" {
		t.Fatalf("saved = %+v", saved)
	}

	var list mcpDashboardsOut
	callTool(t, cs, "list_dashboards", nil, &list)
	if len(list.Dashboards) != 1 || list.Dashboards[0].Slug != "widget-report" || list.Dashboards[0].Panels != 3 {
		t.Fatalf("list = %+v", list.Dashboards)
	}

	var got mcpGetDashboardOut
	res = callTool(t, cs, "get_dashboard", map[string]any{"slug": "widget-report"}, &got)
	if res.IsError {
		t.Fatalf("get error: %s", textContent(t, res))
	}
	if got.Name != "Widget Report" || len(got.Panels) != 3 || got.Variables["region"].Default != "emea" {
		t.Fatalf("get = %+v", got)
	}

	// Render to a temp path: the report must be self-contained HTML with the
	// query results (and the {{region}} default substituted).
	outPath := filepath.Join(t.TempDir(), "report.html")
	var rendered mcpRenderDashboardOut
	res = callTool(t, cs, "render_dashboard", map[string]any{"slug": "widget-report", "out": outPath}, &rendered)
	if res.IsError {
		t.Fatalf("render error: %s", textContent(t, res))
	}
	html, err := os.ReadFile(rendered.Path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	for _, want := range []string{"Widget Report", "emea", "<canvas"} {
		if !strings.Contains(string(html), want) {
			t.Errorf("report missing %q", want)
		}
	}
	// Panels must be siblings, not nested: every opened section is closed
	// (the closing tag was once lost to a deferred write), and the {{region}}
	// in a panel title renders as the raw value.
	opens := strings.Count(string(html), "<section")
	closes := strings.Count(string(html), "</section>")
	if opens != 3 || closes != 3 {
		t.Errorf("sections open/close = %d/%d, want 3/3", opens, closes)
	}
	if !strings.Contains(string(html), "<h2>In emea</h2>") {
		t.Errorf("panel title should substitute {{region}} raw, got: %s",
			regexpFind(string(html), "<h2>[^<]*</h2>"))
	}

	var del mcpDeleteDashboardOut
	res = callTool(t, cs, "delete_dashboard", map[string]any{"slug": "widget-report"}, &del)
	if res.IsError {
		t.Fatalf("delete error: %s", textContent(t, res))
	}
	res = callTool(t, cs, "get_dashboard", map[string]any{"slug": "widget-report"}, nil)
	if !res.IsError {
		t.Fatal("get after delete should fail")
	}
}

func TestMCPSaveDashboardInvalid(t *testing.T) {
	cs := newMCPSession(t)
	res := callTool(t, cs, "save_dashboard", map[string]any{
		"name":   "bad",
		"panels": []map[string]any{{"kind": "sparkline", "query": "SELECT 1"}},
	}, nil)
	if !res.IsError {
		t.Fatal("unknown panel kind must be rejected")
	}
	if text := textContent(t, res); !strings.Contains(text, "unknown kind") {
		t.Errorf("error = %q, want unknown kind", text)
	}
}

func TestMCPRenderDashboardVariableOverride(t *testing.T) {
	cs := newMCPSession(t)
	var saved mcpSaveDashboardOut
	callTool(t, cs, "save_dashboard", map[string]any{
		"name":      "vars",
		"variables": map[string]any{"region": map[string]any{"default": "emea"}},
		"panels": []map[string]any{
			{"kind": "stat", "title": "count", "query": "SELECT COUNT(*) AS n FROM widgets WHERE region = {{region}}"},
		},
	}, &saved)
	outPath := filepath.Join(t.TempDir(), "v.html")
	var rendered mcpRenderDashboardOut
	res := callTool(t, cs, "render_dashboard", map[string]any{
		"slug": saved.Slug, "out": outPath,
		"variables": map[string]any{"region": "amer"},
	}, &rendered)
	if res.IsError {
		t.Fatalf("render error: %s", textContent(t, res))
	}
	html, _ := os.ReadFile(rendered.Path)
	if !strings.Contains(string(html), "region=amer") {
		t.Errorf("report should note the overridden variable, got: %.200s", html)
	}
}

func TestMCPListFunctions(t *testing.T) {
	cs := newMCPSession(t)
	var out mcpFunctionsOut
	res := callTool(t, cs, "list_functions", nil, &out)
	if res.IsError {
		t.Fatalf("unexpected error: %s", textContent(t, res))
	}
	if len(out.Scalar) == 0 || len(out.Aggregate) == 0 || len(out.Keywords) == 0 {
		t.Errorf("functions = %d scalar, %d aggregate, %d keywords — none may be empty",
			len(out.Scalar), len(out.Aggregate), len(out.Keywords))
	}
}
