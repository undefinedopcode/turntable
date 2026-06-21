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

	"github.com/april/octoparser/internal/config"
	"github.com/april/octoparser/internal/connector"
	"github.com/april/octoparser/internal/connector/connectors/csvc"
	"github.com/april/octoparser/internal/connector/connectors/jsonc"
	"github.com/april/octoparser/internal/connector/connectors/sqlc"
	"github.com/april/octoparser/internal/connector/connectors/yamlc"
	"github.com/april/octoparser/internal/engine"
	"github.com/april/octoparser/internal/plan"
	"github.com/april/octoparser/internal/render"
	"github.com/april/octoparser/internal/sql"
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
}

// NewApp builds an App with all built-in connectors registered.
func NewApp() *App {
	reg := connector.NewRegistry()
	_ = reg.RegisterConnector(jsonc.New())
	_ = reg.RegisterConnector(csvc.New())
	_ = reg.RegisterConnector(yamlc.New())
	_ = reg.RegisterConnector(sqlc.New())
	return &App{
		Out:    os.Stdout,
		Err:    os.Stderr,
		Reg:    reg,
		Output: render.FormatTable,
		Funcs:  engine.NewFuncRegistry(),
	}
}

// registerSources wires sources declared in the config file into the registry.
// Each source maps a logical name to a connector + Dataset. Unknown connector
// names are reported but skipped (non-fatal) so qualified refs still work.
func (a *App) registerSources(cfg *config.File) {
	for name, src := range cfg.Sources {
		conn := a.Reg.Connector(src.Connector)
		if conn == nil {
			fmt.Fprintf(a.Err, "warning: source %q uses unknown connector %q\n", name, src.Connector)
			continue
		}
		opts := map[string]any{}
		if src.Delimiter != "" {
			opts["delimiter"] = src.Delimiter
		}
		for k, v := range src.Options {
			opts[k] = v
		}
		// Build a dataset: file connectors use Path; sql connector uses DSN/driver/table.
		if src.Connector == "sql" {
			if src.Driver == "" {
				fmt.Fprintf(a.Err, "warning: source %q requires driver\n", name)
				continue
			}
			if src.DSN == "" {
				fmt.Fprintf(a.Err, "warning: source %q requires dsn\n", name)
				continue
			}
			// The table name defaults to the logical source name when "table"
			// is not specified, so `FROM <name>` resolves to a table of the
			// same name in the database.
			table := src.Table
			if table == "" {
				table = name
			}
			ds := connector.Dataset{
				Name:   table,
				Source: table,
				Options: opts,
			}
			ds.Options["driver"] = src.Driver
			ds.Options["dsn"] = src.DSN
			if err := a.Reg.RegisterSource(name, sqlc.New(), ds); err != nil {
				fmt.Fprintf(a.Err, "warning: %v\n", err)
			}
			continue
		}
		ds := connector.Dataset{Name: name, Source: src.Path, Options: opts}
		if err := a.Reg.RegisterSource(name, conn, ds); err != nil {
			fmt.Fprintf(a.Err, "warning: %v\n", err)
		}
	}
}

// Run parses the given args (excluding the program name) and runs the
// appropriate mode. It returns the process exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("octoparser", flag.ContinueOnError)
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
	)
	fs.StringVar(&configPath, "config", "", "path to octoparser.yaml")
	fs.StringVar(&configPath, "c", "", "short for --config")
	fs.StringVar(&file, "f", "", "read query from file")
	fs.StringVar(&output, "output", "", "output format")
	fs.StringVar(&output, "o", "", "short for --output")
	fs.BoolVar(&repl, "repl", false, "interactive mode")
	fs.BoolVar(&explain, "explain", false, "print plan instead of running")
	fs.BoolVar(&quiet, "quiet", false, "suppress metadata")
	fs.IntVar(&maxRows, "max-rows", 0, "cap rows rendered (0 = unlimited)")
	fs.BoolVar(&strict, "strict", false, "hard errors for type coercion failures")
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
			fmt.Fprintln(a.Err, "usage: octoparser [flags] <query>")
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
	p, err := plan.Build(ctx, stmt.(*sql.SelectStmt), a.Reg, plan.IfStrict(a.strict)...)
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
		return indent + "Scan " + node.Source.Name
	case *plan.NoFrom:
		return indent + "NoFrom"
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
		return indent + "Join\n" + formatPlan(node.Left, depth+1) + "\n" + formatPlan(node.Right, depth+1)
	case *plan.Aggregate:
		var aggNames []string
		for _, ag := range node.Aggs {
			aggNames = append(aggNames, ag.Func)
		}
		return indent + "Aggregate [" + strings.Join(aggNames, ", ") + "]\n" + formatPlan(node.Child, depth+1)
	}
	return indent + fmt.Sprintf("%T", n)
}