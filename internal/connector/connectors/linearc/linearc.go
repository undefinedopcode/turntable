// Package linearc is the Linear connector. It queries the Linear GraphQL API
// (https://api.linear.app/graphql) and exposes a fixed set of datasets —
// issues, teams, projects, users — as flat, typed rows. Nested fields (issue
// state, assignee, team) are flattened into dotted-free column names.
//
// Options:
//
//	dataset   one of issues|teams|projects|users; falls back to the dataset
//	          Source (so the qualified ref `linear:issues` works) or Name.
//	api_key   a Linear personal API key, sent as the raw Authorization header.
//	bearer    an OAuth access token, sent as "Authorization: Bearer <v>".
//	url       override the GraphQL endpoint (default the public API).
//
// Either api_key or bearer is required.
package linearc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

const defaultEndpoint = "https://api.linear.app/graphql"

// pageSize is the GraphQL `first:` page size. maxNodes bounds a single scan so
// a runaway query cannot exhaust memory.
const (
	pageSize = 100
	maxNodes = 50000
)

// Connector queries the Linear GraphQL API.
type Connector struct {
	// Client is the HTTP client used for requests (overridable in tests).
	Client *http.Client
}

// New constructs a Linear connector with a default client.
func New() *Connector {
	return &Connector{Client: &http.Client{Timeout: 30 * time.Second}}
}

func (Connector) Name() string { return "linear" }

func (Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) { return nil, nil }

func (c Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	def, err := datasetFor(ds)
	if err != nil {
		return engine.Schema{}, err
	}
	return def.schema(), nil
}

func (c Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	def, err := datasetFor(req.Dataset)
	if err != nil {
		return nil, err
	}
	limit := maxNodes
	// Honor a row limit only when no predicate must run first (the engine
	// applies WHERE before LIMIT). Ordering/filtering stays with the engine.
	if req.Limit != nil && req.Predicate == nil && *req.Limit < limit {
		limit = *req.Limit
	}
	nodes, err := c.fetchAll(ctx, req.Dataset, def, limit)
	if err != nil {
		return nil, err
	}
	return &nodeIter{nodes: nodes, def: def}, nil
}

// ---- dataset definitions -----------------------------------------------------

type field struct {
	col  string      // output column name
	path []string    // path into the GraphQL node (e.g. ["state","name"])
	typ  engine.Type // declared engine type
}

type datasetDef struct {
	root   string // GraphQL connection field (e.g. "issues")
	selSet string // GraphQL selection inside nodes { ... }
	fields []field
}

func (d datasetDef) schema() engine.Schema {
	cols := make([]engine.Column, len(d.fields))
	for i, f := range d.fields {
		cols[i] = engine.Column{Name: f.col, Type: f.typ, Nullable: true}
	}
	return engine.Schema{Columns: cols}
}

// datasets is the fixed registry of supported Linear datasets.
var datasets = map[string]datasetDef{
	"issues": {
		root:   "issues",
		selSet: "id identifier number title description priority priorityLabel estimate createdAt updatedAt startedAt completedAt canceledAt dueDate url state { name type } assignee { name } creator { name } team { key } project { name } cycle { number } parent { identifier }",
		fields: []field{
			{"id", []string{"id"}, engine.TypeString},
			{"identifier", []string{"identifier"}, engine.TypeString},
			{"number", []string{"number"}, engine.TypeInt},
			{"title", []string{"title"}, engine.TypeString},
			{"description", []string{"description"}, engine.TypeString},
			{"priority", []string{"priority"}, engine.TypeInt},
			{"priority_label", []string{"priorityLabel"}, engine.TypeString},
			{"estimate", []string{"estimate"}, engine.TypeFloat},
			{"state", []string{"state", "name"}, engine.TypeString},
			{"state_type", []string{"state", "type"}, engine.TypeString},
			{"assignee", []string{"assignee", "name"}, engine.TypeString},
			{"creator", []string{"creator", "name"}, engine.TypeString},
			{"team", []string{"team", "key"}, engine.TypeString},
			{"project", []string{"project", "name"}, engine.TypeString},
			{"cycle", []string{"cycle", "number"}, engine.TypeInt},
			{"parent", []string{"parent", "identifier"}, engine.TypeString},
			{"created_at", []string{"createdAt"}, engine.TypeTime},
			{"updated_at", []string{"updatedAt"}, engine.TypeTime},
			{"started_at", []string{"startedAt"}, engine.TypeTime},
			{"completed_at", []string{"completedAt"}, engine.TypeTime},
			{"canceled_at", []string{"canceledAt"}, engine.TypeTime},
			{"due_date", []string{"dueDate"}, engine.TypeString},
			{"url", []string{"url"}, engine.TypeString},
		},
	},
	"teams": {
		root:   "teams",
		selSet: "id key name description private color timezone createdAt",
		fields: []field{
			{"id", []string{"id"}, engine.TypeString},
			{"key", []string{"key"}, engine.TypeString},
			{"name", []string{"name"}, engine.TypeString},
			{"description", []string{"description"}, engine.TypeString},
			{"private", []string{"private"}, engine.TypeBool},
			{"color", []string{"color"}, engine.TypeString},
			{"timezone", []string{"timezone"}, engine.TypeString},
			{"created_at", []string{"createdAt"}, engine.TypeTime},
		},
	},
	"projects": {
		root:   "projects",
		selSet: "id name description state health progress url startDate targetDate startedAt completedAt createdAt lead { name }",
		fields: []field{
			{"id", []string{"id"}, engine.TypeString},
			{"name", []string{"name"}, engine.TypeString},
			{"description", []string{"description"}, engine.TypeString},
			{"state", []string{"state"}, engine.TypeString},
			{"health", []string{"health"}, engine.TypeString},
			{"progress", []string{"progress"}, engine.TypeFloat},
			{"lead", []string{"lead", "name"}, engine.TypeString},
			{"url", []string{"url"}, engine.TypeString},
			{"start_date", []string{"startDate"}, engine.TypeString},
			{"target_date", []string{"targetDate"}, engine.TypeString},
			{"started_at", []string{"startedAt"}, engine.TypeTime},
			{"completed_at", []string{"completedAt"}, engine.TypeTime},
			{"created_at", []string{"createdAt"}, engine.TypeTime},
		},
	},
	"users": {
		root:   "users",
		selSet: "id name displayName email active admin guest timezone lastSeen url createdAt",
		fields: []field{
			{"id", []string{"id"}, engine.TypeString},
			{"name", []string{"name"}, engine.TypeString},
			{"display_name", []string{"displayName"}, engine.TypeString},
			{"email", []string{"email"}, engine.TypeString},
			{"active", []string{"active"}, engine.TypeBool},
			{"admin", []string{"admin"}, engine.TypeBool},
			{"guest", []string{"guest"}, engine.TypeBool},
			{"timezone", []string{"timezone"}, engine.TypeString},
			{"last_seen", []string{"lastSeen"}, engine.TypeTime},
			{"url", []string{"url"}, engine.TypeString},
			{"created_at", []string{"createdAt"}, engine.TypeTime},
		},
	},
}

func datasetFor(ds connector.Dataset) (datasetDef, error) {
	key := stringOpt(ds.Options, "dataset")
	if key == "" {
		key = ds.Source
	}
	if key == "" {
		key = ds.Name
	}
	def, ok := datasets[strings.ToLower(strings.TrimSpace(key))]
	if !ok {
		return datasetDef{}, fmt.Errorf("linear: unknown dataset %q (valid: %s)", key, validDatasets())
	}
	return def, nil
}

func validDatasets() string {
	names := make([]string, 0, len(datasets))
	for k := range datasets {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// ---- GraphQL fetch -----------------------------------------------------------

type gqlResponse struct {
	Data   map[string]json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type connectionPage struct {
	Nodes    []map[string]any `json:"nodes"`
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
}

func (c Connector) httpClient() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// fetchAll pages through the connection until exhausted or limit nodes gathered.
func (c Connector) fetchAll(ctx context.Context, ds connector.Dataset, def datasetDef, limit int) ([]map[string]any, error) {
	endpoint := stringOpt(ds.Options, "url")
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	query := fmt.Sprintf(
		"query($first:Int,$after:String){%s(first:$first,after:$after){nodes{%s} pageInfo{hasNextPage endCursor}}}",
		def.root, def.selSet,
	)
	var out []map[string]any
	var after *string
	for {
		first := pageSize
		if rem := limit - len(out); rem < first {
			first = rem
		}
		if first <= 0 {
			break
		}
		vars := map[string]any{"first": first}
		if after != nil {
			vars["after"] = *after
		}
		page, err := c.post(ctx, endpoint, ds.Options, query, vars, def.root)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Nodes...)
		if !page.PageInfo.HasNextPage || page.PageInfo.EndCursor == "" || len(out) >= limit {
			break
		}
		cursor := page.PageInfo.EndCursor
		after = &cursor
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (c Connector) post(ctx context.Context, endpoint string, opts map[string]any, query string, vars map[string]any, root string) (*connectionPage, error) {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key := stringOpt(opts, "api_key"); key != "" {
		req.Header.Set("Authorization", key)
	} else if tok := stringOpt(opts, "bearer"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	} else {
		return nil, fmt.Errorf("linear: api_key or bearer option is required")
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("linear request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("linear: status %d", resp.StatusCode)
	}

	var gr gqlResponse
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return nil, fmt.Errorf("linear: decode response: %w", err)
	}
	if len(gr.Errors) > 0 {
		return nil, fmt.Errorf("linear: %s", gr.Errors[0].Message)
	}
	raw, ok := gr.Data[root]
	if !ok {
		return nil, fmt.Errorf("linear: response missing %q field", root)
	}
	var page connectionPage
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, fmt.Errorf("linear: decode %q: %w", root, err)
	}
	return &page, nil
}

// ---- iterator ----------------------------------------------------------------

type nodeIter struct {
	nodes []map[string]any
	def   datasetDef
	idx   int
}

func (n *nodeIter) Next() (engine.Row, bool, error) {
	if n.idx >= len(n.nodes) {
		return engine.Row{}, false, nil
	}
	node := n.nodes[n.idx]
	n.idx++
	vals := make([]engine.Value, len(n.def.fields))
	for i, f := range n.def.fields {
		vals[i] = coerce(f.typ, navigate(node, f.path))
	}
	return engine.Row{Values: vals}, true, nil
}

func (n *nodeIter) Close() error { return nil }

// navigate walks a dotted path into a nested object, returning nil if any
// segment is missing or not an object.
func navigate(node map[string]any, path []string) any {
	var cur any = node
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

// coerce converts a decoded JSON value into an engine.Value of the declared
// type, so the value's runtime type always matches its column type. JSON
// numbers decode as float64, so ints are narrowed here.
func coerce(typ engine.Type, raw any) engine.Value {
	if raw == nil {
		return engine.Null()
	}
	switch typ {
	case engine.TypeInt:
		switch v := raw.(type) {
		case float64:
			return engine.IntVal(int64(v))
		case int64:
			return engine.IntVal(v)
		case string:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return engine.IntVal(n)
			}
		}
		return engine.Null()
	case engine.TypeFloat:
		switch v := raw.(type) {
		case float64:
			return engine.FloatVal(v)
		case int64:
			return engine.FloatVal(float64(v))
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

func stringOpt(opts map[string]any, key string) string {
	if v, ok := opts[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
