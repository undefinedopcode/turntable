// Package cli wires command-line flags, config, connector registration, and
// query execution into a runnable Application. It owns the REPL loop too.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/april/turntable/internal/config"
	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/connector/connectors/athenac"
	"github.com/april/turntable/internal/connector/connectors/azdevopsc"
	"github.com/april/turntable/internal/connector/connectors/aztablesc"
	"github.com/april/turntable/internal/connector/connectors/claudelogsc"
	"github.com/april/turntable/internal/connector/connectors/csvc"
	"github.com/april/turntable/internal/connector/connectors/cwlogsc"
	"github.com/april/turntable/internal/connector/connectors/cwmetricsc"
	"github.com/april/turntable/internal/connector/connectors/dynamodbc"
	"github.com/april/turntable/internal/connector/connectors/excelc"
	"github.com/april/turntable/internal/connector/connectors/httpc"
	"github.com/april/turntable/internal/connector/connectors/jsonc"
	"github.com/april/turntable/internal/connector/connectors/linearc"
	"github.com/april/turntable/internal/connector/connectors/logc"
	"github.com/april/turntable/internal/connector/connectors/memc"
	"github.com/april/turntable/internal/connector/connectors/parquetc"
	"github.com/april/turntable/internal/connector/connectors/sqlc"
	"github.com/april/turntable/internal/connector/connectors/trelloc"
	"github.com/april/turntable/internal/connector/connectors/yamlc"
	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/plan"
	"github.com/april/turntable/internal/render"
	"github.com/april/turntable/internal/sql"
)

// App holds CLI state shared across invocations / REPL lines.
type App struct {
	Out    io.Writer
	Err    io.Writer
	Reg    *connector.Registry
	Output render.Format
	Funcs  *engine.FuncRegistry

	// replExplain is toggled by the .explain dot-command in the REPL.
	replExplain bool

	// maxRows caps the number of rows rendered (0 = unlimited). Acts as a
	// safety guard against accidentally dumping huge result sets.
	maxRows int

	// strict makes type-coercion failures hard errors instead of NULL.
	strict bool

	// uploadDir is a per-serve temporary directory holding files uploaded
	// through the web UI; it is created in serve() and removed on shutdown.
	uploadDir string

	// mem backs session-scoped materialized views; matViews keeps each view's
	// defining query (by name) so REFRESH can re-run it. See matview.go.
	mem      *memc.Connector
	matViews map[string]*matView
}

// NewApp builds an App with all built-in connectors registered.
func NewApp() *App {
	reg := connector.NewRegistry()
	_ = reg.RegisterConnector(jsonc.New())
	_ = reg.RegisterConnector(csvc.New())
	_ = reg.RegisterConnector(yamlc.New())
	_ = reg.RegisterConnector(excelc.New())
	_ = reg.RegisterConnector(sqlc.New())
	httpConn := httpc.New()
	_ = reg.RegisterConnector(httpConn)
	_ = reg.RegisterConnectorAs("https", httpConn) // https:// URL refs use the same connector
	_ = reg.RegisterConnector(linearc.New())
	_ = reg.RegisterConnector(logc.New())
	_ = reg.RegisterConnector(parquetc.New())
	_ = reg.RegisterConnector(cwlogsc.New())
	_ = reg.RegisterConnector(cwmetricsc.New())
	_ = reg.RegisterConnector(dynamodbc.New())
	_ = reg.RegisterConnector(aztablesc.New())
	_ = reg.RegisterConnector(athenac.New())
	_ = reg.RegisterConnector(trelloc.New())
	_ = reg.RegisterConnector(azdevopsc.New())
	_ = reg.RegisterConnector(claudelogsc.New())
	mem := memc.New()
	_ = reg.RegisterConnector(mem) // also serves "mem:<view>" qualified refs
	return &App{
		Out:      os.Stdout,
		Err:      os.Stderr,
		Reg:      reg,
		Output:   render.FormatTable,
		Funcs:    engine.NewFuncRegistry(),
		mem:      mem,
		matViews: map[string]*matView{},
	}
}

// registerSources wires sources declared in the config file into the registry.
// Each source maps a logical name to a connector + Dataset. Unknown connector
// names are reported but skipped (non-fatal) so qualified refs still work.
func (a *App) registerSources(cfg *config.File) {
	for name, src := range cfg.Sources {
		if err := a.registerSource(name, src); err != nil {
			fmt.Fprintf(a.Err, "warning: %v\n", err)
		}
	}
}

// registerSource binds one logical name to a connector + Dataset. It returns
// an error (not printed) so callers — config loading and the .use REPL
// command — can decide how to surface failures.
//
// For SQL sources, a missing or "*" table expands to every user table in the
// database, each registered under its own table name.
func (a *App) registerSource(name string, src config.Source) error {
	names, err := a.registerSourceExpand(context.Background(), name, src)
	if err != nil {
		return err
	}
	if len(names) > 1 {
		fmt.Fprintf(a.Err, "source %q expanded to %d tables: %s\n", name, len(names), strings.Join(names, ", "))
	}
	return nil
}

// applySourceField routes one key=value pair onto a config.Source, mapping the
// well-known keys to typed fields and everything else into Options. "path" is a
// file path for file connectors but a connector option (e.g. http's JSON
// pointer) for everyone else, so src.Connector must be set first. Shared by the
// REPL .use command and the web UI's add-source endpoint.
func applySourceField(src *config.Source, key, val string) {
	switch strings.ToLower(key) {
	case "path":
		if isFileConnector(src.Connector) {
			src.Path = val
			return
		}
	case "url":
		src.URL = val
		return
	case "driver":
		src.Driver = val
		return
	case "dsn":
		src.DSN = val
		return
	case "table":
		src.Table = val
		return
	case "sheet":
		src.Sheet = val
		return
	case "delimiter":
		src.Delimiter = val
		return
	}
	if src.Options == nil {
		src.Options = map[string]any{}
	}
	src.Options[key] = val
}

// registerSourceExpand does the work of registerSource and returns the logical
// names that were registered (one for a normal source, many for a wildcard
// SQL source). Splitting it out makes the expansion result testable.
func (a *App) registerSourceExpand(ctx context.Context, name string, src config.Source) ([]string, error) {
	conn := a.Reg.Connector(src.Connector)
	if conn == nil {
		return nil, fmt.Errorf("source %q uses unknown connector %q", name, src.Connector)
	}
	opts := map[string]any{}
	if src.Delimiter != "" {
		opts["delimiter"] = src.Delimiter
	}
	if src.Sheet != "" {
		opts["sheet"] = src.Sheet
	}
	if src.URL != "" {
		opts["url"] = src.URL
	}
	for k, v := range src.Options {
		opts[k] = v
	}
	// Build a dataset: file connectors use Path; sql connector uses DSN/driver/table.
	if src.Connector == "sql" {
		if src.Driver == "" {
			return nil, fmt.Errorf("source %q requires driver", name)
		}
		if src.DSN == "" {
			return nil, fmt.Errorf("source %q requires dsn", name)
		}
		opts["driver"] = src.Driver
		opts["dsn"] = src.DSN

		// An explicit wildcard table ("*") expands to every user table in the
		// database. An omitted table keeps the legacy lazy single-source
		// behavior (the source name is used as the table at query time).
		if src.Table == "*" {
			return a.expandSQLTables(ctx, name, opts)
		}
		table := src.Table
		if table == "" {
			table = name
		}
		ds := connector.Dataset{
			Name:    table,
			Source:  table,
			Options: opts,
		}
		if err := a.Reg.RegisterSource(name, sqlc.New(), ds); err != nil {
			return nil, err
		}
		return []string{name}, nil
	}
	// Excel wildcard: sheet="*" expands to every worksheet in the workbook.
	if src.Connector == "excel" && src.Sheet == "*" {
		return a.expandExcelSheets(ctx, name, src.Path, opts)
	}
	// DynamoDB / Azure Tables / Athena: table names a table; table="*" expands to
	// every table (in the account, or the Athena database), each registered under
	// its own name.
	if src.Connector == "dynamodb" || src.Connector == "azuretables" || src.Connector == "athena" {
		if src.Table == "*" {
			switch src.Connector {
			case "dynamodb":
				return a.expandDynamoTables(ctx, name, opts)
			case "azuretables":
				return a.expandAzureTables(ctx, name, opts)
			default:
				return a.expandAthenaTables(ctx, name, opts)
			}
		}
		table := src.Table
		if table == "" {
			table = name
		}
		opts["table"] = table
		ds := connector.Dataset{Name: table, Source: table, Options: opts}
		if err := a.Reg.RegisterSource(name, conn, ds); err != nil {
			return nil, err
		}
		return []string{name}, nil
	}
	// File connectors locate data by Path; URL/API connectors (http, linear,
	// cloudwatch*) locate it by URL or rely purely on options. Source carries
	// whichever locator is present so the connector can read it from
	// ds.Source as well as ds.Options.
	locator := src.Path
	if locator == "" {
		locator = src.URL
	}
	ds := connector.Dataset{Name: name, Source: locator, Options: opts}
	if err := a.Reg.RegisterSource(name, conn, ds); err != nil {
		return nil, err
	}
	return []string{name}, nil
}

// expandExcelSheets enumerates the worksheets in an Excel workbook and
// registers each one under its own sheet name (with a name prefix on
// collision), mirroring expandSQLTables.
func (a *App) expandExcelSheets(ctx context.Context, name, path string, opts map[string]any) ([]string, error) {
	if path == "" {
		return nil, fmt.Errorf("source %q requires path", name)
	}
	ec := excelc.New()
	datasets, err := ec.DatasetsFor(ctx, connector.Dataset{Source: path, Options: opts})
	if err != nil {
		return nil, fmt.Errorf("enumerate %q: %w", name, err)
	}
	var registered []string
	for _, d := range datasets {
		logical := d.Name
		if _, ok := a.Reg.Resolve(logical); ok {
			logical = name + "_" + d.Name
		}
		if err := a.Reg.RegisterSource(logical, excelc.New(), d); err != nil {
			return registered, err
		}
		registered = append(registered, logical)
	}
	if len(registered) == 0 {
		return nil, fmt.Errorf("source %q: workbook has no sheets", name)
	}
	return registered, nil
}

// expandDynamoTables enumerates the tables in a DynamoDB account and registers
// each one under its own table name (prefixed with the source name on
// collision), mirroring expandSQLTables.
func (a *App) expandDynamoTables(ctx context.Context, name string, opts map[string]any) ([]string, error) {
	dc := dynamodbc.New()
	datasets, err := dc.DatasetsFor(ctx, connector.Dataset{Options: opts})
	if err != nil {
		return nil, fmt.Errorf("enumerate %q: %w", name, err)
	}
	var registered []string
	for _, d := range datasets {
		logical := d.Name
		if _, ok := a.Reg.Resolve(logical); ok {
			logical = name + "_" + d.Name
		}
		if err := a.Reg.RegisterSource(logical, dynamodbc.New(), d); err != nil {
			return registered, err
		}
		registered = append(registered, logical)
	}
	if len(registered) == 0 {
		return nil, fmt.Errorf("source %q: account has no DynamoDB tables", name)
	}
	return registered, nil
}

// expandAzureTables enumerates the tables in an Azure Storage account and
// registers each one under its own table name (prefixed with the source name on
// collision), mirroring expandDynamoTables.
func (a *App) expandAzureTables(ctx context.Context, name string, opts map[string]any) ([]string, error) {
	datasets, err := aztablesc.New().DatasetsFor(ctx, connector.Dataset{Options: opts})
	if err != nil {
		return nil, fmt.Errorf("enumerate %q: %w", name, err)
	}
	var registered []string
	for _, d := range datasets {
		logical := d.Name
		if _, ok := a.Reg.Resolve(logical); ok {
			logical = name + "_" + d.Name
		}
		if err := a.Reg.RegisterSource(logical, aztablesc.New(), d); err != nil {
			return registered, err
		}
		registered = append(registered, logical)
	}
	if len(registered) == 0 {
		return nil, fmt.Errorf("source %q: account has no tables", name)
	}
	return registered, nil
}

// expandAthenaTables enumerates the tables in an Athena database (Glue catalog)
// and registers each one under its own name, mirroring expandDynamoTables.
func (a *App) expandAthenaTables(ctx context.Context, name string, opts map[string]any) ([]string, error) {
	datasets, err := athenac.New().DatasetsFor(ctx, connector.Dataset{Options: opts})
	if err != nil {
		return nil, fmt.Errorf("enumerate %q: %w", name, err)
	}
	var registered []string
	for _, d := range datasets {
		logical := d.Name
		if _, ok := a.Reg.Resolve(logical); ok {
			logical = name + "_" + d.Name
		}
		if err := a.Reg.RegisterSource(logical, athenac.New(), d); err != nil {
			return registered, err
		}
		registered = append(registered, logical)
	}
	if len(registered) == 0 {
		return nil, fmt.Errorf("source %q: database has no Athena tables", name)
	}
	return registered, nil
}

// expandSQLTables enumerates the tables in a SQL database and registers each
// one. The logical name defaults to the table name; if a name is already taken
// it is prefixed with the source name to avoid collisions.
func (a *App) expandSQLTables(ctx context.Context, name string, opts map[string]any) ([]string, error) {
	sc := sqlc.New()
	datasets, err := sc.DatasetsFor(ctx, connector.Dataset{Options: opts})
	if err != nil {
		return nil, fmt.Errorf("enumerate %q: %w", name, err)
	}
	var registered []string
	for _, d := range datasets {
		// Prefer the bare table name; fall back to name_table on collision.
		logical := d.Name
		if _, ok := a.Reg.Resolve(logical); ok {
			logical = name + "_" + d.Name
		}
		if err := a.Reg.RegisterSource(logical, sqlc.New(), d); err != nil {
			return registered, err
		}
		registered = append(registered, logical)
	}
	if len(registered) == 0 {
		return nil, fmt.Errorf("source %q: database has no user tables", name)
	}
	return registered, nil
}

// Run parses the given args (excluding the program name) and runs the
// appropriate mode. It returns the process exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("turntable", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	var (
		configPath string
		file       string
		output     string
		repl       bool
		explain    bool
		quiet      bool
		maxRows    int
		strict     bool
		serve      bool
		addr       string
	)
	fs.StringVar(&configPath, "config", "", "path to turntable.yaml")
	fs.StringVar(&configPath, "c", "", "short for --config")
	fs.StringVar(&file, "f", "", "read query from file")
	fs.StringVar(&output, "output", "", "output format")
	fs.StringVar(&output, "o", "", "short for --output")
	fs.BoolVar(&repl, "repl", false, "interactive mode")
	fs.BoolVar(&explain, "explain", false, "print plan instead of running")
	fs.BoolVar(&quiet, "quiet", false, "suppress metadata")
	fs.IntVar(&maxRows, "max-rows", 0, "cap rows rendered (0 = unlimited)")
	fs.BoolVar(&strict, "strict", false, "hard errors for type coercion failures")
	fs.BoolVar(&serve, "serve", false, "serve the web query UI instead of running a query")
	fs.StringVar(&addr, "addr", "localhost:8080", "address for --serve (host:port)")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	if output != "" {
		a.Output = render.Format(output)
	}
	a.maxRows = maxRows
	a.strict = strict

	// Load config and register named sources.
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(a.Err, "config error: %v\n", err)
		cfg = &config.File{Sources: map[string]config.Source{}}
	}
	a.registerSources(cfg)
	if cfg.Defaults.Output != "" && output == "" {
		a.Output = render.Format(cfg.Defaults.Output)
	}

	if serve {
		return a.serve(ctx, addr)
	}

	if repl {
		return a.repl(ctx)
	}

	// Determine the query text.
	var query string
	switch {
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintf(a.Err, "read %q: %v\n", file, err)
			return 1
		}
		query = strings.TrimSpace(string(b))
	default:
		rest := fs.Args()
		if len(rest) == 0 {
			fmt.Fprintln(a.Err, "usage: turntable [flags] <query>")
			fs.PrintDefaults()
			return 1
		}
		query = strings.Join(rest, " ")
	}

	return a.runQuery(ctx, query, explain, quiet)
}

func (a *App) runQuery(ctx context.Context, query string, explain, quiet bool) int {
	return a.runQueryInto(ctx, query, explain, quiet, a.Out, a.Err)
}

// runQueryInto parses, plans, and executes one query, writing results to out
// and metadata/errors to errw. Splitting the writer out from a.Out lets the
// REPL route output through readline's managed TTY writers.
func (a *App) runQueryInto(ctx context.Context, query string, explain, quiet bool, out, errw io.Writer) int {
	stmt, err := sql.Parse(query)
	if err != nil {
		fmt.Fprintf(errw, "parse error: %v\n", err)
		return 1
	}

	// Materialized-view commands are session statements, not row-producing
	// queries; they are handled outside the plan/exec/render path.
	switch s := stmt.(type) {
	case *sql.CreateMatViewStmt:
		return a.createMatView(ctx, s, explain, out, errw)
	case *sql.RefreshMatViewStmt:
		return a.refreshMatView(ctx, s, explain, out, errw)
	case *sql.DropMatViewStmt:
		return a.dropMatView(s, explain, errw)
	}

	p, err := plan.Build(ctx, stmt, a.Reg, plan.IfStrict(a.strict)...)
	if err != nil {
		fmt.Fprintf(errw, "plan error: %v\n", err)
		return 1
	}

	if explain {
		fmt.Fprintf(out, "%s\n", formatPlan(p.Root, 0))
		return 0
	}

	it, schema, err := plan.Exec(ctx, p)
	if err != nil {
		fmt.Fprintf(errw, "exec error: %v\n", err)
		return 1
	}

	// Table format needs all rows up front (for column widths); other formats
	// stream row-by-row to keep memory bounded.
	if a.Output == render.FormatTable {
		rows, err := engine.Materialize(ctx, it)
		if err != nil {
			fmt.Fprintf(errw, "exec error: %v\n", err)
			return 1
		}
		if a.maxRows > 0 && len(rows) > a.maxRows {
			rows = rows[:a.maxRows]
		}
		r, err := render.New(a.Output)
		if err != nil {
			fmt.Fprintf(errw, "%v\n", err)
			return 1
		}
		if err := r.Render(out, schema, rows); err != nil {
			fmt.Fprintf(errw, "render error: %v\n", err)
			return 1
		}
		if !quiet && len(rows) > 0 {
			fmt.Fprintf(errw, "(%d rows)\n", len(rows))
		}
		return 0
	}

	sr, err := render.NewStream(a.Output)
	if err != nil {
		// Fall back to materialize if streaming isn't supported.
		rows, merr := engine.Materialize(ctx, it)
		if merr != nil {
			fmt.Fprintf(errw, "exec error: %v\n", merr)
			return 1
		}
		r, rerr := render.New(a.Output)
		if rerr != nil {
			fmt.Fprintf(errw, "%v\n", rerr)
			return 1
		}
		if err := r.Render(out, schema, rows); err != nil {
			fmt.Fprintf(errw, "render error: %v\n", err)
			return 1
		}
		if !quiet && len(rows) > 0 {
			fmt.Fprintf(errw, "(%d rows)\n", len(rows))
		}
		return 0
	}
	cappedIt := it
	if a.maxRows > 0 {
		cappedIt = engine.NewLimitIter(it, &a.maxRows, 0)
	}
	n, err := sr.RenderStream(out, schema, cappedIt)
	if err != nil {
		fmt.Fprintf(errw, "render error: %v\n", err)
		return 1
	}
	if !quiet && n > 0 {
		fmt.Fprintf(errw, "(%d rows)\n", n)
	}
	return 0
}

// formatPlan renders a plan tree as indented text for --explain.
func formatPlan(n plan.Node, depth int) string {
	indent := strings.Repeat("  ", depth)
	switch node := n.(type) {
	case *plan.Scan:
		line := indent + "Scan " + node.Source.Name
		var pd []string
		if node.Predicate != nil {
			pd = append(pd, "predicate")
		}
		if node.Limit != nil {
			pd = append(pd, fmt.Sprintf("limit=%d", *node.Limit))
		}
		if len(node.OrderBy) > 0 {
			pd = append(pd, "order")
		}
		if len(pd) > 0 {
			line += " [pushdown: " + strings.Join(pd, ", ") + "]"
		}
		return line
	case *plan.NoFrom:
		return indent + "NoFrom"
	case *plan.Subquery:
		return indent + "Subquery " + node.Alias + "\n" + formatPlan(node.Child, depth+1)
	case *plan.CTERef:
		// Every reference shares one materialization (run once, replayed); the
		// [materialized] tag marks that, and the CTE's plan is shown under each
		// reference for readability.
		line := indent + "CTE " + node.Name + " [materialized]"
		if node.Mat != nil && node.Mat.Plan != nil {
			return line + "\n" + formatPlan(node.Mat.Plan, depth+1)
		}
		return line
	case *plan.SetOp:
		names := map[sql.SetOpKind]string{
			sql.SetUnion: "Union", sql.SetIntersect: "Intersect", sql.SetExcept: "Except",
		}
		kind := names[node.Op]
		if node.All {
			kind += " all"
		}
		return indent + kind + "\n" + formatPlan(node.Left, depth+1) + "\n" + formatPlan(node.Right, depth+1)
	case *plan.Window:
		var names []string
		for _, s := range node.Specs {
			names = append(names, s.Func)
		}
		return indent + "Window [" + strings.Join(names, ", ") + "]\n" + formatPlan(node.Child, depth+1)
	case *plan.Apply:
		s := indent + fmt.Sprintf("Apply [%d subquer%s]", len(node.Specs), map[bool]string{true: "y", false: "ies"}[len(node.Specs) == 1])
		s += "\n" + formatPlan(node.Child, depth+1)
		for _, spec := range node.Specs {
			s += "\n" + formatPlan(spec.Inner, depth+1)
		}
		return s
	case *plan.Filter:
		return indent + "Filter\n" + formatPlan(node.Child, depth+1)
	case *plan.Project:
		var names []string
		for _, o := range node.Outputs {
			names = append(names, o.Name)
		}
		d := ""
		if node.Distinct {
			d = " DISTINCT"
		}
		return indent + "Project" + d + " [" + strings.Join(names, ", ") + "]\n" + formatPlan(node.Child, depth+1)
	case *plan.Sort:
		return indent + "Sort\n" + formatPlan(node.Child, depth+1)
	case *plan.Limit:
		return indent + "Limit\n" + formatPlan(node.Child, depth+1)
	case *plan.Join:
		label := map[sql.JoinKind]string{
			sql.JoinInner: "Join", sql.JoinLeft: "Left join", sql.JoinRight: "Right join",
			sql.JoinFull: "Full join", sql.JoinSemi: "Semi join", sql.JoinAnti: "Anti join",
		}[node.Kind]
		if label == "" {
			label = "Join"
		}
		var notes []string
		switch {
		case len(node.LeftKeys) == 0:
			notes = append(notes, "nested loop")
		case len(node.LeftKeys) > 1:
			notes = append(notes, fmt.Sprintf("%d keys", len(node.LeftKeys)))
		}
		if node.Residual != nil {
			notes = append(notes, "residual")
		}
		if len(notes) > 0 {
			label += " [" + strings.Join(notes, ", ") + "]"
		}
		return indent + label + "\n" + formatPlan(node.Left, depth+1) + "\n" + formatPlan(node.Right, depth+1)
	case *plan.Aggregate:
		var aggNames []string
		for _, ag := range node.Aggs {
			aggNames = append(aggNames, ag.Func)
		}
		return indent + "Aggregate [" + strings.Join(aggNames, ", ") + "]\n" + formatPlan(node.Child, depth+1)
	}
	return indent + fmt.Sprintf("%T", n)
}
