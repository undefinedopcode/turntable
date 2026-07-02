// Package promc is the Prometheus connector. It evaluates a PromQL expression
// (or a plain metric selector) over a time window via the HTTP API's
// /api/v1/query_range and exposes the resulting series as rows: a `ts`
// timestamp, one string column per label (the sorted union across series;
// missing labels are NULL), and a float `value` — one row per (series, sample).
//
// The schema is response-derived (like the other schemaless connectors):
// Resolve and Scan each run the range query and shape identical schemas from
// it, so a query costs two Prometheus calls (plan + exec). No pushdown — the
// window is bounded by start/end/step and the engine filters/aggregates; use
// PromQL (rate(), sum by (...)) to reduce at the source.
package promc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

// defaultTimeRange is the lookback window when no start/time_range is given.
const defaultTimeRange = time.Hour

// minAutoStep floors the automatic step so short windows don't hammer the
// server; the auto step targets ~250 points across the window (the Prometheus
// UI's own default resolution).
const minAutoStep = 15 * time.Second

// Connector implements the prom connector.
type Connector struct {
	client *http.Client
}

func New() *Connector {
	return &Connector{client: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Connector) Name() string { return "prom" }

func (c *Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) {
	return nil, fmt.Errorf("prom requires a configured source: url (server base URL) plus metric or query (PromQL)")
}

// params are the resolved query parameters for one dataset.
type params struct {
	base   string // server base URL
	query  string // PromQL
	start  time.Time
	end    time.Time
	step   time.Duration
	bearer string
}

func stringOpt(opts map[string]any, key string) string {
	if v, ok := opts[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// parseWhen accepts RFC3339 or unix seconds.
func parseWhen(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if sec, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Unix(int64(sec), 0).UTC(), nil
	}
	return time.Time{}, fmt.Errorf("cannot parse time %q (want RFC3339 or unix seconds)", s)
}

func resolveParams(ds connector.Dataset) (params, error) {
	var p params
	p.base = stringOpt(ds.Options, "url")
	if p.base == "" && (strings.HasPrefix(ds.Source, "http://") || strings.HasPrefix(ds.Source, "https://")) {
		p.base = ds.Source
	}
	if p.base == "" {
		return p, fmt.Errorf("prom source needs a url (the server base URL, e.g. http://localhost:9090)")
	}
	p.query = stringOpt(ds.Options, "query")
	if p.query == "" {
		p.query = stringOpt(ds.Options, "metric")
	}
	if p.query == "" && ds.Source != "" && ds.Source != p.base {
		// A qualified ref (`prom:up`) carries the selector in Source.
		p.query = ds.Source
	}
	if p.query == "" {
		return p, fmt.Errorf("prom source needs a metric (name) or query (PromQL) option")
	}

	p.end = time.Now().UTC()
	if s := stringOpt(ds.Options, "end"); s != "" {
		t, err := parseWhen(s)
		if err != nil {
			return p, fmt.Errorf("end: %w", err)
		}
		p.end = t
	}
	lookback := defaultTimeRange
	if s := stringOpt(ds.Options, "time_range"); s != "" {
		sec, err := strconv.ParseInt(s, 10, 64)
		if err != nil || sec <= 0 {
			return p, fmt.Errorf("time_range must be a positive number of seconds")
		}
		lookback = time.Duration(sec) * time.Second
	}
	p.start = p.end.Add(-lookback)
	if s := stringOpt(ds.Options, "start"); s != "" {
		t, err := parseWhen(s)
		if err != nil {
			return p, fmt.Errorf("start: %w", err)
		}
		p.start = t
	}
	if !p.end.After(p.start) {
		return p, fmt.Errorf("prom window is empty (start %s is not before end %s)", p.start, p.end)
	}

	if s := stringOpt(ds.Options, "step"); s != "" {
		sec, err := strconv.ParseInt(s, 10, 64)
		if err != nil || sec <= 0 {
			return p, fmt.Errorf("step must be a positive number of seconds")
		}
		p.step = time.Duration(sec) * time.Second
	} else {
		// ~250 points across the window, floored.
		p.step = (p.end.Sub(p.start) / 250).Truncate(time.Second)
		if p.step < minAutoStep {
			p.step = minAutoStep
		}
	}
	p.bearer = stringOpt(ds.Options, "bearer")
	return p, nil
}

// series is one time series of a range-query matrix result.
type series struct {
	Metric map[string]string `json:"metric"`
	Values [][]any           `json:"values"` // [[unix-seconds, "value"], ...]
}

// fetch runs the range query and returns the matrix, sorted deterministically
// by label fingerprint.
func (c *Connector) fetch(ctx context.Context, ds connector.Dataset) ([]series, error) {
	p, err := resolveParams(ds)
	if err != nil {
		return nil, err
	}
	q := url.Values{
		"query": {p.query},
		"start": {strconv.FormatInt(p.start.Unix(), 10)},
		"end":   {strconv.FormatInt(p.end.Unix(), 10)},
		"step":  {strconv.FormatInt(int64(p.step/time.Second), 10)},
	}
	u := strings.TrimRight(p.base, "/") + "/api/v1/query_range?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if p.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+p.bearer)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prom query: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("prom response: %w", err)
	}

	var out struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string   `json:"resultType"`
			Result     []series `json:"result"`
		} `json:"data"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("prom query failed: HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
		}
		return nil, fmt.Errorf("prom response: %w", err)
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("prom query failed: %s: %s", out.ErrorType, out.Error)
	}
	if out.Data.ResultType != "matrix" {
		return nil, fmt.Errorf("prom: unexpected result type %q (want matrix)", out.Data.ResultType)
	}

	sort.SliceStable(out.Data.Result, func(i, j int) bool {
		return fingerprint(out.Data.Result[i].Metric) < fingerprint(out.Data.Result[j].Metric)
	})
	return out.Data.Result, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// fingerprint is a deterministic ordering key for a label set.
func fingerprint(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("\x01")
		b.WriteString(m[k])
		b.WriteString("\x01")
	}
	return b.String()
}

// shape derives the schema (ts, labels..., value) and the label key each label
// column reads. A label literally named ts/value is prefixed label_ so the
// fixed columns keep their names.
func shape(result []series) (engine.Schema, []string) {
	set := map[string]bool{}
	for _, s := range result {
		for k := range s.Metric {
			set[k] = true
		}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	cols := make([]engine.Column, 0, len(keys)+2)
	cols = append(cols, engine.Column{Name: "ts", Type: engine.TypeTime, Nullable: false})
	for _, k := range keys {
		name := k
		if name == "ts" || name == "value" {
			name = "label_" + name
		}
		cols = append(cols, engine.Column{Name: name, Type: engine.TypeString, Nullable: true})
	}
	cols = append(cols, engine.Column{Name: "value", Type: engine.TypeFloat, Nullable: true})
	return engine.Schema{Columns: cols}, keys
}

// Resolve runs the range query and shapes the schema from the response.
func (c *Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	result, err := c.fetch(ctx, ds)
	if err != nil {
		return engine.Schema{}, err
	}
	schema, _ := shape(result)
	return schema, nil
}

// Scan runs the range query and flattens the matrix to rows.
func (c *Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	result, err := c.fetch(ctx, req.Dataset)
	if err != nil {
		return nil, err
	}
	schema, keys := shape(result)
	rows := make([]engine.Row, 0, 64)
	for _, s := range result {
		labelVals := make([]engine.Value, len(keys))
		for i, k := range keys {
			if v, ok := s.Metric[k]; ok {
				labelVals[i] = engine.StringVal(v)
			} else {
				labelVals[i] = engine.Null()
			}
		}
		for _, sample := range s.Values {
			if len(sample) != 2 {
				continue
			}
			sec, ok := sample[0].(float64)
			if !ok {
				continue
			}
			vals := make([]engine.Value, 0, len(schema.Columns))
			vals = append(vals, engine.TimeVal(time.Unix(int64(sec), int64((sec-math.Floor(sec))*1e9)).UTC()))
			vals = append(vals, labelVals...)
			vals = append(vals, sampleValue(sample[1]))
			rows = append(rows, engine.Row{Values: vals})
		}
	}
	return engine.NewSliceIter(rows), nil
}

// sampleValue parses Prometheus's stringified sample value; NaN/±Inf (staleness
// markers, division artifacts) become NULL so they don't poison aggregates.
func sampleValue(v any) engine.Value {
	s, ok := v.(string)
	if !ok {
		return engine.Null()
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return engine.Null()
	}
	return engine.FloatVal(f)
}
