// Package trelloc is the Trello connector. It queries the Trello REST API and
// exposes a fixed set of datasets — boards, lists, cards, members — flattened
// into typed rows, mirroring the Linear connector.
//
// Trello is reached through a narrow interface (trelloAPI) so tests inject a
// fake without credentials. The real client authenticates with an API key and
// token sent in the Authorization header (not the query string).
//
// Options:
//
//	dataset  one of boards|lists|cards|members; falls back to the dataset
//	         Source (so the qualified ref `trello:boards` works) or Name.
//	key      Trello API key (required).
//	token    Trello API token (required).
//	board    board id; required for lists/cards/members (which are board-scoped).
//	url      override the API base (default https://api.trello.com/1).
package trelloc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

const defaultBase = "https://api.trello.com/1"

// maxItems bounds a single scan to keep memory bounded.
const maxItems = 50000

// trelloAPI is the connector's narrow view of Trello: a GET that returns a JSON
// array of objects for a given path. The real client wraps net/http; tests
// inject a fake.
type trelloAPI interface {
	get(ctx context.Context, path string) ([]map[string]any, error)
}

// Connector queries the Trello REST API.
type Connector struct {
	client trelloAPI // nil until lazily constructed from options
}

// New constructs a Trello connector.
func New() *Connector { return &Connector{} }

// newWithClient returns a Connector backed by an explicit trelloAPI (tests).
func newWithClient(c trelloAPI) *Connector { return &Connector{client: c} }

func (*Connector) Name() string { return "trello" }

func (*Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) { return nil, nil }

func (c *Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	def, err := datasetFor(ds)
	if err != nil {
		return engine.Schema{}, err
	}
	return def.schema(), nil
}

func (c *Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	def, err := datasetFor(req.Dataset)
	if err != nil {
		return nil, err
	}
	path, err := def.buildPath(req.Dataset.Options)
	if err != nil {
		return nil, err
	}
	api, err := c.resolveClient(req.Dataset.Options)
	if err != nil {
		return nil, err
	}

	limit := maxItems
	// Honor a row limit only when no predicate must run first (the engine
	// applies WHERE before LIMIT). Ordering/filtering stays with the engine.
	if req.Predicate == nil && req.Limit != nil && *req.Limit >= 0 && *req.Limit < limit {
		limit = *req.Limit
	}

	items, err := api.get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("trello %s: %w", path, err)
	}
	if limit < len(items) {
		items = items[:limit]
	}
	rows := make([]engine.Row, len(items))
	for i, m := range items {
		vals := make([]engine.Value, len(def.fields))
		for j, f := range def.fields {
			vals[j] = coerce(f.typ, m[f.key])
		}
		rows[i] = engine.Row{Values: vals}
	}
	return engine.NewSliceIter(rows), nil
}

// ---- dataset definitions -----------------------------------------------------

type field struct {
	col string      // output column name
	key string      // Trello JSON property
	typ engine.Type // declared engine type
}

type datasetDef struct {
	name       string
	path       string // REST path; may contain {board}
	needsBoard bool
	fields     []field
}

func (d datasetDef) schema() engine.Schema {
	cols := make([]engine.Column, len(d.fields))
	for i, f := range d.fields {
		cols[i] = engine.Column{Name: f.col, Type: f.typ, Nullable: true}
	}
	return engine.Schema{Columns: cols}
}

// buildPath substitutes the board id into board-scoped paths.
func (d datasetDef) buildPath(opts map[string]any) (string, error) {
	if !d.needsBoard {
		return d.path, nil
	}
	board := stringOpt(opts, "board")
	if board == "" {
		return "", fmt.Errorf("trello dataset %q requires a board option (board id)", d.name)
	}
	return strings.ReplaceAll(d.path, "{board}", board), nil
}

// datasets is the fixed registry of supported Trello datasets.
var datasets = map[string]datasetDef{
	"boards": {
		name: "boards",
		path: "/members/me/boards",
		fields: []field{
			{"id", "id", engine.TypeString},
			{"name", "name", engine.TypeString},
			{"desc", "desc", engine.TypeString},
			{"closed", "closed", engine.TypeBool},
			{"url", "url", engine.TypeString},
			{"id_organization", "idOrganization", engine.TypeString},
			{"date_last_activity", "dateLastActivity", engine.TypeTime},
		},
	},
	"lists": {
		name: "lists", path: "/boards/{board}/lists", needsBoard: true,
		fields: []field{
			{"id", "id", engine.TypeString},
			{"name", "name", engine.TypeString},
			{"closed", "closed", engine.TypeBool},
			{"id_board", "idBoard", engine.TypeString},
			{"pos", "pos", engine.TypeFloat},
		},
	},
	"cards": {
		name: "cards", path: "/boards/{board}/cards", needsBoard: true,
		fields: []field{
			{"id", "id", engine.TypeString},
			{"name", "name", engine.TypeString},
			{"desc", "desc", engine.TypeString},
			{"closed", "closed", engine.TypeBool},
			{"id_board", "idBoard", engine.TypeString},
			{"id_list", "idList", engine.TypeString},
			{"due", "due", engine.TypeTime},
			{"due_complete", "dueComplete", engine.TypeBool},
			{"url", "url", engine.TypeString},
			{"date_last_activity", "dateLastActivity", engine.TypeTime},
			{"pos", "pos", engine.TypeFloat},
		},
	},
	"members": {
		name: "members", path: "/boards/{board}/members", needsBoard: true,
		fields: []field{
			{"id", "id", engine.TypeString},
			{"full_name", "fullName", engine.TypeString},
			{"username", "username", engine.TypeString},
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
		return datasetDef{}, fmt.Errorf("trello: unknown dataset %q (valid: %s)", key, validDatasets())
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

// ---- value coercion ----------------------------------------------------------

// coerce converts a decoded JSON value into an engine.Value of the declared
// type, so a value's runtime type matches its column type. JSON numbers decode
// as float64; Trello timestamps are RFC3339 strings.
func coerce(typ engine.Type, raw any) engine.Value {
	if raw == nil {
		return engine.Null()
	}
	switch typ {
	case engine.TypeInt:
		switch v := raw.(type) {
		case float64:
			return engine.IntVal(int64(v))
		case string:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return engine.IntVal(n)
			}
		}
		return engine.Null()
	case engine.TypeFloat:
		if f, ok := raw.(float64); ok {
			return engine.FloatVal(f)
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

// ---- real Trello client ------------------------------------------------------

func (c *Connector) resolveClient(opts map[string]any) (trelloAPI, error) {
	if c.client != nil {
		return c.client, nil
	}
	key := stringOpt(opts, "key")
	token := stringOpt(opts, "token")
	if key == "" || token == "" {
		return nil, fmt.Errorf("trello connector requires key and token options")
	}
	base := stringOpt(opts, "url")
	if base == "" {
		base = defaultBase
	}
	c.client = &httpClient{
		hc:    &http.Client{Timeout: 30 * time.Second},
		base:  strings.TrimRight(base, "/"),
		key:   key,
		token: token,
	}
	return c.client, nil
}

type httpClient struct {
	hc         *http.Client
	base       string
	key, token string
}

func (h *httpClient) get(ctx context.Context, path string) ([]map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	// Key/token go in the Authorization header, not the query string, to keep
	// the secrets out of URLs (and request logs).
	req.Header.Set("Authorization", fmt.Sprintf(`OAuth oauth_consumer_key="%s", oauth_token="%s"`, h.key, h.token))

	resp, err := h.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	var arr []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&arr); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return arr, nil
}
