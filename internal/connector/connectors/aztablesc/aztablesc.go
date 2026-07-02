// Package aztablesc is the Azure Table Storage connector. It exposes a table as
// a dataset of rows by listing its entities. Azure Tables is schemaless, so the
// schema is inferred from a sample of entities (the union of their property
// names, typed as `any`) — mirroring the JSON and DynamoDB connectors.
//
// Azure Table Storage is reached through a narrow interface (tablesAPI) so tests
// inject a fake without credentials. The real implementation authenticates two
// ways:
//
//   - connection_string — an account-key / SAS / Azurite connection string
//     (covers local development and most production setups).
//   - account or endpoint — Azure AD via DefaultAzureCredential (environment,
//     managed identity, Azure CLI, etc.). endpoint overrides the service URL;
//     otherwise it is https://<account>.table.core.windows.net/.
//
// Options:
//
//	table              the table name; falls back to the dataset Source, then Name.
//	                   "*" expands (via the CLI) to every table in the account.
//	connection_string  account-key / SAS / Azurite connection string.
//	account            storage account name (Azure AD auth).
//	endpoint           service URL override (e.g. Azurite, or a custom domain).
//
// Predicate translation to OData ($filter) is implemented but, like the SQL
// connector's pushdown, is not yet wired into the planner — the engine applies
// WHERE/ORDER BY to the returned rows. The translation runs when a ScanRequest
// carries a Predicate (exercised by unit tests and ready for planner wiring).
package aztablesc

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/data/aztables"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/connector/connectors/azcommon"
	"github.com/april/turntable/internal/engine"
	tsql "github.com/april/turntable/internal/sql"
)

const (
	// sampleSize is how many entities Resolve lists to infer the schema.
	sampleSize = 64
	// maxItems caps a single scan to keep memory bounded.
	maxItems = 100000
	// pageSize bounds the entities requested per list round-trip ($top).
	pageSize = 1000
)

// tablesAPI is the connector's narrow view of Azure Table Storage. The real
// implementation wraps aztables; tests inject a fake. Entities are returned as
// already-decoded property maps so the fake stays trivial.
type tablesAPI interface {
	listEntities(ctx context.Context, table, filter string, limit int) ([]map[string]any, error)
	listTables(ctx context.Context) ([]string, error)
}

// Connector implements the Azure Table Storage connector.
type Connector struct {
	mu     sync.Mutex
	client tablesAPI // nil until lazily constructed from options
}

// New returns a Connector that lazily builds a real client from the dataset's
// auth options on first use.
func New() *Connector { return &Connector{} }

// newWithClient returns a Connector backed by an explicit tablesAPI. Used by
// tests to inject a fake; no Azure credentials are required.
func newWithClient(c tablesAPI) *Connector { return &Connector{client: c} }

func (*Connector) Name() string { return "azuretables" }

func (*Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) { return nil, nil }

// Resolve infers the schema from a sample of the table's entities.
func (c *Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	table, err := tableName(ds)
	if err != nil {
		return engine.Schema{}, err
	}
	api, err := c.resolveClient(ctx, ds.Options)
	if err != nil {
		return engine.Schema{}, err
	}
	entities, err := api.listEntities(ctx, table, "", sampleSize)
	if err != nil {
		return engine.Schema{}, fmt.Errorf("list %q: %w", table, err)
	}
	return schemaFromMaps(entities), nil
}

// Scan lists the table's entities and returns them as rows aligned to the
// inferred schema. When a Predicate is present it is translated to an OData
// $filter where possible; an untranslatable predicate is left to the engine.
// $top (limit) is pushed only when the predicate is absent or fully translated,
// since otherwise the engine must see every matching row first.
func (c *Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	table, err := tableName(req.Dataset)
	if err != nil {
		return nil, err
	}
	schema, err := c.Resolve(ctx, req.Dataset)
	if err != nil {
		return nil, err
	}
	api, err := c.resolveClient(ctx, req.Dataset.Options)
	if err != nil {
		return nil, err
	}

	filter := ""
	predicateHandled := req.Predicate == nil
	if req.Predicate != nil {
		if f, ok := translateOData(req.Predicate); ok {
			filter = f
			predicateHandled = true
		}
	}
	limit := maxItems
	if predicateHandled && req.Limit != nil && *req.Limit >= 0 && *req.Limit < limit {
		limit = *req.Limit
	}

	entities, err := api.listEntities(ctx, table, filter, limit)
	if err != nil {
		return nil, fmt.Errorf("list %q: %w", table, err)
	}
	rows := make([]engine.Row, len(entities))
	for i, m := range entities {
		rows[i] = rowFromMap(m, schema)
	}
	return engine.NewSliceIter(rows), nil
}

// DatasetsFor enumerates every table in the account, returning one Dataset per
// table (carrying the same connection options). It backs the table="*" wildcard.
func (c *Connector) DatasetsFor(ctx context.Context, ds connector.Dataset) ([]connector.Dataset, error) {
	api, err := c.resolveClient(ctx, ds.Options)
	if err != nil {
		return nil, err
	}
	names, err := api.listTables(ctx)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	out := make([]connector.Dataset, 0, len(names))
	for _, n := range names {
		opts := make(map[string]any, len(ds.Options)+1)
		for k, v := range ds.Options {
			opts[k] = v
		}
		opts["table"] = n
		out = append(out, connector.Dataset{Name: n, Source: n, Options: opts})
	}
	return out, nil
}

// ---- helpers -----------------------------------------------------------------

func tableName(ds connector.Dataset) (string, error) {
	if t := stringOpt(ds.Options, "table"); t != "" {
		return t, nil
	}
	if ds.Source != "" {
		return ds.Source, nil
	}
	if ds.Name != "" {
		return ds.Name, nil
	}
	return "", fmt.Errorf("azuretables connector requires a table")
}

func rowFromMap(m map[string]any, schema engine.Schema) engine.Row {
	vals := make([]engine.Value, len(schema.Columns))
	for i, col := range schema.Columns {
		if v, ok := m[col.Name]; ok {
			vals[i] = connector.FromAny(v)
		} else {
			vals[i] = engine.Null()
		}
	}
	return engine.Row{Values: vals}
}

// schemaFromMaps builds an `any`-typed, nullable schema from the union of keys
// across the sampled entities, ordered for determinism.
func schemaFromMaps(maps []map[string]any) engine.Schema {
	seen := map[string]bool{}
	var order []string
	for _, m := range maps {
		for k := range m {
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

func stringOpt(opts map[string]any, key string) string {
	v, ok := opts[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// ---- OData predicate translation ---------------------------------------------

// odataOps maps the dialect's binary operators to OData filter operators.
var odataOps = map[string]string{
	"=": "eq", "<>": "ne", "<": "lt", "<=": "le", ">": "gt", ">=": "ge",
	"AND": "and", "OR": "or",
}

// translateOData converts a predicate to an OData $filter expression. It returns
// ok=false for anything Azure Tables can't express (LIKE, IS NULL, functions),
// so the caller leaves the predicate for the engine.
func translateOData(e tsql.Expr) (string, bool) {
	switch ex := e.(type) {
	case *tsql.LitInt:
		return strconv.FormatInt(ex.V, 10), true
	case *tsql.LitFloat:
		return strconv.FormatFloat(ex.V, 'g', -1, 64), true
	case *tsql.LitString:
		return "'" + strings.ReplaceAll(ex.V, "'", "''") + "'", true
	case *tsql.LitBool:
		if ex.V {
			return "true", true
		}
		return "false", true
	case *tsql.ColRef:
		return ex.Name, true
	case *tsql.BinaryOp:
		left, ok := translateOData(ex.Left)
		if !ok {
			return "", false
		}
		right, ok := translateOData(ex.Right)
		if !ok {
			return "", false
		}
		op, ok := odataOps[ex.Op]
		if !ok {
			return "", false
		}
		return "(" + left + " " + op + " " + right + ")", true
	case *tsql.UnaryOp:
		inner, ok := translateOData(ex.Expr)
		if !ok {
			return "", false
		}
		switch ex.Op {
		case "NOT":
			return "not (" + inner + ")", true
		case "-":
			return "-" + inner, true
		}
	case *tsql.BetweenExpr:
		v, ok := translateOData(ex.Expr)
		lo, ok2 := translateOData(ex.Low)
		hi, ok3 := translateOData(ex.High)
		if !ok || !ok2 || !ok3 {
			return "", false
		}
		expr := "(" + v + " ge " + lo + " and " + v + " le " + hi + ")"
		if ex.Negate {
			return "not " + expr, true
		}
		return expr, true
	case *tsql.InExpr:
		v, ok := translateOData(ex.Expr)
		if !ok {
			return "", false
		}
		parts := make([]string, 0, len(ex.List))
		for _, item := range ex.List {
			iv, ok := translateOData(item)
			if !ok {
				return "", false
			}
			parts = append(parts, v+" eq "+iv)
		}
		joined := "(" + strings.Join(parts, " or ") + ")"
		if ex.Negate {
			return "not " + joined, true
		}
		return joined, true
	}
	return "", false
}

// ---- real Azure client -------------------------------------------------------

// resolveClient returns the injected client if present, else lazily builds a
// real Azure client from the auth options (cached for reuse).
func (c *Connector) resolveClient(ctx context.Context, opts map[string]any) (tablesAPI, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return c.client, nil
	}
	svc, err := buildServiceClient(opts)
	if err != nil {
		return nil, err
	}
	c.client = &azAdapter{svc: svc}
	return c.client, nil
}

// buildServiceClient picks the auth method from the options: a connection string
// (account key / SAS / Azurite) or Azure AD via DefaultAzureCredential when an
// account/endpoint is given.
func buildServiceClient(opts map[string]any) (*aztables.ServiceClient, error) {
	// Retry generously on 429s — Table Storage throttles, and a dashboard
	// refresh fires many panels at once (see azcommon.RetryOptions).
	clientOpts := &aztables.ClientOptions{
		ClientOptions: azcore.ClientOptions{Retry: azcommon.RetryOptions()},
	}
	if cs := stringOpt(opts, "connection_string"); cs != "" {
		return aztables.NewServiceClientFromConnectionString(cs, clientOpts)
	}
	account := stringOpt(opts, "account")
	endpoint := stringOpt(opts, "endpoint")
	if account == "" && endpoint == "" {
		return nil, fmt.Errorf("azuretables requires connection_string, or account/endpoint for Azure AD auth")
	}
	serviceURL := endpoint
	if serviceURL == "" {
		serviceURL = fmt.Sprintf("https://%s.table.core.windows.net/", account)
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("azure credential: %w", err)
	}
	return aztables.NewServiceClient(serviceURL, cred, clientOpts)
}

// azAdapter wraps an aztables.ServiceClient as a tablesAPI: it drives the pagers
// and decodes entity JSON into property maps.
type azAdapter struct {
	svc *aztables.ServiceClient
}

func (a *azAdapter) listEntities(ctx context.Context, table, filter string, limit int) ([]map[string]any, error) {
	listOpts := &aztables.ListEntitiesOptions{}
	if filter != "" {
		listOpts.Filter = &filter
	}
	if limit > 0 {
		top := int32(min(limit, pageSize))
		listOpts.Top = &top
	}
	pager := a.svc.NewClient(table).NewListEntitiesPager(listOpts)
	var out []map[string]any
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, raw := range page.Entities {
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				return nil, fmt.Errorf("decode entity: %w", err)
			}
			out = append(out, cleanEntity(m))
			if limit > 0 && len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

func (a *azAdapter) listTables(ctx context.Context) ([]string, error) {
	pager := a.svc.NewListTablesPager(nil)
	var names []string
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, t := range page.Tables {
			if t != nil && t.Name != nil {
				names = append(names, *t.Name)
			}
		}
	}
	return names, nil
}

// cleanEntity drops OData metadata/annotation keys (e.g. "odata.etag",
// "Age@odata.type") so only the entity's own properties become columns.
func cleanEntity(m map[string]any) map[string]any {
	for k := range m {
		if strings.HasPrefix(k, "odata.") || strings.Contains(k, "@odata") {
			delete(m, k)
		}
	}
	return m
}
