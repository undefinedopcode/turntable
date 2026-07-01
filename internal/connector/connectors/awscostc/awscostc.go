// Package awscostc is the AWS Cost Explorer connector. It exposes cost & usage
// via GetCostAndUsage as a relation, mirroring the CloudWatch metrics connector
// (cwmetricsc): the query is driven by dataset options — the metric(s), time
// granularity, group-by dimensions, and time window — and the engine applies any
// residual WHERE / ORDER BY / LIMIT / further rollup.
//
// Cost Explorer is inherently pre-aggregated (you pick metrics + granularity +
// group-by and it returns grouped time-series rows), so there is no SQL
// aggregate to push down; group-by dimensions become columns the engine can
// GROUP BY / filter on.
//
// AWS is reached through a narrow interface (costAPI) returning normalized rows,
// so tests inject a fake without credentials; the real client wraps the
// aws-sdk-go-v2 costexplorer client.
//
// Options:
//
//	region       AWS region (Cost Explorer is global; defaults to us-east-1).
//	profile      shared-config profile name.
//	granularity  DAILY (default) / MONTHLY / HOURLY.
//	metric(s)    cost metric(s), comma-separated (default UnblendedCost; also
//	             BlendedCost, AmortizedCost, NetUnblendedCost, UsageQuantity, …).
//	group_by     up to 2 group-bys, comma-separated; each "TYPE:KEY" or "KEY"
//	             (default type DIMENSION), e.g. "SERVICE", "REGION", "TAG:env".
//	start,end    window as YYYY-MM-DD (end exclusive); default: last 30 days.
package awscostc

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

const (
	defaultMetric      = "UnblendedCost"
	defaultGranularity = "DAILY"
	defaultWindowDays  = 30
	dateLayout         = "2006-01-02"
	maxRows            = 100000
)

// groupDef is one Cost Explorer group-by: a type (DIMENSION/TAG/COST_CATEGORY)
// and a key.
type groupDef struct {
	typ string
	key string
}

// costRequest is a normalized Cost Explorer request.
type costRequest struct {
	start, end  string // YYYY-MM-DD
	granularity string
	metrics     []string
	groupBy     []groupDef
}

// costResult is one returned bucket: a time period, the group key values (aligned
// to the request's groupBy), the per-metric amounts, and the currency/unit.
type costResult struct {
	start, end time.Time
	groups     []string
	amounts    map[string]float64
	currency   string
}

// costAPI is the connector's narrow view of Cost Explorer. The real client wraps
// costexplorer; tests inject a fake.
type costAPI interface {
	get(ctx context.Context, req costRequest) ([]costResult, error)
}

// Connector implements the AWS Cost Explorer connector.
type Connector struct {
	mu     sync.Mutex
	client costAPI // nil until lazily constructed from options
}

func New() *Connector { return &Connector{} }

func newWithClient(c costAPI) *Connector { return &Connector{client: c} }

func (*Connector) Name() string { return "awscost" }

func (*Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) { return nil, nil }

// Resolve returns the schema derived from the metric/group_by options.
func (*Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	return schemaFor(ds.Options), nil
}

func schemaFor(opts map[string]any) engine.Schema {
	cols := []engine.Column{
		{Name: "period_start", Type: engine.TypeTime, Nullable: true},
		{Name: "period_end", Type: engine.TypeTime, Nullable: true},
	}
	for _, g := range groupBys(opts) {
		cols = append(cols, engine.Column{Name: columnName(g.key), Type: engine.TypeString, Nullable: true})
	}
	for _, m := range metrics(opts) {
		cols = append(cols, engine.Column{Name: columnName(m), Type: engine.TypeFloat, Nullable: true})
	}
	cols = append(cols, engine.Column{Name: "currency", Type: engine.TypeString, Nullable: true})
	return engine.Schema{Columns: cols}
}

func (c *Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	opts := req.Dataset.Options
	ms := metrics(opts)
	gbs := groupBys(opts)
	start, end := window(opts)

	api, err := c.resolveClient(ctx, opts)
	if err != nil {
		return nil, err
	}
	results, err := api.get(ctx, costRequest{
		start:       start,
		end:         end,
		granularity: granularity(opts),
		metrics:     ms,
		groupBy:     gbs,
	})
	if err != nil {
		return nil, fmt.Errorf("awscost: %w", err)
	}

	limit := maxRows
	if req.Predicate == nil && req.Limit != nil && *req.Limit >= 0 && *req.Limit < limit {
		limit = *req.Limit
	}

	rows := make([]engine.Row, 0, len(results))
	for _, r := range results {
		if len(rows) >= limit {
			break
		}
		row := []engine.Value{timeVal(r.start), timeVal(r.end)}
		for i := range gbs {
			if i < len(r.groups) {
				row = append(row, engine.StringVal(r.groups[i]))
			} else {
				row = append(row, engine.Null())
			}
		}
		for _, m := range ms {
			if v, ok := r.amounts[m]; ok {
				row = append(row, engine.FloatVal(v))
			} else {
				row = append(row, engine.Null())
			}
		}
		row = append(row, stringOrNull(r.currency))
		rows = append(rows, engine.Row{Values: row})
	}
	return engine.NewSliceIter(rows), nil
}

// ---- option parsing ----------------------------------------------------------

func metrics(opts map[string]any) []string {
	ms := splitList(stringOpt(opts, "metrics"))
	if len(ms) == 0 {
		ms = splitList(stringOpt(opts, "metric"))
	}
	if len(ms) == 0 {
		return []string{defaultMetric}
	}
	return ms
}

func groupBys(opts map[string]any) []groupDef {
	var out []groupDef
	for _, s := range splitList(stringOpt(opts, "group_by")) {
		typ, key := "DIMENSION", s
		if t, k, ok := strings.Cut(s, ":"); ok {
			typ, key = strings.ToUpper(strings.TrimSpace(t)), strings.TrimSpace(k)
		}
		out = append(out, groupDef{typ: typ, key: key})
	}
	return out
}

func granularity(opts map[string]any) string {
	if g := stringOpt(opts, "granularity"); g != "" {
		return strings.ToUpper(g)
	}
	return defaultGranularity
}

// window returns the start/end dates (YYYY-MM-DD); end is exclusive. Defaults to
// the last 30 days.
func window(opts map[string]any) (string, string) {
	start := stringOpt(opts, "start")
	end := stringOpt(opts, "end")
	if start == "" || end == "" {
		now := time.Now().UTC()
		if end == "" {
			end = now.Format(dateLayout)
		}
		if start == "" {
			start = now.AddDate(0, 0, -defaultWindowDays).Format(dateLayout)
		}
	}
	return start, end
}

// columnName turns a Cost Explorer key/metric (SERVICE, UnblendedCost) into a
// snake_case column name (service, unblended_cost).
func columnName(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 && b.Len() > 0 && b.String()[b.Len()-1] != '_' {
				// insert _ before a capital that follows a lowercase/digit
				prev := rune(s[i-1])
				if prev >= 'a' && prev <= 'z' || prev >= '0' && prev <= '9' {
					b.WriteByte('_')
				}
			}
			b.WriteRune(r + 32)
			continue
		}
		if r == ' ' || r == '-' || r == ':' {
			b.WriteByte('_')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ---- value helpers -----------------------------------------------------------

func timeVal(t time.Time) engine.Value {
	if t.IsZero() {
		return engine.Null()
	}
	return engine.TimeVal(t)
}

func stringOrNull(s string) engine.Value {
	if s == "" {
		return engine.Null()
	}
	return engine.StringVal(s)
}

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

// parseAmount parses a Cost Explorer metric amount string.
func parseAmount(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}
