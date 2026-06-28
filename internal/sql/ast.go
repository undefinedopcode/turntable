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

// SetOpKind identifies a set operation.
type SetOpKind int

const (
	SetUnion SetOpKind = iota
	SetIntersect
	SetExcept
)

// SetOpTerm is one set operator joining two adjacent branches. All distinguishes
// the multiset (ALL) form from the distinct form.
type SetOpTerm struct {
	Kind SetOpKind
	All  bool
}

// SetOpStmt is a chain of two or more SELECT branches combined by set operators.
// Ops[i] joins Selects[i] to Selects[i+1], so len(Ops) == len(Selects)-1. The
// operators are stored flat in source order; the planner applies SQL precedence
// (INTERSECT binds tighter than UNION/EXCEPT). OrderBy/Limit/Offset apply to the
// combined result (lifted from the final branch during parsing).
type SetOpStmt struct {
	Selects []*SelectStmt
	Ops     []SetOpTerm
	OrderBy []OrderTerm
	Limit   *int
	Offset  *int
}

func (*SetOpStmt) stmtNode() {}

// CTE is one named common table expression: `name AS ( <query> )`.
type CTE struct {
	Name  string
	Query Statement // *SelectStmt or *SetOpStmt
}

// WithStmt is a `WITH <cte>, ... <body>` query. Each CTE is in scope for the
// later CTEs and the body; a FROM reference to a CTE name resolves to its query.
type WithStmt struct {
	CTEs []CTE
	Body Statement // *SelectStmt or *SetOpStmt
}

func (*WithStmt) stmtNode() {}

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
	JoinRight
	JoinFull
	// JoinSemi/JoinAnti are produced by the planner (decorrelating EXISTS / NOT
	// EXISTS), never parsed. They emit each left row at most once — when it has
	// (semi) or lacks (anti) a match — and output only the left columns.
	JoinSemi
	JoinAnti
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

// Function call (scalar or aggregate; planner distinguishes by name). When Over
// is set, it is a window-function call (func(...) OVER (...)).
type FuncCall struct {
	Name     string
	Args     []Expr
	Distinct bool
	Over     *WindowSpec
}

func (*FuncCall) exprNode() {}

// WindowSpec is the OVER (...) clause of a window function: an optional
// PARTITION BY and an optional ORDER BY. (Explicit frame clauses are not yet
// supported; default frames apply.)
type WindowSpec struct {
	PartitionBy []Expr
	OrderBy     []OrderTerm
	Frame       *WindowFrame // explicit ROWS frame; nil = the default frame
}

// WindowFrame is an explicit window frame: `ROWS BETWEEN <start> AND <end>`.
// Only the ROWS unit (physical row offsets) is supported.
type WindowFrame struct {
	Unit  string // "ROWS"
	Start FrameBound
	End   FrameBound
}

// FrameBound is one edge of a window frame.
type FrameBound struct {
	// Kind is one of: UNBOUNDED_PRECEDING, PRECEDING, CURRENT_ROW, FOLLOWING,
	// UNBOUNDED_FOLLOWING.
	Kind   string
	Offset int // row count for PRECEDING / FOLLOWING
}

// ExistsExpr is EXISTS (subquery): true if the subquery yields any row. NOT
// EXISTS parses as a prefix NOT wrapping this.
type ExistsExpr struct {
	Query *SelectStmt
}

// ScalarSubquery is a parenthesized subquery used as a value: (SELECT ...). It
// must return a single column; it yields that column of its single row (NULL if
// no rows, an error if more than one).
type ScalarSubquery struct {
	Query *SelectStmt
}

// OuterRef is a correlated reference to a column of an enclosing query. It is
// produced by the planner (never parsed) when a qualified column in a subquery
// resolves to the outer scope; the engine reads it from the bound outer row.
type OuterRef struct {
	Qualifier string
	Name      string
}

func (*ExistsExpr) exprNode()     {}
func (*ScalarSubquery) exprNode() {}
func (*OuterRef) exprNode()       {}

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
