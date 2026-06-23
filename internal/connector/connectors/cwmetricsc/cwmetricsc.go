// Package cwmetricsc is the AWS CloudWatch Metrics connector. It retrieves a
// single metric's data points via the CloudWatch GetMetricData API as a
// fixed-schema relation. The AWS client is accessed through a narrow interface
// (metricsAPI) so tests can inject a fake without real credentials.
package cwmetricsc

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

// maxPoints caps the total number of data points buffered for a single scan to
// keep memory bounded when no limit is supplied by the engine.
const maxPoints = 10000

const (
	defaultStat   = "Average"
	defaultPeriod = 300
)

// metricsAPI is the narrow surface of the CloudWatch client this connector
// uses. The real *cloudwatch.Client satisfies it; tests inject a fake.
type metricsAPI interface {
	GetMetricData(ctx context.Context, in *cloudwatch.GetMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error)
}

// Connector implements the CloudWatch Metrics connector.
type Connector struct {
	mu     sync.Mutex
	client metricsAPI // nil until lazily constructed from options
}

// New returns a Connector that lazily builds a real AWS client from the
// dataset's region/profile options on first use.
func New() *Connector { return &Connector{} }

// newWithClient returns a Connector backed by an explicit metricsAPI. Used by
// tests to inject a fake client; no AWS credentials are required.
func newWithClient(c metricsAPI) *Connector { return &Connector{client: c} }

func (*Connector) Name() string { return "cloudwatch" }

func (*Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) { return nil, nil }

// Resolve returns the fixed schema for a CloudWatch metrics dataset.
func (*Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	return schema(), nil
}

func schema() engine.Schema {
	return engine.Schema{Columns: []engine.Column{
		{Name: "timestamp", Type: engine.TypeTime, Nullable: true},
		{Name: "namespace", Type: engine.TypeString, Nullable: true},
		{Name: "metric", Type: engine.TypeString, Nullable: true},
		{Name: "stat", Type: engine.TypeString, Nullable: true},
		{Name: "value", Type: engine.TypeFloat, Nullable: true},
	}}
}

// Scan builds a single MetricDataQuery from the dataset options, calls
// GetMetricData (paginating via NextToken), and emits one row per
// (timestamp, value) pair. Predicate/OrderBy are ignored; the engine applies
// them to the residual rows.
func (c *Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	opts := req.Dataset.Options

	namespace := stringOpt(opts, "namespace")
	if namespace == "" {
		return nil, fmt.Errorf("cloudwatch connector requires namespace option")
	}
	metricName := stringOpt(opts, "metric")
	if metricName == "" {
		return nil, fmt.Errorf("cloudwatch connector requires metric option")
	}
	stat := stringOpt(opts, "stat")
	if stat == "" {
		stat = defaultStat
	}
	period, err := intOpt(opts, "period", defaultPeriod)
	if err != nil {
		return nil, err
	}

	start, end, err := timeRange(opts)
	if err != nil {
		return nil, err
	}

	api, err := c.resolveClient(ctx, opts)
	if err != nil {
		return nil, err
	}

	query := cwtypes.MetricDataQuery{
		Id: aws.String("m1"),
		MetricStat: &cwtypes.MetricStat{
			Metric: &cwtypes.Metric{
				Namespace:  aws.String(namespace),
				MetricName: aws.String(metricName),
				Dimensions: dimensions(opts),
			},
			Period: aws.Int32(int32(period)),
			Stat:   aws.String(stat),
		},
	}

	in := &cloudwatch.GetMetricDataInput{
		MetricDataQueries: []cwtypes.MetricDataQuery{query},
		StartTime:         aws.Time(start),
		EndTime:           aws.Time(end),
	}

	limit := maxPoints
	if req.Predicate == nil && req.Limit != nil && *req.Limit >= 0 && *req.Limit < limit {
		limit = *req.Limit
	}

	rows := make([]engine.Row, 0, 64)
	for len(rows) < limit {
		out, err := api.GetMetricData(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("GetMetricData: %w", err)
		}
		for _, res := range out.MetricDataResults {
			n := len(res.Timestamps)
			if len(res.Values) < n {
				n = len(res.Values)
			}
			for i := 0; i < n; i++ {
				rows = append(rows, engine.Row{Values: []engine.Value{
					engine.TimeVal(res.Timestamps[i].UTC()),
					engine.StringVal(namespace),
					engine.StringVal(metricName),
					engine.StringVal(stat),
					engine.FloatVal(res.Values[i]),
				}})
				if len(rows) >= limit {
					break
				}
			}
			if len(rows) >= limit {
				break
			}
		}
		if out.NextToken == nil || *out.NextToken == "" || len(rows) >= limit {
			break
		}
		in.NextToken = out.NextToken
	}

	return engine.NewSliceIter(rows), nil
}

// dimensions extracts dim_<Name>=<Value> options into CloudWatch dimensions,
// sorted by name for stable, deterministic queries.
func dimensions(opts map[string]any) []cwtypes.Dimension {
	var dims []cwtypes.Dimension
	for k, v := range opts {
		name, ok := strings.CutPrefix(k, "dim_")
		if !ok || name == "" {
			continue
		}
		val, _ := v.(string)
		dims = append(dims, cwtypes.Dimension{
			Name:  aws.String(name),
			Value: aws.String(val),
		})
	}
	sort.Slice(dims, func(i, j int) bool { return *dims[i].Name < *dims[j].Name })
	return dims
}

// timeRange resolves start/end options, defaulting to the last hour when
// absent.
func timeRange(opts map[string]any) (time.Time, time.Time, error) {
	end := time.Now().UTC()
	if v, ok, err := parseTime(opts, "end"); err != nil {
		return time.Time{}, time.Time{}, err
	} else if ok {
		end = v
	}
	start := end.Add(-time.Hour)
	if v, ok, err := parseTime(opts, "start"); err != nil {
		return time.Time{}, time.Time{}, err
	} else if ok {
		start = v
	}
	return start, end, nil
}

// resolveClient returns the injected client if present, else lazily builds a
// real AWS client from region/profile options (cached for reuse).
func (c *Connector) resolveClient(ctx context.Context, opts map[string]any) (metricsAPI, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return c.client, nil
	}
	var loadOpts []func(*config.LoadOptions) error
	if r := stringOpt(opts, "region"); r != "" {
		loadOpts = append(loadOpts, config.WithRegion(r))
	}
	if p := stringOpt(opts, "profile"); p != "" {
		loadOpts = append(loadOpts, config.WithSharedConfigProfile(p))
	}
	cfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	c.client = cloudwatch.NewFromConfig(cfg)
	return c.client, nil
}

// parseTime reads an option as either RFC3339 or unix milliseconds.
func parseTime(opts map[string]any, key string) (time.Time, bool, error) {
	v, present := opts[key]
	if !present {
		return time.Time{}, false, nil
	}
	switch x := v.(type) {
	case nil:
		return time.Time{}, false, nil
	case time.Time:
		return x.UTC(), true, nil
	case int:
		return time.UnixMilli(int64(x)).UTC(), true, nil
	case int64:
		return time.UnixMilli(x).UTC(), true, nil
	case float64:
		return time.UnixMilli(int64(x)).UTC(), true, nil
	case string:
		if x == "" {
			return time.Time{}, false, nil
		}
		if t, err := time.Parse(time.RFC3339, x); err == nil {
			return t.UTC(), true, nil
		}
		if n, err := strconv.ParseInt(x, 10, 64); err == nil {
			return time.UnixMilli(n).UTC(), true, nil
		}
		return time.Time{}, false, fmt.Errorf("invalid %s %q: want RFC3339 or unix millis", key, x)
	default:
		return time.Time{}, false, fmt.Errorf("invalid %s type %T", key, v)
	}
}

// intOpt reads an option as an integer, accepting numeric or string forms.
func intOpt(opts map[string]any, key string, def int) (int, error) {
	v, ok := opts[key]
	if !ok || v == nil {
		return def, nil
	}
	switch x := v.(type) {
	case int:
		return x, nil
	case int64:
		return int(x), nil
	case float64:
		return int(x), nil
	case string:
		if x == "" {
			return def, nil
		}
		n, err := strconv.Atoi(x)
		if err != nil {
			return 0, fmt.Errorf("invalid %s %q: %w", key, x, err)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("invalid %s type %T", key, v)
	}
}

func stringOpt(opts map[string]any, key string) string {
	v, ok := opts[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}
