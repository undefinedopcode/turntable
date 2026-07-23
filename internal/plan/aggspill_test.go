package plan

import (
	"context"
	"fmt"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/sql"
)

// manyGroupsConn emits nRows rows with a "grp" column of `groups` distinct
// values and an int "v" — the input for an end-to-end GROUP BY spill test.
type manyGroupsConn struct {
	groups, nRows int
}

func (manyGroupsConn) Name() string { return "manygroups" }
func (manyGroupsConn) Datasets(ctx context.Context) ([]connector.Dataset, error) {
	return nil, nil
}
func (manyGroupsConn) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	return engine.Schema{Columns: []engine.Column{
		{Name: "grp", Type: engine.TypeString, Nullable: true},
		{Name: "v", Type: engine.TypeInt, Nullable: true},
	}}, nil
}
func (c manyGroupsConn) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	rows := make([]engine.Row, c.nRows)
	for i := 0; i < c.nRows; i++ {
		g := fmt.Sprintf("g%04d", i%c.groups)
		rows[i] = engine.Row{Values: []engine.Value{engine.StringVal(g), engine.IntVal(int64(i % 11))}}
	}
	return engine.NewSliceIter(rows), nil
}

func manyGroupsRegistry(t *testing.T, groups, nRows int) *connector.Registry {
	t.Helper()
	conn := manyGroupsConn{groups: groups, nRows: nRows}
	reg := connector.NewRegistry()
	_ = reg.RegisterConnector(conn)
	if err := reg.RegisterSource("t", conn, connector.Dataset{Name: "t"}); err != nil {
		t.Fatal(err)
	}
	return reg
}

func runQueryCfg(t *testing.T, reg *connector.Registry, q string, cfg engine.AggConfig) ([]engine.Row, error) {
	t.Helper()
	stmt, err := sql.Parse(q)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p, err := Build(context.Background(), stmt, reg, WithAggConfig(cfg))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	it, _, err := Exec(context.Background(), p)
	if err != nil {
		return nil, err
	}
	return engine.Materialize(context.Background(), it)
}

// TestGroupBySpillEndToEnd runs a GROUP BY through the full parse→build→exec
// path with a tiny group budget + spilling enabled, proving the AggConfig is
// threaded through Exec and that the spilled result matches the in-memory one.
func TestGroupBySpillEndToEnd(t *testing.T) {
	reg := manyGroupsRegistry(t, 300, 3000)
	const q = "SELECT grp, COUNT(*) AS c, SUM(v) AS s FROM t GROUP BY grp"

	want, err := runQueryCfg(t, reg, q, engine.AggConfig{})
	if err != nil {
		t.Fatalf("in-memory: %v", err)
	}
	if len(want) != 300 {
		t.Fatalf("in-memory groups = %d, want 300", len(want))
	}

	got, err := runQueryCfg(t, reg, q, engine.AggConfig{MaxGroups: 8, Spill: true})
	if err != nil {
		t.Fatalf("spilling: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("spilled groups = %d, want %d", len(got), len(want))
	}
	// Compare as sets: grp → (c, s).
	index := map[string][2]int64{}
	for _, r := range want {
		c, _ := r.Values[1].AsInt()
		s, _ := r.Values[2].AsInt()
		index[r.Values[0].AsString()] = [2]int64{c, s}
	}
	for _, r := range got {
		c, _ := r.Values[1].AsInt()
		s, _ := r.Values[2].AsInt()
		w, ok := index[r.Values[0].AsString()]
		if !ok {
			t.Errorf("spilled produced unexpected group %q", r.Values[0].AsString())
			continue
		}
		if w != [2]int64{c, s} {
			t.Errorf("group %q = (c=%d,s=%d), want (c=%d,s=%d)", r.Values[0].AsString(), c, s, w[0], w[1])
		}
	}
}

// TestGroupByGuardrailEndToEnd proves the budget errors cleanly (no spill) when
// group cardinality exceeds the limit.
func TestGroupByGuardrailEndToEnd(t *testing.T) {
	reg := manyGroupsRegistry(t, 100, 1000)
	_, err := runQueryCfg(t, reg,
		"SELECT grp, COUNT(*) FROM t GROUP BY grp",
		engine.AggConfig{MaxGroups: 10, Spill: false})
	if err == nil {
		t.Fatal("expected guardrail error when groups exceed the budget without spilling")
	}
}
