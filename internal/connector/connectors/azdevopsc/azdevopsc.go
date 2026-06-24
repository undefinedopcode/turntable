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
// WIQL fails any query that *matches* more than 20000 items (error VS402337).
// To stay under that, the connector pushes the SQL WHERE into the WIQL where it
// can (translateWIQL), so Azure filters server-side — e.g. `WHERE assigned_to_
// email = 'me@x'` becomes `[System.AssignedTo] = 'me@x'` and only your items are
// matched. The engine still re-applies the full predicate, so an untranslatable
// or looser push stays correct. The default query also pages forward by
// ascending System.Id so a filtered result spanning >20000 still streams. An
// unfiltered query over a >20000-item project can still exceed the cap — add a
// WHERE (or a custom `wiql` WHERE clause, which is run as-is and must itself
// match under 20000).
//
// Filter assignment with assigned_to_email (the identity's unique name / email);
// assigned_to is the display name. Both push to [System.AssignedTo].
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
	tsql "github.com/april/turntable/internal/sql"
)

const (
	defaultBase = "https://dev.azure.com"
	apiVersion  = "7.0"
	// batchSize is the max work item IDs per work-items fetch (API limit is 200).
	batchSize = 200
	// maxItems bounds a single scan (when no LIMIT is pushed) to keep memory
	// bounded; paging can retrieve work items beyond WIQL's 20000 cap up to here.
	maxItems = 50000
)

// wiqlPageSize is the per-page $top for WIQL paging. WIQL fails a query that
// *matches* more than 20000 items (VS402337) regardless of $top, so the default
// query pages forward by ascending System.Id — each page is an index range scan
// that stays under the cap. A var so tests can shrink it.
var wiqlPageSize = 20000

// devopsAPI is the connector's narrow view of Azure DevOps, split so the
// connector can page WIQL itself: queryIDs runs one WIQL flat query (returning
// matching ids, capped at top), and workItems fetches those ids' fields. The
// real client does the HTTP; tests inject a fake.
type devopsAPI interface {
	queryIDs(ctx context.Context, wiql string, top int) ([]int, error)
	workItems(ctx context.Context, ids []int, fields []string) ([]map[string]any, error)
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

	ids, err := collectIDs(ctx, api, req.Dataset.Options, req.Predicate, limit)
	if err != nil {
		return nil, fmt.Errorf("azuredevops query: %w", err)
	}
	items, err := api.workItems(ctx, ids, requestedFields())
	if err != nil {
		return nil, fmt.Errorf("azuredevops fetch: %w", err)
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
	{"assigned_to_email", []string{"System.AssignedTo", "uniqueName"}, engine.TypeString},
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

// collectIDs returns the matching work item ids, up to limit. The default query
// is paged forward by ascending System.Id (watermark = the highest id seen),
// which keeps each WIQL page an index range scan under the 20000 match cap. A
// caller-supplied wiql is run once as-is (it must itself match under the cap).
func collectIDs(ctx context.Context, api devopsAPI, opts map[string]any, predicate tsql.Expr, limit int) ([]int, error) {
	var all []int
	watermark := 0
	for len(all) < limit {
		wiql, pageable := wiqlForPage(opts, predicate, watermark)
		page := wiqlPageSize
		if rem := limit - len(all); rem < page {
			page = rem
		}
		ids, err := api.queryIDs(ctx, wiql, page)
		if err != nil {
			return nil, err
		}
		all = append(all, ids...)
		if !pageable || len(ids) < page {
			break // single-shot custom query, or the default query is drained
		}
		watermark = ids[len(ids)-1] // ids are ascending; advance past this page
	}
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// wiqlForPage builds the WIQL for one page. A caller-supplied "wiql" option is
// returned verbatim with pageable=false (run once). Otherwise it builds the
// default project query filtered to ids above the watermark and ordered by
// ascending System.Id, so the connector can page through arbitrarily many work
// items without tripping WIQL's 20000-match limit. @project resolves from the
// project in the request URL.
func wiqlForPage(opts map[string]any, predicate tsql.Expr, watermark int) (wiql string, pageable bool) {
	if q := stringOpt(opts, "wiql"); q != "" {
		return q, false
	}
	var b strings.Builder
	b.WriteString("SELECT [System.Id] FROM workitems WHERE [System.TeamProject] = @project")
	if typ := stringOpt(opts, "type"); typ != "" {
		fmt.Fprintf(&b, " AND [System.WorkItemType] = '%s'", strings.ReplaceAll(typ, "'", "''"))
	}
	// Push as much of the SQL WHERE into WIQL as translates safely, so Azure
	// filters server-side (essential: an unfiltered query over a >20000-item
	// project fails). The engine still re-applies the full predicate, so a
	// partial/looser translation stays correct.
	if predicate != nil {
		if p, ok := translateWIQL(predicate); ok {
			fmt.Fprintf(&b, " AND (%s)", p)
		}
	}
	fmt.Fprintf(&b, " AND [System.Id] > %d ORDER BY [System.Id] ASC", watermark)
	return b.String(), true
}

// wiqlFieldFor maps an output column to its WIQL field reference, or false if
// the column isn't pushable.
func wiqlFieldFor(col string) (string, bool) {
	if col == "id" {
		return "[System.Id]", true
	}
	for _, f := range fields {
		if f.col == col {
			return "[" + f.path[0] + "]", true
		}
	}
	return "", false
}

// translateWIQL converts a SQL predicate into a WIQL filter that the SQL
// predicate *implies* (so WIQL returns a superset and the engine's re-filter
// stays correct). It returns ok=false for anything it can't safely express.
// AND may drop an untranslatable conjunct (still a superset); OR must translate
// fully or not at all.
func translateWIQL(e tsql.Expr) (string, bool) {
	switch ex := e.(type) {
	case *tsql.BinaryOp:
		switch strings.ToUpper(ex.Op) {
		case "AND":
			l, lok := translateWIQL(ex.Left)
			r, rok := translateWIQL(ex.Right)
			switch {
			case lok && rok:
				return "(" + l + " AND " + r + ")", true
			case lok:
				return l, true
			case rok:
				return r, true
			}
			return "", false
		case "OR":
			l, lok := translateWIQL(ex.Left)
			r, rok := translateWIQL(ex.Right)
			if lok && rok {
				return "(" + l + " OR " + r + ")", true
			}
			return "", false
		case "=", "<>", "<", "<=", ">", ">=":
			f, fok := wiqlOperand(ex.Left)
			v, vok := wiqlLiteral(ex.Right)
			if fok && vok {
				return f + " " + ex.Op + " " + v, true
			}
			return "", false
		}
	case *tsql.InExpr:
		if ex.Negate || ex.Subquery != nil || len(ex.List) == 0 {
			return "", false
		}
		f, fok := wiqlOperand(ex.Expr)
		if !fok {
			return "", false
		}
		parts := make([]string, 0, len(ex.List))
		for _, it := range ex.List {
			v, ok := wiqlLiteral(it)
			if !ok {
				return "", false
			}
			parts = append(parts, v)
		}
		return f + " IN (" + strings.Join(parts, ", ") + ")", true
	}
	return "", false
}

func wiqlOperand(e tsql.Expr) (string, bool) {
	c, ok := e.(*tsql.ColRef)
	if !ok {
		return "", false
	}
	return wiqlFieldFor(c.Name)
}

func wiqlLiteral(e tsql.Expr) (string, bool) {
	switch v := e.(type) {
	case *tsql.LitString:
		return "'" + strings.ReplaceAll(v.V, "'", "''") + "'", true
	case *tsql.LitInt:
		return strconv.FormatInt(v.V, 10), true
	case *tsql.LitFloat:
		return strconv.FormatFloat(v.V, 'g', -1, 64), true
	}
	return "", false
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

// queryIDs runs one WIQL flat query and returns the matching work item ids,
// capped at top via the $top query parameter. The WIQL is project-scoped.
func (h *httpClient) queryIDs(ctx context.Context, wiql string, top int) ([]int, error) {
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
	ids := make([]int, len(wiqlResp.WorkItems))
	for i, w := range wiqlResp.WorkItems {
		ids[i] = w.ID
	}
	return ids, nil
}

// workItems fetches the given ids' fields, chunked to the API's per-call limit.
// Returned maps are each work item's "fields" object plus a top-level "id".
func (h *httpClient) workItems(ctx context.Context, ids []int, reqFields []string) ([]map[string]any, error) {
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
