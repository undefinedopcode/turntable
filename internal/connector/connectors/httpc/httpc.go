// Package httpc is the HTTP/REST connector. It fetches a JSON document from an
// HTTP(S) endpoint and exposes it as a dataset of rows. The response may be a
// top-level array of objects, a single object (one row), or an array nested
// under a dotted path (the "path" option, e.g. "data.items"). Schema is
// inferred from a sample of the records, mirroring the json file connector:
// columns are ordered by sorted key name and typed as `any`.
//
// Options (all optional unless noted):
//
//	url           the endpoint; falls back to the dataset Source
//	path          dotted path to the array within the response (e.g. "data.rows")
//	method        HTTP method (default GET)
//	body          request body sent verbatim
//	bearer        shorthand for an "Authorization: Bearer <v>" header
//	header_<name> sets a request header; underscores become hyphens
//	                (e.g. header_x_api_key=abc -> "X-Api-Key: abc")
package httpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

// Connector fetches JSON over HTTP.
type Connector struct {
	// Client is the HTTP client used for requests. Tests may override it; a nil
	// Client falls back to a default client with a sane timeout.
	Client *http.Client
}

// New constructs an HTTP connector with a default client.
func New() *Connector {
	return &Connector{Client: &http.Client{Timeout: 30 * time.Second}}
}

func (Connector) Name() string { return "http" }

func (Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) { return nil, nil }

func (c Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	records, err := c.fetchRecords(ctx, ds)
	if err != nil {
		return engine.Schema{}, err
	}
	return schemaFromRecords(records), nil
}

func (c Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	records, err := c.fetchRecords(ctx, req.Dataset)
	if err != nil {
		return nil, err
	}
	schema := schemaFromRecords(records)
	// Honor a row limit only when there is no predicate to apply first — the
	// engine applies WHERE before LIMIT, so truncating early under a predicate
	// would be wrong. Ordering/filtering is always left to the engine.
	if req.Limit != nil && req.Predicate == nil && *req.Limit < len(records) {
		records = records[:*req.Limit]
	}
	return &sliceIter{records: records, schema: schema}, nil
}

// ---- fetch + extract ---------------------------------------------------------

func (c Connector) httpClient() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// fetchRecords performs the request and extracts the list of record objects.
func (c Connector) fetchRecords(ctx context.Context, ds connector.Dataset) ([]map[string]any, error) {
	url := stringOpt(ds.Options, "url")
	if url == "" {
		url = ds.Source
	}
	if url == "" {
		return nil, fmt.Errorf("http connector requires a url")
	}
	method := strings.ToUpper(stringOpt(ds.Options, "method"))
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if b := stringOpt(ds.Options, "body"); b != "" {
		body = strings.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	applyHeaders(req, ds.Options)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("http %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("http %s %s: status %d: %s", method, url, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var doc any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return extractRecords(doc, stringOpt(ds.Options, "path"))
}

// applyHeaders sets request headers from options: a "bearer" convenience plus
// any "header_<name>" keys (underscores in <name> become hyphens; net/http
// canonicalizes the casing).
func applyHeaders(req *http.Request, opts map[string]any) {
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	if tok := stringOpt(opts, "bearer"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for k, v := range opts {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if name, found := strings.CutPrefix(strings.ToLower(k), "header_"); found {
			req.Header.Set(strings.ReplaceAll(name, "_", "-"), s)
		}
	}
}

// extractRecords navigates an optional dotted path into the decoded document
// and returns a list of object records. A single object yields one record.
func extractRecords(doc any, path string) ([]map[string]any, error) {
	node := doc
	if path != "" {
		for _, part := range strings.Split(path, ".") {
			m, ok := node.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("path %q: %q is not an object", path, part)
			}
			node, ok = m[part]
			if !ok {
				return nil, fmt.Errorf("path %q: key %q not found", path, part)
			}
		}
	}
	switch v := node.(type) {
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			} else {
				// Non-object array element becomes a single-column row.
				out = append(out, map[string]any{"value": item})
			}
		}
		return out, nil
	case map[string]any:
		return []map[string]any{v}, nil
	case nil:
		return nil, nil
	default:
		return []map[string]any{{"value": v}}, nil
	}
}

// schemaFromRecords infers an `any`-typed, nullable schema from the union of
// keys across the sampled records, ordered for determinism.
func schemaFromRecords(records []map[string]any) engine.Schema {
	seen := map[string]bool{}
	var order []string
	for _, r := range records {
		for k := range r {
			if !seen[k] {
				seen[k] = true
				order = append(order, k)
			}
		}
	}
	sort.Strings(order)
	cols := make([]engine.Column, len(order))
	for i, name := range order {
		cols[i] = engine.Column{Name: name, Type: engine.TypeAny, Nullable: true}
	}
	return engine.Schema{Columns: cols}
}

// ---- iterator ----------------------------------------------------------------

type sliceIter struct {
	records []map[string]any
	schema  engine.Schema
	idx     int
}

func (s *sliceIter) Next() (engine.Row, bool, error) {
	if s.idx >= len(s.records) {
		return engine.Row{}, false, nil
	}
	rec := s.records[s.idx]
	s.idx++
	vals := make([]engine.Value, len(s.schema.Columns))
	for i, c := range s.schema.Columns {
		if v, ok := rec[c.Name]; ok {
			vals[i] = connector.FromAny(v)
		} else {
			vals[i] = engine.Null()
		}
	}
	return engine.Row{Values: vals}, true, nil
}

func (s *sliceIter) Close() error { return nil }

func stringOpt(opts map[string]any, key string) string {
	if v, ok := opts[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
