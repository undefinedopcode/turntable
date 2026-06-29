package pluginc

import (
	"encoding/json"

	"github.com/april/turntable/internal/sql"
)

// The predicate wire format is a small JSON expression tree — a deliberately
// narrow subset of the full sql.Expr AST. A plugin only ever has to understand
// these node kinds; anything turntable cannot express in them is simply not
// pushed, and the engine applies it. Because the engine *always* re-applies the
// full WHERE above the Scan, pushing a partial predicate is never a correctness
// risk — only an optimization to cut rows crossing the stdio boundary.

type predLit struct {
	Type  string `json:"type"` // int | float | string | bool | null
	Value any    `json:"value"`
}

type predNode struct {
	Kind        string     `json:"kind"` // compare|and|or|not|in|between|like|isnull
	Op          string     `json:"op,omitempty"`
	Column      string     `json:"column,omitempty"`
	Value       *predLit   `json:"value,omitempty"`
	Values      []predLit  `json:"values,omitempty"`
	Low         *predLit   `json:"low,omitempty"`
	High        *predLit   `json:"high,omitempty"`
	Pattern     string     `json:"pattern,omitempty"`
	Insensitive bool       `json:"insensitive,omitempty"`
	Negate      bool       `json:"negate,omitempty"`
	Args        []predNode `json:"args,omitempty"`
	Arg         *predNode  `json:"arg,omitempty"`
}

// encodePredicate renders the encodable part of a predicate as JSON, or nil if
// nothing in it can be expressed. It is best-effort: see buildPredicate.
func encodePredicate(e sql.Expr) json.RawMessage {
	node, _ := buildPredicate(e)
	if node == nil {
		return nil
	}
	b, err := json.Marshal(node)
	if err != nil {
		return nil
	}
	return b
}

// buildPredicate converts an Expr to a predNode. The returned exact flag is true
// when the node represents the whole input with nothing dropped; it is false
// when only a subset was encodable (e.g. one conjunct of an AND). exact is used
// by tests and could drive an --explain "fully pushed" annotation; correctness
// does not depend on it.
func buildPredicate(e sql.Expr) (node *predNode, exact bool) {
	switch x := e.(type) {
	case *sql.BinaryOp:
		switch x.Op {
		case "AND":
			l, lok := buildPredicate(x.Left)
			r, rok := buildPredicate(x.Right)
			args := flattenAnd(l, r)
			switch len(args) {
			case 0:
				return nil, false
			case 1:
				// Only one side was encodable; the AND is not fully covered.
				return &args[0], lok && rok && l != nil && r != nil
			default:
				return &predNode{Kind: "and", Args: args}, lok && rok
			}
		case "OR":
			// An OR can only be pushed if *both* branches are fully encodable —
			// applying part of an OR as a filter would drop rows that should pass.
			l, lok := buildPredicate(x.Left)
			r, rok := buildPredicate(x.Right)
			if l == nil || r == nil || !lok || !rok {
				return nil, false
			}
			return &predNode{Kind: "or", Args: flattenOr(l, r)}, true
		case "=", "<>", "<", "<=", ">", ">=":
			return buildCompare(x)
		}
		return nil, false
	case *sql.UnaryOp:
		if x.Op == "NOT" {
			inner, ok := buildPredicate(x.Expr)
			if inner == nil || !ok {
				return nil, false
			}
			return &predNode{Kind: "not", Arg: inner}, true
		}
		return nil, false
	case *sql.InExpr:
		col, ok := columnOf(x.Expr)
		if !ok || x.Subquery != nil {
			return nil, false
		}
		vals := make([]predLit, 0, len(x.List))
		for _, item := range x.List {
			lit, ok := literalOf(item)
			if !ok {
				return nil, false
			}
			vals = append(vals, *lit)
		}
		return &predNode{Kind: "in", Column: col, Values: vals, Negate: x.Negate}, true
	case *sql.BetweenExpr:
		col, ok := columnOf(x.Expr)
		if !ok {
			return nil, false
		}
		lo, lok := literalOf(x.Low)
		hi, hok := literalOf(x.High)
		if !lok || !hok {
			return nil, false
		}
		return &predNode{Kind: "between", Column: col, Low: lo, High: hi, Negate: x.Negate}, true
	case *sql.LikeExpr:
		col, ok := columnOf(x.Expr)
		if !ok {
			return nil, false
		}
		pat, ok := x.Pat.(*sql.LitString)
		if !ok {
			return nil, false
		}
		return &predNode{Kind: "like", Column: col, Pattern: pat.V, Negate: x.Negate, Insensitive: x.Insensitive}, true
	case *sql.IsNullExpr:
		col, ok := columnOf(x.Expr)
		if !ok {
			return nil, false
		}
		return &predNode{Kind: "isnull", Column: col, Negate: x.Negate}, true
	}
	return nil, false
}

// buildCompare encodes a `column OP literal` (or `literal OP column`) comparison.
func buildCompare(x *sql.BinaryOp) (*predNode, bool) {
	if col, ok := columnOf(x.Left); ok {
		if lit, ok := literalOf(x.Right); ok {
			return &predNode{Kind: "compare", Op: x.Op, Column: col, Value: lit}, true
		}
	}
	if col, ok := columnOf(x.Right); ok {
		if lit, ok := literalOf(x.Left); ok {
			return &predNode{Kind: "compare", Op: flipOp(x.Op), Column: col, Value: lit}, true
		}
	}
	return nil, false
}

// flattenAnd / flattenOr collapse nested same-kind nodes into one arg list so
// the wire tree stays shallow.
func flattenAnd(nodes ...*predNode) []predNode {
	var out []predNode
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if n.Kind == "and" {
			out = append(out, n.Args...)
		} else {
			out = append(out, *n)
		}
	}
	return out
}

func flattenOr(nodes ...*predNode) []predNode {
	var out []predNode
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if n.Kind == "or" {
			out = append(out, n.Args...)
		} else {
			out = append(out, *n)
		}
	}
	return out
}

// columnOf returns the bare column name of an unqualified or qualified ColRef.
func columnOf(e sql.Expr) (string, bool) {
	if c, ok := e.(*sql.ColRef); ok {
		return c.Name, true
	}
	return "", false
}

// literalOf encodes a literal expression as a typed wire literal.
func literalOf(e sql.Expr) (*predLit, bool) {
	switch v := e.(type) {
	case *sql.LitInt:
		return &predLit{Type: "int", Value: v.V}, true
	case *sql.LitFloat:
		return &predLit{Type: "float", Value: v.V}, true
	case *sql.LitString:
		return &predLit{Type: "string", Value: v.V}, true
	case *sql.LitBool:
		return &predLit{Type: "bool", Value: v.V}, true
	case *sql.LitNull:
		return &predLit{Type: "null", Value: nil}, true
	}
	return nil, false
}

// flipOp reverses a comparison operator so `literal OP column` becomes
// `column flip(OP) literal`.
func flipOp(op string) string {
	switch op {
	case "<":
		return ">"
	case "<=":
		return ">="
	case ">":
		return "<"
	case ">=":
		return "<="
	default: // = and <> are symmetric
		return op
	}
}
