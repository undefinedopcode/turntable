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
	batchLimit         = 50 // max resources per Metrics Batch API call
)

// metricQuery is a normalized metrics request handed to the client.
type metricQuery struct {
	metricNames []string
	namespace   string
	// aggregations are the lower-case Azure aggregations requested
	// (average/total/minimum/maximum/count). A single Azure query returns every
	// requested aggregation per point at no extra cost, so several may be asked
	// for at once; the connector emits one row per (point, aggregation).
	aggregations []string
	interval     string
	timespan     string // "startISO/endISO"
	filter       string // dimension $filter, e.g. "node eq '*'"
}

// metricSeries is one returned time series: a metric, its unit, its dimension
// values (from the response metadata), and its points. In batch mode `resource`
// names the resource the series came from (empty in per-resource mode — the
// connector fills the column from the single resource option).
type metricSeries struct {
	resource   string
	metric     string
	unit       string
	dimensions map[string]string
	points     []metricPoint
}

type metricPoint struct {
	ts   time.Time
	vals map[string]*float64 // value per requested aggregation (lower-case key); nil/absent = no data
}

// metricsAPI is the connector's narrow view of Azure Monitor metrics: a
// per-resource query (armmonitor) and a batch query for many resources in one
// region+subscription (azmetrics). Tests inject a fake.
type metricsAPI interface {
	list(ctx context.Context, resourceURI string, q metricQuery) ([]metricSeries, error)
	listBatch(ctx context.Context, subscription, region string, resourceIDs []string, q metricQuery) ([]metricSeries, error)
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
		{Name: "unit", Type: engine.TypeString, Nullable: true},
		{Name: "aggregation", Type: engine.TypeString, Nullable: true},
		{Name: "value", Type: engine.TypeFloat, Nullable: true},
	}
	for _, d := range dimensions(ds.Options) {
		cols = append(cols, engine.Column{Name: d, Type: engine.TypeString, Nullable: true})
	}
	return engine.Schema{Columns: cols}, nil
}

// Scan builds a metricQuery from the options and calls the API — the per-resource
// path (one `resource`) or the batch path (a `resources` list, up to 50 per call
// in one region+subscription). It emits one row per (series, point).
// Predicate/OrderBy are ignored; the engine applies them.
func (c *Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	opts := req.Dataset.Options

	metricNames := splitList(stringOpt(opts, "metric"))
	if len(metricNames) == 0 {
		return nil, fmt.Errorf("azmetrics connector requires a metric option")
	}
	aggregations := splitList(stringOpt(opts, "aggregation"))
	if len(aggregations) == 0 {
		aggregations = []string{defaultAggregation}
	}
	for i, a := range aggregations {
		aggregations[i] = strings.ToLower(a)
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
		metricNames:  metricNames,
		namespace:    stringOpt(opts, "namespace"),
		aggregations: aggregations,
		interval:     interval,
		timespan:     timespan(opts),
		filter:       filter,
	}

	api, err := c.resolveClient()
	if err != nil {
		return nil, err
	}

	limit := maxPoints
	if req.Predicate == nil && req.Limit != nil && *req.Limit >= 0 && *req.Limit < limit {
		limit = *req.Limit
	}

	// Batch mode: a `resources` list queries many resources in one region+sub.
	if batch := splitList(stringOpt(opts, "resources")); len(batch) > 0 {
		region := stringOpt(opts, "region")
		if region == "" {
			return nil, fmt.Errorf("azmetrics batch mode (resources=…) requires a region option (the metrics data-plane region, e.g. eastus)")
		}
		sub := subscriptionFromURI(batch[0])
		if sub == "" {
			return nil, fmt.Errorf("azmetrics: resources must be full ARM resource IDs containing /subscriptions/<id>/")
		}
		var series []metricSeries
		for _, chunk := range chunk(batch, batchLimit) {
			s, err := api.listBatch(ctx, sub, region, chunk, q)
			if err != nil {
				return nil, fmt.Errorf("azmetrics batch: %w", err)
			}
			series = append(series, s...)
		}
		return engine.NewSliceIter(buildRows(series, "", aggregations, dims, limit)), nil
	}

	// Per-resource mode.
	resource := stringOpt(opts, "resource")
	if resource == "" {
		resource = req.Dataset.Source // allow the ARM ID via the ref source too
	}
	if resource == "" {
		return nil, fmt.Errorf("azmetrics connector requires a resource option (ARM resource ID), or resources=… for batch mode")
	}
	series, err := api.list(ctx, resource, q)
	if err != nil {
		return nil, fmt.Errorf("azmetrics %s: %w", resource, err)
	}
	return engine.NewSliceIter(buildRows(series, resource, aggregations, dims, limit)), nil
}

// buildRows flattens series into rows [timestamp, resource, metric, unit,
// aggregation, value, dims…]. Each point yields one row per requested
// aggregation (a single Azure query returns them all). The resource column is the
// series' own resource (batch mode) or defaultResource (per-resource mode).
func buildRows(series []metricSeries, defaultResource string, aggregations, dims []string, limit int) []engine.Row {
	var rows []engine.Row
	for _, s := range series {
		resource := s.resource
		if resource == "" {
			resource = defaultResource
		}
		for _, p := range s.points {
			for _, agg := range aggregations {
				if len(rows) >= limit {
					return rows
				}
				row := []engine.Value{
					timeVal(p.ts),
					engine.StringVal(resource),
					engine.StringVal(s.metric),
					stringOrNull(s.unit),
					engine.StringVal(agg),
					floatVal(p.vals[agg]),
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
	}
	return rows
}

// chunk splits ids into slices of at most n.
func chunk(ids []string, n int) [][]string {
	var out [][]string
	for i := 0; i < len(ids); i += n {
		end := i + n
		if end > len(ids) {
			end = len(ids)
		}
		out = append(out, ids[i:end])
	}
	return out
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

func stringOrNull(s string) engine.Value {
	if s == "" {
		return engine.Null()
	}
	return engine.StringVal(s)
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
