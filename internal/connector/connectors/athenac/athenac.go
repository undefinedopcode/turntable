// Package athenac is the AWS Athena connector. Athena is itself a SQL engine
// (Presto/Trino over data in S3, catalogued in Glue), so this connector pushes
// projection / predicate / ORDER BY / LIMIT down as SQL and streams the result.
//
// Schema discovery uses the Glue catalog (GetTableMetadata / ListTableMetadata),
// which costs nothing — only Scan submits a billed query. A query is run
// asynchronously: StartQueryExecution, poll GetQueryExecution until it succeeds,
// then page through GetQueryResults. The AWS client is reached through a narrow
// interface (athenaAPI) so tests inject a fake without credentials.
//
// Options:
//
//	table            the table; falls back to the dataset Source/Name. May be
//	                 "db.table"; "*" expands (via the CLI) to every table.
//	database         Glue database (default "default").
//	catalog          data catalog (default "AwsDataCatalog").
//	output_location  S3 staging location for results, s3://bucket/prefix
//	                 (required unless the workgroup configures one).
//	workgroup        Athena workgroup (optional; AWS defaults to "primary").
//	region/profile/endpoint  AWS client configuration (lazily built).
//	poll_interval_ms results poll interval (default 1000).
package athenac

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	atypes "github.com/aws/aws-sdk-go-v2/service/athena/types"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	osql "github.com/april/turntable/internal/sql"
)

const (
	defaultCatalog  = "AwsDataCatalog"
	defaultDatabase = "default"
	defaultPoll     = time.Second
	resultPageSize  = 1000
)

// athenaAPI is the narrow surface of the Athena client this connector uses. The
// real *athena.Client satisfies it; tests inject a fake.
type athenaAPI interface {
	StartQueryExecution(ctx context.Context, in *athena.StartQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.StartQueryExecutionOutput, error)
	GetQueryExecution(ctx context.Context, in *athena.GetQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.GetQueryExecutionOutput, error)
	GetQueryResults(ctx context.Context, in *athena.GetQueryResultsInput, optFns ...func(*athena.Options)) (*athena.GetQueryResultsOutput, error)
	GetTableMetadata(ctx context.Context, in *athena.GetTableMetadataInput, optFns ...func(*athena.Options)) (*athena.GetTableMetadataOutput, error)
	ListTableMetadata(ctx context.Context, in *athena.ListTableMetadataInput, optFns ...func(*athena.Options)) (*athena.ListTableMetadataOutput, error)
}

// Connector implements the Athena connector.
type Connector struct {
	mu     sync.Mutex
	client athenaAPI // nil until lazily constructed from options
}

func New() *Connector { return &Connector{} }

// newWithClient backs the connector with an explicit athenaAPI (tests).
func newWithClient(c athenaAPI) *Connector { return &Connector{client: c} }

func (*Connector) Name() string { return "athena" }

func (*Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) {
	return nil, fmt.Errorf("Datasets requires a dataset (database/catalog) — use DatasetsFor")
}

// Resolve discovers a table's schema from the Glue catalog (no query cost).
func (c *Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	api, err := c.resolveClient(ctx, ds.Options)
	if err != nil {
		return engine.Schema{}, err
	}
	return resolveSchema(ctx, api, ds)
}

func resolveSchema(ctx context.Context, api athenaAPI, ds connector.Dataset) (engine.Schema, error) {
	db, table := tableParts(ds)
	out, err := api.GetTableMetadata(ctx, &athena.GetTableMetadataInput{
		CatalogName:  aws.String(catalogOf(ds.Options)),
		DatabaseName: aws.String(db),
		TableName:    aws.String(table),
	})
	if err != nil {
		return engine.Schema{}, fmt.Errorf("athena get table metadata %s.%s: %w", db, table, err)
	}
	if out.TableMetadata == nil {
		return engine.Schema{}, fmt.Errorf("athena: no metadata for %s.%s", db, table)
	}
	cols := make([]engine.Column, 0, len(out.TableMetadata.Columns))
	for _, col := range out.TableMetadata.Columns {
		cols = append(cols, engine.Column{
			Name:     aws.ToString(col.Name),
			Type:     athenaType(aws.ToString(col.Type)),
			Nullable: true,
		})
	}
	return engine.Schema{Columns: cols}, nil
}

// DatasetsFor enumerates the tables in the dataset's database (table="*").
func (c *Connector) DatasetsFor(ctx context.Context, ds connector.Dataset) ([]connector.Dataset, error) {
	api, err := c.resolveClient(ctx, ds.Options)
	if err != nil {
		return nil, err
	}
	db, _ := tableParts(ds)
	var out []connector.Dataset
	var token *string
	for {
		page, err := api.ListTableMetadata(ctx, &athena.ListTableMetadataInput{
			CatalogName:  aws.String(catalogOf(ds.Options)),
			DatabaseName: aws.String(db),
			NextToken:    token,
		})
		if err != nil {
			return nil, fmt.Errorf("athena list tables in %s: %w", db, err)
		}
		for _, m := range page.TableMetadataList {
			name := aws.ToString(m.Name)
			opts := map[string]any{}
			for k, v := range ds.Options {
				opts[k] = v
			}
			opts["table"] = name
			out = append(out, connector.Dataset{Name: name, Source: name, Options: opts})
		}
		if page.NextToken == nil {
			break
		}
		token = page.NextToken
	}
	return out, nil
}

// Scan builds a SELECT (with whatever pushdown is safe), runs it, and streams
// the result rows.
func (c *Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	api, err := c.resolveClient(ctx, req.Dataset.Options)
	if err != nil {
		return nil, err
	}
	schema, err := resolveSchema(ctx, api, req.Dataset)
	if err != nil {
		return nil, err
	}
	db, table := tableParts(req.Dataset)
	query := buildQuery(req, db, table)

	qid, err := runQuery(ctx, api, query, req.Dataset.Options, db)
	if err != nil {
		return nil, err
	}

	cols := req.Columns
	if len(cols) == 0 {
		cols = make([]string, len(schema.Columns))
		for i, c := range schema.Columns {
			cols[i] = c.Name
		}
	}
	return &rowIter{ctx: ctx, api: api, qid: qid, schema: schema, columns: cols}, nil
}

// runQuery submits the SQL and waits for it to succeed, returning the query id.
func runQuery(ctx context.Context, api athenaAPI, query string, opts map[string]any, db string) (string, error) {
	in := &athena.StartQueryExecutionInput{
		QueryString: aws.String(query),
		QueryExecutionContext: &atypes.QueryExecutionContext{
			Database: aws.String(db),
			Catalog:  aws.String(catalogOf(opts)),
		},
	}
	if loc := stringOpt(opts, "output_location"); loc != "" {
		in.ResultConfiguration = &atypes.ResultConfiguration{OutputLocation: aws.String(loc)}
	}
	if wg := stringOpt(opts, "workgroup"); wg != "" {
		in.WorkGroup = aws.String(wg)
	}
	start, err := api.StartQueryExecution(ctx, in)
	if err != nil {
		return "", fmt.Errorf("athena start query: %w", err)
	}
	qid := aws.ToString(start.QueryExecutionId)

	poll := defaultPoll
	if ms := stringOpt(opts, "poll_interval_ms"); ms != "" {
		if n, err := strconv.Atoi(ms); err == nil && n > 0 {
			poll = time.Duration(n) * time.Millisecond
		}
	}
	for {
		ge, err := api.GetQueryExecution(ctx, &athena.GetQueryExecutionInput{QueryExecutionId: aws.String(qid)})
		if err != nil {
			return "", fmt.Errorf("athena poll %s: %w", qid, err)
		}
		st := ge.QueryExecution.Status
		switch st.State {
		case atypes.QueryExecutionStateSucceeded:
			return qid, nil
		case atypes.QueryExecutionStateFailed, atypes.QueryExecutionStateCancelled:
			return "", fmt.Errorf("athena query %s: %s", st.State, aws.ToString(st.StateChangeReason))
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(poll):
		}
	}
}

// ---- result iterator ---------------------------------------------------------

type rowIter struct {
	ctx     context.Context
	api     athenaAPI
	qid     string
	schema  engine.Schema
	columns []string
	token   *string
	page    []atypes.Row
	idx     int
	fetched bool // whether the first page (with its header row) was read
	done    bool
}

func (r *rowIter) Next() (engine.Row, bool, error) {
	for {
		if r.idx < len(r.page) {
			row := r.page[r.idx]
			r.idx++
			return r.parse(row), true, nil
		}
		if r.done {
			return engine.Row{}, false, nil
		}
		out, err := r.api.GetQueryResults(r.ctx, &athena.GetQueryResultsInput{
			QueryExecutionId: aws.String(r.qid),
			NextToken:        r.token,
			MaxResults:       aws.Int32(resultPageSize),
		})
		if err != nil {
			return engine.Row{}, false, fmt.Errorf("athena get results: %w", err)
		}
		rows := out.ResultSet.Rows
		// Athena prepends a header row (column names) to the first page only.
		if !r.fetched {
			if len(rows) > 0 {
				rows = rows[1:]
			}
			r.fetched = true
		}
		r.page, r.idx = rows, 0
		r.token = out.NextToken
		if r.token == nil {
			r.done = true
		}
	}
}

func (r *rowIter) parse(row atypes.Row) engine.Row {
	vals := make([]engine.Value, len(r.columns))
	for i, name := range r.columns {
		var s *string
		if i < len(row.Data) {
			s = row.Data[i].VarCharValue
		}
		var t engine.Type
		if idx := r.schema.Index(name); idx >= 0 {
			t = r.schema.Columns[idx].Type
		}
		vals[i] = parseCell(s, t)
	}
	return engine.Row{Values: vals}
}

func (r *rowIter) Close() error { return nil }

// parseCell converts Athena's string cell (nil = SQL NULL) to a typed Value
// using the discovered column type; on a parse miss it falls back to the string.
func parseCell(s *string, t engine.Type) engine.Value {
	if s == nil {
		return engine.Null()
	}
	v := *s
	switch t {
	case engine.TypeInt:
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return engine.IntVal(n)
		}
	case engine.TypeFloat:
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return engine.FloatVal(f)
		}
	case engine.TypeBool:
		if b, err := strconv.ParseBool(v); err == nil {
			return engine.BoolVal(b)
		}
	case engine.TypeTime:
		if tm, ok := parseAthenaTime(v); ok {
			return engine.TimeVal(tm)
		}
	}
	return engine.StringVal(v)
}

func parseAthenaTime(s string) (time.Time, bool) {
	for _, l := range []string{
		"2006-01-02 15:04:05.999",
		"2006-01-02 15:04:05",
		"2006-01-02",
		time.RFC3339,
	} {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// athenaType maps a Presto/Athena column type to an engine type.
func athenaType(t string) engine.Type {
	t = strings.ToLower(t)
	switch {
	case strings.HasPrefix(t, "bool"):
		return engine.TypeBool
	case strings.Contains(t, "int"): // tinyint/smallint/integer/bigint
		return engine.TypeInt
	case strings.Contains(t, "double") || strings.Contains(t, "float") ||
		strings.Contains(t, "real") || strings.Contains(t, "decimal") || strings.Contains(t, "numeric"):
		return engine.TypeFloat
	case strings.Contains(t, "timestamp") || t == "date":
		return engine.TypeTime
	default:
		return engine.TypeString
	}
}

// ---- query building ----------------------------------------------------------

// buildQuery renders the Presto SELECT, pushing projection/predicate/order/limit.
// A LIMIT is pushed only when the predicate was fully translated (the engine
// re-applies any residual, so a partially-pushed WHERE stays correct but must
// not gate the limit).
func buildQuery(req connector.ScanRequest, db, table string) string {
	where, wherePushed := "", false
	predicateHandled := req.Predicate == nil
	if req.Predicate != nil {
		if w, ok := translateExpr(req.Predicate); ok {
			where, wherePushed = w, true
			predicateHandled = true
		}
	}

	var b strings.Builder
	b.WriteString("SELECT ")
	if len(req.Columns) == 0 {
		b.WriteString("*")
	} else {
		for i, c := range req.Columns {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(quoteIdent(c))
		}
	}
	fmt.Fprintf(&b, " FROM %s.%s", quoteIdent(db), quoteIdent(table))
	if wherePushed {
		fmt.Fprintf(&b, " WHERE %s", where)
	}
	if len(req.OrderBy) > 0 {
		b.WriteString(" ORDER BY ")
		for i, o := range req.OrderBy {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(quoteIdent(o.Column))
			if o.Desc {
				b.WriteString(" DESC")
			}
		}
	}
	if req.Limit != nil && predicateHandled {
		fmt.Fprintf(&b, " LIMIT %d", *req.Limit)
	}
	return b.String()
}

// translateExpr renders a turntable expression as Presto SQL, or ok=false for
// expressions we don't push (functions, subqueries, case-insensitive LIKE —
// Presto LIKE is case-sensitive, matching the engine's LIKE but not ILIKE).
func translateExpr(e osql.Expr) (string, bool) {
	switch ex := e.(type) {
	case *osql.LitInt:
		return strconv.FormatInt(ex.V, 10), true
	case *osql.LitFloat:
		return strconv.FormatFloat(ex.V, 'g', -1, 64), true
	case *osql.LitString:
		return quoteString(ex.V), true
	case *osql.LitBool:
		if ex.V {
			return "TRUE", true
		}
		return "FALSE", true
	case *osql.LitNull:
		return "NULL", true
	case *osql.ColRef:
		return quoteIdent(ex.Name), true
	case *osql.BinaryOp:
		l, ok := translateExpr(ex.Left)
		if !ok {
			return "", false
		}
		r, ok := translateExpr(ex.Right)
		if !ok {
			return "", false
		}
		switch ex.Op {
		case "=", "<>", "<", "<=", ">", ">=", "+", "-", "*", "/", "AND", "OR":
			return fmt.Sprintf("(%s %s %s)", l, ex.Op, r), true
		}
	case *osql.UnaryOp:
		in, ok := translateExpr(ex.Expr)
		if !ok {
			return "", false
		}
		switch ex.Op {
		case "NOT":
			return fmt.Sprintf("NOT (%s)", in), true
		case "-":
			return fmt.Sprintf("-(%s)", in), true
		}
	case *osql.InExpr:
		in, ok := translateExpr(ex.Expr)
		if !ok {
			return "", false
		}
		vals := make([]string, 0, len(ex.List))
		for _, l := range ex.List {
			v, ok := translateExpr(l)
			if !ok {
				return "", false
			}
			vals = append(vals, v)
		}
		op := "IN"
		if ex.Negate {
			op = "NOT IN"
		}
		return fmt.Sprintf("%s %s (%s)", in, op, strings.Join(vals, ", ")), true
	case *osql.BetweenExpr:
		v, ok := translateExpr(ex.Expr)
		lo, ok2 := translateExpr(ex.Low)
		hi, ok3 := translateExpr(ex.High)
		if !ok || !ok2 || !ok3 {
			return "", false
		}
		op := "BETWEEN"
		if ex.Negate {
			op = "NOT BETWEEN"
		}
		return fmt.Sprintf("%s %s %s AND %s", v, op, lo, hi), true
	case *osql.IsNullExpr:
		v, ok := translateExpr(ex.Expr)
		if !ok {
			return "", false
		}
		if ex.Negate {
			return fmt.Sprintf("%s IS NOT NULL", v), true
		}
		return fmt.Sprintf("%s IS NULL", v), true
	case *osql.LikeExpr:
		if ex.Insensitive { // Presto has no ILIKE; keep it in the engine.
			return "", false
		}
		v, ok := translateExpr(ex.Expr)
		p, ok2 := translateExpr(ex.Pat)
		if !ok || !ok2 {
			return "", false
		}
		if ex.Negate {
			return fmt.Sprintf("%s NOT LIKE %s", v, p), true
		}
		return fmt.Sprintf("%s LIKE %s", v, p), true
	}
	return "", false
}

func quoteIdent(s string) string  { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func quoteString(s string) string { return `'` + strings.ReplaceAll(s, `'`, `''`) + `'` }

// ---- options / client --------------------------------------------------------

// tableParts resolves the (database, table) for a dataset. A dotted name
// ("db.table") wins; otherwise the database option (default "default") applies.
func tableParts(ds connector.Dataset) (string, string) {
	name := stringOpt(ds.Options, "table")
	if name == "" {
		name = ds.Name
	}
	if name == "" {
		name = ds.Source
	}
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[:i], name[i+1:]
	}
	db := stringOpt(ds.Options, "database")
	if db == "" {
		db = defaultDatabase
	}
	return db, name
}

func catalogOf(opts map[string]any) string {
	if c := stringOpt(opts, "catalog"); c != "" {
		return c
	}
	return defaultCatalog
}

func (c *Connector) resolveClient(ctx context.Context, opts map[string]any) (athenaAPI, error) {
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
	var clientOpts []func(*athena.Options)
	if ep := stringOpt(opts, "endpoint"); ep != "" {
		clientOpts = append(clientOpts, func(o *athena.Options) { o.BaseEndpoint = aws.String(ep) })
	}
	c.client = athena.NewFromConfig(cfg, clientOpts...)
	return c.client, nil
}

func stringOpt(opts map[string]any, key string) string {
	if v, ok := opts[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
