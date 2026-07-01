// Package azmetricsc is the Azure Monitor Metrics connector. It retrieves a
// resource's metric time series via the Azure Monitor "metrics" API
// (Microsoft.Insights/metrics) as a fixed-schema relation, mirroring the AWS
// CloudWatch metrics connector (cwmetricsc): the query is driven by dataset
// options, not by SQL translation, and the engine applies any residual
// WHERE/ORDER BY/LIMIT.
//
// The Azure API is inherently pre-aggregated — you name a metric, an aggregation
// (Average/Total/…) and a bucket interval over a timespan — so there is no
// GROUP BY to push down. A dimension split is exposed as the `dimension` option,
// which adds one column per dimension the engine can group/filter on.
//
// Azure is reached through a narrow interface (metricsAPI) returning normalized
// values, so tests inject a fake without credentials; the real client wraps
// armmonitor + DefaultAzureCredential.
//
// Options:
//
//	resource     ARM resource ID (…/subscriptions/<id>/…/providers/…/<name>); required.
//	metric       metric name(s), comma-separated; required (e.g. "Percentage CPU").
//	aggregation  Average (default) / Total / Minimum / Maximum / Count.
//	interval     ISO-8601 bucket duration (default PT5M).
//	timespan     ISO-8601 "start/end"; default is the last hour.
//	dimension    dimension name(s) to split by, comma-separated (adds columns).
//	namespace    metric namespace (for custom / ambiguous metrics).
//	filter       raw Azure $filter override (advanced; supersedes `dimension`).
package azmetricsc

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

// maxPoints caps the data points buffered per scan when the engine gives no limit.
const maxPoints = 10000

const (
	defaultAggregation = "Average"
	defaultInterval    = "PT5M"
	defaultWindow      = time.Hour
)

// metricQuery is a normalized metrics request handed to the client.
type metricQuery struct {
	metricNames []string
	namespace   string
	aggregation string // lower-case Azure aggregation: average/total/minimum/maximum/count
	interval    string
	timespan    string // "startISO/endISO"
	filter      string // dimension $filter, e.g. "node eq '*'"
}

// metricSeries is one returned time series: a metric, its dimension values (from
// the response metadata), and its points.
type metricSeries struct {
	metric     string
	dimensions map[string]string
	points     []metricPoint
}

type metricPoint struct {
	ts  time.Time
	val *float64 // the value for the requested aggregation; nil = no data
}

// metricsAPI is the connector's narrow view of Azure Monitor metrics. The real
// client wraps armmonitor; tests inject a fake.
type metricsAPI interface {
	list(ctx context.Context, resourceURI string, q metricQuery) ([]metricSeries, error)
}

// Connector implements the Azure Monitor Metrics connector.
type Connector struct {
	mu     sync.Mutex
	client metricsAPI // nil until lazily constructed from the resource's subscription
}

// New returns a Connector that lazily builds a real Azure client on first use.
func New() *Connector { return &Connector{} }

// newWithClient returns a Connector backed by an explicit metricsAPI (tests).
func newWithClient(c metricsAPI) *Connector { return &Connector{client: c} }

func (*Connector) Name() string { return "azmetrics" }

func (*Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) { return nil, nil }

// Resolve returns the schema: the fixed columns plus one column per dimension in
// the `dimension` option.
func (*Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	cols := []engine.Column{
		{Name: "timestamp", Type: engine.TypeTime, Nullable: true},
		{Name: "resource", Type: engine.TypeString, Nullable: true},
		{Name: "metric", Type: engine.TypeString, Nullable: true},
		{Name: "aggregation", Type: engine.TypeString, Nullable: true},
		{Name: "value", Type: engine.TypeFloat, Nullable: true},
	}
	for _, d := range dimensions(ds.Options) {
		cols = append(cols, engine.Column{Name: d, Type: engine.TypeString, Nullable: true})
	}
	return engine.Schema{Columns: cols}, nil
}

// Scan builds a metricQuery from the options, calls the API, and emits one row
// per (series, point). Predicate/OrderBy are ignored; the engine applies them.
func (c *Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	opts := req.Dataset.Options

	resource := stringOpt(opts, "resource")
	if resource == "" {
		resource = req.Dataset.Source // allow the ARM ID via the ref source too
	}
	if resource == "" {
		return nil, fmt.Errorf("azmetrics connector requires a resource option (ARM resource ID)")
	}
	metricNames := splitList(stringOpt(opts, "metric"))
	if len(metricNames) == 0 {
		return nil, fmt.Errorf("azmetrics connector requires a metric option")
	}
	aggregation := stringOpt(opts, "aggregation")
	if aggregation == "" {
		aggregation = defaultAggregation
	}
	interval := stringOpt(opts, "interval")
	if interval == "" {
		interval = defaultInterval
	}

	dims := dimensions(opts)
	filter := stringOpt(opts, "filter")
	if filter == "" && len(dims) > 0 {
		filter = dimensionFilter(dims)
	}

	q := metricQuery{
		metricNames: metricNames,
		namespace:   stringOpt(opts, "namespace"),
		aggregation: strings.ToLower(aggregation),
		interval:    interval,
		timespan:    timespan(opts),
		filter:      filter,
	}

	api, err := c.resolveClient(resource)
	if err != nil {
		return nil, err
	}
	series, err := api.list(ctx, resource, q)
	if err != nil {
		return nil, fmt.Errorf("azmetrics %s: %w", resource, err)
	}

	limit := maxPoints
	if req.Predicate == nil && req.Limit != nil && *req.Limit >= 0 && *req.Limit < limit {
		limit = *req.Limit
	}

	var rows []engine.Row
	for _, s := range series {
		for _, p := range s.points {
			if len(rows) >= limit {
				break
			}
			row := []engine.Value{
				timeVal(p.ts),
				engine.StringVal(resource),
				engine.StringVal(s.metric),
				engine.StringVal(aggregation),
				floatVal(p.val),
			}
			for _, d := range dims {
				if v, ok := s.dimensions[d]; ok {
					row = append(row, engine.StringVal(v))
				} else {
					row = append(row, engine.Null())
				}
			}
			rows = append(rows, engine.Row{Values: row})
		}
	}
	return engine.NewSliceIter(rows), nil
}

// ---- option helpers ----------------------------------------------------------

func dimensions(opts map[string]any) []string { return splitList(stringOpt(opts, "dimension")) }

// dimensionFilter builds an Azure $filter that splits by every named dimension:
// `a eq '*' and b eq '*'`.
func dimensionFilter(dims []string) string {
	parts := make([]string, len(dims))
	for i, d := range dims {
		parts[i] = d + " eq '*'"
	}
	return strings.Join(parts, " and ")
}

// timespan returns the ISO-8601 "start/end" window: the `timespan` option
// verbatim, else the last hour.
func timespan(opts map[string]any) string {
	if ts := stringOpt(opts, "timespan"); ts != "" {
		return ts
	}
	end := time.Now().UTC()
	start := end.Add(-defaultWindow)
	return start.Format(time.RFC3339) + "/" + end.Format(time.RFC3339)
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

func timeVal(t time.Time) engine.Value {
	if t.IsZero() {
		return engine.Null()
	}
	return engine.TimeVal(t)
}

func floatVal(f *float64) engine.Value {
	if f == nil {
		return engine.Null()
	}
	return engine.FloatVal(*f)
}

// subscriptionFromURI extracts the subscription GUID from an ARM resource ID.
func subscriptionFromURI(uri string) string {
	parts := strings.Split(strings.TrimPrefix(uri, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if strings.EqualFold(parts[i], "subscriptions") {
			return parts[i+1]
		}
	}
	return ""
}
