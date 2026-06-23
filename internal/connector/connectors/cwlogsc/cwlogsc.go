// Package cwlogsc is the AWS CloudWatch Logs connector. It streams log events
// from a single log group via the CloudWatch Logs FilterLogEvents API as a
// fixed-schema relation. The AWS client is accessed through a narrow interface
// (logsAPI) so tests can inject a fake without real credentials.
package cwlogsc

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

// maxEvents caps the total number of events buffered for a single scan to keep
// memory bounded when no limit is supplied by the engine.
const maxEvents = 10000

// logsAPI is the narrow surface of the CloudWatch Logs client this connector
// uses. The real *cloudwatchlogs.Client satisfies it; tests inject a fake.
type logsAPI interface {
	FilterLogEvents(ctx context.Context, in *cloudwatchlogs.FilterLogEventsInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error)
}

// Connector implements the CloudWatch Logs connector.
type Connector struct {
	mu     sync.Mutex
	client logsAPI // nil until lazily constructed from options
}

// New returns a Connector that lazily builds a real AWS client from the
// dataset's region/profile options on first use.
func New() *Connector { return &Connector{} }

// newWithClient returns a Connector backed by an explicit logsAPI. Used by
// tests to inject a fake client; no AWS credentials are required.
func newWithClient(c logsAPI) *Connector { return &Connector{client: c} }

func (*Connector) Name() string { return "cloudwatchlogs" }

func (*Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) { return nil, nil }

// Resolve returns the fixed schema for a CloudWatch Logs dataset.
func (*Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	return schema(), nil
}

func schema() engine.Schema {
	return engine.Schema{Columns: []engine.Column{
		{Name: "timestamp", Type: engine.TypeTime, Nullable: true},
		{Name: "message", Type: engine.TypeString, Nullable: true},
		{Name: "log_stream", Type: engine.TypeString, Nullable: true},
		{Name: "event_id", Type: engine.TypeString, Nullable: true},
		{Name: "ingestion_time", Type: engine.TypeTime, Nullable: true},
	}}
}

// Scan fetches matching log events and returns them as an in-memory iterator.
// Pagination is followed via NextToken up to maxEvents (or req.Limit when no
// predicate is pushed down). Predicate/OrderBy are ignored; the engine applies
// them to the residual rows.
func (c *Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	opts := req.Dataset.Options

	logGroup := stringOpt(opts, "log_group")
	if logGroup == "" {
		logGroup = req.Dataset.Source
	}
	if logGroup == "" {
		return nil, fmt.Errorf("cloudwatchlogs connector requires log_group option")
	}

	api, err := c.resolveClient(ctx, opts)
	if err != nil {
		return nil, err
	}

	in := &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName: aws.String(logGroup),
	}
	if pat := stringOpt(opts, "filter"); pat != "" {
		in.FilterPattern = aws.String(pat)
	}
	if start, ok, err := parseTimeMillis(opts, "start"); err != nil {
		return nil, err
	} else if ok {
		in.StartTime = aws.Int64(start)
	}
	if end, ok, err := parseTimeMillis(opts, "end"); err != nil {
		return nil, err
	} else if ok {
		in.EndTime = aws.Int64(end)
	}

	// Honor Limit only when no predicate is pushed (the engine applies the
	// predicate to the full result otherwise).
	limit := maxEvents
	if req.Predicate == nil && req.Limit != nil && *req.Limit >= 0 && *req.Limit < limit {
		limit = *req.Limit
	}

	rows := make([]engine.Row, 0, 64)
	for len(rows) < limit {
		out, err := api.FilterLogEvents(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("FilterLogEvents: %w", err)
		}
		for _, ev := range out.Events {
			rows = append(rows, eventToRow(ev.Timestamp, ev.Message, ev.LogStreamName, ev.EventId, ev.IngestionTime))
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

func eventToRow(ts, msg, stream, id, ingest any) engine.Row {
	return engine.Row{Values: []engine.Value{
		millisToTimeVal(ts.(*int64)),
		strPtrVal(msg.(*string)),
		strPtrVal(stream.(*string)),
		strPtrVal(id.(*string)),
		millisToTimeVal(ingest.(*int64)),
	}}
}

func millisToTimeVal(ms *int64) engine.Value {
	if ms == nil {
		return engine.Null()
	}
	return engine.TimeVal(time.UnixMilli(*ms).UTC())
}

func strPtrVal(s *string) engine.Value {
	if s == nil {
		return engine.Null()
	}
	return engine.StringVal(*s)
}

// resolveClient returns the injected client if present, else lazily builds a
// real AWS client from region/profile options (cached for reuse).
func (c *Connector) resolveClient(ctx context.Context, opts map[string]any) (logsAPI, error) {
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
	c.client = cloudwatchlogs.NewFromConfig(cfg)
	return c.client, nil
}

// parseTimeMillis reads an option as either RFC3339 or unix milliseconds and
// returns the value as unix millis. ok is false when the option is absent.
func parseTimeMillis(opts map[string]any, key string) (int64, bool, error) {
	v, present := opts[key]
	if !present {
		return 0, false, nil
	}
	switch x := v.(type) {
	case nil:
		return 0, false, nil
	case int:
		return int64(x), true, nil
	case int64:
		return x, true, nil
	case float64:
		return int64(x), true, nil
	case time.Time:
		return x.UnixMilli(), true, nil
	case string:
		if x == "" {
			return 0, false, nil
		}
		if t, err := time.Parse(time.RFC3339, x); err == nil {
			return t.UnixMilli(), true, nil
		}
		if n, err := strconv.ParseInt(x, 10, 64); err == nil {
			return n, true, nil
		}
		return 0, false, fmt.Errorf("invalid %s %q: want RFC3339 or unix millis", key, x)
	default:
		return 0, false, fmt.Errorf("invalid %s type %T", key, v)
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
