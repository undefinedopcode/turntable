// Package honeycombc is the Honeycomb.io connector. It exposes Honeycomb through
// four datasets:
//
//   - datasets      metadata: every dataset in the environment (v1 /1/datasets)
//   - columns       metadata: the columns of one dataset (needs dataset=<slug>)
//   - environments  metadata: the team's environments (v2 Management API)
//   - events        queryable event data for one dataset (needs dataset=<slug>)
//
// The metadata datasets are ordinary REST list endpoints flattened into rows,
// mirroring the Linear/Trello connectors. The events dataset is different:
// Honeycomb has no raw-event read API — every query is an aggregation over a
// time window — so events implements connector.AggregatePusher. The planner
// hands it the GROUP BY / aggregates / WHERE of a single-scan aggregate query;
// this connector translates that into a Honeycomb query spec, runs it (create
// query -> create query_result -> poll), and returns the aggregated rows. A
// non-aggregate scan of events is an error (there are no raw rows to return).
//
// Plan note: running event queries uses Honeycomb's Query Data API, which is
// gated to paid plans — on a free plan the query POST returns 403 (surfaced with
// a hint by enterpriseHint). The metadata datasets work on any plan.
//
// Options:
//
//	kind        one of datasets|columns|environments|events; falls back to the
//	            dataset Source (so `honeycomb:datasets` works) then to events.
//	dataset     Honeycomb dataset slug (required for columns and events).
//	api_key     Honeycomb Configuration key -> X-Honeycomb-Team (datasets/columns/events).
//	management_key  Honeycomb Management key "keyID:secret" -> Bearer (environments).
//	team        team slug (required for environments).
//	region      "eu" selects https://api.eu1.honeycomb.io; default is US.
//	url         override the API base entirely.
//	time_range  query window in seconds (events; default 7200 = 2h).
//	start_time,end_time  absolute query window as Unix epoch seconds (events).
package honeycombc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/april/turntable/internal/sql"
)

const (
	baseUS = "https://api.honeycomb.io"
	baseEU = "https://api.eu1.honeycomb.io"
)

// maxItems bounds a metadata scan; defaultLimit bounds an events query.
const (
	maxItems     = 50000
	defaultLimit = 1000
	maxPolls     = 40 // ~ up to maxPolls*pollInterval for a query result
)

// honeyAPI is the connector's narrow view of Honeycomb: one request primitive.
// v2 selects the Management API base + Bearer auth (environments); otherwise the
// v1 Configuration-key base + X-Honeycomb-Team. The real client wraps net/http;
// tests inject a fake.
type honeyAPI interface {
	do(ctx context.Context, method, path string, body any, v2 bool) ([]byte, error)
}

// Connector queries the Honeycomb API.
type Connector struct {
	client       honeyAPI
	pollInterval time.Duration
}

// New constructs a Honeycomb connector.
func New() *Connector { return &Connector{pollInterval: 500 * time.Millisecond} }

// newWithClient returns a Connector backed by an explicit honeyAPI (tests), with
// no inter-poll delay.
func newWithClient(c honeyAPI) *Connector { return &Connector{client: c} }

func (*Connector) Name() string { return "honeycomb" }

func (*Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) { return nil, nil }

// DatasetsFor enumerates the Honeycomb datasets in the environment, returning one
// events dataset per Honeycomb dataset (named by its slug). cli.go uses this to
// expand a `dataset: "*"` source into one queryable source per dataset, mirroring
// the DynamoDB/Athena wildcard connectors.
func (c *Connector) DatasetsFor(ctx context.Context, ds connector.Dataset) ([]connector.Dataset, error) {
	api, err := c.resolveClient(ds.Options, false)
	if err != nil {
		return nil, err
	}
	raw, err := api.do(ctx, "GET", "/1/datasets", nil, false)
	if err != nil {
		return nil, fmt.Errorf("honeycomb list datasets: %w", err)
	}
	var items []struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("honeycomb list datasets: decode: %w", err)
	}
	out := make([]connector.Dataset, 0, len(items))
	for _, it := range items {
		slug := it.Slug
		if slug == "" {
			continue
		}
		opts := map[string]any{"kind": "events", "dataset": slug}
		for k, v := range ds.Options {
			if k == "dataset" {
				continue // replace the "*" wildcard
			}
			opts[k] = v
		}
		out = append(out, connector.Dataset{Name: slug, Source: "events", Options: opts})
	}
	return out, nil
}

// ---- dataset kinds -----------------------------------------------------------

func kindFor(ds connector.Dataset) string {
	k := stringOpt(ds.Options, "kind")
	if k == "" {
		k = ds.Source
	}
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "datasets":
		return "datasets"
	case "columns":
		return "columns"
	case "environments":
		return "environments"
	case "events":
		return "events"
	}
	// A configured source with a dataset slug (or a bare name) is an events source.
	return "events"
}

// datasetSlug returns the Honeycomb dataset slug for columns/events datasets.
func datasetSlug(ds connector.Dataset) string {
	if s := stringOpt(ds.Options, "dataset"); s != "" {
		return s
	}
	return ds.Name
}

func (c *Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	switch kindFor(ds) {
	case "datasets":
		return metaDatasets.schema(), nil
	case "columns":
		return metaColumns.schema(), nil
	case "environments":
		return metaEnvironments.schema(), nil
	case "events":
		return c.eventsSchema(ctx, ds)
	}
	return engine.Schema{}, fmt.Errorf("honeycomb: unknown dataset kind")
}

func (c *Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	ds := req.Dataset
	switch kindFor(ds) {
	case "datasets":
		return c.scanMeta(ctx, ds, metaDatasets, "/1/datasets", false, req.Limit, req.Predicate)
	case "columns":
		slug := datasetSlug(ds)
		if slug == "" {
			return nil, fmt.Errorf("honeycomb columns dataset requires a dataset=<slug> option")
		}
		return c.scanMeta(ctx, ds, metaColumns, "/1/columns/"+slug, false, req.Limit, req.Predicate)
	case "environments":
		return c.scanEnvironments(ctx, ds, req.Limit)
	case "events":
		return c.scanEvents(ctx, req)
	}
	return nil, fmt.Errorf("honeycomb: unknown dataset kind")
}

// ---- metadata datasets -------------------------------------------------------

type field struct {
	col string
	key string
	typ engine.Type
}

type metaDef struct {
	fields []field
}

func (d metaDef) schema() engine.Schema {
	cols := make([]engine.Column, len(d.fields))
	for i, f := range d.fields {
		cols[i] = engine.Column{Name: f.col, Type: f.typ, Nullable: true}
	}
	return engine.Schema{Columns: cols}
}

var metaDatasets = metaDef{fields: []field{
	{"name", "name", engine.TypeString},
	{"slug", "slug", engine.TypeString},
	{"description", "description", engine.TypeString},
	{"columns_count", "regular_columns_count", engine.TypeInt},
	{"created_at", "created_at", engine.TypeTime},
	{"last_written_at", "last_written_at", engine.TypeTime},
}}

var metaColumns = metaDef{fields: []field{
	{"id", "id", engine.TypeString},
	{"key_name", "key_name", engine.TypeString},
	{"type", "type", engine.TypeString},
	{"description", "description", engine.TypeString},
	{"hidden", "hidden", engine.TypeBool},
	{"last_written", "last_written", engine.TypeTime},
	{"created_at", "created_at", engine.TypeTime},
	{"updated_at", "updated_at", engine.TypeTime},
}}

var metaEnvironments = metaDef{fields: []field{
	{"id", "id", engine.TypeString},
	{"name", "name", engine.TypeString},
	{"slug", "slug", engine.TypeString},
	{"description", "description", engine.TypeString},
	{"color", "color", engine.TypeString},
}}

// scanMeta fetches a v1 endpoint returning a JSON array of objects and maps each
// into a row per the metaDef. Ordering/filtering stay with the engine; a LIMIT
// with no predicate is honored to fetch fewer rows.
func (c *Connector) scanMeta(ctx context.Context, ds connector.Dataset, def metaDef, path string, v2 bool, limitp *int, pred sql.Expr) (engine.RowIterator, error) {
	api, err := c.resolveClient(ds.Options, v2)
	if err != nil {
		return nil, err
	}
	raw, err := api.do(ctx, "GET", path, nil, v2)
	if err != nil {
		return nil, fmt.Errorf("honeycomb GET %s: %w", path, err)
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("honeycomb %s: decode: %w", path, err)
	}
	return rowsFrom(def, items, limitp, pred), nil
}

// scanEnvironments fetches the v2 Management API and unwraps the JSON:API
// envelope ({data:[{attributes:{...}}]}) into rows shaped by metaEnvironments.
func (c *Connector) scanEnvironments(ctx context.Context, ds connector.Dataset, limitp *int) (engine.RowIterator, error) {
	team := stringOpt(ds.Options, "team")
	if team == "" {
		return nil, fmt.Errorf("honeycomb environments dataset requires a team=<slug> option")
	}
	api, err := c.resolveClient(ds.Options, true)
	if err != nil {
		return nil, err
	}
	path := "/2/teams/" + team + "/environments"
	raw, err := api.do(ctx, "GET", path, nil, true)
	if err != nil {
		return nil, fmt.Errorf("honeycomb GET %s: %w", path, err)
	}
	var env struct {
		Data []struct {
			ID         string         `json:"id"`
			Attributes map[string]any `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("honeycomb %s: decode: %w", path, err)
	}
	items := make([]map[string]any, len(env.Data))
	for i, e := range env.Data {
		m := map[string]any{"id": e.ID}
		for k, v := range e.Attributes {
			m[k] = v
		}
		items[i] = m
	}
	return rowsFrom(metaEnvironments, items, limitp, nil), nil
}

// rowsFrom maps decoded objects into rows per a metaDef, honoring a no-predicate
// LIMIT.
func rowsFrom(def metaDef, items []map[string]any, limitp *int, pred sql.Expr) engine.RowIterator {
	limit := maxItems
	if pred == nil && limitp != nil && *limitp >= 0 && *limitp < limit {
		limit = *limitp
	}
	if limit < len(items) {
		items = items[:limit]
	}
	rows := make([]engine.Row, len(items))
	for i, m := range items {
		vals := make([]engine.Value, len(def.fields))
		for j, f := range def.fields {
			vals[j] = coerce(f.typ, m[f.key])
		}
		rows[i] = engine.Row{Values: vals}
	}
	return engine.NewSliceIter(rows)
}

// ---- events: schema ----------------------------------------------------------

// eventsSchema resolves the queryable columns of a Honeycomb dataset from the
// columns API, mapping Honeycomb's type names to engine types.
func (c *Connector) eventsSchema(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	cols, err := c.datasetColumns(ctx, ds)
	if err != nil {
		return engine.Schema{}, err
	}
	out := make([]engine.Column, 0, len(cols))
	for _, name := range sortedKeys(cols) {
		out = append(out, engine.Column{Name: name, Type: cols[name], Nullable: true})
	}
	return engine.Schema{Columns: out}, nil
}

// datasetColumns fetches the column name -> engine type map for a dataset.
func (c *Connector) datasetColumns(ctx context.Context, ds connector.Dataset) (map[string]engine.Type, error) {
	slug := datasetSlug(ds)
	if slug == "" {
		return nil, fmt.Errorf("honeycomb events dataset requires a dataset=<slug> option")
	}
	api, err := c.resolveClient(ds.Options, false)
	if err != nil {
		return nil, err
	}
	raw, err := api.do(ctx, "GET", "/1/columns/"+slug, nil, false)
	if err != nil {
		return nil, fmt.Errorf("honeycomb columns %s: %w", slug, err)
	}
	var items []struct {
		KeyName string `json:"key_name"`
		Type    string `json:"type"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("honeycomb columns %s: decode: %w", slug, err)
	}
	out := make(map[string]engine.Type, len(items))
	for _, it := range items {
		out[it.KeyName] = honeyType(it.Type)
	}
	return out, nil
}

func honeyType(t string) engine.Type {
	switch strings.ToLower(t) {
	case "integer":
		return engine.TypeInt
	case "float":
		return engine.TypeFloat
	case "boolean":
		return engine.TypeBool
	case "string":
		return engine.TypeString
	}
	return engine.TypeAny
}

// ---- events: aggregate pushdown ---------------------------------------------

// PushAggregate accepts a grouped aggregation, mapping it to a Honeycomb query.
// It validates that every aggregate op and the predicate are expressible and
// returns the schema of the aggregated rows (breakdowns typed from the dataset
// columns, calculations typed by op). It never runs the query — that happens in
// Scan. Because Honeycomb cannot return raw rows, an unsupported op/filter is a
// hard error rather than a decline.
func (c *Connector) PushAggregate(ctx context.Context, ds connector.Dataset, agg connector.AggregateRequest) (engine.Schema, bool, error) {
	colTypes, err := c.datasetColumns(ctx, ds)
	if err != nil {
		return engine.Schema{}, false, err
	}
	// Validate calculations and the predicate up front (both must translate).
	if _, err := calculations(agg.Aggregates); err != nil {
		return engine.Schema{}, false, err
	}
	if _, _, err := translateFilters(agg.Predicate); err != nil {
		return engine.Schema{}, false, err
	}

	cols := make([]engine.Column, 0, len(agg.GroupBy)+len(agg.Aggregates))
	for _, g := range agg.GroupBy {
		if g.Stride != 0 {
			// Honeycomb breakdowns are plain columns; its time granularity is a
			// query-level setting, not a group-by expression.
			return engine.Schema{}, false, fmt.Errorf("honeycomb: time-bucket GROUP BY (DATE_BIN) is not supported; group by a plain column")
		}
		t, ok := colTypes[g.Column]
		if !ok {
			t = engine.TypeAny
		}
		cols = append(cols, engine.Column{Name: g.Alias, Type: t, Nullable: true})
	}
	for _, op := range agg.Aggregates {
		cols = append(cols, engine.Column{Name: op.Alias, Type: aggResultType(op, colTypes), Nullable: true})
	}
	return engine.Schema{Columns: cols}, true, nil
}

// aggResultType is the engine type of one calculation's output column: COUNT /
// COUNT_DISTINCT are integers; MIN/MAX preserve the source column's type;
// everything else (SUM/AVG/percentiles) is a float.
func aggResultType(op connector.AggregateOp, colTypes map[string]engine.Type) engine.Type {
	switch strings.ToUpper(op.Func) {
	case "COUNT":
		return engine.TypeInt
	case "MIN", "MAX":
		if t, ok := colTypes[op.Column]; ok {
			return t
		}
		return engine.TypeAny
	}
	return engine.TypeFloat
}

// calc is one Honeycomb calculation in a query spec.
type calc struct {
	Op     string `json:"op"`
	Column string `json:"column,omitempty"`
	alias  string // output column (our $aggN); not serialized
	hnyKey string // key under which Honeycomb returns this calc's value
}

// calculations maps AggregateOps to Honeycomb calculations. Supported ops:
// COUNT, COUNT(DISTINCT c) -> COUNT_DISTINCT, SUM, AVG, MIN, MAX, and the pXX
// percentiles (via MEDIAN -> P50). Anything else is an error.
func calculations(ops []connector.AggregateOp) ([]calc, error) {
	out := make([]calc, 0, len(ops))
	for _, op := range ops {
		hop, err := honeyOp(op)
		if err != nil {
			return nil, err
		}
		out = append(out, calc{Op: hop, Column: op.Column, alias: op.Alias, hnyKey: honeyResultKey(hop, op.Column)})
	}
	return out, nil
}

func honeyOp(op connector.AggregateOp) (string, error) {
	name := strings.ToUpper(op.Func)
	if name == "COUNT" {
		if op.Distinct {
			if op.Column == "" {
				return "", fmt.Errorf("honeycomb: COUNT(DISTINCT) requires a column")
			}
			return "COUNT_DISTINCT", nil
		}
		return "COUNT", nil
	}
	if op.Distinct {
		return "", fmt.Errorf("honeycomb: DISTINCT is only supported for COUNT")
	}
	switch name {
	case "SUM", "AVG", "MIN", "MAX":
		if op.Column == "" {
			return "", fmt.Errorf("honeycomb: %s requires a column", name)
		}
		return name, nil
	case "MEDIAN":
		return "P50", nil
	}
	return "", fmt.Errorf("honeycomb: unsupported aggregate %q (supported: COUNT, COUNT(DISTINCT), SUM, AVG, MIN, MAX, MEDIAN)", op.Func)
}

// honeyResultKey is the key under which Honeycomb returns a calculation's value
// in results[].data: the bare op for COUNT, else "OP(column)".
func honeyResultKey(op, column string) string {
	if column == "" {
		return op
	}
	return op + "(" + column + ")"
}

// ---- events: run the query ---------------------------------------------------

// querySpec is the Honeycomb query spec we POST to /1/queries/{slug}.
type querySpec struct {
	Calculations      []calc   `json:"calculations"`
	Breakdowns        []string `json:"breakdowns,omitempty"`
	Filters           []filter `json:"filters,omitempty"`
	FilterCombination string   `json:"filter_combination,omitempty"`
	Limit             int      `json:"limit,omitempty"`
	TimeRange         int      `json:"time_range,omitempty"`
	StartTime         int64    `json:"start_time,omitempty"`
	EndTime           int64    `json:"end_time,omitempty"`
}

func (c *Connector) scanEvents(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	ds := req.Dataset
	if req.Aggregate == nil {
		return nil, fmt.Errorf("honeycomb: dataset %q supports only aggregate queries (GROUP BY / COUNT / SUM / …); Honeycomb has no raw-event read API", datasetSlug(ds))
	}
	slug := datasetSlug(ds)
	calcs, err := calculations(req.Aggregate.Aggregates)
	if err != nil {
		return nil, err
	}
	filters, comb, err := translateFilters(req.Aggregate.Predicate)
	if err != nil {
		return nil, err
	}
	spec := querySpec{
		Calculations:      calcs,
		Breakdowns:        connector.GroupColumns(req.Aggregate.GroupBy),
		Filters:           filters,
		FilterCombination: comb,
		Limit:             eventsLimit(req.Limit),
	}
	applyTimeWindow(&spec, ds.Options)

	api, err := c.resolveClient(ds.Options, false)
	if err != nil {
		return nil, err
	}

	// 1. Create the query -> query id.
	raw, err := api.do(ctx, "POST", "/1/queries/"+slug, spec, false)
	if err != nil {
		return nil, enterpriseHint("create query", err)
	}
	var q struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &q); err != nil || q.ID == "" {
		return nil, fmt.Errorf("honeycomb create query: no id in response")
	}

	// 2. Create the query result -> result id (async run).
	raw, err = api.do(ctx, "POST", "/1/query_results/"+slug, map[string]any{"query_id": q.ID, "disable_series": true}, false)
	if err != nil {
		return nil, enterpriseHint("create query_result", err)
	}
	res, done, err := parseResult(raw)
	if err != nil {
		return nil, err
	}
	if res.ID == "" {
		return nil, fmt.Errorf("honeycomb create query_result: no id in response")
	}

	// 3. Poll until complete.
	for polls := 0; !done && polls < maxPolls; polls++ {
		if c.pollInterval > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(c.pollInterval):
			}
		}
		raw, err = api.do(ctx, "GET", "/1/query_results/"+slug+"/"+res.ID, nil, false)
		if err != nil {
			return nil, fmt.Errorf("honeycomb poll query_result: %w", err)
		}
		res, done, err = parseResult(raw)
		if err != nil {
			return nil, err
		}
	}
	if !done {
		return nil, fmt.Errorf("honeycomb: query_result %s did not complete after %d polls", res.ID, maxPolls)
	}
	return rowsFromResults(res.Results, connector.GroupColumns(req.Aggregate.GroupBy), calcs), nil
}

type queryResult struct {
	ID      string
	Results []map[string]any
}

// parseResult decodes a query_result response, returning the result and whether
// it is complete.
func parseResult(raw []byte) (queryResult, bool, error) {
	var r struct {
		ID       string `json:"id"`
		Complete bool   `json:"complete"`
		Data     struct {
			Results []struct {
				Data map[string]any `json:"data"`
			} `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return queryResult{}, false, fmt.Errorf("honeycomb query_result: decode: %w", err)
	}
	out := queryResult{ID: r.ID}
	for _, res := range r.Data.Results {
		out.Results = append(out.Results, res.Data)
	}
	return out, r.Complete, nil
}

// rowsFromResults turns Honeycomb's results[].data maps into engine rows aligned
// to [breakdowns..., calculations...]. Breakdown values are keyed by column
// name; each calculation by its honeyKey; the output column order and types are
// those PushAggregate reported (breakdown then calc, request order).
func rowsFromResults(results []map[string]any, breakdowns []string, calcs []calc) engine.RowIterator {
	rows := make([]engine.Row, len(results))
	for i, data := range results {
		vals := make([]engine.Value, 0, len(breakdowns)+len(calcs))
		for _, b := range breakdowns {
			vals = append(vals, connector.FromAny(data[b]))
		}
		for _, c := range calcs {
			vals = append(vals, connector.FromAny(data[c.hnyKey]))
		}
		rows[i] = engine.Row{Values: vals}
	}
	return engine.NewSliceIter(rows)
}

func eventsLimit(limitp *int) int {
	if limitp != nil && *limitp > 0 && *limitp <= 10000 {
		return *limitp
	}
	return defaultLimit
}

// applyTimeWindow sets the query's time window from options: explicit
// start_time/end_time (epoch seconds) win; otherwise time_range seconds
// (default 7200).
func applyTimeWindow(spec *querySpec, opts map[string]any) {
	start := intOpt(opts, "start_time")
	end := intOpt(opts, "end_time")
	if start > 0 || end > 0 {
		spec.StartTime = start
		spec.EndTime = end
		return
	}
	if tr := intOpt(opts, "time_range"); tr > 0 {
		spec.TimeRange = int(tr)
		return
	}
	spec.TimeRange = 7200
}

// ---- predicate translation ---------------------------------------------------

type filter struct {
	Column string `json:"column,omitempty"`
	Op     string `json:"op"`
	Value  any    `json:"value,omitempty"`
}

// translateFilters converts a WHERE predicate into Honeycomb filters plus the
// top-level combination ("AND"/"OR"). A nil predicate yields no filters. Because
// the connector must fully apply the predicate, anything it cannot express is an
// error.
func translateFilters(pred sql.Expr) ([]filter, string, error) {
	if pred == nil {
		return nil, "", nil
	}
	conj, comb := flatten(pred)
	if comb == "" {
		comb = "AND"
	}
	out := make([]filter, 0, len(conj))
	for _, e := range conj {
		f, err := translateOne(e)
		if err != nil {
			return nil, "", err
		}
		out = append(out, f)
	}
	return out, comb, nil
}

// flatten splits a predicate into its top-level terms and the single combinator
// joining them. A mixed AND/OR tree is left as one term for translateOne (which
// will reject it), keeping semantics conservative.
func flatten(e sql.Expr) ([]sql.Expr, string) {
	b, ok := e.(*sql.BinaryOp)
	if !ok || (b.Op != "AND" && b.Op != "OR") {
		return []sql.Expr{e}, ""
	}
	var terms []sql.Expr
	var walk func(sql.Expr)
	walk = func(x sql.Expr) {
		if bb, ok := x.(*sql.BinaryOp); ok && bb.Op == b.Op {
			walk(bb.Left)
			walk(bb.Right)
			return
		}
		terms = append(terms, x)
	}
	walk(e)
	return terms, b.Op
}

// translateOne converts a single comparison/predicate term into a filter.
func translateOne(e sql.Expr) (filter, error) {
	switch ex := e.(type) {
	case *sql.BinaryOp:
		op, ok := filterOps[ex.Op]
		if !ok {
			return filter{}, fmt.Errorf("honeycomb: cannot push operator %q", ex.Op)
		}
		col, ok := colName(ex.Left)
		if !ok {
			return filter{}, fmt.Errorf("honeycomb: filter left side must be a column")
		}
		val, ok := literal(ex.Right)
		if !ok {
			return filter{}, fmt.Errorf("honeycomb: filter right side must be a literal")
		}
		return filter{Column: col, Op: op, Value: val}, nil
	case *sql.IsNullExpr:
		col, ok := colName(ex.Expr)
		if !ok {
			return filter{}, fmt.Errorf("honeycomb: IS NULL requires a column")
		}
		if ex.Negate {
			return filter{Column: col, Op: "exists"}, nil
		}
		return filter{Column: col, Op: "does-not-exist"}, nil
	case *sql.InExpr:
		col, ok := colName(ex.Expr)
		if !ok {
			return filter{}, fmt.Errorf("honeycomb: IN requires a column")
		}
		vals := make([]any, 0, len(ex.List))
		for _, item := range ex.List {
			v, ok := literal(item)
			if !ok {
				return filter{}, fmt.Errorf("honeycomb: IN list must be literals")
			}
			vals = append(vals, v)
		}
		op := "in"
		if ex.Negate {
			op = "not-in"
		}
		return filter{Column: col, Op: op, Value: vals}, nil
	case *sql.LikeExpr:
		col, ok := colName(ex.Expr)
		if !ok {
			return filter{}, fmt.Errorf("honeycomb: LIKE requires a column")
		}
		s, ok := likePattern(ex.Pat)
		if !ok {
			return filter{}, fmt.Errorf("honeycomb: LIKE pattern must be a simple %%substring%%")
		}
		if ex.Negate {
			return filter{Column: col, Op: "does-not-contain", Value: s}, nil
		}
		return filter{Column: col, Op: "contains", Value: s}, nil
	}
	return filter{}, fmt.Errorf("honeycomb: cannot push predicate %T", e)
}

var filterOps = map[string]string{
	"=": "=", "!=": "!=", "<>": "!=", ">": ">", ">=": ">=", "<": "<", "<=": "<=",
}

// colName renders a column reference (including a dotted Honeycomb attribute like
// service.name, which SQL lexes as qualifier+name) as its key name.
func colName(e sql.Expr) (string, bool) {
	cr, ok := e.(*sql.ColRef)
	if !ok {
		return "", false
	}
	if cr.Qualifier == "" {
		return cr.Name, true
	}
	return cr.Qualifier + "." + cr.Name, true
}

// literal extracts a scalar Go value from a literal expression.
func literal(e sql.Expr) (any, bool) {
	switch v := e.(type) {
	case *sql.LitInt:
		return v.V, true
	case *sql.LitFloat:
		return v.V, true
	case *sql.LitString:
		return v.V, true
	case *sql.LitBool:
		return v.V, true
	}
	return nil, false
}

// likePattern accepts a LIKE pattern of the form %substring% (no interior
// wildcards) and returns the bare substring for a Honeycomb contains filter.
func likePattern(e sql.Expr) (string, bool) {
	s, ok := e.(*sql.LitString)
	if !ok {
		return "", false
	}
	p := s.V
	if !strings.HasPrefix(p, "%") || !strings.HasSuffix(p, "%") || len(p) < 2 {
		return "", false
	}
	inner := p[1 : len(p)-1]
	if strings.ContainsAny(inner, "%_") {
		return "", false
	}
	return inner, true
}

// ---- value coercion ----------------------------------------------------------

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
	case engine.TypeFloat:
		if f, ok := raw.(float64); ok {
			return engine.FloatVal(f)
		}
		return engine.Null()
	case engine.TypeBool:
		if b, ok := raw.(bool); ok {
			return engine.BoolVal(b)
		}
		return engine.Null()
	case engine.TypeTime:
		if s, ok := raw.(string); ok {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return engine.TimeVal(t)
			}
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

func intOpt(opts map[string]any, key string) int64 {
	switch v := opts[key].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	case string:
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return 0
}

func sortedKeys(m map[string]engine.Type) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---- real Honeycomb client ---------------------------------------------------

// resolveClient lazily builds the HTTP client from options. v2 requests
// (environments) need the Management key; all others need the Configuration key.
func (c *Connector) resolveClient(opts map[string]any, v2 bool) (honeyAPI, error) {
	if c.client != nil {
		return c.client, nil
	}
	base := stringOpt(opts, "url")
	if base == "" {
		if strings.EqualFold(stringOpt(opts, "region"), "eu") {
			base = baseEU
		} else {
			base = baseUS
		}
	}
	apiKey := stringOpt(opts, "api_key")
	if apiKey == "" {
		apiKey = os.Getenv("HONEYCOMB_API_KEY")
	}
	mgmtKey := stringOpt(opts, "management_key")
	if mgmtKey == "" {
		mgmtKey = os.Getenv("HONEYCOMB_MANAGEMENT_KEY")
	}
	hc := &httpClient{
		hc:      &http.Client{Timeout: 30 * time.Second},
		base:    strings.TrimRight(base, "/"),
		apiKey:  apiKey,
		mgmtKey: mgmtKey,
	}
	if v2 {
		if hc.mgmtKey == "" {
			return nil, fmt.Errorf("honeycomb environments require a management_key option (keyID:secret)")
		}
	} else if hc.apiKey == "" {
		return nil, fmt.Errorf("honeycomb connector requires an api_key option")
	}
	return hc, nil
}

type httpClient struct {
	hc      *http.Client
	base    string
	apiKey  string // Configuration key -> X-Honeycomb-Team (v1)
	mgmtKey string // Management key "keyID:secret" -> Bearer (v2)
}

func (h *httpClient) do(ctx context.Context, method, path string, body any, v2 bool) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if v2 {
		req.Header.Set("Authorization", "Bearer "+h.mgmtKey)
		if body != nil {
			req.Header.Set("Content-Type", "application/vnd.api+json")
		}
	} else {
		req.Header.Set("X-Honeycomb-Team", h.apiKey)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	resp, err := h.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &apiError{status: resp.StatusCode, body: strings.TrimSpace(string(data))}
	}
	return data, nil
}

// apiError is a non-2xx HTTP response from Honeycomb, carrying the status so the
// query path can special-case 403 (the Query Data API requires a paid plan).
type apiError struct {
	status int
	body   string
}

func (e *apiError) Error() string { return fmt.Sprintf("status %d: %s", e.status, e.body) }

// enterpriseHint wraps a query-running error, adding an explanation when it is a
// 403: running event queries uses Honeycomb's Query Data API, which is gated to
// paid plans, whereas the metadata datasets work on any plan.
func enterpriseHint(stage string, err error) error {
	var ae *apiError
	if errors.As(err, &ae) && ae.status == 403 {
		return fmt.Errorf("honeycomb %s: %w — running event queries uses Honeycomb's Query Data API, which requires a paid plan (Enterprise/Pro); the metadata datasets (honeycomb:datasets, columns) work on any plan", stage, err)
	}
	return fmt.Errorf("honeycomb %s: %w", stage, err)
}
