// Package azrgraphc is the Azure Resource Graph connector: fleet inventory of
// every Azure resource (AKS clusters, Functions, VMs, NICs, tags, …) across
// subscriptions, queried through one KQL endpoint.
//
// Resource Graph is a KQL engine, so — like athenac with SQL — this connector
// pushes WHERE / ORDER BY / LIMIT down as KQL (via the shared azkql renderer)
// over a table (default `Resources`); a raw `query` option carries a full KQL
// string for anything the translator can't express. Resource Graph rows are
// semi-structured, so the schema is inferred from a sample (like dynamodbc):
// scalar columns get real types, nested `tags`/`properties`/`sku` stay `any` and
// are indexed into with the dialect's JSON paths.
//
// Azure is reached through a narrow interface (graphAPI) so tests inject a fake
// without credentials; the real client wraps armresourcegraph +
// DefaultAzureCredential.
//
// Options:
//
//	table         Resource Graph table (default "Resources"); also the ref Source
//	              (so `azrgraph:Resources` works).
//	query         raw KQL query (overrides table + pushdown).
//	subscriptions comma-separated subscription IDs (default: all accessible).
//	top           safety row cap for a scan (default 5000).
package azrgraphc

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/connector/connectors/azkql"
	"github.com/april/turntable/internal/engine"
)

const (
	defaultTable = "Resources"
	sampleSize   = 32   // rows sampled to infer the schema
	defaultCap   = 5000 // safety row cap per scan
	pageSize     = 1000 // Resource Graph page size
)

// graphAPI is the connector's narrow view of Resource Graph: run a KQL query for
// a page, returning the page's rows (as decoded objects) and the next page
// token. The real client wraps armresourcegraph; tests inject a fake.
type graphAPI interface {
	query(ctx context.Context, subscriptions []string, kql string, top int32, skipToken string) (rows []map[string]any, next string, err error)
}

// Connector queries Azure Resource Graph.
type Connector struct {
	mu     sync.Mutex
	client graphAPI // nil until lazily constructed
}

// New constructs a Resource Graph connector.
func New() *Connector { return &Connector{} }

// newWithClient returns a Connector backed by an explicit graphAPI (tests).
func newWithClient(c graphAPI) *Connector { return &Connector{client: c} }

func (*Connector) Name() string { return "azrgraph" }

func (*Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) { return nil, nil }

// tableFor resolves the Resource Graph table for a dataset (option, then ref
// Source, then the default).
func tableFor(ds connector.Dataset) string {
	if t := stringOpt(ds.Options, "table"); t != "" {
		return t
	}
	if ds.Source != "" {
		return ds.Source
	}
	return defaultTable
}

// baseQuery returns the KQL for a schema probe / unfiltered scan: the raw
// `query` option verbatim, else just the table (pushdown is added per-scan).
func baseQuery(ds connector.Dataset) string {
	if q := stringOpt(ds.Options, "query"); q != "" {
		return q
	}
	return tableFor(ds)
}

// Resolve infers the schema from a sample of rows.
func (c *Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	api, err := c.resolveClient()
	if err != nil {
		return engine.Schema{}, err
	}
	rows, _, err := api.query(ctx, subscriptions(ds.Options), baseQuery(ds), sampleSize, "")
	if err != nil {
		return engine.Schema{}, fmt.Errorf("azrgraph resolve %q: %w", tableFor(ds), err)
	}
	return schemaFromMaps(rows), nil
}

// Scan runs the query (raw or the table with pushed WHERE/ORDER BY/LIMIT),
// paginates to the cap, and maps rows to the inferred schema.
func (c *Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	ds := req.Dataset
	schema, err := c.Resolve(ctx, ds)
	if err != nil {
		return nil, err
	}
	api, err := c.resolveClient()
	if err != nil {
		return nil, err
	}

	rowCap := defaultCap
	if t := intOpt(ds.Options, "top"); t > 0 {
		rowCap = t
	}

	var kql string
	if raw := stringOpt(ds.Options, "query"); raw != "" {
		kql = raw // raw KQL: no pushdown (the user owns the query)
	} else {
		kql = azkql.Build(azkql.Query{
			Table:     tableFor(ds),
			Predicate: req.Predicate,
			OrderBy:   req.OrderBy,
			Limit:     req.Limit,
			Cap:       rowCap,
		})
	}

	subs := subscriptions(ds.Options)
	var maps []map[string]any
	skip := ""
	for {
		page, next, err := api.query(ctx, subs, kql, pageSize, skip)
		if err != nil {
			return nil, fmt.Errorf("azrgraph query: %w", err)
		}
		maps = append(maps, page...)
		if next == "" || len(maps) >= rowCap {
			break
		}
		skip = next
	}
	if len(maps) > rowCap {
		maps = maps[:rowCap]
	}

	rows := make([]engine.Row, len(maps))
	for i, m := range maps {
		vals := make([]engine.Value, len(schema.Columns))
		for j, col := range schema.Columns {
			vals[j] = coerce(col.Type, m[col.Name])
		}
		rows[i] = engine.Row{Values: vals}
	}
	return engine.NewSliceIter(rows), nil
}

// ---- schema inference --------------------------------------------------------

// schemaFromMaps builds a schema from the union of keys across the sampled rows,
// ordered deterministically (so Resolve and Scan agree). Each column's type is
// inferred from the sampled values: string/bool/number get real types, nested
// objects/arrays stay `any`; mixed or all-null columns are `any`.
func schemaFromMaps(maps []map[string]any) engine.Schema {
	types := map[string]engine.Type{}
	seen := map[string]bool{}
	for _, m := range maps {
		for k, v := range m {
			t := inferType(v)
			if !seen[k] {
				seen[k] = true
				types[k] = t
				continue
			}
			if t == engine.TypeNull {
				continue // a null value doesn't constrain the type
			}
			if types[k] == engine.TypeNull {
				types[k] = t
			} else if types[k] != t {
				types[k] = engine.TypeAny
			}
		}
	}
	names := make([]string, 0, len(types))
	for k := range types {
		names = append(names, k)
	}
	sort.Strings(names)
	cols := make([]engine.Column, len(names))
	for i, n := range names {
		t := types[n]
		if t == engine.TypeNull {
			t = engine.TypeAny
		}
		cols[i] = engine.Column{Name: n, Type: t, Nullable: true}
	}
	return engine.Schema{Columns: cols}
}

func inferType(v any) engine.Type {
	switch v.(type) {
	case nil:
		return engine.TypeNull
	case string:
		return engine.TypeString
	case bool:
		return engine.TypeBool
	case float64, float32, int, int64, int32:
		return engine.TypeFloat
	default:
		return engine.TypeAny // objects, arrays
	}
}

// coerce converts a decoded JSON value to an engine.Value of the column type.
func coerce(typ engine.Type, raw any) engine.Value {
	if raw == nil {
		return engine.Null()
	}
	switch typ {
	case engine.TypeString:
		if s, ok := raw.(string); ok {
			return engine.StringVal(s)
		}
		return engine.StringVal(fmt.Sprintf("%v", raw))
	case engine.TypeBool:
		if b, ok := raw.(bool); ok {
			return engine.BoolVal(b)
		}
		return engine.Null()
	case engine.TypeFloat:
		if f, ok := raw.(float64); ok {
			return engine.FloatVal(f)
		}
		return engine.Null()
	default:
		return connector.FromAny(raw)
	}
}

// ---- option helpers ----------------------------------------------------------

func subscriptions(opts map[string]any) []string {
	var out []string
	for _, s := range strings.Split(stringOpt(opts, "subscriptions"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
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
