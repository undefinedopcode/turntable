// Package azdevopsc is the Azure DevOps Boards connector. It exposes a project's
// work items (the Boards backlog/board items) as a single dataset, "work_items",
// of flattened typed rows.
//
// The Azure DevOps work-item API is two-step: a WIQL query returns matching work
// item IDs, then a batch endpoint returns each item's fields. Both calls happen
// behind a narrow interface (devopsAPI) so tests inject a fake without
// credentials. The real client authenticates with a Personal Access Token (PAT)
// via HTTP Basic auth.
//
// Options:
//
//	dataset       must be "work_items" (or empty); the connector has one dataset.
//	organization  Azure DevOps organization (required). Either the bare slug
//	              ("myorg") or a full URL ("https://dev.azure.com/myorg").
//	project       team project (required).
//	pat           personal access token (required).
//	type          optional System.WorkItemType filter for the default query
//	              (e.g. "Bug", "User Story").
//	wiql          optional full WIQL query, overriding the default. Must SELECT
//	              from workitems (flat list), e.g.
//	              "SELECT [System.Id] FROM workitems WHERE [System.State] = 'Active'".
//	url           override the API base (default https://dev.azure.com).
//
// WIQL caps results at 20000 server-side; the connector always sends $top to
// stay within it (at the query's LIMIT when that can be pushed, else 20000),
// taking the most-recently-changed items. A project with more than 20000
// relevant items should narrow server-side via the type filter or a custom
// wiql WHERE clause, since a SQL WHERE filters only the fetched window.
package azdevopsc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

const (
	defaultBase = "https://dev.azure.com"
	apiVersion  = "7.0"
	// batchSize is the max work item IDs per work-items fetch (API limit is 200).
	batchSize = 200
	// wiqlTopMax is the WIQL endpoint's hard result cap. A query that matches
	// more rows than this fails with HTTP 400 ("number of work items returned
	// exceeded limit of 20000"), so we always send $top to cap it server-side.
	wiqlTopMax = 20000
	// maxItems bounds a single scan to keep memory bounded.
	maxItems = wiqlTopMax
)

// devopsAPI is the connector's narrow view of Azure DevOps: run a WIQL query and
// return up to max work items as flattened field maps (each includes "id"). The
// real client performs the WIQL + batch-fetch round trips; tests inject a fake.
type devopsAPI interface {
	queryWorkItems(ctx context.Context, wiql string, fields []string, max int) ([]map[string]any, error)
}

// Connector queries Azure DevOps Boards work items.
type Connector struct {
	client devopsAPI // nil until lazily constructed from options
}

func New() *Connector { return &Connector{} }

// newWithClient returns a Connector backed by an explicit devopsAPI (tests).
func newWithClient(c devopsAPI) *Connector { return &Connector{client: c} }

func (*Connector) Name() string { return "azuredevops" }

func (*Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) { return nil, nil }

func (c *Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	if err := checkDataset(ds); err != nil {
		return engine.Schema{}, err
	}
	return schema(), nil
}

func (c *Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	if err := checkDataset(req.Dataset); err != nil {
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

	wiql := buildWIQL(req.Dataset.Options)
	items, err := api.queryWorkItems(ctx, wiql, requestedFields(), limit)
	if err != nil {
		return nil, fmt.Errorf("azuredevops query: %w", err)
	}
	rows := make([]engine.Row, len(items))
	for i, m := range items {
		vals := make([]engine.Value, len(fields))
		for j, f := range fields {
			vals[j] = coerce(f.typ, navigate(m, f.path))
		}
		rows[i] = engine.Row{Values: vals}
	}
	return engine.NewSliceIter(rows), nil
}

// ---- dataset / schema --------------------------------------------------------

// field maps an output column to a path into the work item's flattened map. The
// first path element is the full field key (which itself contains dots, e.g.
// "System.AssignedTo"); later elements navigate into nested objects.
type field struct {
	col  string
	path []string
	typ  engine.Type
}

var fields = []field{
	{"id", []string{"id"}, engine.TypeInt},
	{"title", []string{"System.Title"}, engine.TypeString},
	{"work_item_type", []string{"System.WorkItemType"}, engine.TypeString},
	{"state", []string{"System.State"}, engine.TypeString},
	{"assigned_to", []string{"System.AssignedTo", "displayName"}, engine.TypeString},
	{"area_path", []string{"System.AreaPath"}, engine.TypeString},
	{"iteration_path", []string{"System.IterationPath"}, engine.TypeString},
	{"tags", []string{"System.Tags"}, engine.TypeString},
	{"priority", []string{"Microsoft.VSTS.Common.Priority"}, engine.TypeInt},
	{"created_date", []string{"System.CreatedDate"}, engine.TypeTime},
	{"changed_date", []string{"System.ChangedDate"}, engine.TypeTime},
}

func schema() engine.Schema {
	cols := make([]engine.Column, len(fields))
	for i, f := range fields {
		cols[i] = engine.Column{Name: f.col, Type: f.typ, Nullable: true}
	}
	return engine.Schema{Columns: cols}
}

// requestedFields is the set of work item field keys to ask the API for (the
// top path element of each column, excluding the synthetic "id").
func requestedFields() []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range fields {
		key := f.path[0]
		if key == "id" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

func checkDataset(ds connector.Dataset) error {
	name := stringOpt(ds.Options, "dataset")
	if name == "" {
		name = ds.Source
	}
	if name == "" {
		name = ds.Name
	}
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "work_items", "workitems":
		return nil
	default:
		return fmt.Errorf("azuredevops: unknown dataset %q (only work_items)", name)
	}
}

// buildWIQL returns the WIQL query: the caller-supplied "wiql" option verbatim,
// or a default selecting the project's work items (optionally filtered by type),
// newest first. @project resolves from the project in the request URL.
func buildWIQL(opts map[string]any) string {
	if q := stringOpt(opts, "wiql"); q != "" {
		return q
	}
	var b strings.Builder
	b.WriteString("SELECT [System.Id] FROM workitems WHERE [System.TeamProject] = @project")
	if typ := stringOpt(opts, "type"); typ != "" {
		fmt.Fprintf(&b, " AND [System.WorkItemType] = '%s'", strings.ReplaceAll(typ, "'", "''"))
	}
	b.WriteString(" ORDER BY [System.ChangedDate] DESC")
	return b.String()
}

// ---- value helpers -----------------------------------------------------------

// navigate walks a path into a nested map, returning nil if any segment is
// missing or not an object.
func navigate(m map[string]any, path []string) any {
	var cur any = m
	for _, p := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[p]
	}
	return cur
}

// coerce converts a decoded JSON value into an engine.Value of the declared
// type so a value's runtime type matches its column type. JSON numbers decode as
// float64; Azure DevOps dates are RFC3339 strings.
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

// ---- real Azure DevOps client ------------------------------------------------

func (c *Connector) resolveClient(opts map[string]any) (devopsAPI, error) {
	if c.client != nil {
		return c.client, nil
	}
	org := stringOpt(opts, "organization")
	project := stringOpt(opts, "project")
	pat := stringOpt(opts, "pat")
	if org == "" || project == "" || pat == "" {
		return nil, fmt.Errorf("azuredevops connector requires organization, project, and pat options")
	}
	base := stringOpt(opts, "url")
	if base == "" {
		base = defaultBase
	}
	c.client = &httpClient{
		hc:      &http.Client{Timeout: 60 * time.Second},
		base:    strings.TrimRight(base, "/"),
		org:     normalizeOrg(org),
		project: project,
		// PAT auth is HTTP Basic with an empty username and the PAT as password.
		auth: "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+pat)),
	}
	return c.client, nil
}

// normalizeOrg accepts either a bare organization slug ("myorg") or a full
// Azure DevOps URL ("https://dev.azure.com/myorg") and returns just the slug.
// Without this, a URL-valued organization puts "https:" into the request path,
// which Azure's front end rejects with HTTP 400 "a potentially dangerous
// Request.Path was detected from the client (:)".
func normalizeOrg(org string) string {
	org = strings.TrimSpace(org)
	if i := strings.Index(org, "://"); i >= 0 {
		org = org[i+3:] // drop scheme
	}
	org = strings.TrimPrefix(org, "dev.azure.com/")
	// Keep only the first path segment (the org slug); drop any trailing path.
	if i := strings.IndexByte(org, '/'); i >= 0 {
		org = org[:i]
	}
	return org
}

type httpClient struct {
	hc           *http.Client
	base         string
	org, project string
	auth         string
}

// orgPath / projectPath escape the org and project as single URL path segments
// so spaces and special characters never reach Azure as raw path bytes.
func (h *httpClient) orgPath() string     { return url.PathEscape(h.org) }
func (h *httpClient) projectPath() string { return url.PathEscape(h.project) }

// queryWorkItems runs the WIQL query, caps the IDs to max, then batch-fetches
// the requested fields. Returned maps are each work item's "fields" object plus
// a top-level "id".
func (h *httpClient) queryWorkItems(ctx context.Context, wiql string, reqFields []string, max int) ([]map[string]any, error) {
	// 1. WIQL -> work item references. The WIQL is project-scoped. $top caps the
	// result server-side: at the engine's row limit when it can be pushed, else
	// at the WIQL hard cap so a large project doesn't fail the whole query.
	top := wiqlTopMax
	if max > 0 && max < top {
		top = max
	}
	wiqlURL := fmt.Sprintf("%s/%s/%s/_apis/wit/wiql?api-version=%s&$top=%d",
		h.base, h.orgPath(), h.projectPath(), apiVersion, top)
	body, _ := json.Marshal(map[string]string{"query": wiql})
	var wiqlResp struct {
		WorkItems []struct {
			ID int `json:"id"`
		} `json:"workItems"`
	}
	if err := h.do(ctx, http.MethodPost, wiqlURL, body, &wiqlResp); err != nil {
		return nil, fmt.Errorf("wiql: %w", err)
	}
	ids := make([]int, 0, len(wiqlResp.WorkItems))
	for _, w := range wiqlResp.WorkItems {
		ids = append(ids, w.ID)
		if len(ids) >= max {
			break
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}

	// 2. Batch-fetch fields, chunked to the API's per-call limit.
	var out []map[string]any
	for start := 0; start < len(ids); start += batchSize {
		end := start + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		items, err := h.fetchBatch(ctx, ids[start:end], reqFields)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

func (h *httpClient) fetchBatch(ctx context.Context, ids []int, reqFields []string) ([]map[string]any, error) {
	idStrs := make([]string, len(ids))
	for i, id := range ids {
		idStrs[i] = strconv.Itoa(id)
	}
	u := fmt.Sprintf("%s/%s/_apis/wit/workitems?ids=%s&fields=%s&api-version=%s",
		h.base, h.orgPath(), strings.Join(idStrs, ","), strings.Join(reqFields, ","), apiVersion)
	var resp struct {
		Value []struct {
			ID     int            `json:"id"`
			Fields map[string]any `json:"fields"`
		} `json:"value"`
	}
	if err := h.do(ctx, http.MethodGet, u, nil, &resp); err != nil {
		return nil, fmt.Errorf("workitems: %w", err)
	}
	out := make([]map[string]any, len(resp.Value))
	for i, wi := range resp.Value {
		m := make(map[string]any, len(wi.Fields)+1)
		for k, v := range wi.Fields {
			m[k] = v
		}
		m["id"] = float64(wi.ID) // align with JSON-number decoding for coerce
		out[i] = m
	}
	return out, nil
}

func (h *httpClient) do(ctx context.Context, method, url string, body []byte, out any) error {
	var rdr io.Reader
	if body != nil {
		rdr = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", h.auth)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
