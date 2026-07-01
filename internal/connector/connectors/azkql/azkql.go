// Package azkql renders turntable pushdown operations (WHERE / ORDER BY / LIMIT)
// into a KQL pipeline over a table. It is shared by the Azure connectors that
// speak KQL — Resource Graph (azrgraphc) and, later, Log Analytics — the way
// sqlc's buildScanQuery is shared across SQL drivers. It is pure and DB-free.
//
// Translation is best-effort and conservative: only the predicate parts KQL can
// express *without narrowing the result* are rendered; the engine always
// re-applies the full predicate/sort/limit, so pushing a subset (or a slightly
// broader filter, e.g. case-insensitive `contains` for `LIKE`) only ever reduces
// data transferred, never changes the answer. Predicates on nested columns
// (tags.env, lexed as qualifier+name) and anything unrecognized are left to the
// engine.
package azkql

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/sql"
)

// Query is the set of operations to render over Table.
type Query struct {
	Table     string
	Predicate sql.Expr
	OrderBy   []connector.OrderTerm
	Limit     *int // explicit row limit (planner guarantees it is safe to apply)
	Cap       int  // safety row cap applied when Limit is nil (0 = none)
}

// Build renders the KQL pipeline: `Table | where … | order by … | take …`,
// including only the clauses it can express.
func Build(q Query) string {
	var b strings.Builder
	b.WriteString(q.Table)

	if q.Predicate != nil {
		if w := whereClause(q.Predicate); w != "" {
			b.WriteString(" | where ")
			b.WriteString(w)
		}
	}
	for i, ot := range q.OrderBy {
		if !safeIdent(ot.Column) {
			continue
		}
		if i == 0 {
			b.WriteString(" | order by ")
		} else {
			b.WriteString(", ")
		}
		b.WriteString(ot.Column)
		if ot.Desc {
			b.WriteString(" desc")
		} else {
			b.WriteString(" asc")
		}
	}
	if n := rowCap(q); n > 0 {
		fmt.Fprintf(&b, " | take %d", n)
	}
	return b.String()
}

func rowCap(q Query) int {
	if q.Limit != nil && *q.Limit >= 0 {
		return *q.Limit
	}
	return q.Cap
}

// whereClause splits the top-level AND into conjuncts and renders the ones it
// can translate, AND-joined. Untranslatable conjuncts are dropped (the engine
// re-applies them).
func whereClause(e sql.Expr) string {
	var parts []string
	for _, c := range conjuncts(e) {
		if s, ok := translate(c); ok {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " and ")
}

// conjuncts flattens a top-level AND chain.
func conjuncts(e sql.Expr) []sql.Expr {
	if b, ok := e.(*sql.BinaryOp); ok && b.Op == "AND" {
		return append(conjuncts(b.Left), conjuncts(b.Right)...)
	}
	return []sql.Expr{e}
}

// translate renders one predicate term. ok=false means "leave to the engine".
// A nested AND/OR is all-or-nothing (translating half an OR would drop rows).
func translate(e sql.Expr) (string, bool) {
	switch ex := e.(type) {
	case *sql.BinaryOp:
		switch ex.Op {
		case "AND", "OR":
			l, lok := translate(ex.Left)
			r, rok := translate(ex.Right)
			if lok && rok {
				return "(" + l + ") " + strings.ToLower(ex.Op) + " (" + r + ")", true
			}
			return "", false
		case "=", "!=", "<>", "<", "<=", ">", ">=":
			col, ok := plainCol(ex.Left)
			if !ok {
				return "", false
			}
			lit, ok := literal(ex.Right)
			if !ok {
				return "", false
			}
			return col + " " + kqlCmp(ex.Op) + " " + lit, true
		}
	case *sql.InExpr:
		col, ok := plainCol(ex.Expr)
		if !ok {
			return "", false
		}
		lits := make([]string, 0, len(ex.List))
		for _, it := range ex.List {
			l, ok := literal(it)
			if !ok {
				return "", false
			}
			lits = append(lits, l)
		}
		op := "in"
		if ex.Negate {
			op = "!in"
		}
		return col + " " + op + " (" + strings.Join(lits, ", ") + ")", true
	case *sql.LikeExpr:
		col, ok := plainCol(ex.Expr)
		if !ok {
			return "", false
		}
		s, ok := containsPattern(ex.Pat)
		if !ok {
			return "", false
		}
		// KQL `contains` is case-insensitive — a superset of SQL LIKE, so safe to
		// push (the engine re-applies the exact match).
		op := "contains"
		if ex.Negate {
			op = "!contains"
		}
		return col + " " + op + " " + kqlString(s), true
	case *sql.IsNullExpr:
		col, ok := plainCol(ex.Expr)
		if !ok {
			return "", false
		}
		if ex.Negate {
			return "isnotnull(" + col + ")", true
		}
		return "isnull(" + col + ")", true
	}
	return "", false
}

func kqlCmp(op string) string {
	switch op {
	case "=":
		return "=="
	case "<>":
		return "!="
	default:
		return op // !=, <, <=, >, >=
	}
}

// plainCol returns a bare, KQL-safe top-level column name, or ok=false for a
// qualified/nested reference (v1 does not translate predicates on tags.*, etc.).
func plainCol(e sql.Expr) (string, bool) {
	cr, ok := e.(*sql.ColRef)
	if !ok || cr.Qualifier != "" || !safeIdent(cr.Name) {
		return "", false
	}
	return cr.Name, true
}

// safeIdent reports whether s is a bare KQL identifier (letters/digits/_),
// avoiding the need for `['…']` quoting and any injection surface.
func safeIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			continue
		}
		if i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

// literal renders a scalar literal as a KQL literal.
func literal(e sql.Expr) (string, bool) {
	switch v := e.(type) {
	case *sql.LitInt:
		return strconv.FormatInt(v.V, 10), true
	case *sql.LitFloat:
		return strconv.FormatFloat(v.V, 'g', -1, 64), true
	case *sql.LitString:
		return kqlString(v.V), true
	case *sql.LitBool:
		if v.V {
			return "true", true
		}
		return "false", true
	}
	return "", false
}

// kqlString renders a KQL double-quoted string literal, escaping backslash and
// double quote.
func kqlString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

// containsPattern accepts a LIKE pattern `%substring%` (no interior wildcards)
// and returns the bare substring for a KQL `contains`.
func containsPattern(e sql.Expr) (string, bool) {
	s, ok := e.(*sql.LitString)
	if !ok {
		return "", false
	}
	p := s.V
	if len(p) < 2 || !strings.HasPrefix(p, "%") || !strings.HasSuffix(p, "%") {
		return "", false
	}
	inner := p[1 : len(p)-1]
	if strings.ContainsAny(inner, "%_") {
		return "", false
	}
	return inner, true
}
