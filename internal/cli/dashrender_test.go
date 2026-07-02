package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDashboardRender(t *testing.T) {
	a := NewApp()
	a.dashDir = t.TempDir()

	d := &Dashboard{
		Name:        "Render Test",
		Description: "unit fixture",
		Variables:   map[string]DashboardVar{"n": {Default: "2"}},
		Panels: []DashboardPanel{
			{Kind: "markdown", Text: "# Hello\nSome **bold** text and `code`.\n- item <script>"},
			{Kind: "stat", Title: "The Number", Query: "SELECT 40 + {{n:raw}} AS answer"},
			{Kind: "table", Title: "Rows", Query: "SELECT 1 AS a, 'x' AS b UNION ALL SELECT 2, 'y'"},
			{Kind: "chart", Title: "Chart", Query: "SELECT 'a' AS k, 1 AS v UNION ALL SELECT 'b', 2",
				View: map[string]any{"chart": map[string]any{
					"type": "bar", "x": "k", "y": []any{"v"}, "thresholds": []any{1.5},
				}}},
			{Kind: "table", Title: "Broken", Query: "SELECT nope FROM nowhere"},
		},
	}
	if err := a.saveDashboard("render-test", d); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "report.html")
	if err := a.renderDashboard(context.Background(), "render-test", out, map[string]string{"n": "2"}); err != nil {
		t.Fatalf("render: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)

	for _, want := range []string{
		"<h1>Render Test</h1>",
		"<h1>Hello</h1>", "<b>bold</b>", "<code>code</code>",
		"&lt;script&gt;",                    // markdown text is escaped
		`<div class="stat-v">42</div>`,      // variable substituted
		"<td>x</td>",                        // table cell
		`data-chart="chart-3"`,              // chart payload embedded
		`"role":"threshold"`,                // threshold dataset generated
		`class="err"`,                       // broken panel renders inline
		"Chart.js v4",                       // chart.js bundle inlined
	} {
		if !strings.Contains(html, want) {
			t.Errorf("report missing %q", want)
		}
	}
	// The broken panel must not sink the report: the good panels are all there.
	if strings.Count(html, `<section class="panel`) != 5 {
		t.Errorf("panel count = %d, want 5", strings.Count(html, `<section class="panel`))
	}
}

func TestSubstituteVarsGo(t *testing.T) {
	vars := map[string]string{"station": "N-04's", "range": "24 hours"}
	got := substituteVars("WHERE s = {{station}} AND ts > NOW() - INTERVAL '{{range:raw}}' AND {{unknown}}", vars)
	want := "WHERE s = 'N-04''s' AND ts > NOW() - INTERVAL '24 hours' AND {{unknown}}"
	if got != want {
		t.Errorf("substituteVars = %q, want %q", got, want)
	}
}
