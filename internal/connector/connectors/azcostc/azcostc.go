// Package azcostc is the Azure Cost Management connector. It exposes cost via the
// Cost Management Query API as a relation, mirroring the AWS Cost Explorer
// connector (awscostc): the query is driven by dataset options — scope, time
// frame, granularity, the aggregation metric, and group-by dimensions — and the
// engine applies any residual WHERE / ORDER BY / LIMIT / further rollup.
//
// The API is pre-aggregated (you pick the aggregation + grouping + granularity),
// so there is no SQL aggregate to push down; it returns typed columns with the
// rows, so the schema is exact (like azlogsc), with no inference.
//
// Azure is reached through a narrow interface (costAPI) so tests inject a fake
// without credentials; the real client wraps armcostmanagement +
// DefaultAzureCredential.
//
// Options:
//
//	subscription  subscription ID -> scope /subscriptions/<id> (shorthand).
//	scope         full ARM scope (overrides subscription), e.g. a management
//	              group or billing account scope.
//	type          ActualCost (default) / AmortizedCost / Usage.
//	timeframe     MonthToDate (default) / TheLastMonth / WeekToDate / … / Custom.
//	granularity   None (default) / Daily.
//	metric        aggregated column (default Cost; also PreTaxCost for EA).
//	group_by      dimensions to group by, comma-separated; "TAG:key" for a tag,
//	              e.g. "ServiceName", "ResourceGroup", "TAG:env".
//	start,end     window as YYYY-MM-DD (sets timeframe=Custom).
package azcostc

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

// costColumn is a normalized Cost Management column (name + engine type).
type costColumn struct {
	name string
	typ  engine.Type
}

// groupDef is one grouping: a column name and whether it's a Dimension or TagKey.
type groupDef struct {
	name  string
	isTag bool
}

// queryDef is a normalized Cost Management query.
type queryDef struct {
	exportType  string
	timeframe   string
	granularity string
	metric      string
	metricAlias string
	start, end  string // YYYY-MM-DD (Custom timeframe)
	grouping    []groupDef
}

// costAPI is the connector's narrow view of Cost Management. The real client
// wraps armcostmanagement; tests inject a fake.
type costAPI interface {
	query(ctx context.Context, scope string, def queryDef) ([]costColumn, [][]any, error)
}

// Connector queries Azure Cost Management.
type Connector struct {
	mu     sync.Mutex
	client costAPI // nil until lazily constructed
}

func New() *Connector { return &Connector{} }

func newWithClient(c costAPI) *Connector { return &Connector{client: c} }

func (*Connector) Name() string { return "azcost" }

func (*Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) { return nil, nil }

func scopeOf(opts map[string]any) string {
	if s := stringOpt(opts, "scope"); s != "" {
		return s
	}
	if sub := stringOpt(opts, "subscription"); sub != "" {
		return "/subscriptions/" + sub
	}
	return ""
}

func queryFor(opts map[string]any) queryDef {
	metric := stringOpt(opts, "metric")
	if metric == "" {
		metric = "Cost"
	}
	timeframe := stringOpt(opts, "timeframe")
	if timeframe == "" {
		timeframe = "MonthToDate"
	}
	start, end := stringOpt(opts, "start"), stringOpt(opts, "end")
	if start != "" || end != "" {
		timeframe = "Custom"
	}
	def := queryDef{
		exportType:  strOrDefault(stringOpt(opts, "type"), "ActualCost"),
		timeframe:   timeframe,
		granularity: stringOpt(opts, "granularity"),
		metric:      metric,
		metricAlias: strings.ToLower(metric),
		start:       start,
		end:         end,
	}
	for _, s := range splitList(stringOpt(opts, "group_by")) {
		if len(s) > 4 && strings.EqualFold(s[:4], "TAG:") {
			def.grouping = append(def.grouping, groupDef{name: strings.TrimSpace(s[4:]), isTag: true})
		} else {
			def.grouping = append(def.grouping, groupDef{name: s})
		}
	}
	return def
}

// Resolve runs the query and returns the schema from the typed result columns.
func (c *Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	scope := scopeOf(ds.Options)
	if scope == "" {
		return engine.Schema{}, fmt.Errorf("azcost connector requires a subscription or scope option")
	}
	api, err := c.resolveClient()
	if err != nil {
		return engine.Schema{}, err
	}
	cols, _, err := api.query(ctx, scope, queryFor(ds.Options))
	if err != nil {
		return engine.Schema{}, fmt.Errorf("azcost resolve: %w", err)
	}
	return schemaFromColumns(cols), nil
}

func (c *Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	ds := req.Dataset
	scope := scopeOf(ds.Options)
	if scope == "" {
		return nil, fmt.Errorf("azcost connector requires a subscription or scope option")
	}
	api, err := c.resolveClient()
	if err != nil {
		return nil, err
	}
	cols, rawRows, err := api.query(ctx, scope, queryFor(ds.Options))
	if err != nil {
		return nil, fmt.Errorf("azcost query: %w", err)
	}
	schema := schemaFromColumns(cols)
	rows := make([]engine.Row, len(rawRows))
	for i, r := range rawRows {
		vals := make([]engine.Value, len(schema.Columns))
		for j := range schema.Columns {
			var raw any
			if j < len(r) {
				raw = r[j]
			}
			vals[j] = coerce(schema.Columns[j].Type, raw)
		}
		rows[i] = engine.Row{Values: vals}
	}
	return engine.NewSliceIter(rows), nil
}

func schemaFromColumns(cols []costColumn) engine.Schema {
	out := make([]engine.Column, len(cols))
	for i, c := range cols {
		out[i] = engine.Column{Name: c.name, Type: c.typ, Nullable: true}
	}
	return engine.Schema{Columns: out}
}

// costColumnType maps a Cost Management column type to an engine type.
func costColumnType(t string) engine.Type {
	switch strings.ToLower(t) {
	case "number":
		return engine.TypeFloat
	case "datetime":
		return engine.TypeTime
	case "string":
		return engine.TypeString
	}
	return engine.TypeAny
}

func coerce(typ engine.Type, raw any) engine.Value {
	if raw == nil {
		return engine.Null()
	}
	switch typ {
	case engine.TypeFloat:
		switch v := raw.(type) {
		case float64:
			return engine.FloatVal(v)
		case string:
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return engine.FloatVal(f)
			}
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

// ---- option helpers ----------------------------------------------------------

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func stringOpt(opts map[string]any, key string) string {
	if v, ok := opts[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func strOrDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
