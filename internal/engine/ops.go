package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/april/turntable/internal/sql"
)

// ---- Filter -----------------------------------------------------------------

// FilterIter applies a predicate to rows from its child.
type FilterIter struct {
	child    RowIterator
	eval     Evaluator
	pred     sql.Expr
	closed   bool
}

// NewFilterIter returns an iterator that yields child rows where pred is true.
func NewFilterIter(child RowIterator, pred sql.Expr, eval Evaluator) *FilterIter {
	return &FilterIter{child: child, eval: eval, pred: pred}
}

func (f *FilterIter) Next() (Row, bool, error) {
	for {
		r, ok, err := f.child.Next()
		if err != nil || !ok {
			return r, ok, err
		}
		v, err := f.eval.Eval(f.pred, r)
		if err != nil {
			return Row{}, false, err
		}
		if v.IsNull() {
			continue
		}
		if b, ok := v.AsBool(); ok && b {
			return r, true, nil
		}
	}
}

func (f *FilterIter) Close() error {
	if f.closed {
		return nil
	}
	f.closed = true
	return f.child.Close()
}

// ---- Project ----------------------------------------------------------------

// ProjectedExpr is one output column of a Project: the expression plus the
// output column name (alias or inferred).
type ProjectedExpr struct {
	Expr sql.Expr
	Name string
}

// ProjectIter evaluates the select list against child rows, producing rows
// conforming to outSchema.
type ProjectIter struct {
	child   RowIterator
	out     []ProjectedExpr
	eval    Evaluator
	closed  bool
}

// NewProjectIter returns an iterator that projects child rows via the given
// output expressions.
func NewProjectIter(child RowIterator, out []ProjectedExpr, eval Evaluator) *ProjectIter {
	return &ProjectIter{child: child, out: out, eval: eval}
}

func (p *ProjectIter) Next() (Row, bool, error) {
	r, ok, err := p.child.Next()
	if err != nil || !ok {
		return Row{}, ok, err
	}
	vals := make([]Value, len(p.out))
	for i, pe := range p.out {
		v, err := p.eval.Eval(pe.Expr, r)
		if err != nil {
			return Row{}, false, fmt.Errorf("project %s: %w", pe.Name, err)
		}
		vals[i] = v
	}
	return Row{Values: vals}, true, nil
}

func (p *ProjectIter) Close() error {
	if p.closed {
		return nil
	}
	p.closed = true
	return p.child.Close()
}

// ---- Sort -------------------------------------------------------------------

// SortIter materializes child rows, sorts them, and yields them in order.
type SortIter struct {
	child  RowIterator
	terms  []sortKey
	eval   Evaluator
	rows   []Row
	i      int
	closed bool
}

type sortKey struct {
	expr sql.Expr
	desc bool
}

// NewSortIter returns an iterator that yields child rows ordered by terms.
// The first term is the primary key.
func NewSortIter(child RowIterator, terms []sql.OrderTerm, eval Evaluator) *SortIter {
	keys := make([]sortKey, len(terms))
	for i, t := range terms {
		keys[i] = sortKey{expr: t.Expr, desc: t.Desc}
	}
	return &SortIter{child: child, terms: keys, eval: eval}
}

func (s *SortIter) Next() (Row, bool, error) {
	if s.rows == nil {
		for {
			r, ok, err := s.child.Next()
			if err != nil {
				return Row{}, false, err
			}
			if !ok {
				break
			}
			s.rows = append(s.rows, r)
		}
		sortRows(s.rows, s.terms, s.eval)
	}
	if s.i >= len(s.rows) {
		return Row{}, false, nil
	}
	r := s.rows[s.i]
	s.i++
	return r, true, nil
}

func (s *SortIter) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.child.Close()
}

func sortRows(rows []Row, terms []sortKey, eval Evaluator) {
	// Evaluate keys once per row for stability and speed.
	type keyed struct {
		row  Row
		keys []Value
	}
	kd := make([]keyed, len(rows))
	for i, r := range rows {
		keys := make([]Value, len(terms))
		for j, t := range terms {
			v, err := eval.Eval(t.expr, r)
			if err != nil {
				keys[j] = Null()
			} else {
				keys[j] = v
			}
		}
		kd[i] = keyed{row: r, keys: keys}
	}
	less := func(a, b keyed) bool {
		for j := range terms {
			c := Compare(a.keys[j], b.keys[j])
			if c == 0 {
				continue
			}
			if terms[j].desc {
				c = -c
			}
			return c < 0
		}
		return false
	}
	sort.SliceStable(kd, func(i, j int) bool { return less(kd[i], kd[j]) })
	for i, k := range kd {
		rows[i] = k.row
	}
}

// ---- Limit ------------------------------------------------------------------

// LimitIter applies LIMIT/OFFSET to child rows.
type LimitIter struct {
	child  RowIterator
	limit  *int
	offset int
	skipped int
	yielded int
	closed bool
}

// NewLimitIter returns an iterator that skips offset rows then yields up to
// limit rows (nil limit = unlimited).
func NewLimitIter(child RowIterator, limit *int, offset int) *LimitIter {
	return &LimitIter{child: child, limit: limit, offset: offset}
}

func (l *LimitIter) Next() (Row, bool, error) {
	for l.skipped < l.offset {
		_, ok, err := l.child.Next()
		if err != nil || !ok {
			return Row{}, ok, err
		}
		l.skipped++
	}
	if l.limit != nil && l.yielded >= *l.limit {
		return Row{}, false, nil
	}
	r, ok, err := l.child.Next()
	if err != nil || !ok {
		return Row{}, ok, err
	}
	l.yielded++
	return r, true, nil
}

func (l *LimitIter) Close() error {
	if l.closed {
		return nil
	}
	l.closed = true
	return l.child.Close()
}

// ---- Hash Join --------------------------------------------------------------

// KeyExtractor extracts the join key value from a single-side row. The
// planner constructs one KeyExtractor per join side after binding column
// references to positional indices, so the join itself never needs to resolve
// column names or know about aliases/schemas.
type KeyExtractor func(row Row) Value

// HashJoinIter implements an in-memory hash equi-join. The left side is
// materialized as the build side; the right side is streamed as the probe side.
// Only equi-join predicates (a.col = b.col) are supported; residual non-equi
// predicates should be applied by a separate Filter above the join.
type HashJoinIter struct {
	left, right RowIterator
	leftKey, rightKey KeyExtractor

	kind      sql.JoinKind // inner or left
	rightWidth int          // number of right-side columns (for NULL padding)

	bucket  map[string][]Row
	keys    []string // ordered build keys for deterministic left order
	matched map[string]bool

	pending []Row // matched build rows for current probe row
	pendi   int

	closed bool
}

// NewHashJoinIter builds a hash join. leftKey/rightKey extract the join key
// from each side's rows; rightWidth pads NULLs for unmatched left rows in a
// LEFT JOIN.
func NewHashJoinIter(
	left, right RowIterator,
	leftKey, rightKey KeyExtractor,
	kind sql.JoinKind,
	rightWidth int,
) *HashJoinIter {
	return &HashJoinIter{
		left: left, right: right,
		leftKey: leftKey, rightKey: rightKey,
		kind: kind, rightWidth: rightWidth,
	}
}

func (j *HashJoinIter) build() error {
	j.bucket = map[string][]Row{}
	j.matched = map[string]bool{}
	for {
		r, ok, err := j.left.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		k := keyString(j.leftKey(r))
		j.bucket[k] = append(j.bucket[k], r)
		j.keys = append(j.keys, k)
	}
	return nil
}

func keyString(v Value) string {
	if v.IsNull() {
		return "\x00null\x00"
	}
	return v.AsString()
}

func (j *HashJoinIter) Next() (Row, bool, error) {
	if j.bucket == nil {
		if err := j.build(); err != nil {
			return Row{}, false, err
		}
	}
	for {
		if len(j.pending) > 0 && j.pendi < len(j.pending) {
			r := j.pending[j.pendi]
			j.pendi++
			return r, true, nil
		}
		rr, ok, err := j.right.Next()
		if err != nil {
			return Row{}, false, err
		}
		if !ok {
			if j.kind == sql.JoinLeft {
				return j.emitUnmatchedLeft()
			}
			return Row{}, false, nil
		}
		k := keyString(j.rightKey(rr))
		matches := j.bucket[k]
		if len(matches) == 0 {
			continue
		}
		j.matched[k] = true
		j.pending = make([]Row, 0, len(matches))
		for _, lr := range matches {
			j.pending = append(j.pending, mergeRows(lr, rr))
		}
		j.pendi = 0
	}
}

func mergeRows(l, r Row) Row {
	vals := make([]Value, 0, len(l.Values)+len(r.Values))
	vals = append(vals, l.Values...)
	vals = append(vals, r.Values...)
	return Row{Values: vals}
}

func (j *HashJoinIter) emitUnmatchedLeft() (Row, bool, error) {
	for _, k := range j.keys {
		if j.matched[k] {
			continue
		}
		j.matched[k] = true
		lr := j.bucket[k][0]
		nulls := make([]Value, j.rightWidth)
		for i := range nulls {
			nulls[i] = Null()
		}
		return mergeRows(lr, Row{Values: nulls}), true, nil
	}
	return Row{}, false, nil
}

func (j *HashJoinIter) Close() error {
	if j.closed {
		return nil
	}
	j.closed = true
	err1 := j.left.Close()
	err2 := j.right.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// ---- Aggregate --------------------------------------------------------------

// AggSpec describes one aggregate output column: the function name, the
// argument expression (evaluated against child rows), and the output name.
type AggSpec struct {
	Func string // COUNT, SUM, AVG, MIN, MAX
	Arg  sql.Expr
	Name string
}

// AggregateIter groups child rows by the group-key expressions and computes
// the specified aggregates per group. It materializes all groups in memory
// (v0.1 assumption: bounded datasets). After grouping, an optional HAVING
// predicate (evaluated against the aggregate-output row) filters groups.
//
// The output row layout is: [group key values..., aggregate values...].
type AggregateIter struct {
	child    RowIterator
	keys     []sql.Expr
	aggs     []AggSpec
	having   sql.Expr
	eval     Evaluator
	havingEval Evaluator // resolver over the aggregate output schema
	outSchema Schema

	groups   []*aggGroup
	gi       int
	closed   bool
}

type aggGroup struct {
	key   string
	keyVals []Value
	rows  []Row
}

// NewAggregateIter returns an iterator that groups and aggregates child rows.
// outSchema is the schema of the produced rows (group cols + agg cols).
// havingEval resolves columns against outSchema; pass nil if no HAVING.
func NewAggregateIter(
	child RowIterator,
	keys []sql.Expr,
	aggs []AggSpec,
	having sql.Expr,
	eval Evaluator,
	havingEval Evaluator,
	outSchema Schema,
) *AggregateIter {
	return &AggregateIter{
		child: child, keys: keys, aggs: aggs, having: having,
		eval: eval, havingEval: havingEval, outSchema: outSchema,
	}
}

func (a *AggregateIter) Next() (Row, bool, error) {
	if a.groups == nil {
		if err := a.collect(); err != nil {
			return Row{}, false, err
		}
	}
	for a.gi < len(a.groups) {
		g := a.groups[a.gi]
		a.gi++
		out := make([]Value, 0, len(g.keyVals)+len(a.aggs))
		out = append(out, g.keyVals...)
		for _, spec := range a.aggs {
			v, err := computeAgg(spec, g.rows, a.eval)
			if err != nil {
				return Row{}, false, err
			}
			out = append(out, v)
		}
		// Apply HAVING if present.
		if a.having != nil && a.havingEval.Resolve != nil {
			v, err := a.havingEval.Eval(a.having, Row{Values: out})
			if err != nil {
				return Row{}, false, err
			}
			if v.IsNull() {
				continue
			}
			if b, ok := v.AsBool(); !ok || !b {
				continue
			}
		}
		return Row{Values: out}, true, nil
	}
	return Row{}, false, nil
}

func (a *AggregateIter) collect() error {
	index := map[string]int{}
	for {
		r, ok, err := a.child.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		keyVals := make([]Value, len(a.keys))
		var kb strings.Builder
		for i, ke := range a.keys {
			v, err := a.eval.Eval(ke, r)
			if err != nil {
				return err
			}
			keyVals[i] = v
			kb.WriteString(keyString(v))
			kb.WriteString("\x01")
		}
		ks := kb.String()
		idx, seen := index[ks]
		if !seen {
			idx = len(a.groups)
			index[ks] = idx
			a.groups = append(a.groups, &aggGroup{key: ks, keyVals: keyVals})
		}
		a.groups[idx].rows = append(a.groups[idx].rows, r)
	}
	return nil
}

func (a *AggregateIter) Close() error {
	if a.closed {
		return nil
	}
	a.closed = true
	return a.child.Close()
}

func computeAgg(spec AggSpec, rows []Row, eval Evaluator) (Value, error) {
	name := strings.ToUpper(spec.Func)
	if name == "COUNT" {
		// COUNT(*) counts rows; COUNT(col) counts non-null values.
		if cr, ok := spec.Arg.(*sql.ColRef); ok && cr.Name == "*" && cr.Qualifier == "" {
			return IntVal(int64(len(rows))), nil
		}
		var n int64
		for _, r := range rows {
			v, err := eval.Eval(spec.Arg, r)
			if err != nil {
				return Value{}, err
			}
			if !v.IsNull() {
				n++
			}
		}
		return IntVal(n), nil
	}
	// SUM/AVG/MIN/MAX ignore NULLs.
	var sum float64
	var count int
	var minV, maxV Value
	haveVal := false
	for _, r := range rows {
		v, err := eval.Eval(spec.Arg, r)
		if err != nil {
			return Value{}, err
		}
		if v.IsNull() {
			continue
		}
		count++
		switch name {
		case "SUM", "AVG":
			f, ok := v.AsFloat()
			if !ok {
				return Value{}, fmt.Errorf("%s requires numeric arg", name)
			}
			sum += f
		case "MIN":
			if !haveVal {
				minV = v
			} else if Compare(v, minV) < 0 {
				minV = v
			}
		case "MAX":
			if !haveVal {
				maxV = v
			} else if Compare(v, maxV) > 0 {
				maxV = v
			}
		}
		if !haveVal {
			haveVal = true
		}
	}
	switch name {
	case "SUM":
		if !haveVal {
			return Null(), nil
		}
		return FloatVal(sum), nil
	case "AVG":
		if !haveVal || count == 0 {
			return Null(), nil
		}
		return FloatVal(sum / float64(count)), nil
	case "MIN":
		if !haveVal {
			return Null(), nil
		}
		return minV, nil
	case "MAX":
		if !haveVal {
			return Null(), nil
		}
		return maxV, nil
	}
	return Value{}, fmt.Errorf("unknown aggregate %s", name)
}

// ---- Distinct ---------------------------------------------------------------

// DistinctIter yields distinct rows (by value tuple). Order-preserving.
type DistinctIter struct {
	child  RowIterator
	seen   map[string]bool
	closed bool
}

// NewDistinctIter returns an iterator that drops duplicate rows.
func NewDistinctIter(child RowIterator) *DistinctIter {
	return &DistinctIter{child: child, seen: map[string]bool{}}
}

func (d *DistinctIter) Next() (Row, bool, error) {
	for {
		r, ok, err := d.child.Next()
		if err != nil || !ok {
			return Row{}, ok, err
		}
		key := rowKey(r)
		if d.seen[key] {
			continue
		}
		d.seen[key] = true
		return r, true, nil
	}
}

func (d *DistinctIter) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	return d.child.Close()
}

// ---- Concat -----------------------------------------------------------------

// ConcatIter yields all rows from each child in order (UNION ALL's plumbing).
// Children must share a column layout; the caller validates that. Each child is
// closed as it is exhausted.
type ConcatIter struct {
	children []RowIterator
	idx      int
	closed   bool
}

// NewConcatIter returns an iterator that concatenates the children in order.
func NewConcatIter(children []RowIterator) *ConcatIter {
	return &ConcatIter{children: children}
}

func (c *ConcatIter) Next() (Row, bool, error) {
	for c.idx < len(c.children) {
		r, ok, err := c.children[c.idx].Next()
		if err != nil {
			return Row{}, false, err
		}
		if ok {
			return r, true, nil
		}
		// Exhausted: close this child and move to the next.
		c.children[c.idx].Close()
		c.idx++
	}
	return Row{}, false, nil
}

func (c *ConcatIter) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	// Children before idx were already closed as they were exhausted; close any
	// remaining (including a partially-consumed current child after early stop).
	var firstErr error
	for i := c.idx; i < len(c.children); i++ {
		if err := c.children[i].Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func rowKey(r Row) string {
	var b strings.Builder
	for _, v := range r.Values {
		if v.IsNull() {
			b.WriteString("\x00")
		} else {
			b.WriteString(v.AsString())
		}
		b.WriteString("\x01")
	}
	return b.String()
}