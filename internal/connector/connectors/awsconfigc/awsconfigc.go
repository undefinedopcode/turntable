// Package awsconfigc is the AWS Config Advanced Query connector: account/region
// resource inventory (EC2, Lambda, EKS, RDS, … — every type Config records),
// queried through Config's SQL SELECT surface and exposed as turntable SQL.
//
// It is the AWS analogue of the Azure Resource Graph connector. Config's query
// language is already SQL-shaped, so — like athenac — this connector pushes
// WHERE / LIMIT down as a Config SELECT over the well-known top-level resource
// properties (resourceId, resourceType, awsRegion, tags, configuration, …);
// nested `configuration`/`tags` come through as JSON to index into, and a raw
// `query` option carries a full Config SELECT for resource-type-specific paths.
//
// Config's top-level schema is fixed and documented, so table mode needs no
// sampling; raw mode infers the schema from a sample (like dynamodbc). AWS is
// reached through a narrow interface (configAPI) so tests inject a fake without
// credentials; the real client wraps the aws-sdk-go-v2 configservice client.
//
// Options:
//
//	region      AWS region (defaults to the environment/profile).
//	profile     shared-config profile name.
//	aggregator  a Config configuration aggregator name -> query across the
//	            accounts/regions it aggregates (SelectAggregateResourceConfig).
//	query       a raw Config SELECT expression (overrides table + pushdown).
//	top         safety row cap for a scan (default 5000).
package awsconfigc

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/sql"
)

const (
	defaultCap = 5000
	pageSize   = 100 // Config SelectResourceConfig max Limit per page
	sampleSize = 50  // rows sampled to infer a raw query's schema
)

// configAPI is the connector's narrow view of AWS Config: run a Config SELECT
// expression for a page, returning the page's JSON result strings and the next
// page token. aggregator, when non-empty, selects the cross-account aggregate
// API. The real client wraps configservice; tests inject a fake.
type configAPI interface {
	query(ctx context.Context, expression, aggregator string, limit int32, nextToken string) (results []string, next string, err error)
}

// Connector queries AWS Config.
type Connector struct {
	mu     sync.Mutex
	client configAPI // nil until lazily constructed from options
}

// New constructs an AWS Config connector.
func New() *Connector { return &Connector{} }

// newWithClient returns a Connector backed by an explicit configAPI (tests).
func newWithClient(c configAPI) *Connector { return &Connector{client: c} }

func (*Connector) Name() string { return "awsconfig" }

func (*Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) { return nil, nil }

// tableField is one fixed top-level Config property exposed as a column.
type tableField struct {
	name     string // column / Config property name (Config-native camelCase)
	typ      engine.Type
	pushable bool // may appear in a pushed-down WHERE (scalar top-level only)
}

// tableFields is Config's well-known top-level resource schema.
var tableFields = []tableField{
	{"resourceId", engine.TypeString, true},
	{"resourceType", engine.TypeString, true},
	{"resourceName", engine.TypeString, true},
	{"arn", engine.TypeString, true},
	{"awsRegion", engine.TypeString, true},
	{"availabilityZone", engine.TypeString, true},
	{"accountId", engine.TypeString, true},
	{"resourceCreationTime", engine.TypeTime, false},
	{"tags", engine.TypeAny, false},
	{"configuration", engine.TypeAny, false},
}

func tableSchema() engine.Schema {
	cols := make([]engine.Column, len(tableFields))
	for i, f := range tableFields {
		cols[i] = engine.Column{Name: f.name, Type: f.typ, Nullable: true}
	}
	return engine.Schema{Columns: cols}
}

// selectList is the SELECT clause for table mode: every top-level property.
func selectList() string {
	names := make([]string, len(tableFields))
	for i, f := range tableFields {
		names[i] = f.name
	}
	return strings.Join(names, ", ")
}

func pushableColumns() map[string]bool {
	m := map[string]bool{}
	for _, f := range tableFields {
		if f.pushable {
			m[f.name] = true
		}
	}
	return m
}

// Resolve returns the fixed schema (table mode) or infers it from a sample (raw
// query mode).
func (c *Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	raw := stringOpt(ds.Options, "query")
	if raw == "" {
		return tableSchema(), nil
	}
	api, err := c.resolveClient(ctx, ds.Options)
	if err != nil {
		return engine.Schema{}, err
	}
	results, _, err := api.query(ctx, raw, stringOpt(ds.Options, "aggregator"), sampleSize, "")
	if err != nil {
		return engine.Schema{}, fmt.Errorf("awsconfig resolve: %w", err)
	}
	return inferSchema(parseResults(results)), nil
}

// Scan builds the Config SELECT (table mode with pushed WHERE/LIMIT, or the raw
// query), paginates to the cap, and maps the JSON results to rows.
func (c *Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	ds := req.Dataset
	api, err := c.resolveClient(ctx, ds.Options)
	if err != nil {
		return nil, err
	}

	rowCap := defaultCap
	if t := intOpt(ds.Options, "top"); t > 0 {
		rowCap = t
	}

	raw := stringOpt(ds.Options, "query")
	var expr string
	var schema engine.Schema
	if raw != "" {
		expr = raw // raw Config SELECT: the user owns it
	} else {
		expr = "SELECT " + selectList()
		if w := buildWhere(req.Predicate); w != "" {
			expr += " WHERE " + w
		}
		schema = tableSchema()
	}

	aggregator := stringOpt(ds.Options, "aggregator")
	var maps []map[string]any
	next := ""
	for {
		limit := int32(pageSize)
		if remaining := rowCap - len(maps); remaining < pageSize {
			limit = int32(remaining)
		}
		results, nt, err := api.query(ctx, expr, aggregator, limit, next)
		if err != nil {
			return nil, fmt.Errorf("awsconfig query: %w", err)
		}
		maps = append(maps, parseResults(results)...)
		if nt == "" || len(maps) >= rowCap {
			break
		}
		next = nt
	}
	if len(maps) > rowCap {
		maps = maps[:rowCap]
	}
	if raw != "" {
		schema = inferSchema(maps)
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

// parseResults decodes each Config result JSON string into a map.
func parseResults(results []string) []map[string]any {
	out := make([]map[string]any, 0, len(results))
	for _, s := range results {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err == nil {
			out = append(out, m)
		}
	}
	return out
}

// ---- WHERE translation (Config SQL) -----------------------------------------

// buildWhere renders the pushable conjuncts of a predicate as a Config WHERE
// body. Only top-level scalar columns with =, IN, LIKE (AND/OR-combined) push;
// everything else is left to the engine (which re-applies the full predicate).
func buildWhere(pred sql.Expr) string {
	if pred == nil {
		return ""
	}
	push := pushableColumns()
	var parts []string
	for _, c := range conjuncts(pred) {
		if s, ok := translate(c, push); ok {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " AND ")
}

func conjuncts(e sql.Expr) []sql.Expr {
	if b, ok := e.(*sql.BinaryOp); ok && b.Op == "AND" {
		return append(conjuncts(b.Left), conjuncts(b.Right)...)
	}
	return []sql.Expr{e}
}

func translate(e sql.Expr, push map[string]bool) (string, bool) {
	switch ex := e.(type) {
	case *sql.BinaryOp:
		switch ex.Op {
		case "AND", "OR":
			l, lok := translate(ex.Left, push)
			r, rok := translate(ex.Right, push)
			if lok && rok {
				return "(" + l + ") " + ex.Op + " (" + r + ")", true
			}
			return "", false
		case "=":
			col, ok := pushCol(ex.Left, push)
			if !ok {
				return "", false
			}
			lit, ok := literal(ex.Right)
			if !ok {
				return "", false
			}
			return col + " = " + lit, true
		}
	case *sql.InExpr:
		col, ok := pushCol(ex.Expr, push)
		if !ok || ex.Negate {
			return "", false
		}
		lits := make([]string, 0, len(ex.List))
		for _, it := range ex.List {
			l, ok := literal(it)
			if !ok {
				return "", false
			}
			lits = append(lits, l)
		}
		return col + " IN (" + strings.Join(lits, ", ") + ")", true
	case *sql.LikeExpr:
		col, ok := pushCol(ex.Expr, push)
		if !ok || ex.Negate {
			return "", false
		}
		p, ok := ex.Pat.(*sql.LitString)
		if !ok {
			return "", false
		}
		return col + " LIKE " + configString(p.V), true
	}
	return "", false
}

// pushCol returns a bare column name if it is a pushable top-level Config column.
func pushCol(e sql.Expr, push map[string]bool) (string, bool) {
	cr, ok := e.(*sql.ColRef)
	if !ok || cr.Qualifier != "" || !push[cr.Name] {
		return "", false
	}
	return cr.Name, true
}

func literal(e sql.Expr) (string, bool) {
	switch v := e.(type) {
	case *sql.LitString:
		return configString(v.V), true
	case *sql.LitInt:
		return strconv.FormatInt(v.V, 10), true
	case *sql.LitFloat:
		return strconv.FormatFloat(v.V, 'g', -1, 64), true
	case *sql.LitBool:
		if v.V {
			return "'true'", true
		}
		return "'false'", true
	}
	return "", false
}

// configString renders a single-quoted Config string literal (doubling any
// embedded quote).
func configString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// ---- schema inference (raw mode) --------------------------------------------

func inferSchema(maps []map[string]any) engine.Schema {
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
				continue
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
		return engine.TypeAny
	}
}

// ---- value coercion ----------------------------------------------------------

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
	case engine.TypeTime:
		if s, ok := raw.(string); ok {
			if t, err := parseTime(s); err == nil {
				return engine.TimeVal(t)
			}
		}
		return engine.Null()
	default:
		return connector.FromAny(raw)
	}
}

func parseTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time %q", s)
}

// ---- option helpers ----------------------------------------------------------

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
