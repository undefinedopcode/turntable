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

// createMatView handles CREATE MATERIALIZED VIEW: it plans (and, unless WITH NO
// DATA, executes) the defining query, buffers the rows in the mem connector, and
// registers the view as a source usable by later queries. Under --explain it
// prints the inner query's plan and creates nothing.
func (a *App) createMatView(ctx context.Context, s *sql.CreateMatViewStmt, explain bool, out, errw io.Writer) int {
	if explain {
		return a.explainStatement(ctx, s.Query, out, errw)
	}
	if _, exists := a.Reg.Resolve(s.Name); exists {
		if s.IfNotExists {
			fmt.Fprintf(errw, "materialized view %q already exists, skipping\n", s.Name)
			return 0
		}
		fmt.Fprintf(errw, "error: a source named %q already exists\n", s.Name)
		return 1
	}
	tbl, err := a.materialize(ctx, s.Query, !s.WithNoData)
	if err != nil {
		fmt.Fprintf(errw, "error: %v\n", err)
		return 1
	}
	a.mem.Put(s.Name, tbl)
	if err := a.Reg.RegisterSource(s.Name, a.mem, connector.Dataset{Name: s.Name, Source: s.Name}); err != nil {
		a.mem.Drop(s.Name)
		fmt.Fprintf(errw, "error: %v\n", err)
		return 1
	}
	a.matViews[s.Name] = &matView{name: s.Name, query: s.Query}
	if s.WithNoData {
		fmt.Fprintf(errw, "materialized view %q created (no data; run REFRESH to populate)\n", s.Name)
	} else {
		fmt.Fprintf(errw, "materialized view %q created (%d rows)\n", s.Name, len(tbl.Rows))
	}
	return 0
}

// refreshMatView re-runs a view's stored query and replaces its buffered rows.
// Under --explain it prints the stored query's plan and refreshes nothing.
func (a *App) refreshMatView(ctx context.Context, s *sql.RefreshMatViewStmt, explain bool, out, errw io.Writer) int {
	mv, ok := a.matViews[s.Name]
	if !ok {
		fmt.Fprintf(errw, "error: materialized view %q does not exist\n", s.Name)
		return 1
	}
	if explain {
		return a.explainStatement(ctx, mv.query, out, errw)
	}
	tbl, err := a.materialize(ctx, mv.query, !s.WithNoData)
	if err != nil {
		fmt.Fprintf(errw, "error: %v\n", err)
		return 1
	}
	a.mem.Put(s.Name, tbl)
	if s.WithNoData {
		fmt.Fprintf(errw, "materialized view %q reset (no data)\n", s.Name)
	} else {
		fmt.Fprintf(errw, "materialized view %q refreshed (%d rows)\n", s.Name, len(tbl.Rows))
	}
	return 0
}

// dropMatView removes a view's rows and unregisters its source.
func (a *App) dropMatView(s *sql.DropMatViewStmt, explain bool, errw io.Writer) int {
	if explain {
		fmt.Fprintf(errw, "DROP MATERIALIZED VIEW %s (no plan)\n", s.Name)
		return 0
	}
	if !a.mem.Has(s.Name) {
		if s.IfExists {
			fmt.Fprintf(errw, "materialized view %q does not exist, skipping\n", s.Name)
			return 0
		}
		fmt.Fprintf(errw, "error: materialized view %q does not exist\n", s.Name)
		return 1
	}
	a.mem.Drop(s.Name)
	a.Reg.RemoveSource(s.Name)
	delete(a.matViews, s.Name)
	fmt.Fprintf(errw, "materialized view %q dropped\n", s.Name)
	return 0
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
