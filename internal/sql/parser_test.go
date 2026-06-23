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
		"SELECT 1;;",        // multiple trailing semicolons
		"SELECT 1;\n",       // trailing newline after the semicolon
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
	if s.From.Subquery.From.Name != "t" {
		t.Errorf("inner from = %q, want t", s.From.Subquery.From.Name)
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
	if len(set.All) != 1 || set.All[0] {
		t.Errorf("All = %v, want [false]", set.All)
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
	if !set.All[0] || set.All[1] {
		t.Errorf("All = %v, want [true false]", set.All)
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