// Package sql implements the lexer, parser, and AST for the Turntable SQL
// dialect (see DESIGN.md §4). This file defines the AST node types.
package sql

// Statement is the top-level node (currently only Select).
type Statement interface{ stmtNode() }

// SelectStmt is a SELECT query.
type SelectStmt struct {
	Distinct bool
	Items    SelectList
	From     TableRef
	NoFrom   bool // true for "SELECT <expr>" with no FROM (single synthetic row)
	Joins    []Join
	Where    Expr
	GroupBy  []Expr
	Having   Expr
	OrderBy  []OrderTerm
	Limit    *int
	Offset   *int
}

func (*SelectStmt) stmtNode() {}

// SetOpStmt is a UNION of two or more SELECT branches. All[i] reports whether
// the i-th UNION (joining Selects[i] to Selects[i+1]) is UNION ALL, so len(All)
// == len(Selects)-1. OrderBy/Limit/Offset apply to the combined result (they
// are lifted from the final branch during parsing, where SQL places them).
type SetOpStmt struct {
	Selects []*SelectStmt
	All     []bool
	OrderBy []OrderTerm
	Limit   *int
	Offset  *int
}

func (*SetOpStmt) stmtNode() {}

// SelectList is the projection list.
type SelectList struct {
	Items []SelectItem
}

// SelectItem is one element of the projection.
type SelectItem struct {
	Star bool   // * or alias.*
	Expr Expr   // non-star item
	As   string // optional alias
}

// TableRef identifies a source in the FROM clause.
type TableRef struct {
	// Qualified form: prefix and source (e.g. "csv", "./sales.csv").
	// When Prefix is empty, Name is resolved via the Registry.
	Prefix string
	Source string
	Name   string

	Subquery Statement // when FROM (SELECT ... [UNION ...]) AS alias; a *SelectStmt or *SetOpStmt
	Alias    string
}

// Join is a join clause.
type Join struct {
	Kind JoinKind
	Ref  TableRef
	On   Expr
}

type JoinKind int

const (
	JoinInner JoinKind = iota
	JoinLeft
)

// OrderTerm is one element of ORDER BY.
type OrderTerm struct {
	Expr Expr
	Desc bool
}

// Expr is any expression node.
type Expr interface{ exprNode() }

// Literals
type LitInt struct{ V int64 }
type LitFloat struct{ V float64 }
type LitString struct{ V string }
type LitBool struct{ V bool }
type LitNull struct{}

func (*LitInt) exprNode()    {}
func (*LitFloat) exprNode()  {}
func (*LitString) exprNode() {}
func (*LitBool) exprNode()   {}
func (*LitNull) exprNode()   {}

// Refs
type ColRef struct {
	Qualifier string // table alias or "" 
	Name       string
}

func (*ColRef) exprNode() {}

// Operators
type BinaryOp struct {
	Op    string // "=", "<>", "<", "<=", ">", ">=", "+", "-", "*", "/", "AND", "OR"
	Left  Expr
	Right Expr
}

type UnaryOp struct {
	Op   string // "NOT", "-"
	Expr Expr
}

func (*BinaryOp) exprNode() {}
func (*UnaryOp) exprNode()  {}

// Predicates
type InExpr struct {
	Expr     Expr
	List     []Expr      // value list; mutually exclusive with Subquery
	Subquery *SelectStmt // x IN (SELECT ...); non-correlated, single column
	Negate   bool
}

type BetweenExpr struct {
	Expr   Expr
	Low    Expr
	High   Expr
	Negate bool
}

type LikeExpr struct {
	Expr        Expr
	Pat         Expr
	Negate      bool
	Insensitive bool // ILIKE: case-insensitive match (LIKE is case-sensitive)
}

type IsNullExpr struct {
	Expr   Expr
	Negate bool
}

func (*InExpr) exprNode()       {}
func (*BetweenExpr) exprNode()  {}
func (*LikeExpr) exprNode()     {}
func (*IsNullExpr) exprNode()   {}

// Function call (scalar or aggregate; planner distinguishes by name).
type FuncCall struct {
	Name     string
	Args     []Expr
	Distinct bool
}

func (*FuncCall) exprNode() {}

// CASE WHEN ... THEN ... ELSE ... END
type CaseExpr struct {
	Whens []CaseWhen
	Else  Expr
}

type CaseWhen struct {
	Cond Expr
	Then Expr
}

func (*CaseExpr) exprNode() {}

// Cast expression: CAST(expr AS type)
type CastExpr struct {
	Expr Expr
	Type string
}

func (*CastExpr) exprNode() {}

// EXTRACT(field FROM source), e.g. EXTRACT(YEAR FROM created_at).
type ExtractExpr struct {
	Field  string // YEAR, MONTH, DAY, HOUR, MINUTE, SECOND, DOW, DOY, EPOCH
	Source Expr
}

func (*ExtractExpr) exprNode() {}

// POSITION(substr IN str) returns the 1-based index of substr in str.
type PositionExpr struct {
	Substr Expr
	Str    Expr
}

func (*PositionExpr) exprNode() {}
