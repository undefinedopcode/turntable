package pluginc

import (
	"encoding/json"
	"testing"

	"github.com/april/turntable/internal/sql"
)

func col(name string) *sql.ColRef  { return &sql.ColRef{Name: name} }
func iLit(v int64) *sql.LitInt     { return &sql.LitInt{V: v} }
func sLit(v string) *sql.LitString { return &sql.LitString{V: v} }

func TestBuildPredicate(t *testing.T) {
	tests := []struct {
		name      string
		expr      sql.Expr
		wantNil   bool
		wantExact bool
		wantKind  string
	}{
		{
			name:      "simple equality",
			expr:      &sql.BinaryOp{Op: "=", Left: col("pid"), Right: iLit(42)},
			wantExact: true,
			wantKind:  "compare",
		},
		{
			name:      "reversed comparison flips operator",
			expr:      &sql.BinaryOp{Op: "<", Left: iLit(10), Right: col("n")},
			wantExact: true,
			wantKind:  "compare",
		},
		{
			name: "AND of two encodable conjuncts",
			expr: &sql.BinaryOp{Op: "AND",
				Left:  &sql.BinaryOp{Op: "=", Left: col("a"), Right: iLit(1)},
				Right: &sql.BinaryOp{Op: ">", Left: col("b"), Right: iLit(2)}},
			wantExact: true,
			wantKind:  "and",
		},
		{
			name: "AND drops non-encodable conjunct but keeps the other",
			expr: &sql.BinaryOp{Op: "AND",
				Left:  &sql.BinaryOp{Op: "=", Left: col("a"), Right: iLit(1)},
				Right: &sql.BinaryOp{Op: "=", Left: col("x"), Right: col("y")}}, // col=col not encodable
			wantExact: false,
			wantKind:  "compare", // only the encodable conjunct survives
		},
		{
			name: "OR with a non-encodable branch is dropped entirely",
			expr: &sql.BinaryOp{Op: "OR",
				Left:  &sql.BinaryOp{Op: "=", Left: col("a"), Right: iLit(1)},
				Right: &sql.BinaryOp{Op: "=", Left: col("x"), Right: col("y")}},
			wantNil: true,
		},
		{
			name:      "IN list",
			expr:      &sql.InExpr{Expr: col("status"), List: []sql.Expr{sLit("a"), sLit("b")}},
			wantExact: true,
			wantKind:  "in",
		},
		{
			name:      "IS NULL",
			expr:      &sql.IsNullExpr{Expr: col("deleted_at")},
			wantExact: true,
			wantKind:  "isnull",
		},
		{
			name:      "LIKE with literal pattern",
			expr:      &sql.LikeExpr{Expr: col("name"), Pat: sLit("foo%")},
			wantExact: true,
			wantKind:  "like",
		},
		{
			name:    "bare function call is not encodable",
			expr:    &sql.FuncCall{Name: "lower", Args: []sql.Expr{col("name")}},
			wantNil: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node, exact := buildPredicate(tt.expr)
			if tt.wantNil {
				if node != nil {
					t.Fatalf("want nil node, got %+v", node)
				}
				return
			}
			if node == nil {
				t.Fatalf("got nil node, want kind %q", tt.wantKind)
			}
			if node.Kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", node.Kind, tt.wantKind)
			}
			if exact != tt.wantExact {
				t.Errorf("exact = %v, want %v", exact, tt.wantExact)
			}
			// Must marshal cleanly to JSON.
			if _, err := json.Marshal(node); err != nil {
				t.Errorf("marshal: %v", err)
			}
		})
	}
}

func TestEncodePredicateRoundTrip(t *testing.T) {
	expr := &sql.BinaryOp{Op: "AND",
		Left:  &sql.BinaryOp{Op: "=", Left: col("name"), Right: sLit("PATH")},
		Right: &sql.InExpr{Expr: col("kind"), List: []sql.Expr{iLit(1), iLit(2)}}}
	raw := encodePredicate(expr)
	if raw == nil {
		t.Fatal("encodePredicate returned nil")
	}
	var node predNode
	if err := json.Unmarshal(raw, &node); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if node.Kind != "and" || len(node.Args) != 2 {
		t.Fatalf("got %+v", node)
	}
}
