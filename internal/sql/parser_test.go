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