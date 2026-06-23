// Package dynamodbc is the AWS DynamoDB connector. It exposes a DynamoDB table
// as a dataset of rows by scanning its items. DynamoDB is schemaless, so the
// schema is inferred from a sample of items (the union of their attribute
// names, ordered) and typed as `any` — mirroring the JSON connector. The AWS
// client is reached through a narrow interface (dynamoAPI) so tests can inject
// a fake without real credentials.
//
// Options:
//
//	table     the table name; falls back to the dataset Source, then Name.
//	          A value of "*" expands (via the CLI) to every table in the account.
//	region    AWS region for the lazily-built client.
//	profile   shared-config profile for the lazily-built client.
//	endpoint  override the service endpoint (e.g. http://localhost:8000 for
//	          DynamoDB Local).
package dynamodbc

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

const (
	// sampleSize is how many items Resolve scans to infer the schema.
	sampleSize = 64
	// maxItems caps a single scan to keep memory bounded when no engine limit
	// is supplied.
	maxItems = 100000
	// scanPageSize bounds the items requested per Scan round-trip.
	scanPageSize = 1000
)

// dynamoAPI is the narrow surface of the DynamoDB client this connector uses.
// The real *dynamodb.Client satisfies it; tests inject a fake.
type dynamoAPI interface {
	Scan(ctx context.Context, in *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
	ListTables(ctx context.Context, in *dynamodb.ListTablesInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ListTablesOutput, error)
}

// Connector implements the DynamoDB connector.
type Connector struct {
	mu     sync.Mutex
	client dynamoAPI // nil until lazily constructed from options
}

// New returns a Connector that lazily builds a real AWS client from the
// dataset's region/profile/endpoint options on first use.
func New() *Connector { return &Connector{} }

// newWithClient returns a Connector backed by an explicit dynamoAPI. Used by
// tests to inject a fake client; no AWS credentials are required.
func newWithClient(c dynamoAPI) *Connector { return &Connector{client: c} }

func (*Connector) Name() string { return "dynamodb" }

func (*Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) { return nil, nil }

// Resolve infers the schema from a sample of the table's items.
func (c *Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	table, err := tableName(ds)
	if err != nil {
		return engine.Schema{}, err
	}
	api, err := c.resolveClient(ctx, ds.Options)
	if err != nil {
		return engine.Schema{}, err
	}
	out, err := api.Scan(ctx, &dynamodb.ScanInput{
		TableName: aws.String(table),
		Limit:     aws.Int32(sampleSize),
	})
	if err != nil {
		return engine.Schema{}, fmt.Errorf("scan %q: %w", table, err)
	}
	maps, err := unmarshalItems(out.Items)
	if err != nil {
		return engine.Schema{}, err
	}
	return schemaFromMaps(maps), nil
}

// Scan reads the table's items (paginated) and returns them as rows aligned to
// the inferred schema. Pagination follows LastEvaluatedKey up to maxItems (or
// req.Limit when no predicate is pushed down). Predicate/OrderBy are ignored;
// the engine applies them to the residual rows.
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

	// Honor Limit only when no predicate is pushed (the engine applies the
	// predicate to the full result first otherwise).
	limit := maxItems
	if req.Predicate == nil && req.Limit != nil && *req.Limit >= 0 && *req.Limit < limit {
		limit = *req.Limit
	}

	rows := make([]engine.Row, 0, 64)
	var startKey map[string]types.AttributeValue
	for len(rows) < limit {
		in := &dynamodb.ScanInput{
			TableName: aws.String(table),
			Limit:     aws.Int32(int32(min(limit-len(rows), scanPageSize))),
		}
		if startKey != nil {
			in.ExclusiveStartKey = startKey
		}
		out, err := api.Scan(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("scan %q: %w", table, err)
		}
		for _, item := range out.Items {
			row, err := itemToRow(item, schema)
			if err != nil {
				return nil, err
			}
			rows = append(rows, row)
			if len(rows) >= limit {
				break
			}
		}
		if len(out.LastEvaluatedKey) == 0 || len(rows) >= limit {
			break
		}
		startKey = out.LastEvaluatedKey
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
	var names []string
	var start *string
	for {
		out, err := api.ListTables(ctx, &dynamodb.ListTablesInput{ExclusiveStartTableName: start})
		if err != nil {
			return nil, fmt.Errorf("list tables: %w", err)
		}
		names = append(names, out.TableNames...)
		if out.LastEvaluatedTableName == nil || *out.LastEvaluatedTableName == "" {
			break
		}
		start = out.LastEvaluatedTableName
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
	return "", fmt.Errorf("dynamodb connector requires a table")
}

func unmarshalItems(items []map[string]types.AttributeValue) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		var m map[string]any
		if err := attributevalue.UnmarshalMap(item, &m); err != nil {
			return nil, fmt.Errorf("decode item: %w", err)
		}
		out = append(out, m)
	}
	return out, nil
}

func itemToRow(item map[string]types.AttributeValue, schema engine.Schema) (engine.Row, error) {
	var m map[string]any
	if err := attributevalue.UnmarshalMap(item, &m); err != nil {
		return engine.Row{}, fmt.Errorf("decode item: %w", err)
	}
	vals := make([]engine.Value, len(schema.Columns))
	for i, col := range schema.Columns {
		if v, ok := m[col.Name]; ok {
			vals[i] = connector.FromAny(v)
		} else {
			vals[i] = engine.Null()
		}
	}
	return engine.Row{Values: vals}, nil
}

// schemaFromMaps builds an `any`-typed, nullable schema from the union of keys
// across the sampled items, ordered for determinism.
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

// resolveClient returns the injected client if present, else lazily builds a
// real AWS client from region/profile/endpoint options (cached for reuse).
func (c *Connector) resolveClient(ctx context.Context, opts map[string]any) (dynamoAPI, error) {
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
	var clientOpts []func(*dynamodb.Options)
	if ep := stringOpt(opts, "endpoint"); ep != "" {
		clientOpts = append(clientOpts, func(o *dynamodb.Options) { o.BaseEndpoint = aws.String(ep) })
	}
	c.client = dynamodb.NewFromConfig(cfg, clientOpts...)
	return c.client, nil
}

func stringOpt(opts map[string]any, key string) string {
	v, ok := opts[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}
