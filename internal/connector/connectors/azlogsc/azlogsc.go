// Package azlogsc is the Azure Monitor Logs / Log Analytics connector: query a
// Log Analytics workspace (AKS container logs, Function logs, app traces, …)
// with KQL, exposed as SQL. It is the Azure twin of cwlogsc.
//
// Log Analytics is a KQL engine, so — like azrgraphc — this connector pushes
// WHERE / ORDER BY / LIMIT down as KQL over a table via the shared azkql
// renderer; a raw `query` option carries a full KQL string for anything the
// translator can't express. Unlike Resource Graph, the API returns typed columns
// alongside the rows, so the schema comes straight from the result (no sampling
// inference) and there is no pagination (one query returns the result set,
// bounded by the KQL `take` cap and the timespan).
//
// Azure is reached through a narrow interface (logsAPI) so tests inject a fake
// without credentials; the real client wraps azlogs + DefaultAzureCredential.
//
// Options:
//
//	workspace  Log Analytics workspace ID (GUID); required.
//	table      the table to query (e.g. ContainerLogV2, AppRequests); also the
//	           ref Source (so `azlogs:AppRequests` works).
//	query      raw KQL query (overrides table + pushdown).
//	timespan   ISO-8601 duration or "start/end" window; default P1D (last day).
//	top        safety row cap for a scan (default 30000).
package azlogsc

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/connector/connectors/azkql"
	"github.com/april/turntable/internal/engine"
)

const (
	sampleTake      = 1     // rows fetched for the schema probe (columns are typed regardless)
	defaultCap      = 30000 // safety row cap per scan
	defaultTimespan = "P1D"
)

// logColumn is a normalized Log Analytics column (name + engine type).
type logColumn struct {
	name string
	typ  engine.Type
}

// logsAPI is the connector's narrow view of Log Analytics: run a KQL query over
// a workspace for a timespan, returning typed columns and rows. The real client
// wraps azlogs; tests inject a fake.
type logsAPI interface {
	query(ctx context.Context, workspace, kql, timespan string) ([]logColumn, [][]any, error)
}

// Connector queries Azure Monitor Logs.
type Connector struct {
	mu     sync.Mutex
	client logsAPI // nil until lazily constructed
}

// New constructs a Log Analytics connector.
func New() *Connector { return &Connector{} }

// newWithClient returns a Connector backed by an explicit logsAPI (tests).
func newWithClient(c logsAPI) *Connector { return &Connector{client: c} }

func (*Connector) Name() string { return "azlogs" }

func (*Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) { return nil, nil }

func tableFor(ds connector.Dataset) string {
	if t := stringOpt(ds.Options, "table"); t != "" {
		return t
	}
	return ds.Source
}

func timespanFor(ds connector.Dataset) string {
	if ts := stringOpt(ds.Options, "timespan"); ts != "" {
		return ts
	}
	return defaultTimespan
}

// Resolve probes the schema by running a bounded query and reading the returned
// (typed) columns.
func (c *Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	kql, err := probeQuery(ds)
	if err != nil {
		return engine.Schema{}, err
	}
	api, err := c.resolveClient()
	if err != nil {
		return engine.Schema{}, err
	}
	cols, _, err := api.query(ctx, workspace(ds), kql, timespanFor(ds))
	if err != nil {
		return engine.Schema{}, fmt.Errorf("azlogs resolve: %w", err)
	}
	return schemaFromColumns(cols), nil
}

// Scan runs the query (raw or the table with pushed WHERE/ORDER BY/LIMIT) and
// maps the typed result to rows.
func (c *Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	ds := req.Dataset
	ws := workspace(ds)
	if ws == "" {
		return nil, fmt.Errorf("azlogs connector requires a workspace option (Log Analytics workspace ID)")
	}

	rowCap := defaultCap
	if t := intOpt(ds.Options, "top"); t > 0 {
		rowCap = t
	}

	var kql string
	if raw := stringOpt(ds.Options, "query"); raw != "" {
		kql = raw // raw KQL: the user owns the query
	} else {
		table := tableFor(ds)
		if table == "" {
			return nil, fmt.Errorf("azlogs connector requires a table or query option")
		}
		kql = azkql.Build(azkql.Query{
			Table:     table,
			Predicate: req.Predicate,
			OrderBy:   req.OrderBy,
			Limit:     req.Limit,
			Cap:       rowCap,
		})
	}

	api, err := c.resolveClient()
	if err != nil {
		return nil, err
	}
	cols, rawRows, err := api.query(ctx, ws, kql, timespanFor(ds))
	if err != nil {
		return nil, fmt.Errorf("azlogs query: %w", err)
	}
	if len(rawRows) > rowCap {
		rawRows = rawRows[:rowCap] // safety cap (raw mode may not carry a take)
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

// probeQuery is the bounded query used to fetch the schema: the raw query with a
// take appended, else the table with a take.
func probeQuery(ds connector.Dataset) (string, error) {
	take := fmt.Sprintf(" | take %d", sampleTake)
	if raw := stringOpt(ds.Options, "query"); raw != "" {
		return raw + take, nil
	}
	table := tableFor(ds)
	if table == "" {
		return "", fmt.Errorf("azlogs connector requires a table or query option")
	}
	return table + take, nil
}

// schemaFromColumns maps the typed Log Analytics columns to an engine schema,
// preserving order.
func schemaFromColumns(cols []logColumn) engine.Schema {
	out := make([]engine.Column, len(cols))
	for i, c := range cols {
		out[i] = engine.Column{Name: c.name, Type: c.typ, Nullable: true}
	}
	return engine.Schema{Columns: out}
}

// coerce converts a decoded JSON value to an engine.Value of the column type.
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

// ---- option helpers ----------------------------------------------------------

func workspace(ds connector.Dataset) string { return stringOpt(ds.Options, "workspace") }

func stringOpt(opts map[string]any, key string) string {
	if v, ok := opts[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func intOpt(opts map[string]any, key string) int {
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
	return 0
}
