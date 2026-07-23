package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/plan"
	"github.com/april/turntable/internal/sql"
)

// Regular (non-materialized) views. A view stores only its defining query in the
// registry; the planner expands a referenced view inline (once per query, like a
// CTE — see plan.buildView). createView/dropView adapt the shared *Core
// operations to the CLI/REPL; serve.go reuses the *Core methods for the web API.

func (a *App) createView(ctx context.Context, s *sql.CreateViewStmt, explain bool, out, errw io.Writer) int {
	if explain {
		return a.explainStatement(ctx, s.Query, out, errw)
	}
	notice, err := a.createViewCore(ctx, s)
	return noticeExit(notice, err, errw)
}

func (a *App) dropView(s *sql.DropViewStmt, explain bool, errw io.Writer) int {
	if explain {
		fmt.Fprintf(errw, "DROP VIEW %s (no plan)\n", s.Name)
		return 0
	}
	notice, err := a.dropViewCore(s)
	return noticeExit(notice, err, errw)
}

// createViewCore validates the defining query (by planning it) and registers the
// view. CREATE OR REPLACE redefines an existing view.
func (a *App) createViewCore(ctx context.Context, s *sql.CreateViewStmt) (string, error) {
	if _, exists := a.Reg.Resolve(s.Name); exists {
		return "", fmt.Errorf("a source named %q already exists", s.Name)
	}
	if a.Reg.HasView(s.Name) && !s.OrReplace {
		return "", fmt.Errorf("view %q already exists (use CREATE OR REPLACE VIEW)", s.Name)
	}
	// Bind the definition now so errors surface at create time, like PostgreSQL.
	if _, err := plan.Build(ctx, s.Query, a.Reg, a.planOpts()...); err != nil {
		return "", fmt.Errorf("plan: %w", err)
	}
	if err := a.Reg.RegisterView(s.Name, s.Query, s.OrReplace); err != nil {
		return "", err
	}
	verb := "created"
	if s.OrReplace {
		verb = "created or replaced"
	}
	return fmt.Sprintf("view %q %s", s.Name, verb), nil
}

// dropViewCore unregisters a view. An IF EXISTS miss is a non-error no-op.
func (a *App) dropViewCore(s *sql.DropViewStmt) (string, error) {
	if !a.Reg.HasView(s.Name) {
		if s.IfExists {
			return fmt.Sprintf("view %q does not exist, skipping", s.Name), nil
		}
		return "", fmt.Errorf("view %q does not exist", s.Name)
	}
	a.Reg.RemoveView(s.Name)
	return fmt.Sprintf("view %q dropped", s.Name), nil
}

// viewSchemaFor builds a view's query to obtain its output schema (for .schema /
// the web schema endpoint, where a view has no connector to Resolve).
func (a *App) viewSchemaFor(ctx context.Context, name string) (engine.Schema, bool, error) {
	q, ok := a.Reg.View(name)
	if !ok {
		return engine.Schema{}, false, nil
	}
	p, err := plan.Build(ctx, q, a.Reg, a.planOpts()...)
	if err != nil {
		return engine.Schema{}, true, fmt.Errorf("view %q: %w", name, err)
	}
	return p.OutputSchema, true, nil
}
