package cli

// Headless dashboard rendering: `turntable dashboard render <slug> [out.html]
// [name=value …]` executes every panel of a dashboard (variables resolved from
// their defaults, overridable with name=value args) and writes one
// self-contained HTML report — markdown/stat/table/pivot rendered server-side,
// charts via an embedded Chart.js bundle driven by each panel's frozen view
// config. The report needs no server and no network: a story becomes an
// emailable, archivable artifact. `turntable dashboard list` lists dashboards.

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/april/turntable/internal/engine"
)

//go:embed reportjs/chart.umd.js
var chartJS string

//go:embed reportjs/chartjs-adapter-date-fns.bundle.min.js
var chartAdapterJS string

// dashboardCmd dispatches the `dashboard` CLI subcommand.
func (a *App) dashboardCmd(ctx context.Context, args []string) int {
	if a.dashDir == "" {
		a.dashDir = dashDirPath
	}
	if len(args) == 0 {
		fmt.Fprintln(a.Err, "usage: turntable dashboard list | turntable dashboard render <slug> [out.html] [name=value …]")
		return 1
	}
	switch args[0] {
	case "list":
		list, err := a.listDashboards()
		if err != nil {
			fmt.Fprintf(a.Err, "list dashboards: %v\n", err)
			return 1
		}
		if len(list) == 0 {
			fmt.Fprintf(a.Err, "no dashboards in %s\n", a.dashDir)
			return 0
		}
		for _, d := range list {
			line := fmt.Sprintf("%-24s %s (%d panels)", d.Slug, d.Name, d.Panels)
			if d.Error != "" {
				line = fmt.Sprintf("%-24s ⚠ %s", d.Slug, d.Error)
			}
			fmt.Fprintln(a.Out, line)
		}
		return 0
	case "render":
		if len(args) < 2 {
			fmt.Fprintln(a.Err, "usage: turntable dashboard render <slug> [out.html] [name=value …]")
			return 1
		}
		slug := args[1]
		out := slug + ".html"
		vars := map[string]string{}
		for _, arg := range args[2:] {
			if k, v, ok := strings.Cut(arg, "="); ok {
				vars[k] = v
			} else {
				out = arg
			}
		}
		if err := a.renderDashboard(ctx, slug, out, vars); err != nil {
			fmt.Fprintf(a.Err, "render dashboard: %v\n", err)
			return 1
		}
		fmt.Fprintf(a.Err, "wrote %s\n", out)
		return 0
	}
	fmt.Fprintf(a.Err, "unknown dashboard subcommand %q (want list or render)\n", args[0])
	return 1
}

// substituteVars mirrors the web UI's substitution: {{name}} becomes a quoted
// SQL string literal (' doubled), {{name:raw}} the raw value. Unknown
// variables are left as-is so the mistake stays visible in the query error.
var varRef = regexp.MustCompile(`\{\{\s*(\w+)(:raw)?\s*\}\}`)

func substituteVars(q string, vars map[string]string) string {
	return varRef.ReplaceAllStringFunc(q, func(m string) string {
		g := varRef.FindStringSubmatch(m)
		v, ok := vars[g[1]]
		if !ok {
			return m
		}
		if g[2] == ":raw" {
			return v
		}
		return "'" + strings.ReplaceAll(v, "'", "''") + "'"
	})
}

// substituteVarsRaw replaces {{var}} references with the raw value (no SQL
// quoting) — for panel titles, which are prose, not queries.
func substituteVarsRaw(s string, vars map[string]string) string {
	return varRef.ReplaceAllStringFunc(s, func(m string) string {
		g := varRef.FindStringSubmatch(m)
		if v, ok := vars[g[1]]; ok {
			return v
		}
		return m
	})
}

func (a *App) renderDashboard(ctx context.Context, slug, outPath string, overrides map[string]string) error {
	d, err := a.loadDashboard(slug)
	if err != nil {
		return err
	}
	vars := map[string]string{}
	for k, v := range d.Variables {
		vars[k] = v.Default
	}
	for k, v := range overrides {
		vars[k] = v
	}

	var body strings.Builder
	var chartPayloads []string
	for i, p := range d.Panels {
		html, payload := a.renderPanel(ctx, i, p, vars)
		body.WriteString(html)
		if payload != "" {
			chartPayloads = append(chartPayloads, payload)
		}
	}

	page := reportPage{
		Title:       d.Name,
		Description: d.Description,
		Generated:   time.Now().Format("2006-01-02 15:04 MST"),
		Vars:        varSummary(vars),
		Body:        template.HTML(body.String()),
	}
	if len(chartPayloads) > 0 {
		page.ChartJS = template.JS(chartJS)
		page.AdapterJS = template.JS(chartAdapterJS)
		page.ReportJS = template.JS(reportJS)
	}

	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return reportTmpl.Execute(f, page)
}

func varSummary(vars map[string]string) string {
	if len(vars) == 0 {
		return ""
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + vars[k]
	}
	return strings.Join(parts, "  ")
}

// renderPanel renders one panel to an HTML fragment (plus, for a chart panel,
// its JSON payload embedded alongside). Query failures render inline as an
// error block — one broken panel never sinks the report.
func (a *App) renderPanel(ctx context.Context, idx int, p DashboardPanel, vars map[string]string) (string, string) {
	width := "full"
	if p.Width == "half" {
		width = "half"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<section class="panel %s">`, width)
	// NB: written explicitly, not deferred — a deferred write would run after
	// the return value's b.String() is taken and the closing tag would be lost,
	// nesting every panel inside the previous one.
	payload := a.renderPanelBody(&b, ctx, idx, p, vars)
	b.WriteString("</section>\n")
	return b.String(), payload
}

// renderPanelBody writes a panel's inner HTML and returns the chart payload
// (empty for non-chart panels).
func (a *App) renderPanelBody(b *strings.Builder, ctx context.Context, idx int, p DashboardPanel, vars map[string]string) string {
	if p.Kind == "markdown" {
		b.WriteString(`<div class="md">` + renderMarkdown(p.Text) + `</div>`)
		return ""
	}

	if p.Title != "" {
		// Titles take {{var}} substitution too — as the raw value, not a quoted
		// SQL literal ("Revenue by region (paid)").
		fmt.Fprintf(b, `<h2>%s</h2>`, template.HTMLEscapeString(substituteVarsRaw(p.Title, vars)))
	}
	schema, rows, truncated, err := a.execQuery(ctx, substituteVars(p.Query, vars))
	if err != nil {
		fmt.Fprintf(b, `<div class="err">%s</div>`, template.HTMLEscapeString(err.Error()))
		return ""
	}
	meta := fmt.Sprintf("%d rows", len(rows))
	if truncated {
		meta += " (truncated)"
	}

	payload := ""
	switch p.Kind {
	case "stat":
		b.WriteString(renderStat(schema, rows))
	case "pivot":
		b.WriteString(renderPivot(schema, rows, subView(p.View, "pivot")))
	case "chart":
		if pl, ok := buildChartPayload(schema, rows, subView(p.View, "chart")); ok {
			payload = pl
			fmt.Fprintf(b, `<div class="chartbox"><canvas id="chart-%d"></canvas></div>`, idx)
			fmt.Fprintf(b, `<script type="application/json" data-chart="chart-%d">%s</script>`, idx, payload)
		} else {
			// Chart types the report cannot draw (heatmap/graph/…) fall back
			// to the data itself.
			b.WriteString(`<div class="note">chart type not supported in reports — showing rows</div>`)
			b.WriteString(renderTable(schema, rows))
		}
	default: // table
		b.WriteString(renderTable(schema, rows))
	}
	fmt.Fprintf(b, `<div class="meta">%s</div>`, template.HTMLEscapeString(meta))
	return payload
}

// subView extracts a panel view sub-config (view.chart / view.pivot) as a map.
func subView(view map[string]any, key string) map[string]any {
	if view == nil {
		return nil
	}
	m, _ := view[key].(map[string]any)
	return m
}

// ---- value formatting ----------------------------------------------------------

func cellString(v engine.Value) string {
	if v.IsNull() {
		return ""
	}
	if t, ok := v.V.(time.Time); ok {
		return t.UTC().Format(time.RFC3339)
	}
	return v.AsString()
}

func cellFloat(v engine.Value) (float64, bool) {
	if v.IsNull() {
		return 0, false
	}
	return v.AsFloat()
}

// ---- stat / table / pivot ------------------------------------------------------

func renderStat(schema engine.Schema, rows []engine.Row) string {
	value, label := "—", ""
	if len(schema.Columns) > 0 {
		label = schema.Columns[0].Name
	}
	if len(rows) > 0 && len(rows[0].Values) > 0 {
		value = cellString(rows[0].Values[0])
	}
	return fmt.Sprintf(`<div class="stat"><div class="stat-v">%s</div><div class="stat-l">%s</div></div>`,
		template.HTMLEscapeString(value), template.HTMLEscapeString(label))
}

const reportRowCap = 200

func renderTable(schema engine.Schema, rows []engine.Row) string {
	var b strings.Builder
	b.WriteString(`<div class="tblwrap"><table><thead><tr>`)
	for _, c := range schema.Columns {
		fmt.Fprintf(&b, `<th>%s</th>`, template.HTMLEscapeString(c.Name))
	}
	b.WriteString(`</tr></thead><tbody>`)
	n := len(rows)
	if n > reportRowCap {
		n = reportRowCap
	}
	for _, r := range rows[:n] {
		b.WriteString("<tr>")
		for i, v := range r.Values {
			cls := ""
			if i < len(schema.Columns) &&
				(schema.Columns[i].Type == engine.TypeInt || schema.Columns[i].Type == engine.TypeFloat) {
				cls = ` class="num"`
			}
			fmt.Fprintf(&b, `<td%s>%s</td>`, cls, template.HTMLEscapeString(cellString(v)))
		}
		b.WriteString("</tr>")
	}
	b.WriteString(`</tbody></table></div>`)
	if len(rows) > reportRowCap {
		fmt.Fprintf(&b, `<div class="note">showing first %d of %d rows</div>`, reportRowCap, len(rows))
	}
	return b.String()
}

// renderPivot is the Go port of the web UI's pivot(): row dim × column dim,
// one aggregated measure per cell, optional heat colouring.
func renderPivot(schema engine.Schema, rows []engine.Row, view map[string]any) string {
	colIdx := func(name string, fallback int) int {
		for i, c := range schema.Columns {
			if strings.EqualFold(c.Name, name) {
				return i
			}
		}
		return fallback
	}
	rd := colIdx(viewStr(view, "rows"), 0)
	cd := colIdx(viewStr(view, "cols"), 1)
	if cd == rd {
		cd = (rd + 1) % max(len(schema.Columns), 1)
	}
	val := colIdx(viewStr(view, "value"), -1)
	if val < 0 {
		for i, c := range schema.Columns {
			if c.Type == engine.TypeInt || c.Type == engine.TypeFloat {
				val = i
				break
			}
		}
	}
	agg := viewStr(view, "agg")
	if agg == "" {
		agg = "sum"
	}
	color := view == nil || view["color"] != false
	if len(schema.Columns) < 2 || val < 0 || rd >= len(schema.Columns) || cd >= len(schema.Columns) {
		return `<div class="note">pivot needs two columns and a numeric measure</div>`
	}

	const colCap, rowCap = 60, 300
	var colKeys, rowKeys []string
	colSeen := map[string]int{}
	rowSeen := map[string]int{}
	cells := map[[2]int][]float64{}
	for _, r := range rows {
		ck := cellString(r.Values[cd])
		rk := cellString(r.Values[rd])
		ci, ok := colSeen[ck]
		if !ok {
			ci = len(colKeys)
			colSeen[ck] = ci
			colKeys = append(colKeys, ck)
		}
		ri, ok := rowSeen[rk]
		if !ok {
			ri = len(rowKeys)
			rowSeen[rk] = ri
			rowKeys = append(rowKeys, rk)
		}
		if ci >= colCap || ri >= rowCap {
			continue
		}
		if f, ok := cellFloat(r.Values[val]); ok {
			cells[[2]int{ri, ci}] = append(cells[[2]int{ri, ci}], f)
		} else if agg == "count" {
			cells[[2]int{ri, ci}] = append(cells[[2]int{ri, ci}], 0)
		}
	}
	if len(colKeys) > colCap {
		colKeys = colKeys[:colCap]
	}
	if len(rowKeys) > rowCap {
		rowKeys = rowKeys[:rowCap]
	}

	reduce := func(nums []float64) (float64, bool) {
		if len(nums) == 0 {
			return 0, false
		}
		switch agg {
		case "count":
			return float64(len(nums)), true
		case "avg":
			s := 0.0
			for _, f := range nums {
				s += f
			}
			return s / float64(len(nums)), true
		case "min":
			m := nums[0]
			for _, f := range nums[1:] {
				m = math.Min(m, f)
			}
			return m, true
		case "max":
			m := nums[0]
			for _, f := range nums[1:] {
				m = math.Max(m, f)
			}
			return m, true
		default: // sum
			s := 0.0
			for _, f := range nums {
				s += f
			}
			return s, true
		}
	}

	// Two passes: bounds for the heat scale, then the table.
	lo, hi := math.Inf(1), math.Inf(-1)
	vals := make(map[[2]int]float64)
	for k, nums := range cells {
		if v, ok := reduce(nums); ok {
			vals[k] = v
			lo = math.Min(lo, v)
			hi = math.Max(hi, v)
		}
	}

	var b strings.Builder
	b.WriteString(`<div class="tblwrap"><table><thead><tr><th class="corner"></th>`)
	for _, ck := range colKeys {
		fmt.Fprintf(&b, `<th>%s</th>`, template.HTMLEscapeString(ck))
	}
	b.WriteString(`</tr></thead><tbody>`)
	for ri, rk := range rowKeys {
		fmt.Fprintf(&b, `<tr><th>%s</th>`, template.HTMLEscapeString(rk))
		for ci := range colKeys {
			v, ok := vals[[2]int{ri, ci}]
			if !ok {
				b.WriteString(`<td class="num"></td>`)
				continue
			}
			style := ""
			if color && hi > lo {
				style = fmt.Sprintf(` style="background:%s"`, heatColor((v-lo)/(hi-lo)))
			}
			fmt.Fprintf(&b, `<td class="num"%s>%s</td>`, style, template.HTMLEscapeString(fmtNum(v)))
		}
		b.WriteString("</tr>")
	}
	b.WriteString(`</tbody></table></div>`)
	return b.String()
}

func fmtNum(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%.2f", v)
}

// heatColor maps t in [0,1] to the web UI's blue→accent→amber gradient.
func heatColor(t float64) string {
	stops := [3][3]float64{{28, 42, 74}, {110, 168, 254}, {224, 179, 65}}
	seg, f := 0, t/0.5
	if t > 0.5 {
		seg, f = 1, (t-0.5)/0.5
	}
	a, bb := stops[seg], stops[seg+1]
	return fmt.Sprintf("rgba(%d, %d, %d, 0.5)",
		int(a[0]+(bb[0]-a[0])*f), int(a[1]+(bb[1]-a[1])*f), int(a[2]+(bb[2]-a[2])*f))
}

func viewStr(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func viewStrs(m map[string]any, key string) []string {
	if m == nil {
		return nil
	}
	raw, _ := m[key].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// ---- chart payloads --------------------------------------------------------------

// chartDS is one dataset of a report chart. Role steers the report JS's
// styling: series (palette colour, honoring Axis), band_lo/band_hi (the
// translucent envelope), threshold (dashed red reference line).
type chartDS struct {
	Label string `json:"label"`
	Data  []any  `json:"data"`
	Axis  string `json:"axis,omitempty"`
	Role  string `json:"role"`
}

type chartPayload struct {
	Kind     string    `json:"kind"` // line|area|bar|pie|scatter
	TimeX    bool      `json:"timeX"`
	Labels   []string  `json:"labels,omitempty"`
	Datasets []chartDS `json:"datasets"`
	DualAxis bool      `json:"dualAxis,omitempty"`
}

// buildChartPayload turns a panel result + its frozen chart view config into
// the JSON the report's inline JS feeds to Chart.js. Row data is capped like
// the web chart. ok=false for chart types the report cannot draw.
func buildChartPayload(schema engine.Schema, rows []engine.Row, view map[string]any) (string, bool) {
	kind := viewStr(view, "type")
	if kind == "" {
		kind = "bar"
	}
	switch kind {
	case "line", "area", "bar", "pie", "scatter":
	default:
		return "", false
	}
	const rowCapChart = 2000
	if len(rows) > rowCapChart {
		rows = rows[:rowCapChart]
	}

	colIdx := func(name string) int {
		for i, c := range schema.Columns {
			if strings.EqualFold(c.Name, name) {
				return i
			}
		}
		return -1
	}
	numericCols := func() []int {
		var out []int
		for i, c := range schema.Columns {
			if c.Type == engine.TypeInt || c.Type == engine.TypeFloat {
				out = append(out, i)
			}
		}
		return out
	}()

	x := colIdx(viewStr(view, "x"))
	if x < 0 {
		x = 0
	}
	pickCols := func(names []string) []int {
		var out []int
		for _, n := range names {
			if i := colIdx(n); i >= 0 && i != x {
				out = append(out, i)
			}
		}
		return out
	}
	ys := pickCols(viewStrs(view, "y"))
	y2s := pickCols(viewStrs(view, "y2"))
	if len(ys) == 0 && len(y2s) == 0 {
		for _, i := range numericCols {
			if i != x {
				ys = []int{i}
				break
			}
		}
	}
	if len(ys) == 0 && len(y2s) == 0 {
		return "", false
	}

	timeX := (kind == "line" || kind == "area" || kind == "scatter") &&
		x < len(schema.Columns) && schema.Columns[x].Type == engine.TypeTime

	pl := chartPayload{Kind: kind, TimeX: timeX, DualAxis: len(y2s) > 0}
	if !timeX {
		pl.Labels = make([]string, len(rows))
		for i, r := range rows {
			pl.Labels[i] = cellString(r.Values[x])
		}
	}

	num := func(v engine.Value) any {
		if f, ok := cellFloat(v); ok {
			return f
		}
		return nil
	}
	xVal := func(v engine.Value) (any, bool) {
		if timeX {
			if t, ok := v.V.(time.Time); ok {
				return t.UnixMilli(), true
			}
			return nil, false
		}
		return num(v), true
	}
	seriesData := func(col int) []any {
		out := make([]any, 0, len(rows))
		for _, r := range rows {
			y := num(r.Values[col])
			if timeX || kind == "scatter" {
				xv, ok := xVal(r.Values[x])
				if !ok || y == nil {
					continue
				}
				out = append(out, map[string]any{"x": xv, "y": y})
			} else {
				out = append(out, y)
			}
		}
		return out
	}

	addSeries := func(cols []int, axis string) {
		for _, c := range cols {
			pl.Datasets = append(pl.Datasets, chartDS{
				Label: schema.Columns[c].Name, Data: seriesData(c), Axis: axis, Role: "series",
			})
		}
	}
	addSeries(ys, "")
	addSeries(y2s, "y2")

	// Envelope band (line/area only).
	if kind == "line" || kind == "area" {
		lo, hi := colIdx(viewStr(view, "bandLo")), colIdx(viewStr(view, "bandHi"))
		if lo >= 0 && hi >= 0 {
			pl.Datasets = append(pl.Datasets,
				chartDS{Label: schema.Columns[lo].Name, Data: seriesData(lo), Role: "band_lo"},
				chartDS{Label: schema.Columns[hi].Name, Data: seriesData(hi), Role: "band_hi"},
			)
		}
	}

	// Thresholds render as constant dashed datasets — no annotation plugin
	// needed in the report.
	if raw, ok := view["thresholds"].([]any); ok && kind != "pie" {
		for _, tv := range raw {
			f, ok := tv.(float64)
			if !ok {
				continue
			}
			var data []any
			if timeX || kind == "scatter" {
				if first, ok1 := xVal(rows[0].Values[x]); ok1 && len(rows) > 0 {
					last, _ := xVal(rows[len(rows)-1].Values[x])
					data = []any{
						map[string]any{"x": first, "y": f},
						map[string]any{"x": last, "y": f},
					}
				}
			} else {
				data = make([]any, len(pl.Labels))
				for i := range data {
					data[i] = f
				}
			}
			pl.Datasets = append(pl.Datasets, chartDS{Label: fmtNum(f), Data: data, Role: "threshold"})
		}
	}

	buf, err := json.Marshal(pl)
	if err != nil {
		return "", false
	}
	// </script> inside a JSON string would terminate the embedding script tag.
	return strings.ReplaceAll(string(buf), "</", `<\/`), true
}

// ---- markdown --------------------------------------------------------------------

// renderMarkdown renders the same minimal subset as the web UI's Markdown.tsx —
// #–#### headings, bullet/numbered lists, ``` fences, paragraphs, inline
// **bold** / *italic* / `code` / [links](https://…) — with all text escaped.
func renderMarkdown(text string) string {
	var b strings.Builder
	lines := strings.Split(text, "\n")
	bullet := regexp.MustCompile(`^\s*[-*]\s+`)
	ordered := regexp.MustCompile(`^\s*\d+\.\s+`)
	heading := regexp.MustCompile(`^(#{1,4})\s+(.*)$`)
	i := 0
	for i < len(lines) {
		line := strings.TrimRight(lines[i], "\r")
		switch {
		case strings.TrimSpace(line) == "":
			i++
		case strings.HasPrefix(line, "```"):
			i++
			var code []string
			for i < len(lines) && !strings.HasPrefix(lines[i], "```") {
				code = append(code, lines[i])
				i++
			}
			i++
			b.WriteString("<pre><code>" + template.HTMLEscapeString(strings.Join(code, "\n")) + "</code></pre>")
		case heading.MatchString(line):
			m := heading.FindStringSubmatch(line)
			level := len(m[1])
			fmt.Fprintf(&b, "<h%d>%s</h%d>", level, mdInline(m[2]), level)
			i++
		case bullet.MatchString(line) || ordered.MatchString(line):
			isOrdered := ordered.MatchString(line)
			marker := bullet
			tag := "ul"
			if isOrdered {
				marker, tag = ordered, "ol"
			}
			b.WriteString("<" + tag + ">")
			for i < len(lines) && marker.MatchString(strings.TrimRight(lines[i], "\r")) {
				item := marker.ReplaceAllString(strings.TrimRight(lines[i], "\r"), "")
				b.WriteString("<li>" + mdInline(item) + "</li>")
				i++
			}
			b.WriteString("</" + tag + ">")
		default:
			var para []string
			for i < len(lines) {
				l := strings.TrimRight(lines[i], "\r")
				if strings.TrimSpace(l) == "" || heading.MatchString(l) ||
					strings.HasPrefix(l, "```") || bullet.MatchString(l) || ordered.MatchString(l) {
					break
				}
				para = append(para, l)
				i++
			}
			b.WriteString("<p>" + mdInline(strings.Join(para, " ")) + "</p>")
		}
	}
	return b.String()
}

var mdInlineRe = regexp.MustCompile("\\*\\*[^*]+\\*\\*|\\*[^*]+\\*|`[^`]+`|\\[[^\\]]+\\]\\(https?://[^\\s)]+\\)")

// mdInline renders the inline spans of one line, escaping every text piece.
func mdInline(s string) string {
	var b strings.Builder
	for {
		loc := mdInlineRe.FindStringIndex(s)
		if loc == nil {
			b.WriteString(template.HTMLEscapeString(s))
			return b.String()
		}
		b.WriteString(template.HTMLEscapeString(s[:loc[0]]))
		tok := s[loc[0]:loc[1]]
		switch {
		case strings.HasPrefix(tok, "**"):
			b.WriteString("<b>" + template.HTMLEscapeString(tok[2:len(tok)-2]) + "</b>")
		case strings.HasPrefix(tok, "*"):
			b.WriteString("<i>" + template.HTMLEscapeString(tok[1:len(tok)-1]) + "</i>")
		case strings.HasPrefix(tok, "`"):
			b.WriteString("<code>" + template.HTMLEscapeString(tok[1:len(tok)-1]) + "</code>")
		default: // [text](url)
			end := strings.Index(tok, "](")
			text := tok[1:end]
			url := tok[end+2 : len(tok)-1]
			fmt.Fprintf(&b, `<a href="%s">%s</a>`,
				template.HTMLEscapeString(url), template.HTMLEscapeString(text))
		}
		s = s[loc[1]:]
	}
}

// ---- page template ---------------------------------------------------------------

type reportPage struct {
	Title       string
	Description string
	Generated   string
	Vars        string
	Body        template.HTML
	ChartJS     template.JS
	AdapterJS   template.JS
	ReportJS    template.JS
}

var reportTmpl = template.Must(template.New("report").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>{{.Title}}</title><style>
:root{--bg:#0f1115;--panel:#171a21;--border:#2a2f3a;--fg:#e6e9ef;--muted:#8b93a7;--accent:#6ea8fe;--err:#ff6b6b}
body{background:var(--bg);color:var(--fg);font:14px ui-monospace,SFMono-Regular,Menlo,monospace;margin:0;padding:24px}
header{margin-bottom:18px}h1{font-size:20px;margin:0 0 4px}h2{font-size:14px;margin:0 0 8px}
.desc,.meta,.note{color:var(--muted);font-size:12px}.gen{color:var(--muted);font-size:11px;margin-top:4px}
main{display:flex;flex-wrap:wrap;gap:12px}
.panel{background:var(--panel);border:1px solid var(--border);border-radius:8px;padding:12px 14px;flex:1 1 100%;min-width:0;box-sizing:border-box}
.panel.half{flex:1 1 calc(50% - 12px)}
.md{background:transparent;border:none;line-height:1.55;max-width:76ch}
.md pre{background:#1e222b;border:1px solid var(--border);border-radius:6px;padding:8px 10px;overflow-x:auto}
.md code{background:#1e222b;border-radius:4px;padding:0 4px}.md a{color:var(--accent)}
.stat-v{font-size:34px;font-weight:700;color:var(--accent)}.stat-l{color:var(--muted);font-size:12px}
.tblwrap{overflow-x:auto;max-height:480px;overflow-y:auto}
table{border-collapse:collapse;width:100%;font-size:12.5px}
th,td{border-bottom:1px solid var(--border);padding:4px 8px;text-align:left;white-space:nowrap}
th{color:var(--muted);position:sticky;top:0;background:var(--panel)}td.num{text-align:right}
.err{color:var(--err);border:1px solid var(--err);border-radius:6px;padding:8px 10px;font-size:12.5px}
.chartbox{position:relative;height:320px}.meta{margin-top:6px}
</style></head><body>
<header><h1>{{.Title}}</h1>
{{if .Description}}<div class="desc">{{.Description}}</div>{{end}}
<div class="gen">generated {{.Generated}}{{if .Vars}} · {{.Vars}}{{end}} · turntable dashboard render</div></header>
<main>
{{.Body}}
</main>
{{if .ChartJS}}<script>{{.ChartJS}}</script><script>{{.AdapterJS}}</script><script>{{.ReportJS}}</script>{{end}}
</body></html>
`))

// reportJS builds a Chart.js chart from each embedded payload. Kept dumb: the
// Go side computed labels/datasets/roles; this only maps roles to styling.
const reportJS = `
(function () {
  var PALETTE = ["#6ea8fe","#5fd38d","#e0b341","#c08bd6","#ff6b6b","#4dd4c8","#f4a259","#9aa7ff"];
  Chart.defaults.color = "#8b93a7";
  Chart.defaults.borderColor = "rgba(138,147,167,0.15)";
  Chart.defaults.font.family = "ui-monospace, SFMono-Regular, Menlo, monospace";
  document.querySelectorAll('script[type="application/json"][data-chart]').forEach(function (tag) {
    var p = JSON.parse(tag.textContent);
    var canvas = document.getElementById(tag.getAttribute("data-chart"));
    if (!canvas) return;
    var filled = p.kind === "area";
    var type = p.kind === "area" ? "line" : p.kind;
    var si = 0;
    var datasets = p.datasets.map(function (d) {
      if (d.role === "band_lo" || d.role === "band_hi") {
        return { label: d.label, data: d.data, borderColor: "rgba(138,147,167,0.35)",
          borderWidth: 0.5, pointRadius: 0, tension: 0.3, spanGaps: true, $band: true,
          fill: d.role === "band_hi" ? "-1" : false,
          backgroundColor: "rgba(138,147,167,0.16)" };
      }
      if (d.role === "threshold") {
        return { label: d.label, data: d.data, borderColor: "#ff6b6b", borderDash: [6, 4],
          borderWidth: 1.5, pointRadius: 0, fill: false, $band: true, type: "line" };
      }
      var color = PALETTE[si++ % PALETTE.length];
      if (p.kind === "pie") {
        return { label: d.label, data: d.data,
          backgroundColor: p.labels.map(function (_, i) { return PALETTE[i % PALETTE.length]; }),
          borderColor: "#171a21", borderWidth: 1 };
      }
      return { label: d.label, data: d.data, borderColor: color,
        backgroundColor: type === "line" ? (filled ? color + "33" : color) : color + "cc",
        borderWidth: 2, fill: filled, tension: 0.3, spanGaps: true,
        pointRadius: p.kind === "scatter" ? 3 : (p.timeX ? 0 : 2),
        yAxisID: d.axis === "y2" ? "y2" : "y",
        showLine: p.kind !== "scatter" };
    });
    var scales = {};
    if (p.kind !== "pie") {
      scales.x = p.timeX ? { type: "time" } : (p.kind === "scatter" ? { type: "linear" } : {});
      scales.y = { beginAtZero: true };
      if (p.dualAxis) scales.y2 = { position: "right", beginAtZero: true, grid: { drawOnChartArea: false } };
    }
    new Chart(canvas, {
      type: type,
      data: { labels: p.labels || undefined, datasets: datasets },
      options: {
        responsive: true, maintainAspectRatio: false, animation: false,
        parsing: p.timeX ? false : undefined,
        plugins: { legend: { display: p.kind === "pie" || datasets.filter(function (d) { return !d.$band; }).length > 1,
          position: p.kind === "pie" ? "right" : "bottom",
          labels: { boxWidth: 12, filter: function (item, data) { return !data.datasets[item.datasetIndex].$band; } } } },
        scales: scales,
      },
    });
  });
})();
`
