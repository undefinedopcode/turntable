// Package sqlc is the SQL database connector. It reads tables/views from a
// database via database/sql, with predicate/limit/order pushdown into the
// underlying query.
package sqlc

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	oparseSQL "github.com/april/turntable/internal/sql"
	_ "modernc.org/sqlite" // pure-Go SQLite driver; v0.2 default
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
	db, table, err := openAndTable(ds)
	if err != nil {
		return engine.Schema{}, err
	}
	defer db.Close()

	cols, err := discoverColumns(ctx, db, table)
	if err != nil {
		return engine.Schema{}, fmt.Errorf("discover %q: %w", table.name, err)
	}
	return engine.Schema{Columns: cols}, nil
}

// Scan executes a SELECT against the table, pushing down whatever the request
// asks for. Unpushable predicates are not pushed; the engine applies them in
// memory via a Filter.
func (Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	db, table, err := openAndTable(req.Dataset)
	if err != nil {
		return nil, err
	}

	// Build the query.
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
	fmt.Fprintf(&b, " FROM %s", table.quoted())

	// Try to push down the predicate. If translation fails (e.g. contains an
	// unsupported function or expression), we simply omit the WHERE clause and let
	// the engine filter in memory.
	if req.Predicate != nil {
		if where, ok := translateExpr(req.Predicate); ok {
			fmt.Fprintf(&b, " WHERE %s", where)
		}
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

	if req.Limit != nil {
		fmt.Fprintf(&b, " LIMIT %d", *req.Limit)
	}

	schema, err := discoverSchema(ctx, db, table)
	if err != nil {
		db.Close()
		return nil, err
	}

	rows, err := db.QueryContext(ctx, b.String())
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

func (t tableRef) quoted() string {
	parts := []string{}
	if t.catalog != "" {
		parts = append(parts, quoteIdent(t.catalog))
	}
	if t.schema != "" {
		parts = append(parts, quoteIdent(t.schema))
	}
	parts = append(parts, quoteIdent(t.name))
	return strings.Join(parts, ".")
}

func openAndTable(ds connector.Dataset) (*sql.DB, tableRef, error) {
	driver := stringOpt(ds.Options, "driver")
	dsn := stringOpt(ds.Options, "dsn")
	if driver == "" {
		driver = "sqlite"
	}
	if dsn == "" {
		return nil, tableRef{}, fmt.Errorf("sql connector requires dsn option")
	}
	name := ds.Name
	if name == "" {
		name = ds.Source
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, tableRef{}, fmt.Errorf("open %s: %w", driver, err)
	}
	return db, parseTableName(name), nil
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

func discoverSchema(ctx context.Context, db *sql.DB, t tableRef) (engine.Schema, error) {
	cols, err := discoverColumns(ctx, db, t)
	if err != nil {
		return engine.Schema{}, err
	}
	return engine.Schema{Columns: cols}, nil
}

func discoverColumns(ctx context.Context, db *sql.DB, t tableRef) ([]engine.Column, error) {
	// Discovery is dialect-specific. We try SQLite's PRAGMA first, then fall
	// back to information_schema (Postgres/MySQL), then DESCRIBE (MySQL).
	// Try SQLite pragma first.
	pragmaSQL := fmt.Sprintf("PRAGMA table_info(%s)", t.quoted())
	if rows, err := db.QueryContext(ctx, pragmaSQL); err == nil {
		defer rows.Close()
		return readColumnRows(rows)
	}

	// Try information_schema. Postgres/MySQL/SQLite all support it.
	var query string
	var args []any
	if t.catalog != "" {
		query = `SELECT column_name, data_type, is_nullable 
			 FROM information_schema.columns 
			 WHERE table_catalog = ? AND table_schema = ? AND table_name = ?
			 ORDER BY ordinal_position`
		args = []any{t.catalog, t.schema, t.name}
	} else if t.schema != "" {
		query = `SELECT column_name, data_type, is_nullable 
			 FROM information_schema.columns 
			 WHERE table_schema = ? AND table_name = ?
			 ORDER BY ordinal_position`
		args = []any{t.schema, t.name}
	} else {
		query = `SELECT column_name, data_type, is_nullable 
			 FROM information_schema.columns 
			 WHERE table_name = ?
			 ORDER BY ordinal_position`
		args = []any{t.name}
	}
	if rows, err := db.QueryContext(ctx, query, args...); err == nil {
		defer rows.Close()
		return readColumnRows(rows)
	}

	// Last resort: DESCRIBE / SHOW COLUMNS (MySQL).
	rows, err := db.QueryContext(ctx, fmt.Sprintf("DESCRIBE %s", t.quoted()))
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
func translateExpr(e oparseSQL.Expr) (string, bool) {
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
		if ex.Qualifier != "" {
			// Qualifiers are not needed for simple single-table scans; push down
			// only the column name.
			return quoteIdent(ex.Name), true
		}
		return quoteIdent(ex.Name), true

	case *oparseSQL.BinaryOp:
		return translateBinaryOp(ex)

	case *oparseSQL.UnaryOp:
		inner, ok := translateExpr(ex.Expr)
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
		inner, ok := translateExpr(ex.Expr)
		if !ok {
			return "", false
		}
		var vals []string
		for _, l := range ex.List {
			v, ok := translateExpr(l)
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
		v, ok := translateExpr(ex.Expr)
		if !ok {
			return "", false
		}
		lo, ok := translateExpr(ex.Low)
		if !ok {
			return "", false
		}
		hi, ok := translateExpr(ex.High)
		if !ok {
			return "", false
		}
		if ex.Negate {
			return fmt.Sprintf("%s NOT BETWEEN %s AND %s", v, lo, hi), true
		}
		return fmt.Sprintf("%s BETWEEN %s AND %s", v, lo, hi), true

	case *oparseSQL.IsNullExpr:
		v, ok := translateExpr(ex.Expr)
		if !ok {
			return "", false
		}
		if ex.Negate {
			return fmt.Sprintf("%s IS NOT NULL", v), true
		}
		return fmt.Sprintf("%s IS NULL", v), true

	case *oparseSQL.LikeExpr:
		v, ok := translateExpr(ex.Expr)
		if !ok {
			return "", false
		}
		p, ok := translateExpr(ex.Pat)
		if !ok {
			return "", false
		}
		if ex.Negate {
			return fmt.Sprintf("%s NOT LIKE %s", v, p), true
		}
		return fmt.Sprintf("%s LIKE %s", v, p), true
	}
	return "", false
}

func translateBinaryOp(ex *oparseSQL.BinaryOp) (string, bool) {
	left, ok := translateExpr(ex.Left)
	if !ok {
		return "", false
	}
	right, ok := translateExpr(ex.Right)
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

// quoteIdent wraps identifiers in double quotes for portability.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// quoteString escapes string literals with single quotes.
func quoteString(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}
