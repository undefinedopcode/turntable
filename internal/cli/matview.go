package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/connector/connectors/memc"
	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/plan"
	"github.com/april/turntable/internal/sql"
)

// matView records a materialized view's defining query so REFRESH can re-run it.
// The buffered rows themselves live in App.mem (the in-memory connector).
type matView struct {
	name  string
	query sql.Statement
}

// The createMatView/refreshMatView/dropMatView wrappers adapt the shared core
// operations (which return a status notice + error) to the CLI/REPL: --explain
// prints the inner plan, a notice goes to errw, and an error sets exit code 1.
// serve.go reuses the same *Core methods for the web API.

func (a *App) createMatView(ctx context.Context, s *sql.CreateMatViewStmt, explain bool, out, errw io.Writer) int {
	if explain {
		return a.explainStatement(ctx, s.Query, out, errw)
	}
	notice, err := a.createMatViewCore(ctx, s)
	return noticeExit(notice, err, errw)
}

func (a *App) refreshMatView(ctx context.Context, s *sql.RefreshMatViewStmt, explain bool, out, errw io.Writer) int {
	if explain {
		mv, ok := a.matViews[s.Name]
		if !ok {
			fmt.Fprintf(errw, "error: materialized view %q does not exist\n", s.Name)
			return 1
		}
		return a.explainStatement(ctx, mv.query, out, errw)
	}
	notice, err := a.refreshMatViewCore(ctx, s)
	return noticeExit(notice, err, errw)
}

func (a *App) dropMatView(s *sql.DropMatViewStmt, explain bool, errw io.Writer) int {
	if explain {
		fmt.Fprintf(errw, "DROP MATERIALIZED VIEW %s (no plan)\n", s.Name)
		return 0
	}
	notice, err := a.dropMatViewCore(s)
	return noticeExit(notice, err, errw)
}

// noticeExit prints a core operation's notice (or error) and returns the CLI
// exit code.
func noticeExit(notice string, err error, errw io.Writer) int {
	if err != nil {
		fmt.Fprintf(errw, "error: %v\n", err)
		return 1
	}
	fmt.Fprintln(errw, notice)
	return 0
}

// createMatViewCore plans (and, unless WITH NO DATA, executes) the defining
// query, buffers the rows in the mem connector, and registers the view as a
// source. It returns a status notice; an IF NOT EXISTS collision is a non-error
// no-op.
func (a *App) createMatViewCore(ctx context.Context, s *sql.CreateMatViewStmt) (string, error) {
	if _, exists := a.Reg.Resolve(s.Name); exists {
		if s.IfNotExists {
			return fmt.Sprintf("materialized view %q already exists, skipping", s.Name), nil
		}
		return "", fmt.Errorf("a source named %q already exists", s.Name)
	}
	tbl, err := a.materialize(ctx, s.Query, !s.WithNoData)
	if err != nil {
		return "", err
	}
	a.mem.Put(s.Name, tbl)
	if err := a.Reg.RegisterSource(s.Name, a.mem, connector.Dataset{Name: s.Name, Source: s.Name}); err != nil {
		a.mem.Drop(s.Name)
		return "", err
	}
	a.matViews[s.Name] = &matView{name: s.Name, query: s.Query}
	if s.WithNoData {
		return fmt.Sprintf("materialized view %q created (no data; run REFRESH to populate)", s.Name), nil
	}
	return fmt.Sprintf("materialized view %q created (%d rows)", s.Name, len(tbl.Rows)), nil
}

// refreshMatViewCore re-runs a view's stored query and replaces its buffered
// rows.
func (a *App) refreshMatViewCore(ctx context.Context, s *sql.RefreshMatViewStmt) (string, error) {
	mv, ok := a.matViews[s.Name]
	if !ok {
		return "", fmt.Errorf("materialized view %q does not exist", s.Name)
	}
	tbl, err := a.materialize(ctx, mv.query, !s.WithNoData)
	if err != nil {
		return "", err
	}
	a.mem.Put(s.Name, tbl)
	if s.WithNoData {
		return fmt.Sprintf("materialized view %q reset (no data)", s.Name), nil
	}
	return fmt.Sprintf("materialized view %q refreshed (%d rows)", s.Name, len(tbl.Rows)), nil
}

// dropMatViewCore removes a view's rows and unregisters its source. An IF EXISTS
// miss is a non-error no-op.
func (a *App) dropMatViewCore(s *sql.DropMatViewStmt) (string, error) {
	if !a.mem.Has(s.Name) {
		if s.IfExists {
			return fmt.Sprintf("materialized view %q does not exist, skipping", s.Name), nil
		}
		return "", fmt.Errorf("materialized view %q does not exist", s.Name)
	}
	a.mem.Drop(s.Name)
	a.Reg.RemoveSource(s.Name)
	delete(a.matViews, s.Name)
	return fmt.Sprintf("materialized view %q dropped", s.Name), nil
}

// materialize plans the view's query and, when populate is true, executes it and
// buffers the rows. With populate false (WITH NO DATA) it captures only the
// schema, leaving the table unpopulated. The stored schema is normalized so the
// view exposes clean, unqualified column names (like a SQL view).
func (a *App) materialize(ctx context.Context, query sql.Statement, populate bool) (*memc.Table, error) {
	p, err := plan.Build(ctx, query, a.Reg, plan.IfStrict(a.strict)...)
	if err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}
	schema, err := viewSchema(p.OutputSchema)
	if err != nil {
		return nil, err
	}
	if !populate {
		return &memc.Table{Schema: schema, Populated: false}, nil
	}
	it, _, err := plan.Exec(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("exec: %w", err)
	}
	rows, err := engine.Materialize(ctx, it)
	if err != nil {
		return nil, fmt.Errorf("materialize: %w", err)
	}
	return &memc.Table{Schema: schema, Rows: rows, Populated: true}, nil
}

// viewSchema produces the materialized view's stored schema from the query's
// output schema: each column name is reduced to its unqualified form (a
// projected `e.name` becomes `name`), so later queries reference the view's
// columns by plain name. A genuine duplicate after unqualifying is rejected —
// the same rule PostgreSQL applies to view columns — pointing the user at an AS
// alias.
func viewSchema(out engine.Schema) (engine.Schema, error) {
	cols := make([]engine.Column, len(out.Columns))
	seen := make(map[string]bool, len(out.Columns))
	for i, c := range out.Columns {
		name := unqualifyName(c.Name)
		if seen[strings.ToLower(name)] {
			return engine.Schema{}, fmt.Errorf("column %q specified more than once; add an AS alias in the view query", name)
		}
		seen[strings.ToLower(name)] = true
		c.Name = name
		cols[i] = c
	}
	return engine.Schema{Columns: cols}, nil
}

// unqualifyName strips a leading `qualifier.` from a projected column name.
func unqualifyName(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 && i < len(name)-1 {
		return name[i+1:]
	}
	return name
}

// explainStatement builds query and prints its plan tree (shared by the --explain
// path of CREATE/REFRESH).
func (a *App) explainStatement(ctx context.Context, query sql.Statement, out, errw io.Writer) int {
	p, err := plan.Build(ctx, query, a.Reg, plan.IfStrict(a.strict)...)
	if err != nil {
		fmt.Fprintf(errw, "plan error: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "%s\n", formatPlan(p.Root, 0))
	return 0
}
