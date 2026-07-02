package sql

import "testing"

func TestParseQualifiedStar(t *testing.T) {
	stmt, err := Parse("SELECT o.*, name FROM orders AS o")
	if err != nil {
		t.Fatal(err)
	}
	sel := stmt.(*SelectStmt)
	items := sel.Items.Items
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if !items[0].Star || items[0].StarQualifier != "o" {
		t.Errorf("item 0 = %+v, want qualified star o.*", items[0])
	}
	if items[1].Star || items[1].Expr == nil {
		t.Errorf("item 1 = %+v, want plain expr", items[1])
	}
}

func TestParseBareStarUnchanged(t *testing.T) {
	stmt, err := Parse("SELECT * FROM t")
	if err != nil {
		t.Fatal(err)
	}
	it := stmt.(*SelectStmt).Items.Items[0]
	if !it.Star || it.StarQualifier != "" {
		t.Errorf("item = %+v, want bare star", it)
	}
}

func TestParseQualifiedColTimesColIsNotStar(t *testing.T) {
	// a.b * c must stay a multiplication, not lex-glue into a qualified star.
	stmt, err := Parse("SELECT a.b * c FROM t AS a")
	if err != nil {
		t.Fatal(err)
	}
	it := stmt.(*SelectStmt).Items.Items[0]
	if it.Star {
		t.Fatalf("item = %+v, want a multiplication expression", it)
	}
	bin, ok := it.Expr.(*BinaryOp)
	if !ok || bin.Op != "*" {
		t.Fatalf("expr = %#v, want BinaryOp *", it.Expr)
	}
}

func TestSourceStringStopsAtWhitespace(t *testing.T) {
	// A bare alias after an inline file ref must not fuse into the path:
	// FROM csv:./orders.csv o  →  source "./orders.csv", alias "o".
	stmt, err := Parse("SELECT a FROM csv:./data/orders.csv o")
	if err != nil {
		t.Fatal(err)
	}
	from := stmt.(*SelectStmt).From
	if from.Prefix != "csv" || from.Source != "./data/orders.csv" {
		t.Fatalf("ref = %+v, want csv:./data/orders.csv", from)
	}
	if from.Alias != "o" {
		t.Fatalf("alias = %q, want o", from.Alias)
	}
}

func TestSourceStringDashesAndDotsStillWhole(t *testing.T) {
	stmt, err := Parse("SELECT a FROM csv:./my-data.v2.csv AS m")
	if err != nil {
		t.Fatal(err)
	}
	from := stmt.(*SelectStmt).From
	if from.Source != "./my-data.v2.csv" || from.Alias != "m" {
		t.Fatalf("ref = %+v, want ./my-data.v2.csv AS m", from)
	}
}

func TestSourceStringJoinAliasGap(t *testing.T) {
	// Both sides of a join with bare aliases on inline refs.
	stmt, err := Parse("SELECT a FROM csv:./a.csv x JOIN json:./b.json y ON x.id = y.id")
	if err != nil {
		t.Fatal(err)
	}
	sel := stmt.(*SelectStmt)
	if sel.From.Source != "./a.csv" || sel.From.Alias != "x" {
		t.Fatalf("from = %+v", sel.From)
	}
	if len(sel.Joins) != 1 || sel.Joins[0].Ref.Source != "./b.json" || sel.Joins[0].Ref.Alias != "y" {
		t.Fatalf("join = %+v", sel.Joins)
	}
}
