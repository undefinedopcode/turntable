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
	"github.com/april/octoparser/internal/connector/connectors/yamlc"
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
}

// NewApp builds an App with all built-in connectors registered.
func NewApp() *App {
	reg := connector.NewRegistry()
	_ = reg.RegisterConnector(jsonc.New())
	_ = reg.RegisterConnector(csvc.New())
	_ = reg.RegisterConnector(yamlc.New())
	return &App{
		Out: os.Stdout,
		Err: os.Stderr,
		Reg: reg,
		Output: render.FormatTable,
	}
}

// Flags parses the given args (excluding the program name) and runs the
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
	)
	fs.StringVar(&configPath, "config", "", "path to octoparser.yaml")
	fs.StringVar(&configPath, "c", "", "short for --config")
	fs.StringVar(&file, "f", "", "read query from file")
	fs.StringVar(&output, "output", "", "output format")
	fs.StringVar(&output, "o", "", "short for --output")
	fs.BoolVar(&repl, "repl", false, "interactive mode")
	fs.BoolVar(&explain, "explain", false, "print plan instead of running")
	fs.BoolVar(&quiet, "quiet", false, "suppress metadata")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	if output != "" {
		a.Output = render.Format(output)
	}

	// Load config (sources registered in v0.1).
	_, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(a.Err, "config error: %v\n", err)
		// non-fatal: qualified refs may still work
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

	return a.runQuery(ctx, query, explain)
}

func (a *App) runQuery(ctx context.Context, query string, explain bool) int {
	stmt, err := sql.Parse(query)
	if err != nil {
		fmt.Fprintf(a.Err, "parse error: %v\n", err)
		return 1
	}
	p, err := plan.Build(ctx, stmt.(*sql.SelectStmt), a.Reg)
	if err != nil {
		fmt.Fprintf(a.Err, "plan error: %v\n", err)
		return 1
	}

	if explain {
		fmt.Fprintf(a.Out, "plan: %+v\n", p.Root)
		return 0
	}

	// Execution lands in v0.1. For now we report that the pipeline is wired.
	r, err := render.New(a.Output)
	if err != nil {
		fmt.Fprintf(a.Err, "%v\n", err)
		return 1
	}
	if err := r.Render(a.Out, p.OutputSchema, nil); err != nil {
		fmt.Fprintf(a.Err, "render error: %v\n", err)
		return 1
	}
	return 0
}

func (a *App) repl(ctx context.Context) int {
	// TODO(v0.3): full REPL with history + completion.
	fmt.Fprintln(a.Err, "REPL not yet implemented (v0.3)")
	return 1
}