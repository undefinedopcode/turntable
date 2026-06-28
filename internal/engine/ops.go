package engine

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

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

// HashJoinIter implements an in-memory hash equi-join supporting INNER, LEFT,
// RIGHT, and FULL kinds. The left side is materialized as the build side; the
// right side is streamed as the probe side. Only equi-join predicates
// (a.col = b.col) are supported; residual non-equi predicates should be applied
// by a separate Filter above the join. Output rows are always [left..., right...]
// with the unmatched side NULL-padded.
type HashJoinIter struct {
	left, right       RowIterator
	leftKey, rightKey KeyExtractor

	kind                  sql.JoinKind
	leftWidth, rightWidth int // column counts, for NULL padding the missing side

	leftRows []Row
	// leftMatched is parallel to leftRows, set as probe rows match; bucket maps a
	// join key to indices into leftRows.
	leftMatched []bool
	bucket      map[string][]int
	built       bool

	pending []Row // join rows produced for the current probe row
	pendi   int

	rightDone bool
	ui        int // scan cursor for emitting unmatched left rows (LEFT/FULL)

	closed bool
}

// NewHashJoinIter builds a hash join. leftKey/rightKey extract the join key from
// each side's rows; leftWidth/rightWidth give the column counts used to NULL-pad
// the missing side for outer joins.
func NewHashJoinIter(
	left, right RowIterator,
	leftKey, rightKey KeyExtractor,
	kind sql.JoinKind,
	leftWidth, rightWidth int,
) *HashJoinIter {
	return &HashJoinIter{
		left: left, right: right,
		leftKey: leftKey, rightKey: rightKey,
		kind: kind, leftWidth: leftWidth, rightWidth: rightWidth,
	}
}

func (j *HashJoinIter) build() error {
	j.bucket = map[string][]int{}
	for {
		r, ok, err := j.left.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		idx := len(j.leftRows)
		j.leftRows = append(j.leftRows, r)
		k := keyString(j.leftKey(r))
		j.bucket[k] = append(j.bucket[k], idx)
	}
	j.leftMatched = make([]bool, len(j.leftRows))
	j.built = true
	return nil
}

func keyString(v Value) string {
	if v.IsNull() {
		return "\x00null\x00"
	}
	return v.AsString()
}

func (j *HashJoinIter) keepsUnmatchedLeft() bool {
	return j.kind == sql.JoinLeft || j.kind == sql.JoinFull
}

func (j *HashJoinIter) keepsUnmatchedRight() bool {
	return j.kind == sql.JoinRight || j.kind == sql.JoinFull
}

func (j *HashJoinIter) Next() (Row, bool, error) {
	if !j.built {
		if err := j.build(); err != nil {
			return Row{}, false, err
		}
	}
	if j.kind == sql.JoinSemi || j.kind == sql.JoinAnti {
		return j.nextSemiAnti()
	}
	for {
		// Drain join rows buffered for the current probe row.
		if j.pendi < len(j.pending) {
			r := j.pending[j.pendi]
			j.pendi++
			return r, true, nil
		}

		if !j.rightDone {
			rr, ok, err := j.right.Next()
			if err != nil {
				return Row{}, false, err
			}
			if !ok {
				j.rightDone = true
				continue
			}
			idxs := j.bucket[keyString(j.rightKey(rr))]
			if len(idxs) == 0 {
				// Unmatched right row: emit it NULL-padded on the left for
				// RIGHT/FULL; otherwise drop it.
				if j.keepsUnmatchedRight() {
					return mergeRows(nullRow(j.leftWidth), rr), true, nil
				}
				continue
			}
			j.pending = j.pending[:0]
			for _, i := range idxs {
				j.leftMatched[i] = true
				j.pending = append(j.pending, mergeRows(j.leftRows[i], rr))
			}
			j.pendi = 0
			continue
		}

		// Right side exhausted: emit unmatched left rows for LEFT/FULL.
		if !j.keepsUnmatchedLeft() {
			return Row{}, false, nil
		}
		for j.ui < len(j.leftRows) {
			i := j.ui
			j.ui++
			if !j.leftMatched[i] {
				return mergeRows(j.leftRows[i], nullRow(j.rightWidth)), true, nil
			}
		}
		return Row{}, false, nil
	}
}

// nextSemiAnti drains the right side once (marking which left rows have a match)
// then emits left rows: the matched ones for SEMI, the unmatched for ANTI. Output
// is the left columns only. A NULL join key never matches (so a NULL-keyed left
// row is kept by ANTI and dropped by SEMI), matching SQL EXISTS semantics.
func (j *HashJoinIter) nextSemiAnti() (Row, bool, error) {
	if !j.rightDone {
		for {
			rr, ok, err := j.right.Next()
			if err != nil {
				return Row{}, false, err
			}
			if !ok {
				break
			}
			rk := j.rightKey(rr)
			if rk.IsNull() {
				continue
			}
			for _, i := range j.bucket[keyString(rk)] {
				j.leftMatched[i] = true
			}
		}
		j.rightDone = true
	}
	want := j.kind == sql.JoinSemi
	for j.ui < len(j.leftRows) {
		i := j.ui
		j.ui++
		if j.leftMatched[i] == want {
			return j.leftRows[i], true, nil
		}
	}
	return Row{}, false, nil
}

func mergeRows(l, r Row) Row {
	vals := make([]Value, 0, len(l.Values)+len(r.Values))
	vals = append(vals, l.Values...)
	vals = append(vals, r.Values...)
	return Row{Values: vals}
}

// nullRow returns a row of width NULL values, used to pad the absent side of an
// outer-join output row.
func nullRow(width int) Row {
	vals := make([]Value, width)
	for i := range vals {
		vals[i] = Null()
	}
	return Row{Values: vals}
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
	Func     string // COUNT, SUM, AVG, MIN, MAX, MEDIAN, STDDEV*, VAR*, STRING_AGG
	Arg      sql.Expr
	Arg2     sql.Expr // second argument, e.g. the STRING_AGG delimiter
	Name     string
	Distinct bool // COUNT(DISTINCT x) etc.: dedupe arg values before aggregating
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
	// Non-nil so Next does not re-collect when the input is empty.
	a.groups = []*aggGroup{}
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
	// A global aggregate (no GROUP BY) over an empty input still yields one row:
	// COUNT(*) = 0, SUM/MIN/MAX/AVG = NULL.
	if len(a.groups) == 0 && len(a.keys) == 0 {
		a.groups = append(a.groups, &aggGroup{})
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

// computeRegr computes the two-argument statistical aggregates (correlation,
// covariance, linear regression) over paired (y, x) values from spec.Arg (y) and
// spec.Arg2 (x), skipping rows where either is NULL. Argument order follows SQL:
// the dependent variable y is first, e.g. CORR(y, x) / REGR_SLOPE(y, x).
func computeRegr(name string, spec AggSpec, rows []Row, eval Evaluator) (Value, error) {
	if spec.Arg2 == nil {
		return Value{}, fmt.Errorf("%s expects 2 args, e.g. %s(y, x)", name, name)
	}
	var n int
	var sx, sy, sxx, syy, sxy float64
	for _, r := range rows {
		yv, err := eval.Eval(spec.Arg, r)
		if err != nil {
			return Value{}, err
		}
		xv, err := eval.Eval(spec.Arg2, r)
		if err != nil {
			return Value{}, err
		}
		if yv.IsNull() || xv.IsNull() {
			continue
		}
		y, ok1 := yv.AsFloat()
		x, ok2 := xv.AsFloat()
		if !ok1 || !ok2 {
			return Value{}, fmt.Errorf("%s requires numeric args", name)
		}
		n++
		sx, sy, sxx, syy, sxy = sx+x, sy+y, sxx+x*x, syy+y*y, sxy+x*y
	}
	if name == "REGR_COUNT" {
		return IntVal(int64(n)), nil
	}
	if n == 0 {
		return Null(), nil
	}
	fn := float64(n)
	mx, my := sx/fn, sy/fn
	// Centered sums of squares / cross-products: Σ(x-x̄)², Σ(y-ȳ)², Σ(x-x̄)(y-ȳ).
	cxx := sxx - sx*sx/fn
	cyy := syy - sy*sy/fn
	cxy := sxy - sx*sy/fn
	switch name {
	case "REGR_AVGX":
		return FloatVal(mx), nil
	case "REGR_AVGY":
		return FloatVal(my), nil
	case "COVAR_POP":
		return FloatVal(cxy / fn), nil
	case "COVAR_SAMP":
		if n < 2 {
			return Null(), nil
		}
		return FloatVal(cxy / (fn - 1)), nil
	case "REGR_SLOPE":
		if cxx == 0 {
			return Null(), nil
		}
		return FloatVal(cxy / cxx), nil
	case "REGR_INTERCEPT":
		if cxx == 0 {
			return Null(), nil
		}
		return FloatVal(my - (cxy/cxx)*mx), nil
	case "CORR":
		if cxx == 0 || cyy == 0 {
			return Null(), nil
		}
		return FloatVal(cxy / math.Sqrt(cxx*cyy)), nil
	case "REGR_R2":
		if cxx == 0 {
			return Null(), nil
		}
		if cyy == 0 {
			return FloatVal(1), nil // x varies but y is constant
		}
		r := cxy / math.Sqrt(cxx*cyy)
		return FloatVal(r * r), nil
	}
	return Value{}, fmt.Errorf("unknown aggregate %s", name)
}

func computeAgg(spec AggSpec, rows []Row, eval Evaluator) (Value, error) {
	name := strings.ToUpper(spec.Func)
	// COUNT(*) counts rows regardless of nullness; DISTINCT is meaningless on it.
	if name == "COUNT" {
		if cr, ok := spec.Arg.(*sql.ColRef); ok && cr.Name == "*" && cr.Qualifier == "" {
			return IntVal(int64(len(rows))), nil
		}
	}

	// Two-argument statistical aggregates pair both columns per row.
	switch name {
	case "CORR", "COVAR_POP", "COVAR_SAMP", "REGR_SLOPE", "REGR_INTERCEPT",
		"REGR_R2", "REGR_COUNT", "REGR_AVGX", "REGR_AVGY":
		return computeRegr(name, spec, rows, eval)
	}

	// Evaluate the argument over each row, dropping NULLs and (with DISTINCT)
	// duplicates. The value-based aggregates below operate on this list.
	var vals []Value
	var seen map[string]bool
	if spec.Distinct {
		seen = make(map[string]bool)
	}
	for _, r := range rows {
		v, err := eval.Eval(spec.Arg, r)
		if err != nil {
			return Value{}, err
		}
		if v.IsNull() {
			continue
		}
		if seen != nil {
			k := keyString(v)
			if seen[k] {
				continue
			}
			seen[k] = true
		}
		vals = append(vals, v)
	}

	switch name {
	case "COUNT":
		return IntVal(int64(len(vals))), nil
	case "MIN", "MAX":
		if len(vals) == 0 {
			return Null(), nil
		}
		best := vals[0]
		for _, v := range vals[1:] {
			c := Compare(v, best)
			if (name == "MIN" && c < 0) || (name == "MAX" && c > 0) {
				best = v
			}
		}
		return best, nil
	case "STRING_AGG":
		if len(vals) == 0 {
			return Null(), nil
		}
		sep, err := aggSeparator(spec, rows, eval)
		if err != nil {
			return Value{}, err
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = v.AsString()
		}
		return StringVal(strings.Join(parts, sep)), nil
	}

	// The remaining aggregates are numeric.
	nums := make([]float64, len(vals))
	for i, v := range vals {
		f, ok := v.AsFloat()
		if !ok {
			return Value{}, fmt.Errorf("%s requires numeric arg", name)
		}
		nums[i] = f
	}
	switch name {
	case "SUM":
		if len(nums) == 0 {
			return Null(), nil
		}
		var s float64
		for _, f := range nums {
			s += f
		}
		return FloatVal(s), nil
	case "AVG":
		if len(nums) == 0 {
			return Null(), nil
		}
		return FloatVal(mean(nums)), nil
	case "MEDIAN":
		if len(nums) == 0 {
			return Null(), nil
		}
		s := append([]float64(nil), nums...)
		sort.Float64s(s)
		n := len(s)
		if n%2 == 1 {
			return FloatVal(s[n/2]), nil
		}
		return FloatVal((s[n/2-1] + s[n/2]) / 2), nil
	case "VARIANCE", "VAR_SAMP", "VAR_POP", "STDDEV", "STDDEV_SAMP", "STDDEV_POP":
		pop := name == "VAR_POP" || name == "STDDEV_POP"
		v, ok := variance(nums, pop)
		if !ok {
			return Null(), nil
		}
		if strings.HasPrefix(name, "STDDEV") {
			return FloatVal(math.Sqrt(v)), nil
		}
		return FloatVal(v), nil
	case "PERCENTILE_CONT", "QUANTILE", "PERCENTILE_DISC":
		if len(nums) == 0 {
			return Null(), nil
		}
		p, err := aggPercentile(spec, rows, eval)
		if err != nil {
			return Value{}, err
		}
		s := append([]float64(nil), nums...)
		sort.Float64s(s)
		n := len(s)
		if name == "PERCENTILE_DISC" {
			// Smallest value whose cumulative distribution >= p.
			idx := int(math.Ceil(p*float64(n))) - 1
			if idx < 0 {
				idx = 0
			}
			return FloatVal(s[idx]), nil
		}
		// PERCENTILE_CONT / QUANTILE: linear interpolation between neighbours.
		rank := p * float64(n-1)
		lo := int(math.Floor(rank))
		hi := int(math.Ceil(rank))
		if lo == hi {
			return FloatVal(s[lo]), nil
		}
		return FloatVal(s[lo] + (s[hi]-s[lo])*(rank-float64(lo))), nil
	}
	return Value{}, fmt.Errorf("unknown aggregate %s", name)
}

// aggPercentile evaluates the percentile fraction (second argument) of a
// PERCENTILE_*/QUANTILE aggregate, clamped to [0,1]. It is normally a constant.
func aggPercentile(spec AggSpec, rows []Row, eval Evaluator) (float64, error) {
	if spec.Arg2 == nil {
		return 0, fmt.Errorf("%s requires a percentile in [0,1], e.g. %s(x, 0.95)", spec.Func, spec.Func)
	}
	probe := Row{}
	if len(rows) > 0 {
		probe = rows[0]
	}
	pv, err := eval.Eval(spec.Arg2, probe)
	if err != nil {
		return 0, err
	}
	p, ok := pv.AsFloat()
	if !ok {
		return 0, fmt.Errorf("%s percentile must be numeric", spec.Func)
	}
	if p < 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	return p, nil
}

// aggSeparator evaluates a STRING_AGG delimiter (its second argument), which is
// normally a constant. It defaults to "" when absent.
func aggSeparator(spec AggSpec, rows []Row, eval Evaluator) (string, error) {
	if spec.Arg2 == nil {
		return "", nil
	}
	probe := Row{}
	if len(rows) > 0 {
		probe = rows[0]
	}
	sv, err := eval.Eval(spec.Arg2, probe)
	if err != nil {
		return "", err
	}
	if sv.IsNull() {
		return "", nil
	}
	return sv.AsString(), nil
}

func mean(nums []float64) float64 {
	var s float64
	for _, f := range nums {
		s += f
	}
	return s / float64(len(nums))
}

// variance returns the population or sample variance. ok=false (→ NULL) when
// there are too few values: none for either, or a single value for the sample
// form (its n-1 denominator is zero), matching SQL's *_samp semantics.
func variance(nums []float64, pop bool) (float64, bool) {
	n := len(nums)
	if n == 0 || (!pop && n < 2) {
		return 0, false
	}
	m := mean(nums)
	var ss float64
	for _, f := range nums {
		d := f - m
		ss += d * d
	}
	denom := float64(n)
	if !pop {
		denom = float64(n - 1)
	}
	return ss / denom, true
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

// ---- Intersect / Except -----------------------------------------------------

// IntersectIter yields rows present in both inputs (INTERSECT). With all=false it
// yields each common row once; with all=true a row's multiplicity is the minimum
// of its counts in the two inputs. The right side is consumed up front into a
// count map; the left side then streams. NULLs compare equal (set-op semantics).
type IntersectIter struct {
	left, right RowIterator
	all         bool
	rightCount  map[string]int
	emitted     map[string]bool // distinct mode: rows already yielded
	built       bool
	closed      bool
}

// NewIntersectIter returns an INTERSECT (all=false) or INTERSECT ALL iterator.
func NewIntersectIter(left, right RowIterator, all bool) *IntersectIter {
	return &IntersectIter{left: left, right: right, all: all}
}

func (it *IntersectIter) build() error {
	it.rightCount = map[string]int{}
	for {
		r, ok, err := it.right.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		it.rightCount[rowKey(r)]++
	}
	if !it.all {
		it.emitted = map[string]bool{}
	}
	it.built = true
	return nil
}

func (it *IntersectIter) Next() (Row, bool, error) {
	if !it.built {
		if err := it.build(); err != nil {
			return Row{}, false, err
		}
	}
	for {
		r, ok, err := it.left.Next()
		if err != nil || !ok {
			return Row{}, ok, err
		}
		k := rowKey(r)
		if it.all {
			if it.rightCount[k] > 0 {
				it.rightCount[k]--
				return r, true, nil
			}
			continue
		}
		if it.rightCount[k] > 0 && !it.emitted[k] {
			it.emitted[k] = true
			return r, true, nil
		}
	}
}

func (it *IntersectIter) Close() error {
	if it.closed {
		return nil
	}
	it.closed = true
	err1 := it.left.Close()
	err2 := it.right.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// ExceptIter yields rows of the left input not "cancelled" by the right (EXCEPT).
// With all=false it yields each left row once when its key is absent from the
// right; with all=true a row's multiplicity is max(0, leftCount - rightCount).
// The right side is consumed up front; NULLs compare equal (set-op semantics).
type ExceptIter struct {
	left, right RowIterator
	all         bool
	rightCount  map[string]int
	emitted     map[string]bool // distinct mode: rows already yielded
	built       bool
	closed      bool
}

// NewExceptIter returns an EXCEPT (all=false) or EXCEPT ALL iterator.
func NewExceptIter(left, right RowIterator, all bool) *ExceptIter {
	return &ExceptIter{left: left, right: right, all: all}
}

func (it *ExceptIter) build() error {
	it.rightCount = map[string]int{}
	for {
		r, ok, err := it.right.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		it.rightCount[rowKey(r)]++
	}
	if !it.all {
		it.emitted = map[string]bool{}
	}
	it.built = true
	return nil
}

func (it *ExceptIter) Next() (Row, bool, error) {
	if !it.built {
		if err := it.build(); err != nil {
			return Row{}, false, err
		}
	}
	for {
		r, ok, err := it.left.Next()
		if err != nil || !ok {
			return Row{}, ok, err
		}
		k := rowKey(r)
		if it.all {
			if it.rightCount[k] > 0 {
				it.rightCount[k]-- // cancelled by a right-side copy
				continue
			}
			return r, true, nil
		}
		if it.rightCount[k] == 0 && !it.emitted[k] {
			it.emitted[k] = true
			return r, true, nil
		}
	}
}

func (it *ExceptIter) Close() error {
	if it.closed {
		return nil
	}
	it.closed = true
	err1 := it.left.Close()
	err2 := it.right.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// ---- Window -----------------------------------------------------------------

// WindowSpec describes one window-function output column. Args are evaluated
// against the input rows; PartitionBy/OrderBy define the window. There is no
// explicit frame: aggregates use the whole partition when OrderBy is empty, and
// a running RANGE frame (cumulative through each peer group) when it is present.
type WindowSpec struct {
	Name        string
	Func        string
	Args        []sql.Expr
	PartitionBy []sql.Expr
	OrderBy     []sql.OrderTerm
	Frame       *sql.WindowFrame // explicit ROWS frame; nil = default
}

// WindowIter computes window-function columns and appends them to each child
// row, preserving the child's row order. All rows are materialized up front.
type WindowIter struct {
	child  RowIterator
	specs  []WindowSpec
	eval   Evaluator
	rows   []Row
	pos    int
	built  bool
	closed bool
}

// NewWindowIter returns an iterator that appends the window columns to its rows.
func NewWindowIter(child RowIterator, specs []WindowSpec, eval Evaluator) *WindowIter {
	return &WindowIter{child: child, specs: specs, eval: eval}
}

func (w *WindowIter) build() error {
	for {
		r, ok, err := w.child.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		w.rows = append(w.rows, r)
	}
	cols := make([][]Value, len(w.specs))
	for si, spec := range w.specs {
		vals, err := computeWindow(spec, w.rows, w.eval)
		if err != nil {
			return err
		}
		cols[si] = vals
	}
	for i := range w.rows {
		for si := range w.specs {
			w.rows[i].Values = append(w.rows[i].Values, cols[si][i])
		}
	}
	w.built = true
	return nil
}

func (w *WindowIter) Next() (Row, bool, error) {
	if !w.built {
		if err := w.build(); err != nil {
			return Row{}, false, err
		}
	}
	if w.pos >= len(w.rows) {
		return Row{}, false, nil
	}
	r := w.rows[w.pos]
	w.pos++
	return r, true, nil
}

func (w *WindowIter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	return w.child.Close()
}

// computeWindow returns one value per input row (indexed by original position)
// for a single window spec.
func computeWindow(spec WindowSpec, rows []Row, eval Evaluator) ([]Value, error) {
	result := make([]Value, len(rows))
	partitions := map[string][]int{}
	var order []string
	for i, r := range rows {
		k, err := windowPartitionKey(spec.PartitionBy, r, eval)
		if err != nil {
			return nil, err
		}
		if _, ok := partitions[k]; !ok {
			order = append(order, k)
		}
		partitions[k] = append(partitions[k], i)
	}
	for _, k := range order {
		idxs := partitions[k]
		if len(spec.OrderBy) > 0 {
			if err := sortPartition(idxs, rows, spec.OrderBy, eval); err != nil {
				return nil, err
			}
		}
		if err := computeWindowPartition(spec, idxs, rows, eval, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func windowPartitionKey(exprs []sql.Expr, r Row, eval Evaluator) (string, error) {
	var b strings.Builder
	for _, e := range exprs {
		v, err := eval.Eval(e, r)
		if err != nil {
			return "", err
		}
		b.WriteString(keyString(v))
		b.WriteString("\x01")
	}
	return b.String(), nil
}

// sortPartition stably sorts the partition's row indices by the window ORDER BY.
func sortPartition(idxs []int, rows []Row, terms []sql.OrderTerm, eval Evaluator) error {
	var sortErr error
	sort.SliceStable(idxs, func(a, b int) bool {
		for _, t := range terms {
			va, err := eval.Eval(t.Expr, rows[idxs[a]])
			if err != nil {
				sortErr = err
				return false
			}
			vb, err := eval.Eval(t.Expr, rows[idxs[b]])
			if err != nil {
				sortErr = err
				return false
			}
			c := Compare(va, vb)
			if c == 0 {
				continue
			}
			if t.Desc {
				return c > 0
			}
			return c < 0
		}
		return false
	})
	return sortErr
}

// windowPeers reports whether two rows tie on every window ORDER BY term.
func windowPeers(terms []sql.OrderTerm, r1, r2 Row, eval Evaluator) (bool, error) {
	for _, t := range terms {
		v1, err := eval.Eval(t.Expr, r1)
		if err != nil {
			return false, err
		}
		v2, err := eval.Eval(t.Expr, r2)
		if err != nil {
			return false, err
		}
		if Compare(v1, v2) != 0 {
			return false, nil
		}
	}
	return true, nil
}

func computeWindowPartition(spec WindowSpec, idxs []int, rows []Row, eval Evaluator, result []Value) error {
	switch strings.ToUpper(spec.Func) {
	case "ROW_NUMBER":
		for pos, ri := range idxs {
			result[ri] = IntVal(int64(pos + 1))
		}
		return nil
	case "RANK", "DENSE_RANK":
		dense, rank := 0, 0
		for pos, ri := range idxs {
			newGroup := pos == 0
			if !newGroup {
				eq, err := windowPeers(spec.OrderBy, rows[idxs[pos-1]], rows[ri], eval)
				if err != nil {
					return err
				}
				newGroup = !eq
			}
			if newGroup {
				dense++
				rank = pos + 1
			}
			if strings.EqualFold(spec.Func, "RANK") {
				result[ri] = IntVal(int64(rank))
			} else {
				result[ri] = IntVal(int64(dense))
			}
		}
		return nil
	case "LAG", "LEAD":
		return computeWindowOffset(spec, idxs, rows, eval, result, strings.EqualFold(spec.Func, "LEAD"))
	case "NTILE":
		return computeNtile(spec, idxs, rows, eval, result)
	case "PERCENT_RANK", "CUME_DIST":
		return computeDistRank(spec, idxs, rows, eval, result, strings.ToUpper(spec.Func))
	case "SUM", "AVG", "COUNT", "MIN", "MAX":
		return computeWindowAgg(spec, idxs, rows, eval, result)
	}
	return fmt.Errorf("unknown window function %s", spec.Func)
}

// computeNtile assigns each partition row to one of n buckets (1..n) as evenly as
// possible: the first (rowcount mod n) buckets take one extra row.
func computeNtile(spec WindowSpec, idxs []int, rows []Row, eval Evaluator, result []Value) error {
	if len(spec.Args) == 0 {
		return fmt.Errorf("NTILE requires a bucket count, e.g. NTILE(4)")
	}
	nv, err := eval.Eval(spec.Args[0], rows[idxs[0]])
	if err != nil {
		return err
	}
	bk, ok := nv.AsInt()
	if !ok || bk < 1 {
		return fmt.Errorf("NTILE bucket count must be a positive integer")
	}
	total, n := len(idxs), int(bk)
	base, extra := total/n, total%n
	pos := 0
	for b := 1; b <= n && pos < total; b++ {
		size := base
		if b <= extra {
			size++
		}
		for k := 0; k < size; k++ {
			result[idxs[pos]] = IntVal(int64(b))
			pos++
		}
	}
	return nil
}

// computeDistRank implements PERCENT_RANK = (rank-1)/(rows-1) and
// CUME_DIST = (rows through the current peer group)/rows, both peer-aware.
func computeDistRank(spec WindowSpec, idxs []int, rows []Row, eval Evaluator, result []Value, name string) error {
	total := len(idxs)
	for i := 0; i < total; {
		j := i + 1
		for j < total {
			eq, err := windowPeers(spec.OrderBy, rows[idxs[j-1]], rows[idxs[j]], eval)
			if err != nil {
				return err
			}
			if !eq {
				break
			}
			j++
		}
		var pr float64
		if total > 1 {
			pr = float64(i) / float64(total-1) // rank-1 == i (0-based start of group)
		}
		cd := float64(j) / float64(total)
		for k := i; k < j; k++ {
			if name == "PERCENT_RANK" {
				result[idxs[k]] = FloatVal(pr)
			} else {
				result[idxs[k]] = FloatVal(cd)
			}
		}
		i = j
	}
	return nil
}

// computeWindowOffset implements LAG/LEAD: the argument value of the row `offset`
// positions back (LAG) or forward (LEAD) in the window order, else the default
// (3rd arg) or NULL.
func computeWindowOffset(spec WindowSpec, idxs []int, rows []Row, eval Evaluator, result []Value, lead bool) error {
	if len(spec.Args) == 0 {
		return fmt.Errorf("%s requires an argument", spec.Func)
	}
	offset := 1
	if len(spec.Args) >= 2 {
		v, err := eval.Eval(spec.Args[1], rows[idxs[0]])
		if err != nil {
			return err
		}
		if n, ok := v.AsInt(); ok {
			offset = int(n)
		}
	}
	for pos, ri := range idxs {
		src := pos - offset
		if lead {
			src = pos + offset
		}
		switch {
		case src >= 0 && src < len(idxs):
			v, err := eval.Eval(spec.Args[0], rows[idxs[src]])
			if err != nil {
				return err
			}
			result[ri] = v
		case len(spec.Args) >= 3:
			v, err := eval.Eval(spec.Args[2], rows[ri])
			if err != nil {
				return err
			}
			result[ri] = v
		default:
			result[ri] = Null()
		}
	}
	return nil
}

// computeWindowAgg implements SUM/AVG/COUNT/MIN/MAX as window functions: the
// whole partition when there is no ORDER BY, else a running RANGE frame whose
// value advances at each peer-group boundary.
func computeWindowAgg(spec WindowSpec, idxs []int, rows []Row, eval Evaluator, result []Value) error {
	star := false
	if len(spec.Args) == 1 {
		if cr, ok := spec.Args[0].(*sql.ColRef); ok && cr.Name == "*" {
			star = true
		}
	}
	add := func(acc *winAcc, ri int) error {
		if star {
			acc.count++
			return nil
		}
		v, err := eval.Eval(spec.Args[0], rows[ri])
		if err != nil {
			return err
		}
		return acc.add(v)
	}

	// An explicit frame: recompute the aggregate over the per-row window. ROWS
	// uses physical offsets (moving averages / rolling sums); RANGE uses the
	// ORDER BY value (so peers share a frame, and offsets are value windows).
	if spec.Frame != nil {
		if strings.EqualFold(spec.Frame.Unit, "RANGE") {
			return computeWindowRange(spec, idxs, rows, eval, result, add)
		}
		startOff, err := frameOffsetInt(spec.Frame.Start, eval)
		if err != nil {
			return err
		}
		endOff, err := frameOffsetInt(spec.Frame.End, eval)
		if err != nil {
			return err
		}
		n := len(idxs)
		for pos := 0; pos < n; pos++ {
			lo := frameBoundIndex(spec.Frame.Start.Kind, startOff, pos, n)
			hi := frameBoundIndex(spec.Frame.End.Kind, endOff, pos, n)
			if lo < 0 {
				lo = 0
			}
			if hi > n-1 {
				hi = n - 1
			}
			acc := winAcc{fn: strings.ToUpper(spec.Func)}
			for j := lo; j <= hi; j++ { // lo > hi (empty frame) just skips
				if err := add(&acc, idxs[j]); err != nil {
					return err
				}
			}
			result[idxs[pos]] = acc.result()
		}
		return nil
	}

	if len(spec.OrderBy) == 0 {
		acc := winAcc{fn: strings.ToUpper(spec.Func)}
		for _, ri := range idxs {
			if err := add(&acc, ri); err != nil {
				return err
			}
		}
		v := acc.result()
		for _, ri := range idxs {
			result[ri] = v
		}
		return nil
	}

	acc := winAcc{fn: strings.ToUpper(spec.Func)}
	groupStart := 0
	for pos := 0; pos < len(idxs); pos++ {
		if err := add(&acc, idxs[pos]); err != nil {
			return err
		}
		end := pos == len(idxs)-1
		if !end {
			eq, err := windowPeers(spec.OrderBy, rows[idxs[pos]], rows[idxs[pos+1]], eval)
			if err != nil {
				return err
			}
			end = !eq
		}
		if end {
			v := acc.result()
			for j := groupStart; j <= pos; j++ {
				result[idxs[j]] = v
			}
			groupStart = pos + 1
		}
	}
	return nil
}

// frameBoundIndex resolves a ROWS frame bound to a (possibly out-of-range)
// row index within an n-row partition, for the row at ordinal `pos`.
func frameBoundIndex(kind string, offset, pos, n int) int {
	switch kind {
	case "UNBOUNDED_PRECEDING":
		return 0
	case "UNBOUNDED_FOLLOWING":
		return n - 1
	case "PRECEDING":
		return pos - offset
	case "FOLLOWING":
		return pos + offset
	default: // CURRENT_ROW
		return pos
	}
}

// frameOffsetInt evaluates a ROWS frame bound's (constant) integer row offset.
func frameOffsetInt(b sql.FrameBound, eval Evaluator) (int, error) {
	if b.Offset == nil || (b.Kind != "PRECEDING" && b.Kind != "FOLLOWING") {
		return 0, nil
	}
	v, err := eval.Eval(b.Offset, Row{})
	if err != nil {
		return 0, err
	}
	n, ok := v.AsInt()
	if !ok || n < 0 {
		return 0, fmt.Errorf("ROWS frame offset must be a non-negative integer")
	}
	return int(n), nil
}

// computeWindowRange evaluates a RANGE frame: the frame for a row is defined by
// the ORDER BY *value*, not row position. Peers (equal values) share a frame,
// and PRECEDING/FOLLOWING offsets select a value window (cur ± n). It requires a
// single ORDER BY column; offset bounds additionally require it to be numeric.
func computeWindowRange(spec WindowSpec, idxs []int, rows []Row, eval Evaluator, result []Value, add func(*winAcc, int) error) error {
	if len(spec.OrderBy) != 1 {
		return fmt.Errorf("RANGE frame requires exactly one ORDER BY column")
	}
	desc := spec.OrderBy[0].Desc
	n := len(idxs)
	ovals := make([]Value, n) // the order-by value at each ordered position
	for pos, ri := range idxs {
		v, err := eval.Eval(spec.OrderBy[0].Expr, rows[ri])
		if err != nil {
			return err
		}
		ovals[pos] = v
	}
	for pos := 0; pos < n; pos++ {
		lo, hi, err := rangeFrameBounds(spec.Frame, ovals, pos, desc, eval)
		if err != nil {
			return err
		}
		acc := winAcc{fn: strings.ToUpper(spec.Func)}
		for j := lo; j <= hi; j++ {
			if err := add(&acc, idxs[j]); err != nil {
				return err
			}
		}
		result[idxs[pos]] = acc.result()
	}
	return nil
}

// rangeFrameBounds resolves the [lo, hi] ordered-position range covered by a
// RANGE frame for the row at `pos`, given the ordered values and sort direction.
func rangeFrameBounds(frame *sql.WindowFrame, ovals []Value, pos int, desc bool, eval Evaluator) (int, int, error) {
	n := len(ovals)
	cur := ovals[pos]
	lo := 0
	if frame.Start.Kind != "UNBOUNDED_PRECEDING" {
		target, err := rangeTarget(frame.Start, cur, desc, eval)
		if err != nil {
			return 0, 0, err
		}
		for lo < n && !rangeAtOrAfter(ovals[lo], target, desc) {
			lo++
		}
	}
	hi := n - 1
	if frame.End.Kind != "UNBOUNDED_FOLLOWING" {
		target, err := rangeTarget(frame.End, cur, desc, eval)
		if err != nil {
			return 0, 0, err
		}
		for hi >= 0 && !rangeAtOrBefore(ovals[hi], target, desc) {
			hi--
		}
	}
	return lo, hi, nil
}

// rangeTarget computes the value edge a RANGE bound refers to: the current value
// for CURRENT ROW, or cur ± offset for PRECEDING/FOLLOWING (sign flipped for
// DESC). The offset is numeric for a numeric ORDER BY column, or an INTERVAL
// duration for a timestamp column.
func rangeTarget(b sql.FrameBound, cur Value, desc bool, eval Evaluator) (Value, error) {
	if b.Kind == "CURRENT_ROW" {
		return cur, nil
	}
	off, err := eval.Eval(b.Offset, Row{})
	if err != nil {
		return Value{}, err
	}
	add := b.Kind == "FOLLOWING"
	if desc {
		add = !add
	}
	// Timestamp ORDER BY column: the offset must be an INTERVAL duration.
	if ct, ok := cur.V.(time.Time); ok {
		d, ok := off.V.(time.Duration)
		if !ok {
			return Value{}, fmt.Errorf("RANGE over a timestamp needs an INTERVAL offset")
		}
		if !add {
			d = -d
		}
		return TimeVal(ct.Add(d)), nil
	}
	// Numeric ORDER BY column.
	f, ok1 := cur.AsFloat()
	of, ok2 := off.AsFloat()
	if !ok1 || !ok2 {
		return Value{}, fmt.Errorf("RANGE offset must be numeric (or INTERVAL for a timestamp)")
	}
	if add {
		return FloatVal(f + of), nil
	}
	return FloatVal(f - of), nil
}

// rangeAtOrAfter / rangeAtOrBefore test a value against a frame edge in sort
// order (for DESC the comparisons invert, since values descend along the rows).
func rangeAtOrAfter(v, target Value, desc bool) bool {
	if desc {
		return Compare(v, target) <= 0
	}
	return Compare(v, target) >= 0
}

func rangeAtOrBefore(v, target Value, desc bool) bool {
	if desc {
		return Compare(v, target) >= 0
	}
	return Compare(v, target) <= 0
}

// winAcc accumulates a window aggregate. NULLs are skipped (except COUNT(*),
// which the caller counts directly into count).
type winAcc struct {
	fn         string
	sum        float64
	count      int64
	have       bool
	minV, maxV Value
}

func (a *winAcc) add(v Value) error {
	if v.IsNull() {
		return nil
	}
	a.count++
	switch a.fn {
	case "SUM", "AVG":
		f, ok := v.AsFloat()
		if !ok {
			return fmt.Errorf("%s requires numeric arg", a.fn)
		}
		a.sum += f
	case "MIN":
		if !a.have || Compare(v, a.minV) < 0 {
			a.minV = v
		}
	case "MAX":
		if !a.have || Compare(v, a.maxV) > 0 {
			a.maxV = v
		}
	}
	a.have = true
	return nil
}

func (a *winAcc) result() Value {
	switch a.fn {
	case "COUNT":
		return IntVal(a.count)
	case "SUM":
		if a.count == 0 {
			return Null()
		}
		return FloatVal(a.sum)
	case "AVG":
		if a.count == 0 {
			return Null()
		}
		return FloatVal(a.sum / float64(a.count))
	case "MIN":
		if !a.have {
			return Null()
		}
		return a.minV
	case "MAX":
		if !a.have {
			return Null()
		}
		return a.maxV
	}
	return Null()
}