// Package grafanac is the Grafana connector — a datasource *proxy*. Rather than
// talking to Prometheus/Loki/a SQL database directly, it runs queries through a
// Grafana instance's HTTP API (POST /api/ds/query), so a single Grafana token
// and its already-configured datasources are the only credentials needed.
//
// It exposes two things:
//
//   - datasources  metadata: every datasource Grafana knows about (GET
//     /api/datasources) — id/uid/name/type/url/is_default/…. This is the
//     table-of-contents: it tells you what you can query and, crucially, each
//     datasource's uid and type (both required to build a query).
//
//   - query mode   option-driven: given datasource=<name-or-uid> plus a native
//     query (query=/expr=/raw_sql=), it POSTs to /api/ds/query and returns the
//     resulting rows. The query body is datasource-type-specific (Prometheus/Loki
//     take `expr`, SQL datasources take `rawSql`, InfluxDB `query`, Graphite
//     `target`), so the connector first resolves the datasource's type, then
//     renders the right request.
//
// Grafana answers /api/ds/query with the "dataframe" format: each frame carries
// a typed field schema plus columnar values. So — like the Azure Logs connector
// — the schema is *exact* (no inference): field types map straight to engine
// types and the columnar values map straight to rows. Multiple frames (e.g. one
// per Prometheus series) are flattened into a single relation: the union of the
// frames' fields, plus one string column per distinct series label.
//
// Like the Prometheus connector, query-mode Resolve runs the query to shape the
// schema, so planning a query-mode SQL query costs two Grafana calls (plan +
// exec). No SQL pushdown — reduce at the source via the native query (PromQL,
// rawSql) and let the engine apply the residual.
//
// Options:
//
//	url          Grafana base URL (e.g. https://grafana.example.com). Also taken
//	             from an http(s) ref Source.
//	token        Grafana API key / service-account token -> Authorization: Bearer.
//	             Falls back to $GRAFANA_TOKEN. (api_key is an accepted alias.)
//	kind         "datasources" selects the metadata dataset; falls back to the
//	             ref Source (so `grafana:datasources` works). Default: query mode.
//	datasource   name or uid of the datasource to query (query mode).
//	uid          datasource uid, if you'd rather skip name resolution.
//	query        the native query text (generic). expr / raw_sql are type-specific
//	             aliases that take precedence when set.
//	query_field  override which request field the query text goes under (advanced;
//	             for datasource types this connector doesn't map by default).
//	format       SQL datasources: "table" (default) or "time_series".
//	from,to      query window; Grafana relative ("now-1h"/"now", the defaults) or
//	             epoch-millisecond strings.
//	max_data_points, interval_ms   time-series resolution hints.
package grafanac

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

// grafanaAPI is the connector's narrow view of Grafana: one request primitive.
// The real client wraps net/http (base URL + Bearer token); tests inject a fake.
type grafanaAPI interface {
	do(ctx context.Context, method, path string, body any) (data []byte, status int, err error)
}

// Connector is the Grafana proxy connector.
type Connector struct {
	client grafanaAPI // nil in production: resolveClient builds one per source
}

// New constructs a Grafana connector.
func New() *Connector { return &Connector{} }

// newWithClient returns a Connector backed by an explicit grafanaAPI (tests).
func newWithClient(c grafanaAPI) *Connector { return &Connector{client: c} }

func (*Connector) Name() string { return "grafana" }

func (*Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) {
	return nil, fmt.Errorf("grafana requires a configured source: url plus token; then either kind=datasources or datasource=<name> with a query")
}

// ---- mode selection ----------------------------------------------------------

func modeFor(ds connector.Dataset) string {
	k := stringOpt(ds.Options, "kind")
	if k == "" && ds.Source != "" && !strings.HasPrefix(ds.Source, "http") {
		k = ds.Source
	}
	if strings.EqualFold(strings.TrimSpace(k), "datasources") {
		return "datasources"
	}
	return "query"
}

func (c *Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	switch modeFor(ds) {
	case "datasources":
		return datasourcesSchema, nil
	default:
		frames, err := c.runQuery(ctx, ds)
		if err != nil {
			return engine.Schema{}, err
		}
		schema, _ := shapeFrames(frames)
		return schema, nil
	}
}

func (c *Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	ds := req.Dataset
	switch modeFor(ds) {
	case "datasources":
		return c.scanDatasources(ctx, ds, req.Limit)
	default:
		frames, err := c.runQuery(ctx, ds)
		if err != nil {
			return nil, err
		}
		return rowsFromFrames(frames), nil
	}
}

// ---- datasources (the TOC) ---------------------------------------------------

type dsField struct {
	col string
	key string
	typ engine.Type
}

var datasourcesFields = []dsField{
	{"id", "id", engine.TypeInt},
	{"uid", "uid", engine.TypeString},
	{"name", "name", engine.TypeString},
	{"type", "type", engine.TypeString},
	{"type_name", "typeName", engine.TypeString},
	{"url", "url", engine.TypeString},
	{"access", "access", engine.TypeString},
	{"database", "database", engine.TypeString},
	{"user", "user", engine.TypeString},
	{"is_default", "isDefault", engine.TypeBool},
	{"read_only", "readOnly", engine.TypeBool},
}

var datasourcesSchema = func() engine.Schema {
	cols := make([]engine.Column, len(datasourcesFields))
	for i, f := range datasourcesFields {
		cols[i] = engine.Column{Name: f.col, Type: f.typ, Nullable: true}
	}
	return engine.Schema{Columns: cols}
}()

func (c *Connector) scanDatasources(ctx context.Context, ds connector.Dataset, limitp *int) (engine.RowIterator, error) {
	api, err := c.resolveClient(ds)
	if err != nil {
		return nil, err
	}
	raw, status, err := api.do(ctx, "GET", "/api/datasources", nil)
	if err != nil {
		return nil, fmt.Errorf("grafana list datasources: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("grafana list datasources: HTTP %d: %s", status, truncate(string(raw), 200))
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("grafana list datasources: decode: %w", err)
	}
	limit := len(items)
	if limitp != nil && *limitp >= 0 && *limitp < limit {
		limit = *limitp
	}
	rows := make([]engine.Row, 0, limit)
	for _, m := range items[:limit] {
		vals := make([]engine.Value, len(datasourcesFields))
		for j, f := range datasourcesFields {
			vals[j] = coerce(f.typ, m[f.key])
		}
		rows = append(rows, engine.Row{Values: vals})
	}
	return engine.NewSliceIter(rows), nil
}

// ---- query mode --------------------------------------------------------------

// datasourceRef is the (uid, type) pair the /api/ds/query body needs.
type datasourceRef struct {
	UID  string `json:"uid"`
	Type string `json:"type"`
}

// resolveDatasource looks up the target datasource's uid+type. A uid option
// (or a datasource value that resolves as a uid) avoids the name lookup; a
// name goes through /api/datasources/name/{name}.
func (c *Connector) resolveDatasource(ctx context.Context, api grafanaAPI, opts map[string]any) (datasourceRef, error) {
	var ref datasourceRef
	uid := stringOpt(opts, "uid")
	name := stringOpt(opts, "datasource")
	if uid == "" && name == "" {
		return ref, fmt.Errorf("grafana query needs a datasource=<name-or-uid> (or uid=) option; list them with `SELECT name, uid, type FROM grafana:datasources`")
	}

	var path string
	if uid != "" {
		path = "/api/datasources/uid/" + uid
	} else {
		path = "/api/datasources/name/" + name
	}
	raw, status, err := api.do(ctx, "GET", path, nil)
	if err != nil {
		return ref, fmt.Errorf("grafana resolve datasource: %w", err)
	}
	if status == http.StatusNotFound && uid == "" {
		// The name wasn't found; the value may itself be a uid.
		raw, status, err = api.do(ctx, "GET", "/api/datasources/uid/"+name, nil)
		if err != nil {
			return ref, fmt.Errorf("grafana resolve datasource: %w", err)
		}
	}
	if status < 200 || status >= 300 {
		return ref, fmt.Errorf("grafana resolve datasource %q: HTTP %d: %s", firstNonEmpty(uid, name), status, truncate(string(raw), 200))
	}
	var d struct {
		UID  string `json:"uid"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return ref, fmt.Errorf("grafana resolve datasource: decode: %w", err)
	}
	if d.UID == "" {
		return ref, fmt.Errorf("grafana resolve datasource %q: no uid in response", firstNonEmpty(uid, name))
	}
	return datasourceRef{UID: d.UID, Type: d.Type}, nil
}

// queryField picks the request field the native query text goes under, and
// whether a SQL-style format field applies, from the datasource type.
func queryField(dsType string, opts map[string]any) (field string, isRange, isSQL bool) {
	if f := stringOpt(opts, "query_field"); f != "" {
		return f, false, false
	}
	t := strings.ToLower(dsType)
	switch {
	case strings.Contains(t, "prometheus"), strings.Contains(t, "loki"):
		return "expr", true, false
	case strings.Contains(t, "influx"):
		return "query", false, false
	case strings.Contains(t, "graphite"):
		return "target", false, false
	case strings.Contains(t, "postgres"), strings.Contains(t, "mysql"),
		strings.Contains(t, "mssql"), strings.Contains(t, "sql"):
		return "rawSql", false, true
	default:
		// Best-effort: most datasource plugins accept `expr`.
		return "expr", false, false
	}
}

// buildQuery assembles the single /api/ds/query request body.
func buildQuery(ref datasourceRef, opts map[string]any) (map[string]any, error) {
	text := firstNonEmpty(stringOpt(opts, "expr"), stringOpt(opts, "raw_sql"), stringOpt(opts, "rawSql"), stringOpt(opts, "query"))
	if text == "" {
		return nil, fmt.Errorf("grafana query needs a query (or expr / raw_sql) option carrying the native query text")
	}
	field, isRange, isSQL := queryField(ref.Type, opts)

	q := map[string]any{
		"refId":         "A",
		"datasource":    ref,
		"maxDataPoints": intOptDefault(opts, "max_data_points", 1000),
		"intervalMs":    intOptDefault(opts, "interval_ms", 15000),
		field:           text,
	}
	if isRange {
		q["range"] = true
		q["instant"] = false
	}
	if isSQL {
		format := stringOpt(opts, "format")
		if format == "" {
			format = "table"
		}
		q["format"] = format
	}

	body := map[string]any{
		"queries": []any{q},
		"from":    firstNonEmpty(stringOpt(opts, "from"), "now-1h"),
		"to":      firstNonEmpty(stringOpt(opts, "to"), "now"),
	}
	return body, nil
}

// runQuery resolves the datasource, POSTs /api/ds/query, and returns refId "A"'s
// frames.
func (c *Connector) runQuery(ctx context.Context, ds connector.Dataset) ([]gfFrame, error) {
	api, err := c.resolveClient(ds)
	if err != nil {
		return nil, err
	}
	ref, err := c.resolveDatasource(ctx, api, ds.Options)
	if err != nil {
		return nil, err
	}
	body, err := buildQuery(ref, ds.Options)
	if err != nil {
		return nil, err
	}
	raw, status, err := api.do(ctx, "POST", "/api/ds/query", body)
	if err != nil {
		return nil, fmt.Errorf("grafana query: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("grafana query: HTTP %d: %s", status, truncate(string(raw), 300))
	}
	var resp struct {
		Results map[string]struct {
			Status int       `json:"status"`
			Error  string    `json:"error"`
			Frames []gfFrame `json:"frames"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("grafana query: decode: %w", err)
	}
	res, ok := resp.Results["A"]
	if !ok {
		return nil, fmt.Errorf("grafana query: no result for refId A")
	}
	if res.Error != "" {
		return nil, fmt.Errorf("grafana query failed: %s", res.Error)
	}
	return res.Frames, nil
}

// ---- dataframe -> rows -------------------------------------------------------

type gfField struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	TypeInfo struct {
		Frame string `json:"frame"`
	} `json:"typeInfo"`
	Labels map[string]string `json:"labels"`
}

type gfFrame struct {
	Schema struct {
		Fields []gfField `json:"fields"`
	} `json:"schema"`
	Data struct {
		Values [][]json.RawMessage `json:"values"`
	} `json:"data"`
}

// fieldType maps a Grafana dataframe field type to an engine type.
func fieldType(f gfField) engine.Type {
	switch strings.ToLower(f.Type) {
	case "time":
		return engine.TypeTime
	case "number":
		if strings.Contains(strings.ToLower(f.TypeInfo.Frame), "int") {
			return engine.TypeInt
		}
		return engine.TypeFloat
	case "boolean", "bool":
		return engine.TypeBool
	case "string", "enum":
		return engine.TypeString
	default:
		return engine.TypeAny
	}
}

// shapeFrames derives the unified schema for a set of frames: the union of the
// frames' fields (first-seen order, typed from the field schema), followed by
// one string column per distinct series label. Returns the schema plus the
// ordered field-column names and label keys used to place values.
func shapeFrames(frames []gfFrame) (engine.Schema, frameLayout) {
	var layout frameLayout
	fieldIdx := map[string]int{}
	labelSet := map[string]bool{}

	for _, fr := range frames {
		for _, f := range fr.Schema.Fields {
			name := f.Name
			if name == "" {
				name = "value"
			}
			if _, seen := fieldIdx[name]; !seen {
				fieldIdx[name] = len(layout.fieldCols)
				layout.fieldCols = append(layout.fieldCols, frameCol{name: name, typ: fieldType(f)})
			}
			for k := range f.Labels {
				labelSet[k] = true
			}
		}
	}
	for k := range labelSet {
		layout.labelKeys = append(layout.labelKeys, k)
	}
	sort.Strings(layout.labelKeys)

	cols := make([]engine.Column, 0, len(layout.fieldCols)+len(layout.labelKeys))
	taken := map[string]bool{}
	for _, fc := range layout.fieldCols {
		taken[fc.name] = true
		cols = append(cols, engine.Column{Name: fc.name, Type: fc.typ, Nullable: true})
	}
	layout.labelCol = make([]string, len(layout.labelKeys))
	for i, k := range layout.labelKeys {
		name := k
		if taken[name] { // a label colliding with a field column name
			name = "label_" + name
		}
		layout.labelCol[i] = name
		cols = append(cols, engine.Column{Name: name, Type: engine.TypeString, Nullable: true})
	}
	return engine.Schema{Columns: cols}, layout
}

type frameCol struct {
	name string
	typ  engine.Type
}

type frameLayout struct {
	fieldCols []frameCol // union of field columns, in order
	labelKeys []string   // sorted distinct label keys
	labelCol  []string   // output column name for each labelKey (collision-adjusted)
}

func (l frameLayout) fieldColIndex(name string) int {
	for i, fc := range l.fieldCols {
		if fc.name == name {
			return i
		}
	}
	return -1
}

// rowsFromFrames flattens all frames into rows aligned to shapeFrames' schema.
func rowsFromFrames(frames []gfFrame) engine.RowIterator {
	schema, layout := shapeFrames(frames)
	nCols := len(schema.Columns)
	labelBase := len(layout.fieldCols)

	var rows []engine.Row
	for _, fr := range frames {
		// The frame's labels (constant for the frame) — merge across its fields.
		frameLabels := map[string]string{}
		for _, f := range fr.Schema.Fields {
			for k, v := range f.Labels {
				frameLabels[k] = v
			}
		}
		rowCount := 0
		for _, col := range fr.Data.Values {
			if len(col) > rowCount {
				rowCount = len(col)
			}
		}
		for r := 0; r < rowCount; r++ {
			vals := make([]engine.Value, nCols)
			for i := range vals {
				vals[i] = engine.Null()
			}
			for fi, f := range fr.Schema.Fields {
				name := f.Name
				if name == "" {
					name = "value"
				}
				ci := layout.fieldColIndex(name)
				if ci < 0 || fi >= len(fr.Data.Values) {
					continue
				}
				colVals := fr.Data.Values[fi]
				if r >= len(colVals) {
					continue
				}
				vals[ci] = decodeCell(colVals[r], schema.Columns[ci].Type)
			}
			for li, k := range layout.labelKeys {
				if v, ok := frameLabels[k]; ok {
					vals[labelBase+li] = engine.StringVal(v)
				}
			}
			rows = append(rows, engine.Row{Values: vals})
		}
	}
	return engine.NewSliceIter(rows)
}

// decodeCell converts one columnar dataframe cell (raw JSON) to a typed Value.
func decodeCell(raw json.RawMessage, typ engine.Type) engine.Value {
	if len(raw) == 0 || string(raw) == "null" {
		return engine.Null()
	}
	switch typ {
	case engine.TypeTime:
		// Grafana time fields are epoch milliseconds (numbers); tolerate RFC3339.
		var ms float64
		if err := json.Unmarshal(raw, &ms); err == nil {
			return engine.TimeVal(time.UnixMilli(int64(ms)).UTC())
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return engine.TimeVal(t)
			}
		}
		return engine.Null()
	case engine.TypeInt:
		var f float64
		if err := json.Unmarshal(raw, &f); err == nil {
			return engine.IntVal(int64(f))
		}
		return engine.Null()
	case engine.TypeFloat:
		var f float64
		if err := json.Unmarshal(raw, &f); err == nil {
			return engine.FloatVal(f)
		}
		return engine.Null()
	case engine.TypeBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err == nil {
			return engine.BoolVal(b)
		}
		return engine.Null()
	case engine.TypeString:
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return engine.StringVal(s)
		}
		return engine.StringVal(strings.Trim(string(raw), `"`))
	default:
		var v any
		if err := json.Unmarshal(raw, &v); err == nil {
			return connector.FromAny(v)
		}
		return engine.Null()
	}
}

// ---- value coercion (datasources metadata) -----------------------------------

func coerce(typ engine.Type, raw any) engine.Value {
	if raw == nil {
		return engine.Null()
	}
	switch typ {
	case engine.TypeInt:
		switch v := raw.(type) {
		case float64:
			return engine.IntVal(int64(v))
		case string:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return engine.IntVal(n)
			}
		}
		return engine.Null()
	case engine.TypeBool:
		if b, ok := raw.(bool); ok {
			return engine.BoolVal(b)
		}
		return engine.Null()
	case engine.TypeString:
		if s, ok := raw.(string); ok {
			return engine.StringVal(s)
		}
		return engine.StringVal(fmt.Sprintf("%v", raw))
	default:
		return connector.FromAny(raw)
	}
}

// ---- helpers -----------------------------------------------------------------

func stringOpt(opts map[string]any, key string) string {
	if v, ok := opts[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func intOptDefault(opts map[string]any, key string, def int) int {
	switch v := opts[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ---- real Grafana client -----------------------------------------------------

func (c *Connector) resolveClient(ds connector.Dataset) (grafanaAPI, error) {
	if c.client != nil {
		return c.client, nil
	}
	// The `url:` config / `.use` field arrives as Dataset.Source; a `url` option
	// (or an http(s) qualified ref) is also accepted.
	base := stringOpt(ds.Options, "url")
	if base == "" && strings.HasPrefix(ds.Source, "http") {
		base = ds.Source
	}
	if base == "" {
		return nil, fmt.Errorf("grafana source needs a url (the Grafana base URL, e.g. https://grafana.example.com)")
	}
	token := firstNonEmpty(stringOpt(ds.Options, "token"), stringOpt(ds.Options, "api_key"), os.Getenv("GRAFANA_TOKEN"))
	return &httpClient{
		hc:    &http.Client{Timeout: 60 * time.Second},
		base:  strings.TrimRight(base, "/"),
		token: token,
	}, nil
}

type httpClient struct {
	hc    *http.Client
	base  string
	token string
}

func (h *httpClient) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	resp, err := h.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	return data, resp.StatusCode, nil
}
