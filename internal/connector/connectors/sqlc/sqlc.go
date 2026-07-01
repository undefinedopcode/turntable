// Package sqlc is the SQL database connector. It reads tables/views from a
// database via database/sql, with predicate/limit/order pushdown into the
// underlying query.
package sqlc

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	oparseSQL "github.com/april/turntable/internal/sql"

	_ "github.com/go-sql-driver/mysql"  // registers the "mysql" driver
	_ "github.com/lib/pq"               // registers the "postgres" driver
	_ "github.com/microsoft/go-mssqldb" // registers the "sqlserver" driver
	_ "modernc.org/sqlite"              // pure-Go SQLite driver; v0.2 default
)

// Connector implements a database/sql connector.
type Connector struct{}

func New() *Connector { return &Connector{} }

func (Connector) Name() string { return "sql" }

// Datasets enumerates the user tables in the database described by the
// dataset's options (driver, dsn). It is used to expand a SQL source that
// omits a table name into one dataset per table.
//
// Discovery is dialect-aware: SQLite uses PRAGMA table_list (falling back to
// sqlite_master when unavailable), while Postgres/MySQL use
// information_schema.tables. System tables (sqlite_*) are filtered out.
func (Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) {
	return nil, fmt.Errorf("Datasets requires a dataset (driver/dsn) — use DatasetsFor")
}

// DatasetsFor enumerates tables in the database identified by ds.Options
// (driver + dsn). It returns one Dataset per user table, carrying the same
// options so each can be scanned independently.
func (Connector) DatasetsFor(ctx context.Context, ds connector.Dataset) ([]connector.Dataset, error) {
	driver := stringOpt(ds.Options, "driver")
	dsn := stringOpt(ds.Options, "dsn")
	if dsn == "" {
		return nil, fmt.Errorf("sql connector requires dsn option")
	}
	if driver == "" {
		driver = "sqlite"
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", driver, err)
	}
	defer db.Close()

	names, err := listTables(ctx, db, driver)
	if err != nil {
		return nil, err
	}
	out := make([]connector.Dataset, 0, len(names))
	for _, n := range names {
		// Each dataset shares the connection options but names a specific table.
		opts := map[string]any{}
		for k, v := range ds.Options {
			opts[k] = v
		}
		d := connector.Dataset{Name: n, Source: n, Options: opts}
		out = append(out, d)
	}
	return out, nil
}

// listTables returns user table names in the database, filtering out system
// tables. It tries, in order: SQLite PRAGMA table_list, information_schema, and
// sqlite_master.
func listTables(ctx context.Context, db *sql.DB, driver string) ([]string, error) {
	// SQLite: PRAGMA table_list returns (schema, name, type, ncol, wr, strict).
	if rows, err := db.QueryContext(ctx, "PRAGMA table_list"); err == nil {
		defer rows.Close()
		var names []string
		for rows.Next() {
			cols, _ := rows.Columns()
			dest := make([]sql.NullString, len(cols))
			ptrs := make([]any, len(cols))
			for i := range dest {
				ptrs[i] = &dest[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return nil, err
			}
			// name is the 2nd column; type is the 3rd.
			name := dest[1].String
			kind := ""
			if len(dest) > 2 {
				kind = dest[2].String
			}
			if strings.HasPrefix(name, "sqlite_") {
				continue
			}
			// Include tables and views.
			if kind == "" || strings.EqualFold(kind, "table") || strings.EqualFold(kind, "view") {
				names = append(names, name)
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if names != nil {
			return names, nil
		}
	}

	// Postgres / MySQL / anything with information_schema.
	query := `SELECT table_name FROM information_schema.tables
		  WHERE table_schema NOT IN ('pg_catalog','information_schema')
		    AND table_name NOT LIKE 'sqlite_%'
		  ORDER BY table_name`
	if rows, err := db.QueryContext(ctx, query); err == nil {
		defer rows.Close()
		var names []string
		for rows.Next() {
			var n sql.NullString
			if err := rows.Scan(&n); err != nil {
				return nil, err
			}
			if n.Valid && !strings.HasPrefix(n.String, "sqlite_") {
				names = append(names, n.String)
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if names != nil {
			return names, nil
		}
	}

	// SQLite fallback: sqlite_master.
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("could not list tables: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n sql.NullString
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		if n.Valid {
			names = append(names, n.String)
		}
	}
	return names, rows.Err()
}

// Resolve discovers the schema for a dataset. The dataset name is treated as a
// table identifier (possibly qualified as "schema.table" or "catalog.schema.table").
func (Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	db, table, dial, err := openAndTable(ds)
	if err != nil {
		return engine.Schema{}, err
	}
	defer db.Close()

	cols, err := discoverColumns(ctx, db, table, dial)
	if err != nil {
		return engine.Schema{}, fmt.Errorf("discover %q: %w", table.name, err)
	}
	return engine.Schema{Columns: cols}, nil
}

// PushAggregate implements connector.AggregatePusher: a GROUP BY/aggregate
// query over one table is computed in the database when every part is
// expressible in the target dialect — plain-column or DATE_BIN-bucket group-by
// terms, the standard aggregate ops, and a WHERE that translates *exactly*.
// (A superset predicate like the MySQL/SQLite case-insensitive LIKE is fine
// for a raw scan, where the engine re-filters — but after aggregation there
// are no raw rows left to refine, so anything inexact declines.) Declining
// (ok=false) falls back to the engine aggregating the connector's raw rows,
// which is always correct; pushing is an optimization only.
func (Connector) PushAggregate(ctx context.Context, ds connector.Dataset, agg connector.AggregateRequest) (engine.Schema, bool, error) {
	db, table, dial, err := openAndTable(ds)
	if err != nil {
		return engine.Schema{}, false, err
	}
	defer db.Close()
	base, err := discoverSchema(ctx, db, table, dial)
	if err != nil {
		return engine.Schema{}, false, err
	}
	schema, ok := aggregateSchema(agg, base)
	if !ok {
		return engine.Schema{}, false, nil
	}
	if _, ok := buildAggQuery(agg, table, dial); !ok {
		return engine.Schema{}, false, nil
	}
	return schema, true, nil
}

// aggregateSchema types the aggregated rows (group terms then ops, in request
// order), or ok=false when an op or bucket is not supported: buckets must be a
// whole positive number of seconds over an existing column, ops must be
// COUNT/SUM/AVG/MIN/MAX.
func aggregateSchema(agg connector.AggregateRequest, base engine.Schema) (engine.Schema, bool) {
	colType := func(name string) (engine.Type, bool) {
		if i := base.Index(name); i >= 0 {
			return base.Columns[i].Type, true
		}
		return engine.TypeAny, false
	}
	cols := make([]engine.Column, 0, len(agg.GroupBy)+len(agg.Aggregates))
	for _, g := range agg.GroupBy {
		t, exists := colType(g.Column)
		if !exists {
			return engine.Schema{}, false
		}
		if g.Stride != 0 {
			if g.Stride < time.Second || g.Stride%time.Second != 0 {
				return engine.Schema{}, false
			}
			t = engine.TypeTime
		}
		cols = append(cols, engine.Column{Name: g.Alias, Type: t, Nullable: true})
	}
	for _, op := range agg.Aggregates {
		var t engine.Type
		switch strings.ToUpper(op.Func) {
		case "COUNT":
			t = engine.TypeInt
		case "SUM", "AVG":
			if op.Column == "" {
				return engine.Schema{}, false
			}
			t = engine.TypeFloat
		case "MIN", "MAX":
			ct, exists := colType(op.Column)
			if !exists {
				return engine.Schema{}, false
			}
			t = ct
		default:
			return engine.Schema{}, false
		}
		if op.Column != "" {
			if _, exists := colType(op.Column); !exists {
				return engine.Schema{}, false
			}
		}
		cols = append(cols, engine.Column{Name: op.Alias, Type: t, Nullable: true})
	}
	return engine.Schema{Columns: cols}, true
}

// buildAggQuery renders the aggregated SELECT for a pushed AggregateRequest.
// Like buildScanQuery it is pure (no DB) so the per-dialect SQL — bucket
// arithmetic included — is unit-testable. ok=false when the dialect cannot
// express a part (bucket SQL, or a predicate that doesn't translate exactly).
func buildAggQuery(agg connector.AggregateRequest, table tableRef, dial dialect) (string, bool) {
	var sel, group []string
	for _, g := range agg.GroupBy {
		expr := dial.quoteIdent(g.Column)
		if g.Stride != 0 {
			be, ok := dial.bucketExpr(expr, int64(g.Stride/time.Second))
			if !ok {
				return "", false
			}
			expr = be
		}
		sel = append(sel, expr+" AS "+dial.quoteIdent(g.Alias))
		group = append(group, expr)
	}
	for _, op := range agg.Aggregates {
		arg := "*"
		if op.Column != "" {
			arg = dial.quoteIdent(op.Column)
		}
		if op.Distinct {
			arg = "DISTINCT " + arg
		}
		sel = append(sel, fmt.Sprintf("%s(%s) AS %s", strings.ToUpper(op.Func), arg, dial.quoteIdent(op.Alias)))
	}
	if len(sel) == 0 {
		return "", false
	}

	var b strings.Builder
	b.WriteString("SELECT ")
	b.WriteString(strings.Join(sel, ", "))
	fmt.Fprintf(&b, " FROM %s", table.quoted(dial))
	if agg.Predicate != nil {
		where, ok := translateExpr(agg.Predicate, dial)
		if !ok || !predicateExact(agg.Predicate, dial) {
			return "", false
		}
		fmt.Fprintf(&b, " WHERE %s", where)
	}
	if len(group) > 0 {
		b.WriteString(" GROUP BY ")
		b.WriteString(strings.Join(group, ", "))
	}
	return b.String(), true
}

// Scan executes a SELECT against the table, pushing down whatever the request
// asks for. Unpushable predicates are not pushed; the engine applies them in
// memory via a Filter. A req.Aggregate (accepted earlier by PushAggregate)
// switches the scan to the aggregated query: the rows returned are the
// grouped/aggregated result.
func (Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	db, table, dial, err := openAndTable(req.Dataset)
	if err != nil {
		return nil, err
	}

	if req.Aggregate != nil {
		base, err := discoverSchema(ctx, db, table, dial)
		if err != nil {
			db.Close()
			return nil, err
		}
		schema, sok := aggregateSchema(*req.Aggregate, base)
		query, qok := buildAggQuery(*req.Aggregate, table, dial)
		if !sok || !qok {
			// PushAggregate accepted this request, so it must render.
			db.Close()
			return nil, fmt.Errorf("sql: pushed aggregate no longer renderable (schema changed?)")
		}
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("aggregate query: %w", err)
		}
		return &rowIter{db: db, rows: rows, schema: schema, columns: schemaColumnNames(schema)}, nil
	}

	query := buildScanQuery(req, table, dial)

	schema, err := discoverSchema(ctx, db, table, dial)
	if err != nil {
		db.Close()
		return nil, err
	}

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("query: %w", err)
	}

	cols := req.Columns
	if len(cols) == 0 {
		cols = schemaColumnNames(schema)
	}
	return &rowIter{db: db, rows: rows, schema: schema, columns: cols}, nil
}

// buildScanQuery renders the SELECT for a scan, applying whatever pushdown is
// safe. It is pure (no DB), so the SQL shape — per-dialect quoting and the
// LIMIT-vs-TOP rendering — is unit-testable.
//
// Predicate pushdown is resolved up front: translation may fail (an unsupported
// expression → the WHERE is omitted and the engine filters in memory), and only
// a predicate applied in-DB *exactly* (predicateExact) lets the row limit be
// pushed — an inexact LIKE still filters as a superset the engine refines, but
// must not gate the limit, or the DB could drop matching rows the engine has not
// seen. This is decided before building the SELECT because SQL Server expresses
// the limit as a leading TOP (n), ahead of the column list.
func buildScanQuery(req connector.ScanRequest, table tableRef, dial dialect) string {
	whereClause, wherePushed := "", false
	predicateHandled := req.Predicate == nil
	if req.Predicate != nil {
		if where, ok := translateExpr(req.Predicate, dial); ok {
			whereClause, wherePushed = where, true
			predicateHandled = predicateExact(req.Predicate, dial)
		}
	}
	pushLimit := req.Limit != nil && predicateHandled

	var b strings.Builder
	b.WriteString("SELECT ")
	if pushLimit && dial.usesTop() {
		fmt.Fprintf(&b, "TOP (%d) ", *req.Limit)
	}
	if len(req.Columns) == 0 {
		b.WriteString("*")
	} else {
		for i, c := range req.Columns {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(dial.quoteIdent(c))
		}
	}
	fmt.Fprintf(&b, " FROM %s", table.quoted(dial))

	if wherePushed {
		fmt.Fprintf(&b, " WHERE %s", whereClause)
	}

	if len(req.OrderBy) > 0 {
		b.WriteString(" ORDER BY ")
		for i, o := range req.OrderBy {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(dial.quoteIdent(o.Column))
			if o.Desc {
				b.WriteString(" DESC")
			}
		}
	}

	// A trailing LIMIT for engines that use it (SQL Server used TOP above).
	if pushLimit && !dial.usesTop() {
		fmt.Fprintf(&b, " LIMIT %d", *req.Limit)
	}
	return b.String()
}

func schemaColumnNames(schema engine.Schema) []string {
	out := make([]string, len(schema.Columns))
	for i, c := range schema.Columns {
		out[i] = c.Name
	}
	return out
}

// ---- db / table helpers ------------------------------------------------------

type tableRef struct {
	catalog string
	schema  string
	name    string
}

func (t tableRef) quoted(d dialect) string {
	parts := []string{}
	if t.catalog != "" {
		parts = append(parts, d.quoteIdent(t.catalog))
	}
	if t.schema != "" {
		parts = append(parts, d.quoteIdent(t.schema))
	}
	parts = append(parts, d.quoteIdent(t.name))
	return strings.Join(parts, ".")
}

func openAndTable(ds connector.Dataset) (*sql.DB, tableRef, dialect, error) {
	driver := stringOpt(ds.Options, "driver")
	dsn := stringOpt(ds.Options, "dsn")
	if driver == "" {
		driver = "sqlite"
	}
	if dsn == "" {
		return nil, tableRef{}, dialect{}, fmt.Errorf("sql connector requires dsn option")
	}
	name := ds.Name
	if name == "" {
		name = ds.Source
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, tableRef{}, dialect{}, fmt.Errorf("open %s: %w", driver, err)
	}
	return db, parseTableName(name), dialectFor(driver), nil
}

func parseTableName(name string) tableRef {
	parts := strings.Split(name, ".")
	switch len(parts) {
	case 2:
		return tableRef{schema: parts[0], name: parts[1]}
	case 3:
		return tableRef{catalog: parts[0], schema: parts[1], name: parts[2]}
	default:
		return tableRef{name: name}
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

func discoverSchema(ctx context.Context, db *sql.DB, t tableRef, d dialect) (engine.Schema, error) {
	cols, err := discoverColumns(ctx, db, t, d)
	if err != nil {
		return engine.Schema{}, err
	}
	return engine.Schema{Columns: cols}, nil
}

func discoverColumns(ctx context.Context, db *sql.DB, t tableRef, d dialect) ([]engine.Column, error) {
	// Discovery is dialect-specific. We try SQLite's PRAGMA first, then fall
	// back to information_schema (Postgres/MySQL), then DESCRIBE (MySQL).
	// Try SQLite pragma first.
	pragmaSQL := fmt.Sprintf("PRAGMA table_info(%s)", t.quoted(d))
	if rows, err := db.QueryContext(ctx, pragmaSQL); err == nil {
		defer rows.Close()
		return readColumnRows(rows)
	}

	// Try information_schema. Postgres/MySQL/SQLite all support it. Placeholders
	// are dialect-specific ($1.. for Postgres, ? elsewhere), so build the
	// predicate with the dialect's placeholder syntax.
	var conds []string
	var args []any
	addCond := func(col, val string) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf("%s = %s", col, d.placeholder(len(args))))
	}
	if t.catalog != "" {
		addCond("table_catalog", t.catalog)
	}
	if t.schema != "" {
		addCond("table_schema", t.schema)
	}
	addCond("table_name", t.name)
	query := `SELECT column_name, data_type, is_nullable
		 FROM information_schema.columns
		 WHERE ` + strings.Join(conds, " AND ") + `
		 ORDER BY ordinal_position`
	if rows, err := db.QueryContext(ctx, query, args...); err == nil {
		defer rows.Close()
		return readColumnRows(rows)
	}

	// Last resort: DESCRIBE / SHOW COLUMNS (MySQL).
	rows, err := db.QueryContext(ctx, fmt.Sprintf("DESCRIBE %s", t.quoted(d)))
	if err == nil {
		defer rows.Close()
		return readDescribeRows(rows)
	}

	return nil, fmt.Errorf("could not discover columns for %q", t.name)
}

func readColumnRows(rows *sql.Rows) ([]engine.Column, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	// information_schema.columns: column_name, data_type, is_nullable
	// PRAGMA table_info: cid, name, type, notnull, dflt_value, pk
	nameIdx, typeIdx, nullIdx := -1, -1, -1
	for i, c := range cols {
		switch strings.ToLower(c) {
		case "column_name", "name":
			nameIdx = i
		case "data_type", "type":
			typeIdx = i
		case "is_nullable", "notnull":
			nullIdx = i
		}
	}
	if nameIdx < 0 || typeIdx < 0 {
		return nil, fmt.Errorf("discovery query returned unsupported columns: %v", cols)
	}

	var out []engine.Column
	for rows.Next() {
		dest := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range dest {
			ptrs[i] = &dest[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		name := dest[nameIdx].String
		typ := dest[typeIdx].String
		nullable := true
		if nullIdx >= 0 {
			n := dest[nullIdx].String
			// information_schema: YES/NO; PRAGMA table_info notnull: 1 = not null.
			nullable = !strings.EqualFold(n, "NO") && n != "1"
		}
		out = append(out, engine.Column{
			Name:     name,
			Type:     sqlTypeToEngine(typ),
			Nullable: nullable,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func readDescribeRows(rows *sql.Rows) ([]engine.Column, error) {
	// DESCRIBE returns Field, Type, Null, Key, Default, Extra.
	cols := []engine.Column{}
	for rows.Next() {
		var field, typ, null, key, def, extra sql.NullString
		if err := rows.Scan(&field, &typ, &null, &key, &def, &extra); err != nil {
			return nil, err
		}
		nullable := !null.Valid || strings.EqualFold(null.String, "YES")
		cols = append(cols, engine.Column{
			Name:     field.String,
			Type:     sqlTypeToEngine(typ.String),
			Nullable: nullable,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return cols, nil
}

func sqlTypeToEngine(t string) engine.Type {
	t = strings.ToLower(t)
	switch {
	case strings.Contains(t, "int") || t == "serial" || t == "bigserial":
		return engine.TypeInt
	case strings.Contains(t, "real") || strings.Contains(t, "float") || strings.Contains(t, "double") || strings.Contains(t, "numeric") || strings.Contains(t, "decimal"):
		return engine.TypeFloat
	case strings.Contains(t, "bool"):
		return engine.TypeBool
	case strings.Contains(t, "timestamp") || strings.Contains(t, "date") || strings.Contains(t, "time"):
		return engine.TypeTime
	default:
		return engine.TypeString
	}
}

// ---- row iterator ------------------------------------------------------------

type rowIter struct {
	db      *sql.DB
	rows    *sql.Rows
	schema  engine.Schema
	columns []string
	closed  bool
}

func (r *rowIter) Next() (engine.Row, bool, error) {
	if r.closed {
		return engine.Row{}, false, nil
	}
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return engine.Row{}, false, err
		}
		return engine.Row{}, false, nil
	}
	dest := make([]any, len(r.columns))
	for i := range dest {
		dest[i] = new(any)
	}
	if err := r.rows.Scan(dest...); err != nil {
		return engine.Row{}, false, err
	}
	vals := make([]engine.Value, len(r.columns))
	for i, name := range r.columns {
		idx := r.schema.Index(name)
		var t engine.Type
		if idx >= 0 {
			t = r.schema.Columns[idx].Type
		}
		ptr := dest[i].(*any)
		vals[i] = scanValue(*ptr, t)
	}
	return engine.Row{Values: vals}, true, nil
}

func (r *rowIter) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	err1 := r.rows.Close()
	err2 := r.db.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

func scanValue(raw any, t engine.Type) engine.Value {
	if raw == nil {
		return engine.Null()
	}
	switch v := raw.(type) {
	case int64:
		if t == engine.TypeFloat {
			return engine.FloatVal(float64(v))
		}
		if t == engine.TypeBool {
			return engine.BoolVal(v != 0)
		}
		return engine.IntVal(v)
	case float64:
		if t == engine.TypeInt {
			return engine.IntVal(int64(v))
		}
		return engine.FloatVal(v)
	case float32:
		return engine.FloatVal(float64(v))
	case int:
		return engine.IntVal(int64(v))
	case int32:
		return engine.IntVal(int64(v))
	case bool:
		return engine.BoolVal(v)
	case time.Time:
		return engine.TimeVal(v)
	case string:
		if t == engine.TypeInt {
			// Try to parse if the column was discovered as int but driver returned string.
			return engine.StringVal(v)
		}
		return engine.StringVal(v)
	case []byte:
		return engine.StringVal(string(v))
	default:
		return engine.AnyVal(raw)
	}
}

// ---- predicate / expression pushdown -----------------------------------------

// translateExpr converts a Turntable expression into a SQL string that can
// be sent to the remote database. It returns ok=false for expressions we choose
// not to push (functions, subqueries, etc.).
func translateExpr(e oparseSQL.Expr, d dialect) (string, bool) {
	switch ex := e.(type) {
	case *oparseSQL.LitInt:
		return fmt.Sprintf("%d", ex.V), true
	case *oparseSQL.LitFloat:
		return fmt.Sprintf("%g", ex.V), true
	case *oparseSQL.LitString:
		return quoteString(ex.V), true
	case *oparseSQL.LitBool:
		if ex.V {
			return "TRUE", true
		}
		return "FALSE", true
	case *oparseSQL.LitNull:
		return "NULL", true

	case *oparseSQL.ColRef:
		// Qualifiers are not needed for simple single-table scans; push down
		// only the column name.
		return d.quoteIdent(ex.Name), true

	case *oparseSQL.BinaryOp:
		return translateBinaryOp(ex, d)

	case *oparseSQL.UnaryOp:
		inner, ok := translateExpr(ex.Expr, d)
		if !ok {
			return "", false
		}
		switch ex.Op {
		case "NOT":
			return fmt.Sprintf("NOT (%s)", inner), true
		case "-":
			return fmt.Sprintf("-(%s)", inner), true
		}

	case *oparseSQL.InExpr:
		inner, ok := translateExpr(ex.Expr, d)
		if !ok {
			return "", false
		}
		var vals []string
		for _, l := range ex.List {
			v, ok := translateExpr(l, d)
			if !ok {
				return "", false
			}
			vals = append(vals, v)
		}
		if ex.Negate {
			return fmt.Sprintf("%s NOT IN (%s)", inner, strings.Join(vals, ", ")), true
		}
		return fmt.Sprintf("%s IN (%s)", inner, strings.Join(vals, ", ")), true

	case *oparseSQL.BetweenExpr:
		v, ok := translateExpr(ex.Expr, d)
		if !ok {
			return "", false
		}
		lo, ok := translateExpr(ex.Low, d)
		if !ok {
			return "", false
		}
		hi, ok := translateExpr(ex.High, d)
		if !ok {
			return "", false
		}
		if ex.Negate {
			return fmt.Sprintf("%s NOT BETWEEN %s AND %s", v, lo, hi), true
		}
		return fmt.Sprintf("%s BETWEEN %s AND %s", v, lo, hi), true

	case *oparseSQL.IsNullExpr:
		v, ok := translateExpr(ex.Expr, d)
		if !ok {
			return "", false
		}
		if ex.Negate {
			return fmt.Sprintf("%s IS NOT NULL", v), true
		}
		return fmt.Sprintf("%s IS NULL", v), true

	case *oparseSQL.LikeExpr:
		if !d.pushesLike() {
			return "", false
		}
		v, ok := translateExpr(ex.Expr, d)
		if !ok {
			return "", false
		}
		p, ok := translateExpr(ex.Pat, d)
		if !ok {
			return "", false
		}
		op := d.likeOp(ex.Insensitive)
		if ex.Negate {
			op = "NOT " + op
		}
		return fmt.Sprintf("%s %s %s", v, op, p), true
	}
	return "", false
}

// predicateExact reports whether the in-DB evaluation of a (already fully
// translatable) predicate matches the engine's semantics exactly. It is only
// false for a LIKE whose case-sensitivity the dialect can't reproduce — pushing
// such a predicate is still a correct *filter* (a superset the engine refines),
// but it must not gate LIMIT pushdown or the DB could truncate matching rows
// the engine has not yet seen.
func predicateExact(e oparseSQL.Expr, d dialect) bool {
	switch ex := e.(type) {
	case *oparseSQL.LikeExpr:
		return d.likeExact(ex.Insensitive)
	case *oparseSQL.BinaryOp:
		return predicateExact(ex.Left, d) && predicateExact(ex.Right, d)
	case *oparseSQL.UnaryOp:
		return predicateExact(ex.Expr, d)
	case *oparseSQL.InExpr:
		return predicateExact(ex.Expr, d)
	case *oparseSQL.BetweenExpr:
		return predicateExact(ex.Expr, d)
	case *oparseSQL.IsNullExpr:
		return predicateExact(ex.Expr, d)
	}
	return true
}

func translateBinaryOp(ex *oparseSQL.BinaryOp, d dialect) (string, bool) {
	left, ok := translateExpr(ex.Left, d)
	if !ok {
		return "", false
	}
	right, ok := translateExpr(ex.Right, d)
	if !ok {
		return "", false
	}

	switch ex.Op {
	case "=", "<>", "<", "<=", ">", ">=", "+", "-", "*", "/":
		return fmt.Sprintf("(%s %s %s)", left, ex.Op, right), true
	case "AND", "OR":
		return fmt.Sprintf("(%s %s %s)", left, ex.Op, right), true
	}
	return "", false
}

// dialect captures the per-driver SQL differences the connector must honor:
// identifier quoting and bind-placeholder syntax.
type dialect struct{ driver string }

// dialectFor returns the dialect for a database/sql driver name, defaulting to
// SQLite when unset.
func dialectFor(driver string) dialect {
	if driver == "" {
		driver = "sqlite"
	}
	return dialect{driver: driver}
}

// quoteIdent quotes an identifier for the dialect. MySQL uses backticks;
// sqlite and postgres use standard double quotes. (MySQL reads a double-quoted
// token as a string literal unless ANSI_QUOTES is set, so double-quoting a
// column there would select a constant string, not the column.)
func (d dialect) quoteIdent(s string) string {
	switch d.driver {
	case "mysql":
		return "`" + strings.ReplaceAll(s, "`", "``") + "`"
	case "sqlserver", "mssql":
		// SQL Server uses [bracket] quoting; a literal ] is doubled.
		return "[" + strings.ReplaceAll(s, "]", "]]") + "]"
	default:
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
}

// placeholder returns the bind placeholder for the n-th parameter (1-based).
// Postgres (lib/pq, pgx) uses $1, $2, ...; SQL Server uses @p1, @p2, ...; sqlite
// and mysql use ?.
func (d dialect) placeholder(n int) string {
	switch d.driver {
	case "postgres", "pgx":
		return fmt.Sprintf("$%d", n)
	case "sqlserver", "mssql":
		return fmt.Sprintf("@p%d", n)
	default:
		return "?"
	}
}

// usesTop reports whether a row limit is expressed as a leading SELECT TOP (n)
// (SQL Server) rather than a trailing LIMIT n.
func (d dialect) usesTop() bool {
	return d.driver == "sqlserver" || d.driver == "mssql"
}

// bucketExpr renders the SQL computing an epoch-aligned time bucket of `sec`
// seconds over the (already-quoted) column expression — the engine's 2-arg
// DATE_BIN. All three use the same floor(epoch/sec)*sec arithmetic, so the
// bucket boundaries agree with the engine's exactly. SQL Server declines:
// DATEDIFF(second, epoch, ts) overflows int for timestamps ±68 years out, so
// the engine buckets its rows instead (correct, just not pushed).
func (d dialect) bucketExpr(col string, sec int64) (string, bool) {
	switch d.driver {
	case "sqlite":
		return fmt.Sprintf("datetime((CAST(strftime('%%s', %s) AS INTEGER) / %d) * %d, 'unixepoch')", col, sec, sec), true
	case "postgres", "pgx":
		return fmt.Sprintf("to_timestamp(floor(extract(epoch from %s) / %d) * %d)", col, sec, sec), true
	case "mysql":
		return fmt.Sprintf("from_unixtime(floor(unix_timestamp(%s) / %d) * %d)", col, sec, sec), true
	}
	return "", false
}

// pushesLike reports whether LIKE/ILIKE predicates may be pushed to the server.
// SQL Server's LIKE case-sensitivity is collation-dependent and not knowable
// here, so pushing it could drop rows the engine must see (the pushdown must be
// a superset). We therefore keep LIKE in the engine for SQL Server.
func (d dialect) pushesLike() bool {
	return !(d.driver == "sqlserver" || d.driver == "mssql")
}

// likeOp returns the SQL operator for an engine LIKE/ILIKE. The engine's LIKE is
// case-sensitive and ILIKE case-insensitive. Postgres has both natively. MySQL
// and SQLite have only LIKE, which is case-insensitive by default — so it
// reproduces the engine's ILIKE, and approximates LIKE as a superset (the engine
// re-filters; see likeExact).
func (d dialect) likeOp(insensitive bool) string {
	if insensitive && (d.driver == "postgres" || d.driver == "pgx") {
		return "ILIKE"
	}
	return "LIKE"
}

// likeExact reports whether likeOp reproduces the engine's match exactly (vs.
// returning a superset the engine must refine). Postgres is always exact; the
// case-insensitive LIKE of MySQL/SQLite is exact only for the engine's ILIKE.
func (d dialect) likeExact(insensitive bool) bool {
	if d.driver == "postgres" || d.driver == "pgx" {
		return true
	}
	return insensitive
}

// quoteString escapes string literals with single quotes.
func quoteString(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}
