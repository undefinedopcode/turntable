package sql

import "testing"

func TestParseSelectBasic(t *testing.T) {
	stmt, err := Parse("SELECT a, b AS x FROM t WHERE a > 1 ORDER BY b DESC LIMIT 5")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	s := stmt.(*SelectStmt)
	if s.From.Name != "t" {
		t.Errorf("from = %q, want t", s.From.Name)
	}
	if s.Limit == nil || *s.Limit != 5 {
		t.Errorf("limit = %v, want 5", s.Limit)
	}
	if len(s.Items.Items) != 2 {
		t.Fatalf("select list len = %d, want 2", len(s.Items.Items))
	}
	if s.Items.Items[1].As != "x" {
		t.Errorf("alias = %q, want x", s.Items.Items[1].As)
	}
	if s.Where == nil {
		t.Error("where is nil")
	}
	if len(s.OrderBy) != 1 || !s.OrderBy[0].Desc {
		t.Error("order by wrong")
	}
}

func TestParseImplicitAlias(t *testing.T) {
	// `<expr> alias` without AS, including on an aggregate before FROM.
	stmt, err := Parse("SELECT lvl x, COUNT(*) c FROM t GROUP BY lvl ORDER BY c DESC")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	s := stmt.(*SelectStmt)
	if len(s.Items.Items) != 2 {
		t.Fatalf("select list len = %d, want 2", len(s.Items.Items))
	}
	if s.Items.Items[0].As != "x" {
		t.Errorf("item0 alias = %q, want x", s.Items.Items[0].As)
	}
	if s.Items.Items[1].As != "c" {
		t.Errorf("item1 alias = %q, want c", s.Items.Items[1].As)
	}
	if s.From.Name != "t" {
		t.Errorf("from = %q, want t (FROM must not be grabbed as an alias)", s.From.Name)
	}
	// A bare column with no alias must not swallow the following keyword.
	s2 := mustParseSelect(t, "SELECT a FROM t")
	if s2.Items.Items[0].As != "" || s2.From.Name != "t" {
		t.Errorf("bare column: alias=%q from=%q", s2.Items.Items[0].As, s2.From.Name)
	}
}

func TestParseExtractValue(t *testing.T) {
	// Both EXTRACT_VALUE(key FROM source) and EXTRACT_VALUE(source, key) lower to
	// a FuncCall with args ordered (source, key).
	for _, q := range []string{
		"SELECT EXTRACT_VALUE('status' FROM msg) AS s FROM t",
		"SELECT EXTRACT_VALUE(msg, 'status') AS s FROM t",
	} {
		s := mustParseSelect(t, q)
		fc, ok := s.Items.Items[0].Expr.(*FuncCall)
		if !ok || fc.Name != "EXTRACT_VALUE" || len(fc.Args) != 2 {
			t.Fatalf("%q: expected EXTRACT_VALUE FuncCall with 2 args, got %#v", q, s.Items.Items[0].Expr)
		}
		if _, ok := fc.Args[0].(*ColRef); !ok {
			t.Errorf("%q: arg0 should be the source column msg, got %#v", q, fc.Args[0])
		}
		key, ok := fc.Args[1].(*LitString)
		if !ok || key.V != "status" {
			t.Errorf("%q: arg1 should be the key 'status', got %#v", q, fc.Args[1])
		}
	}
}

func mustParseSelect(t *testing.T, q string) *SelectStmt {
	t.Helper()
	stmt, err := Parse(q)
	if err != nil {
		t.Fatalf("Parse(%q): %v", q, err)
	}
	return stmt.(*SelectStmt)
}

func TestParseQualifiedRef(t *testing.T) {
	stmt, err := Parse("SELECT * FROM csv:./data/sales.csv")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	s := stmt.(*SelectStmt)
	if s.From.Prefix != "csv" {
		t.Errorf("prefix = %q, want csv", s.From.Prefix)
	}
	if s.From.Source != "./data/sales.csv" {
		t.Errorf("source = %q", s.From.Source)
	}
}

func TestParseJoin(t *testing.T) {
	stmt, err := Parse("SELECT a.x, b.y FROM a JOIN b ON a.id = b.id")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	s := stmt.(*SelectStmt)
	if len(s.Joins) != 1 {
		t.Fatalf("joins = %d, want 1", len(s.Joins))
	}
	if s.Joins[0].Ref.Name != "b" {
		t.Errorf("join ref = %q", s.Joins[0].Ref.Name)
	}
}

func TestParseExistsAndScalarSubquery(t *testing.T) {
	// EXISTS in WHERE.
	stmt, err := Parse("SELECT a FROM t WHERE EXISTS (SELECT 1 FROM u WHERE u.x = t.a)")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if _, ok := stmt.(*SelectStmt).Where.(*ExistsExpr); !ok {
		t.Errorf("WHERE = %T, want *ExistsExpr", stmt.(*SelectStmt).Where)
	}

	// NOT EXISTS parses as a prefix NOT wrapping EXISTS.
	stmt, _ = Parse("SELECT a FROM t WHERE NOT EXISTS (SELECT 1 FROM u)")
	u, ok := stmt.(*SelectStmt).Where.(*UnaryOp)
	if !ok || u.Op != "NOT" {
		t.Fatalf("WHERE = %T, want UnaryOp NOT", stmt.(*SelectStmt).Where)
	}
	if _, ok := u.Expr.(*ExistsExpr); !ok {
		t.Errorf("NOT operand = %T, want *ExistsExpr", u.Expr)
	}

	// Scalar subquery in the select list.
	stmt, _ = Parse("SELECT a, (SELECT COUNT(*) FROM u WHERE u.x = t.a) AS c FROM t")
	if _, ok := stmt.(*SelectStmt).Items.Items[1].Expr.(*ScalarSubquery); !ok {
		t.Errorf("item1 = %T, want *ScalarSubquery", stmt.(*SelectStmt).Items.Items[1].Expr)
	}

	// A parenthesized non-SELECT is still a grouped expression, not a subquery.
	stmt, _ = Parse("SELECT (a + 1) * 2 AS x FROM t")
	if _, ok := stmt.(*SelectStmt).Items.Items[0].Expr.(*BinaryOp); !ok {
		t.Errorf("item0 = %T, want *BinaryOp (grouped expr)", stmt.(*SelectStmt).Items.Items[0].Expr)
	}
}

func TestParseWindowFunction(t *testing.T) {
	stmt, err := Parse("SELECT ROW_NUMBER() OVER (PARTITION BY a, b ORDER BY c DESC, d) AS rn FROM t")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	fc, ok := stmt.(*SelectStmt).Items.Items[0].Expr.(*FuncCall)
	if !ok || fc.Over == nil {
		t.Fatalf("expected a window FuncCall, got %T", stmt.(*SelectStmt).Items.Items[0].Expr)
	}
	if len(fc.Over.PartitionBy) != 2 {
		t.Errorf("PARTITION BY = %d exprs, want 2", len(fc.Over.PartitionBy))
	}
	if len(fc.Over.OrderBy) != 2 || !fc.Over.OrderBy[0].Desc || fc.Over.OrderBy[1].Desc {
		t.Errorf("ORDER BY = %+v, want [c DESC, d ASC]", fc.Over.OrderBy)
	}
	if fc.Over.Frame != nil {
		t.Errorf("no frame expected, got %+v", fc.Over.Frame)
	}
}

func frameOffsetLit(e Expr) int64 {
	if l, ok := e.(*LitInt); ok {
		return l.V
	}
	return -1
}

func TestParseWindowFrame(t *testing.T) {
	// BETWEEN form with numeric + CURRENT ROW bounds.
	s := mustParseSelect(t, "SELECT AVG(v) OVER (ORDER BY t ROWS BETWEEN 2 PRECEDING AND CURRENT ROW) FROM x")
	f := s.Items.Items[0].Expr.(*FuncCall).Over.Frame
	if f == nil || f.Unit != "ROWS" {
		t.Fatalf("frame = %+v, want ROWS", f)
	}
	if f.Start.Kind != "PRECEDING" || frameOffsetLit(f.Start.Offset) != 2 {
		t.Errorf("start = %+v, want 2 PRECEDING", f.Start)
	}
	if f.End.Kind != "CURRENT_ROW" {
		t.Errorf("end = %+v, want CURRENT ROW", f.End)
	}

	// Single-bound shorthand: `ROWS UNBOUNDED PRECEDING` => end CURRENT ROW.
	s = mustParseSelect(t, "SELECT SUM(v) OVER (ORDER BY t ROWS UNBOUNDED PRECEDING) FROM x")
	f = s.Items.Items[0].Expr.(*FuncCall).Over.Frame
	if f.Start.Kind != "UNBOUNDED_PRECEDING" || f.End.Kind != "CURRENT_ROW" {
		t.Errorf("shorthand frame = %+v", f)
	}

	// FOLLOWING bound.
	s = mustParseSelect(t, "SELECT SUM(v) OVER (ORDER BY t ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM x")
	f = s.Items.Items[0].Expr.(*FuncCall).Over.Frame
	if f.End.Kind != "FOLLOWING" || frameOffsetLit(f.End.Offset) != 1 {
		t.Errorf("end = %+v, want 1 FOLLOWING", f.End)
	}

	// INTERVAL offset (time-series) parses to an IntervalLit.
	s = mustParseSelect(t, "SELECT AVG(v) OVER (ORDER BY t RANGE BETWEEN INTERVAL '7 days' PRECEDING AND CURRENT ROW) FROM x")
	f = s.Items.Items[0].Expr.(*FuncCall).Over.Frame
	il, ok := f.Start.Offset.(*IntervalLit)
	if f.Unit != "RANGE" || !ok || il.Spec != "7 days" {
		t.Errorf("interval frame = %+v / %+v", f, f.Start.Offset)
	}

	// RANGE parses (Unit reflects it); GROUPS is rejected.
	s = mustParseSelect(t, "SELECT SUM(v) OVER (ORDER BY t RANGE BETWEEN 1 PRECEDING AND CURRENT ROW) FROM x")
	if rf := s.Items.Items[0].Expr.(*FuncCall).Over.Frame; rf == nil || rf.Unit != "RANGE" {
		t.Errorf("RANGE frame = %+v, want Unit RANGE", rf)
	}
	if _, err := Parse("SELECT SUM(v) OVER (ORDER BY t GROUPS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM x"); err == nil {
		t.Error("expected an error for a GROUPS frame")
	}
}

func TestParseUnaryMinus(t *testing.T) {
	stmt, err := Parse("SELECT -1 AS a, 2 * -3 AS b")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	items := stmt.(*SelectStmt).Items.Items
	if u, ok := items[0].Expr.(*UnaryOp); !ok || u.Op != "-" {
		t.Errorf("item0 = %T, want UnaryOp -", items[0].Expr)
	}
	// 2 * -3 parses as 2 * (-3), unary binding tighter than *.
	if b, ok := items[1].Expr.(*BinaryOp); !ok || b.Op != "*" {
		t.Fatalf("item1 = %T, want BinaryOp *", items[1].Expr)
	} else if _, ok := b.Right.(*UnaryOp); !ok {
		t.Errorf("rhs = %T, want UnaryOp", b.Right)
	}
}

func TestParseJoinKinds(t *testing.T) {
	cases := []struct {
		q    string
		kind JoinKind
	}{
		{"SELECT * FROM a JOIN b ON a.id = b.id", JoinInner},
		{"SELECT * FROM a INNER JOIN b ON a.id = b.id", JoinInner},
		{"SELECT * FROM a LEFT JOIN b ON a.id = b.id", JoinLeft},
		{"SELECT * FROM a LEFT OUTER JOIN b ON a.id = b.id", JoinLeft},
		{"SELECT * FROM a RIGHT JOIN b ON a.id = b.id", JoinRight},
		{"SELECT * FROM a RIGHT OUTER JOIN b ON a.id = b.id", JoinRight},
		{"SELECT * FROM a FULL JOIN b ON a.id = b.id", JoinFull},
		{"SELECT * FROM a FULL OUTER JOIN b ON a.id = b.id", JoinFull},
	}
	for _, c := range cases {
		stmt, err := Parse(c.q)
		if err != nil {
			t.Fatalf("parse %q: %v", c.q, err)
		}
		j := stmt.(*SelectStmt).Joins
		if len(j) != 1 {
			t.Fatalf("%q: joins = %d, want 1", c.q, len(j))
		}
		if j[0].Kind != c.kind {
			t.Errorf("%q: kind = %d, want %d", c.q, j[0].Kind, c.kind)
		}
	}
}

func TestParseGroupByAndAgg(t *testing.T) {
	stmt, err := Parse("SELECT region, COUNT(*) AS n FROM sales GROUP BY region HAVING n > 1")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	s := stmt.(*SelectStmt)
	if len(s.GroupBy) != 1 {
		t.Fatalf("group by len = %d", len(s.GroupBy))
	}
	if s.Having == nil {
		t.Error("having is nil")
	}
}

func TestParseDistinct(t *testing.T) {
	stmt, err := Parse("SELECT DISTINCT a FROM t")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	s := stmt.(*SelectStmt)
	if !s.Distinct {
		t.Error("distinct not set")
	}
}

func TestParseInBetweenLike(t *testing.T) {
	cases := []string{
		"SELECT a FROM t WHERE a IN (1, 2, 3)",
		"SELECT a FROM t WHERE a NOT IN (1, 2)",
		"SELECT a FROM t WHERE a BETWEEN 1 AND 5",
		"SELECT a FROM t WHERE a LIKE 'x%'",
		"SELECT a FROM t WHERE a IS NULL",
		"SELECT a FROM t WHERE a IS NOT NULL",
	}
	for _, q := range cases {
		if _, err := Parse(q); err != nil {
			t.Errorf("Parse(%q) error: %v", q, err)
		}
	}
}

func TestParseCase(t *testing.T) {
	cases := []string{
		"SELECT CASE WHEN a > 1 THEN 'big' ELSE 'small' END AS s FROM t",
		"SELECT CASE WHEN a > 1 THEN 'big' END FROM t",
		"SELECT CASE a WHEN 1 THEN 'one' WHEN 2 THEN 'two' ELSE 'other' END FROM t",
		"SELECT CASE WHEN a > 1 THEN CASE WHEN b > 2 THEN 'x' END ELSE 'y' END FROM t",
	}
	for _, q := range cases {
		stmt, err := Parse(q)
		if err != nil {
			t.Errorf("Parse(%q) error: %v", q, err)
			continue
		}
		s := stmt.(*SelectStmt)
		_, ok := s.Items.Items[0].Expr.(*CaseExpr)
		if !ok {
			t.Errorf("Parse(%q): expected CaseExpr, got %T", q, s.Items.Items[0].Expr)
		}
	}
}

func TestParseCast(t *testing.T) {
	cases := []string{
		"SELECT CAST(a AS int) FROM t",
		"SELECT CAST(a AS float) AS f FROM t",
		"SELECT CAST(a AS string) FROM t",
		"SELECT CAST(a AS timestamp) FROM t",
		"SELECT CAST(a AS varchar(255)) FROM t",
		"SELECT CAST(a AS decimal(10,2)) FROM t",
	}
	for _, q := range cases {
		stmt, err := Parse(q)
		if err != nil {
			t.Errorf("Parse(%q) error: %v", q, err)
			continue
		}
		s := stmt.(*SelectStmt)
		c, ok := s.Items.Items[0].Expr.(*CastExpr)
		if !ok {
			t.Errorf("Parse(%q): expected CastExpr, got %T", q, s.Items.Items[0].Expr)
			continue
		}
		if c.Type == "" {
			t.Errorf("Parse(%q): empty cast type", q)
		}
	}
}

func TestParseNoFrom(t *testing.T) {
	stmt, err := Parse("SELECT 1 + 1 AS two")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	s := stmt.(*SelectStmt)
	if !s.NoFrom {
		t.Error("NoFrom not set for FROM-less SELECT")
	}
	if len(s.Items.Items) != 1 {
		t.Fatalf("select list len = %d", len(s.Items.Items))
	}
}

func TestParseExtract(t *testing.T) {
	stmt, err := Parse("SELECT EXTRACT(YEAR FROM t.col) AS y FROM t")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	s := stmt.(*SelectStmt)
	ex, ok := s.Items.Items[0].Expr.(*ExtractExpr)
	if !ok {
		t.Fatalf("expected ExtractExpr, got %T", s.Items.Items[0].Expr)
	}
	if ex.Field != "YEAR" {
		t.Errorf("field = %q, want YEAR", ex.Field)
	}
}

func TestParsePosition(t *testing.T) {
	stmt, err := Parse("SELECT POSITION('x' IN name) AS p FROM t")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	s := stmt.(*SelectStmt)
	if _, ok := s.Items.Items[0].Expr.(*PositionExpr); !ok {
		t.Fatalf("expected PositionExpr, got %T", s.Items.Items[0].Expr)
	}
}

func TestParseTrailingSemicolon(t *testing.T) {
	ok := []string{
		"SELECT 1;",
		"SELECT 1 ;",
		"SELECT region FROM t WHERE x = 1 LIMIT 5;",
		"SELECT 1;;",                        // multiple trailing semicolons
		"SELECT 1;\n",                       // trailing newline after the semicolon
		"SELECT * FROM http://h/data.json;", // trailing ';' after a URL ref
	}
	for _, q := range ok {
		if _, err := Parse(q); err != nil {
			t.Errorf("Parse(%q) errored: %v", q, err)
		}
	}

	// A semicolon inside a string literal is preserved (not a terminator).
	stmt, err := Parse("SELECT ';' AS s")
	if err != nil {
		t.Fatalf("Parse string-with-semicolon: %v", err)
	}
	lit, ok2 := stmt.(*SelectStmt).Items.Items[0].Expr.(*LitString)
	if !ok2 || lit.V != ";" {
		t.Errorf("string literal = %+v, want ';'", stmt.(*SelectStmt).Items.Items[0].Expr)
	}

	// A mid-query semicolon still errors — the dialect is single-statement.
	if _, err := Parse("SELECT 1; SELECT 2"); err == nil {
		t.Error("expected error for a mid-query semicolon (multi-statement)")
	}
}

func TestParseSubqueryFrom(t *testing.T) {
	stmt, err := Parse("SELECT x.a FROM (SELECT a FROM t WHERE a > 1) AS x")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	s := stmt.(*SelectStmt)
	if s.From.Subquery == nil {
		t.Fatal("expected From.Subquery to be set")
	}
	if s.From.Alias != "x" {
		t.Errorf("alias = %q, want x", s.From.Alias)
	}
	inner, ok := s.From.Subquery.(*SelectStmt)
	if !ok {
		t.Fatalf("subquery is %T, want *SelectStmt", s.From.Subquery)
	}
	if inner.From.Name != "t" {
		t.Errorf("inner from = %q, want t", inner.From.Name)
	}
}

func TestParseSubqueryUnionFrom(t *testing.T) {
	// A derived table may itself be a UNION.
	stmt, err := Parse("SELECT u.r FROM (SELECT a AS r FROM t UNION ALL SELECT b FROM t) AS u")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	s := stmt.(*SelectStmt)
	set, ok := s.From.Subquery.(*SetOpStmt)
	if !ok {
		t.Fatalf("subquery is %T, want *SetOpStmt", s.From.Subquery)
	}
	if len(set.Selects) != 2 || len(set.Ops) != 1 || set.Ops[0].Kind != SetUnion || !set.Ops[0].All {
		t.Errorf("union shape = %d selects, Ops=%+v", len(set.Selects), set.Ops)
	}
}

func TestParseWith(t *testing.T) {
	stmt, err := Parse("WITH a AS (SELECT x FROM t), b AS (SELECT y FROM u UNION SELECT z FROM v) SELECT * FROM a JOIN b ON a.x = b.y")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	w, ok := stmt.(*WithStmt)
	if !ok {
		t.Fatalf("stmt is %T, want *WithStmt", stmt)
	}
	if len(w.CTEs) != 2 || w.CTEs[0].Name != "a" || w.CTEs[1].Name != "b" {
		t.Fatalf("CTEs = %+v, want [a b]", w.CTEs)
	}
	if _, ok := w.CTEs[0].Query.(*SelectStmt); !ok {
		t.Errorf("CTE a query is %T, want *SelectStmt", w.CTEs[0].Query)
	}
	if _, ok := w.CTEs[1].Query.(*SetOpStmt); !ok {
		t.Errorf("CTE b query is %T, want *SetOpStmt (a UNION)", w.CTEs[1].Query)
	}
	if _, ok := w.Body.(*SelectStmt); !ok {
		t.Errorf("body is %T, want *SelectStmt", w.Body)
	}
}

func TestParseWithBodyUnion(t *testing.T) {
	stmt, err := Parse("WITH a AS (SELECT x FROM t) SELECT x FROM a UNION SELECT x FROM a")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if _, ok := stmt.(*WithStmt).Body.(*SetOpStmt); !ok {
		t.Errorf("body is %T, want *SetOpStmt", stmt.(*WithStmt).Body)
	}
}

func TestParseILike(t *testing.T) {
	for _, c := range []struct {
		q           string
		insensitive bool
		negate      bool
	}{
		{"SELECT a FROM t WHERE a LIKE 'x%'", false, false},
		{"SELECT a FROM t WHERE a ILIKE 'x%'", true, false},
		{"SELECT a FROM t WHERE a NOT ILIKE 'x%'", true, true},
		{"SELECT a FROM t WHERE a NOT LIKE 'x%'", false, true},
	} {
		stmt, err := Parse(c.q)
		if err != nil {
			t.Fatalf("parse %q: %v", c.q, err)
		}
		le, ok := stmt.(*SelectStmt).Where.(*LikeExpr)
		if !ok {
			t.Fatalf("%q: where is %T, want *LikeExpr", c.q, stmt.(*SelectStmt).Where)
		}
		if le.Insensitive != c.insensitive || le.Negate != c.negate {
			t.Errorf("%q: insensitive=%v negate=%v, want %v/%v", c.q, le.Insensitive, le.Negate, c.insensitive, c.negate)
		}
	}
}

func TestParseCountDistinct(t *testing.T) {
	stmt, err := Parse("SELECT COUNT(DISTINCT region) FROM t")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	fc, ok := stmt.(*SelectStmt).Items.Items[0].Expr.(*FuncCall)
	if !ok {
		t.Fatalf("item is %T, want *FuncCall", stmt.(*SelectStmt).Items.Items[0].Expr)
	}
	if !fc.Distinct {
		t.Error("FuncCall.Distinct = false, want true")
	}
	if fc.Name != "COUNT" || len(fc.Args) != 1 {
		t.Errorf("fc = %q with %d args", fc.Name, len(fc.Args))
	}
}

func TestParseInSubquery(t *testing.T) {
	stmt, err := Parse("SELECT name FROM t WHERE id IN (SELECT uid FROM o)")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	in, ok := stmt.(*SelectStmt).Where.(*InExpr)
	if !ok {
		t.Fatalf("expected *InExpr, got %T", stmt.(*SelectStmt).Where)
	}
	if in.Subquery == nil {
		t.Fatal("expected InExpr.Subquery to be set")
	}
	if len(in.List) != 0 {
		t.Errorf("List should be empty for a subquery IN, got %d", len(in.List))
	}
	if in.Subquery.From.Name != "o" {
		t.Errorf("subquery from = %q, want o", in.Subquery.From.Name)
	}
}

func TestParseInValueListStillWorks(t *testing.T) {
	stmt, err := Parse("SELECT name FROM t WHERE id IN (1, 2, 3)")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	in := stmt.(*SelectStmt).Where.(*InExpr)
	if in.Subquery != nil {
		t.Error("value-list IN should not set Subquery")
	}
	if len(in.List) != 3 {
		t.Errorf("List len = %d, want 3", len(in.List))
	}
}

func TestParseNotInSubquery(t *testing.T) {
	stmt, err := Parse("SELECT name FROM t WHERE id NOT IN (SELECT uid FROM o)")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	in := stmt.(*SelectStmt).Where.(*InExpr)
	if !in.Negate || in.Subquery == nil {
		t.Errorf("expected negated subquery IN, got Negate=%v Subquery=%v", in.Negate, in.Subquery)
	}
}

func TestParseQualifiedRefWithDashes(t *testing.T) {
	// Dashes appear in filenames and Claude Code project slugs; the source
	// string must capture them, not stop at the first '-'.
	stmt, err := Parse("SELECT * FROM claudelogs:/home/x/.claude/projects/-home-x-proj")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	s := stmt.(*SelectStmt)
	if s.From.Prefix != "claudelogs" {
		t.Errorf("prefix = %q", s.From.Prefix)
	}
	if s.From.Source != "/home/x/.claude/projects/-home-x-proj" {
		t.Errorf("source = %q (dashes lost)", s.From.Source)
	}
}

func TestParseUnion(t *testing.T) {
	stmt, err := Parse("SELECT a FROM t UNION SELECT a FROM u")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	set, ok := stmt.(*SetOpStmt)
	if !ok {
		t.Fatalf("expected *SetOpStmt, got %T", stmt)
	}
	if len(set.Selects) != 2 {
		t.Fatalf("branches = %d, want 2", len(set.Selects))
	}
	if len(set.Ops) != 1 || set.Ops[0].Kind != SetUnion || set.Ops[0].All {
		t.Errorf("Ops = %+v, want one distinct UNION", set.Ops)
	}
}

func TestParseUnionAllAndChain(t *testing.T) {
	stmt, err := Parse("SELECT a FROM t UNION ALL SELECT a FROM u UNION SELECT a FROM v")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	set := stmt.(*SetOpStmt)
	if len(set.Selects) != 3 {
		t.Fatalf("branches = %d, want 3", len(set.Selects))
	}
	if !set.Ops[0].All || set.Ops[1].All {
		t.Errorf("Ops = %+v, want [UNION ALL, UNION]", set.Ops)
	}
}

func TestParseSetOpKinds(t *testing.T) {
	stmt, err := Parse("SELECT a FROM t INTERSECT SELECT a FROM u EXCEPT ALL SELECT a FROM v")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	set := stmt.(*SetOpStmt)
	if len(set.Ops) != 2 {
		t.Fatalf("ops = %d, want 2", len(set.Ops))
	}
	if set.Ops[0].Kind != SetIntersect || set.Ops[0].All {
		t.Errorf("op0 = %+v, want INTERSECT (distinct)", set.Ops[0])
	}
	if set.Ops[1].Kind != SetExcept || !set.Ops[1].All {
		t.Errorf("op1 = %+v, want EXCEPT ALL", set.Ops[1])
	}
}

func TestParseUnionTrailingOrderByLifted(t *testing.T) {
	stmt, err := Parse("SELECT a FROM t UNION SELECT a FROM u ORDER BY a DESC LIMIT 5")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	set := stmt.(*SetOpStmt)
	// ORDER BY / LIMIT belong to the union, not the last branch.
	if len(set.OrderBy) != 1 || !set.OrderBy[0].Desc {
		t.Errorf("set.OrderBy = %+v, want one DESC term", set.OrderBy)
	}
	if set.Limit == nil || *set.Limit != 5 {
		t.Errorf("set.Limit = %v, want 5", set.Limit)
	}
	last := set.Selects[1]
	if len(last.OrderBy) != 0 || last.Limit != nil {
		t.Errorf("last branch should have no ORDER BY/LIMIT, got order=%v limit=%v", last.OrderBy, last.Limit)
	}
}

func TestParseURLRef(t *testing.T) {
	stmt, err := Parse("SELECT * FROM http://example.com/users.json")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	s := stmt.(*SelectStmt)
	if s.From.Prefix != "http" {
		t.Errorf("prefix = %q, want http", s.From.Prefix)
	}
	if s.From.Source != "http://example.com/users.json" {
		t.Errorf("source = %q", s.From.Source)
	}
}

func TestParseURLRefHTTPSWithAliasAndWhere(t *testing.T) {
	stmt, err := Parse("SELECT name FROM https://api.test/v1/items?active=true AS feed WHERE name LIKE 'a%'")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	s := stmt.(*SelectStmt)
	if s.From.Prefix != "https" {
		t.Errorf("prefix = %q, want https", s.From.Prefix)
	}
	if s.From.Source != "https://api.test/v1/items?active=true" {
		t.Errorf("source = %q", s.From.Source)
	}
	if s.From.Alias != "feed" {
		t.Errorf("alias = %q, want feed", s.From.Alias)
	}
	if s.Where == nil {
		t.Error("where clause lost after URL ref")
	}
}

func TestParseURLRefInJoin(t *testing.T) {
	stmt, err := Parse("SELECT * FROM users u JOIN http://h/orders.json o ON o.uid = u.id")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	s := stmt.(*SelectStmt)
	if len(s.Joins) != 1 {
		t.Fatalf("joins = %d, want 1", len(s.Joins))
	}
	if s.Joins[0].Ref.Prefix != "http" || s.Joins[0].Ref.Source != "http://h/orders.json" {
		t.Errorf("join ref = %+v", s.Joins[0].Ref)
	}
	if s.Joins[0].Ref.Alias != "o" {
		t.Errorf("join alias = %q, want o", s.Joins[0].Ref.Alias)
	}
}

func TestParsePrefixedDSNRef(t *testing.T) {
	stmt, err := Parse("SELECT * FROM sql:postgres://user@host:5432/db")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	s := stmt.(*SelectStmt)
	if s.From.Prefix != "sql" {
		t.Errorf("prefix = %q, want sql", s.From.Prefix)
	}
	if s.From.Source != "postgres://user@host:5432/db" {
		t.Errorf("source = %q", s.From.Source)
	}
}

func TestParseTableFunc(t *testing.T) {
	s := mustParseSelect(t, "SELECT value FROM generate_series(1, 10, 2) AS g")
	if s.From.Func == nil || s.From.Func.Name != "generate_series" || len(s.From.Func.Args) != 3 {
		t.Fatalf("FROM func = %+v, want generate_series with 3 args", s.From.Func)
	}
	if s.From.Alias != "g" {
		t.Errorf("alias = %q, want g", s.From.Alias)
	}
}

func TestParseColumnAliases(t *testing.T) {
	s := mustParseSelect(t, "SELECT k, v FROM (SELECT a, b FROM t) AS s(k, v)")
	if got := s.From.ColAliases; len(got) != 2 || got[0] != "k" || got[1] != "v" {
		t.Fatalf("ColAliases = %v, want [k v]", got)
	}
	if s.From.Alias != "s" {
		t.Errorf("alias = %q, want s", s.From.Alias)
	}
	// The list also attaches to a table function alias.
	s = mustParseSelect(t, "SELECT n FROM generate_series(1, 3) AS g(n)")
	if got := s.From.ColAliases; len(got) != 1 || got[0] != "n" {
		t.Errorf("table-func ColAliases = %v, want [n]", got)
	}
}
