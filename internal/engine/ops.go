package engine

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/april/turntable/internal/sql"
)

// ---- Filter -----------------------------------------------------------------

// FilterIter applies a predicate to rows from its child.
type FilterIter struct {
	child  RowIterator
	eval   Evaluator
	pred   sql.Expr
	closed bool
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

// ProjectedExpr is one output column of a Project: the expression, the output
// column name (alias or inferred), and the inferred result type. A zero Type
// (TypeInvalid) means "unknown" and is rendered as TypeAny.
type ProjectedExpr struct {
	Expr sql.Expr
	Name string
	Type Type
}

// ProjectIter evaluates the select list against child rows, producing rows
// conforming to outSchema.
type ProjectIter struct {
	child  RowIterator
	out    []ProjectedExpr
	eval   Evaluator
	closed bool
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
	child   RowIterator
	limit   *int
	offset  int
	skipped int
	yielded int
	closed  bool
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

// HashJoinIter implements an in-memory join supporting INNER, LEFT, RIGHT, and
// FULL kinds. The left side is materialized as the build side; the right side is
// streamed as the probe side. The ON condition is split by the planner into zero
// or more equi-key pairs (leftKeys/rightKeys, hashed into a composite bucket key)
// plus an optional residual predicate evaluated on each candidate pair. With at
// least one key pair this is a hash join; with none it degenerates to a nested
// loop (every probe row pairs with every build row), the residual deciding each
// match. Output rows are always [left..., right...] with the unmatched side
// NULL-padded.
type HashJoinIter struct {
	left, right         RowIterator
	leftKeys, rightKeys []KeyExtractor
	residual            sql.Expr  // extra ON conditions, evaluated per candidate pair (nil if none)
	eval                Evaluator // over the combined [left..., right...] schema, for residual

	kind                  sql.JoinKind
	leftWidth, rightWidth int // column counts, for NULL padding the missing side

	leftRows []Row
	// leftMatched is parallel to leftRows, set as probe rows match; bucket maps a
	// composite join key to indices into leftRows.
	leftMatched []bool
	bucket      map[string][]int
	built       bool

	pending []Row // join rows produced for the current probe row
	pendi   int

	rightDone bool
	ui        int // scan cursor for emitting unmatched left rows (LEFT/FULL)

	closed bool
}

// NewHashJoinIter builds a join. leftKeys/rightKeys extract the composite join
// key from each side's rows (empty → nested loop); residual (with eval over the
// combined schema) is the non-equi remainder of ON; leftWidth/rightWidth give the
// column counts used to NULL-pad the missing side for outer joins.
func NewHashJoinIter(
	left, right RowIterator,
	leftKeys, rightKeys []KeyExtractor,
	residual sql.Expr,
	eval Evaluator,
	kind sql.JoinKind,
	leftWidth, rightWidth int,
) *HashJoinIter {
	return &HashJoinIter{
		left: left, right: right,
		leftKeys: leftKeys, rightKeys: rightKeys,
		residual: residual, eval: eval,
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
		k := compositeKey(j.leftKeys, r)
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

// compositeKey joins each extractor's key into one bucket key. With no
// extractors it returns a constant, so every row lands in a single bucket and
// the join becomes a nested loop: each probe row pairs with every build row,
// filtered by the residual.
func compositeKey(keys []KeyExtractor, row Row) string {
	switch len(keys) {
	case 0:
		return "" // single bucket: nested loop
	case 1:
		return keyString(keys[0](row))
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = keyString(k(row))
	}
	return strings.Join(parts, "\x00|\x00")
}

// anyNullKey reports whether any composite-key component is NULL (a NULL key
// never matches, per SQL EXISTS / equi-join semantics).
func anyNullKey(keys []KeyExtractor, row Row) bool {
	for _, k := range keys {
		if k(row).IsNull() {
			return true
		}
	}
	return false
}

// rowMatches reports whether a merged candidate row satisfies the residual ON
// predicate (true when there is no residual). A NULL or non-true result is no
// match, mirroring Filter semantics.
func (j *HashJoinIter) rowMatches(merged Row) (bool, error) {
	if j.residual == nil {
		return true, nil
	}
	v, err := j.eval.Eval(j.residual, merged)
	if err != nil {
		return false, err
	}
	if v.IsNull() {
		return false, nil
	}
	b, ok := v.AsBool()
	return ok && b, nil
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
			j.pending = j.pending[:0]
			j.pendi = 0
			for _, i := range j.bucket[compositeKey(j.rightKeys, rr)] {
				merged := mergeRows(j.leftRows[i], rr)
				m, err := j.rowMatches(merged)
				if err != nil {
					return Row{}, false, err
				}
				if m {
					j.leftMatched[i] = true
					j.pending = append(j.pending, merged)
				}
			}
			// No build row matched (empty bucket, or all failed the residual):
			// emit the right row NULL-padded on the left for RIGHT/FULL.
			if len(j.pending) == 0 && j.keepsUnmatchedRight() {
				j.pending = append(j.pending, mergeRows(nullRow(j.leftWidth), rr))
			}
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
			if anyNullKey(j.rightKeys, rr) {
				continue
			}
			for _, i := range j.bucket[compositeKey(j.rightKeys, rr)] {
				if j.residual != nil {
					m, err := j.rowMatches(mergeRows(j.leftRows[i], rr))
					if err != nil {
						return Row{}, false, err
					}
					if !m {
						continue
					}
				}
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

// ---- ASOF join ----------------------------------------------------------------

// AsofSpec describes the inequality of an ASOF join, normalized to
// `left OP right`: for each left row the join picks the single right partner
// whose key is nearest per Op (">=" / ">" pick the greatest right key at/below
// the left key; "<=" / "<" the smallest at/above).
type AsofSpec struct {
	LeftKey  KeyExtractor
	RightKey KeyExtractor
	Op       string // ">=", ">", "<=", "<"
}

type asofEntry struct {
	key Value
	row Row
}

// AsofJoinIter implements ASOF [LEFT] JOIN: the right side is materialized,
// grouped by the equality keys, and each group sorted by the ASOF key; the left
// side streams, each row matched (binary search) to at most one right row —
// the nearest per the spec. NULL equality or ASOF keys never match, mirroring
// the hash join. Output rows are [left..., right...], right NULL-padded for an
// unmatched left row under JoinLeft (inner drops it).
type AsofJoinIter struct {
	left, right         RowIterator
	leftKeys, rightKeys []KeyExtractor
	spec                AsofSpec
	kind                sql.JoinKind // JoinInner or JoinLeft
	rightWidth          int

	groups map[string][]asofEntry
	built  bool
	closed bool
}

// NewAsofJoinIter builds an ASOF join. leftKeys/rightKeys are the equality
// conjuncts (may be empty: one global group); spec is the normalized
// inequality; rightWidth NULL-pads unmatched left rows under JoinLeft.
func NewAsofJoinIter(
	left, right RowIterator,
	leftKeys, rightKeys []KeyExtractor,
	spec AsofSpec,
	kind sql.JoinKind,
	rightWidth int,
) *AsofJoinIter {
	return &AsofJoinIter{
		left: left, right: right,
		leftKeys: leftKeys, rightKeys: rightKeys,
		spec: spec, kind: kind, rightWidth: rightWidth,
	}
}

func (j *AsofJoinIter) build() error {
	j.groups = map[string][]asofEntry{}
	for {
		r, ok, err := j.right.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		if anyNullKey(j.rightKeys, r) {
			continue // NULL equality keys never match
		}
		k := j.spec.RightKey(r)
		if k.IsNull() {
			continue // a NULL ASOF key can never be nearest
		}
		gk := compositeKey(j.rightKeys, r)
		j.groups[gk] = append(j.groups[gk], asofEntry{key: k, row: r})
	}
	for _, g := range j.groups {
		sort.SliceStable(g, func(a, b int) bool { return Compare(g[a].key, g[b].key) < 0 })
	}
	j.built = true
	return nil
}

// match returns the index of the matching entry in g for left ASOF key lv, or
// -1. Entries are sorted ascending by key.
func (j *AsofJoinIter) match(g []asofEntry, lv Value) int {
	switch j.spec.Op {
	case ">=": // greatest right key <= lv
		i := sort.Search(len(g), func(i int) bool { return Compare(g[i].key, lv) > 0 })
		return i - 1
	case ">": // greatest right key < lv
		i := sort.Search(len(g), func(i int) bool { return Compare(g[i].key, lv) >= 0 })
		return i - 1
	case "<=": // smallest right key >= lv
		i := sort.Search(len(g), func(i int) bool { return Compare(g[i].key, lv) >= 0 })
		if i == len(g) {
			return -1
		}
		return i
	case "<": // smallest right key > lv
		i := sort.Search(len(g), func(i int) bool { return Compare(g[i].key, lv) > 0 })
		if i == len(g) {
			return -1
		}
		return i
	}
	return -1
}

func (j *AsofJoinIter) Next() (Row, bool, error) {
	if !j.built {
		if err := j.build(); err != nil {
			return Row{}, false, err
		}
	}
	for {
		l, ok, err := j.left.Next()
		if err != nil || !ok {
			return Row{}, false, err
		}
		mi := -1
		var g []asofEntry
		if !anyNullKey(j.leftKeys, l) {
			if lv := j.spec.LeftKey(l); !lv.IsNull() {
				g = j.groups[compositeKey(j.leftKeys, l)]
				mi = j.match(g, lv)
			}
		}
		if mi < 0 {
			if j.kind != sql.JoinLeft {
				continue // inner: an unmatched left row is dropped
			}
			out := make([]Value, 0, len(l.Values)+j.rightWidth)
			out = append(out, l.Values...)
			for i := 0; i < j.rightWidth; i++ {
				out = append(out, Null())
			}
			return Row{Values: out}, true, nil
		}
		r := g[mi].row
		out := make([]Value, 0, len(l.Values)+len(r.Values))
		out = append(out, l.Values...)
		out = append(out, r.Values...)
		return Row{Values: out}, true, nil
	}
}

func (j *AsofJoinIter) Close() error {
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
// the specified aggregates per group. Each group holds one streaming
// accumulator per aggregate (see aggacc.go), folding rows as they arrive rather
// than buffering them — so memory is O(number of groups) plus, for the holistic
// aggregates (MEDIAN/PERCENTILE/STDDEV/STRING_AGG/DISTINCT), the retained
// argument values, never the whole input. After grouping, an optional HAVING
// predicate (evaluated against the aggregate-output row) filters groups.
//
// Aggregation runs as a sequence of passes. The first pass reads the child; if
// the group budget (AggConfig.MaxGroups) is reached and spilling is enabled,
// rows for not-yet-seen groups are partitioned to disk (see aggspill.go) and
// each partition becomes a later pass. Groups already resident in a pass keep
// aggregating in memory, so no group is ever split across passes and the
// holistic aggregates still see every value of their group. With the default
// config (MaxGroups == 0) there is exactly one pass and no spilling.
//
// The output row layout is: [group key values..., aggregate values...].
type AggregateIter struct {
	child      RowIterator
	keys       []sql.Expr
	aggs       []AggSpec
	having     sql.Expr
	eval       Evaluator
	havingEval Evaluator // resolver over the aggregate output schema
	outSchema  Schema
	cfg        AggConfig

	childStarted bool                // whether the child pass has begun
	pending      []*spillPartition   // spilled partitions awaiting their pass
	spillDirs    map[string]struct{} // temp dirs to remove on Close
	groups       []*aggGroup         // current pass's groups being emitted
	gi           int
	closed       bool
}

type aggGroup struct {
	keyVals []Value
	accs    []aggAccumulator
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

// SetAggConfig applies the memory budget / spill configuration. Call before the
// first Next; the zero value (unlimited, no spill) is the default.
func (a *AggregateIter) SetAggConfig(cfg AggConfig) { a.cfg = cfg }

func (a *AggregateIter) Next() (Row, bool, error) {
	for {
		if a.groups == nil {
			src, depth, ok, err := a.nextSource()
			if err != nil {
				return Row{}, false, err
			}
			if !ok {
				return Row{}, false, nil
			}
			if err := a.runPass(src, depth); err != nil {
				return Row{}, false, err
			}
		}
		for a.gi < len(a.groups) {
			g := a.groups[a.gi]
			a.gi++
			out := make([]Value, 0, len(g.keyVals)+len(a.aggs))
			out = append(out, g.keyVals...)
			for _, acc := range g.accs {
				v, err := acc.Result()
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
		// This pass is exhausted; loop to fetch the next source (spill partition).
		a.groups = nil
	}
}

// nextSource returns the row source for the next aggregation pass: the child
// first, then each spilled partition in turn. depth varies the spill hash so a
// re-partitioned pass distributes keys differently.
func (a *AggregateIter) nextSource() (RowIterator, int, bool, error) {
	if !a.childStarted {
		a.childStarted = true
		return a.child, 0, true, nil
	}
	if len(a.pending) > 0 {
		p := a.pending[0]
		a.pending = a.pending[1:]
		it, err := p.iterator()
		if err != nil {
			return nil, 0, false, err
		}
		return it, p.depth, true, nil
	}
	return nil, 0, false, nil
}

// runPass consumes src into this pass's in-memory groups, spilling rows for
// groups beyond the budget to disk (when enabled). It fills a.groups and resets
// a.gi; spilled partitions are appended to a.pending for later passes.
func (a *AggregateIter) runPass(src RowIterator, depth int) error {
	defer src.Close()
	a.groups = []*aggGroup{}
	a.gi = 0
	index := map[string]int{}
	limit := a.cfg.MaxGroups
	var spill *spillSet

	for {
		r, ok, err := src.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		keyVals, ks, err := a.keyOf(r)
		if err != nil {
			return err
		}
		if idx, seen := index[ks]; seen {
			// A group already resident in this pass always aggregates in memory,
			// so it is never split across the memory/spill boundary.
			for _, acc := range a.groups[idx].accs {
				if err := acc.Add(r); err != nil {
					return err
				}
			}
			continue
		}
		// A new group: admit it in memory while under budget and not already
		// spilling; once spilling has begun in this pass, every new group spills.
		if spill == nil && (limit <= 0 || len(a.groups) < limit) {
			g, err := a.newGroup(keyVals)
			if err != nil {
				return err
			}
			index[ks] = len(a.groups)
			a.groups = append(a.groups, g)
			for _, acc := range g.accs {
				if err := acc.Add(r); err != nil {
					return err
				}
			}
			continue
		}
		if !a.cfg.Spill {
			return fmt.Errorf("aggregation exceeded the in-memory group limit of %d; "+
				"reduce cardinality at the source, use an APPROX_* aggregate, or enable spilling (--spill)", limit)
		}
		if spill == nil {
			spill, err = newSpillSet(a.cfg.SpillDir, depth)
			if err != nil {
				return err
			}
			if a.spillDirs == nil {
				a.spillDirs = map[string]struct{}{}
			}
			a.spillDirs[spill.dir] = struct{}{}
		}
		if err := spill.write(ks, r); err != nil {
			return err
		}
	}

	if spill != nil {
		parts, err := spill.finish()
		if err != nil {
			return err
		}
		a.pending = append(a.pending, parts...)
	}
	// A global aggregate (no GROUP BY) over an empty input still yields one row:
	// COUNT(*) = 0, SUM/MIN/MAX/AVG = NULL. Only the first (child) pass can be
	// empty-with-no-keys — a spill partition always holds rows.
	if depth == 0 && len(a.groups) == 0 && len(a.keys) == 0 {
		g, err := a.newGroup(nil)
		if err != nil {
			return err
		}
		a.groups = append(a.groups, g)
	}
	return nil
}

// keyOf evaluates the group-key expressions for a row, returning the key values
// and their string encoding (the map key + spill hash input).
func (a *AggregateIter) keyOf(r Row) ([]Value, string, error) {
	keyVals := make([]Value, len(a.keys))
	var kb strings.Builder
	for i, ke := range a.keys {
		v, err := a.eval.Eval(ke, r)
		if err != nil {
			return nil, "", err
		}
		keyVals[i] = v
		kb.WriteString(keyString(v))
		kb.WriteString("\x01")
	}
	return keyVals, kb.String(), nil
}

// newGroup builds a group with a fresh accumulator per aggregate spec.
func (a *AggregateIter) newGroup(keyVals []Value) (*aggGroup, error) {
	accs := make([]aggAccumulator, len(a.aggs))
	for i, spec := range a.aggs {
		acc, err := newAggAccumulator(spec, a.eval)
		if err != nil {
			return nil, err
		}
		accs[i] = acc
	}
	return &aggGroup{keyVals: keyVals, accs: accs}, nil
}

func (a *AggregateIter) Close() error {
	if a.closed {
		return nil
	}
	a.closed = true
	// Remove any spill temp dirs (files for unconsumed partitions live here; a
	// consumed partition removes its own file when its reader closes).
	for dir := range a.spillDirs {
		os.RemoveAll(dir)
	}
	return a.child.Close()
}

// computeFirstLast implements FIRST(value, ord) / LAST(value, ord): the value
// paired with the smallest (FIRST) / largest (LAST) ordering value in the
// group — e.g. LAST(reading, ts) is the most recent reading. Rows whose
// ordering value is NULL are skipped; the selected row's value may itself be
// NULL. On ties of the ordering value, FIRST keeps the earliest input row and
// LAST the latest, matching their names.
func computeFirstLast(name string, spec AggSpec, rows []Row, eval Evaluator) (Value, error) {
	if spec.Arg2 == nil {
		return Value{}, fmt.Errorf("%s expects 2 args, e.g. %s(value, ts)", name, name)
	}
	acc := &firstLastAcc{name: strings.ToUpper(name), arg: spec.Arg, ord: spec.Arg2, eval: eval}
	for i := range rows {
		if err := acc.Add(rows[i]); err != nil {
			return Value{}, err
		}
	}
	return acc.Result()
}

// computeAgg is the batch aggregate API: it folds all rows through a fresh
// streaming accumulator (see aggacc.go). AggregateIter uses the accumulators
// directly, one per group, so it never buffers rows; this wrapper serves the
// window/frame callers and tests that aggregate a materialized slice.
func computeAgg(spec AggSpec, rows []Row, eval Evaluator) (Value, error) {
	acc, err := newAggAccumulator(spec, eval)
	if err != nil {
		return Value{}, err
	}
	for i := range rows {
		if err := acc.Add(rows[i]); err != nil {
			return Value{}, err
		}
	}
	return acc.Result()
}

// aggPercentile evaluates the percentile fraction (second argument) of a
// PERCENTILE_*/QUANTILE aggregate, clamped to [0,1]. It is normally a constant.
func aggPercentile(spec AggSpec, probe Row, eval Evaluator) (float64, error) {
	if spec.Arg2 == nil {
		return 0, fmt.Errorf("%s requires a percentile in [0,1], e.g. %s(x, 0.95)", spec.Func, spec.Func)
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
func aggSeparator(spec AggSpec, probe Row, eval Evaluator) (string, error) {
	if spec.Arg2 == nil {
		return "", nil
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
	case "FIRST_VALUE", "LAST_VALUE", "NTH_VALUE":
		return computeWindowValue(spec, idxs, rows, eval, result, strings.ToUpper(spec.Func))
	case "SUM", "AVG", "COUNT", "MIN", "MAX":
		return computeWindowAgg(spec, idxs, rows, eval, result)
	case "FIRST", "LAST":
		return computeWindowFirstLast(spec, idxs, rows, eval, result, strings.ToUpper(spec.Func))
	case "LOCF":
		return computeLOCF(spec, idxs, rows, eval, result)
	case "DELTA", "RATE":
		return computeDeltaRate(spec, idxs, rows, eval, result, strings.ToUpper(spec.Func))
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

// windowFrameRanges returns, per ordered position, the [lo, hi] index range of
// the frame: the default frame (whole partition, or running-through-peers when
// ORDER BY'd) when none is given, else an explicit ROWS / RANGE frame. Reuses
// frameBoundIndex / rangeFrameBounds.
func windowFrameRanges(spec WindowSpec, idxs []int, rows []Row, eval Evaluator) ([]int, []int, error) {
	n := len(idxs)
	los, his := make([]int, n), make([]int, n)
	switch {
	case spec.Frame == nil && len(spec.OrderBy) == 0:
		for i := 0; i < n; i++ {
			los[i], his[i] = 0, n-1
		}
	case spec.Frame == nil: // default running frame: [0, last peer]
		for i := 0; i < n; {
			j := i + 1
			for j < n {
				eq, err := windowPeers(spec.OrderBy, rows[idxs[j-1]], rows[idxs[j]], eval)
				if err != nil {
					return nil, nil, err
				}
				if !eq {
					break
				}
				j++
			}
			for k := i; k < j; k++ {
				los[k], his[k] = 0, j-1
			}
			i = j
		}
	case strings.EqualFold(spec.Frame.Unit, "RANGE"):
		if len(spec.OrderBy) != 1 {
			return nil, nil, fmt.Errorf("RANGE frame requires exactly one ORDER BY column")
		}
		desc := spec.OrderBy[0].Desc
		ovals := make([]Value, n)
		for pos, ri := range idxs {
			v, err := eval.Eval(spec.OrderBy[0].Expr, rows[ri])
			if err != nil {
				return nil, nil, err
			}
			ovals[pos] = v
		}
		for pos := 0; pos < n; pos++ {
			lo, hi, err := rangeFrameBounds(spec.Frame, ovals, pos, desc, eval)
			if err != nil {
				return nil, nil, err
			}
			los[pos], his[pos] = lo, hi
		}
	default: // ROWS
		startOff, err := frameOffsetInt(spec.Frame.Start, eval)
		if err != nil {
			return nil, nil, err
		}
		endOff, err := frameOffsetInt(spec.Frame.End, eval)
		if err != nil {
			return nil, nil, err
		}
		for pos := 0; pos < n; pos++ {
			lo := frameBoundIndex(spec.Frame.Start.Kind, startOff, pos, n)
			hi := frameBoundIndex(spec.Frame.End.Kind, endOff, pos, n)
			if lo < 0 {
				lo = 0
			}
			if hi > n-1 {
				hi = n - 1
			}
			los[pos], his[pos] = lo, hi
		}
	}
	return los, his, nil
}

// computeWindowValue implements FIRST_VALUE / LAST_VALUE / NTH_VALUE: the
// argument evaluated at the first / last / n-th row of each row's frame (NULL if
// that position falls outside the frame). NB: with the default frame, LAST_VALUE
// is the current row — use an explicit frame (e.g. ROWS BETWEEN UNBOUNDED
// PRECEDING AND UNBOUNDED FOLLOWING) for the partition's last value.
func computeWindowValue(spec WindowSpec, idxs []int, rows []Row, eval Evaluator, result []Value, which string) error {
	if len(spec.Args) == 0 {
		return fmt.Errorf("%s requires an argument", which)
	}
	nth := 1
	if which == "NTH_VALUE" {
		if len(spec.Args) < 2 {
			return fmt.Errorf("NTH_VALUE requires (expr, n)")
		}
		nv, err := eval.Eval(spec.Args[1], rows[idxs[0]])
		if err != nil {
			return err
		}
		k, ok := nv.AsInt()
		if !ok || k < 1 {
			return fmt.Errorf("NTH_VALUE n must be a positive integer")
		}
		nth = int(k)
	}
	los, his, err := windowFrameRanges(spec, idxs, rows, eval)
	if err != nil {
		return err
	}
	for pos := 0; pos < len(idxs); pos++ {
		lo, hi := los[pos], his[pos]
		src := lo
		switch which {
		case "LAST_VALUE":
			src = hi
		case "NTH_VALUE":
			src = lo + nth - 1
		}
		if lo > hi || src < lo || src > hi {
			result[idxs[pos]] = Null()
			continue
		}
		v, err := eval.Eval(spec.Args[0], rows[idxs[src]])
		if err != nil {
			return err
		}
		result[idxs[pos]] = v
	}
	return nil
}

// computeWindowFirstLast implements FIRST/LAST(value, ord) as window
// aggregates over each row's frame — e.g. LAST(reading, ts) OVER (PARTITION BY
// station) attaches the station's most recent reading to every row. Frame
// semantics match the other window aggregates (whole partition, or the running
// frame when ORDER BY'd, or an explicit ROWS/RANGE frame).
func computeWindowFirstLast(spec WindowSpec, idxs []int, rows []Row, eval Evaluator, result []Value, name string) error {
	if len(spec.Args) < 2 {
		return fmt.Errorf("%s expects 2 args, e.g. %s(value, ts)", name, name)
	}
	los, his, err := windowFrameRanges(spec, idxs, rows, eval)
	if err != nil {
		return err
	}
	agg := AggSpec{Func: name, Arg: spec.Args[0], Arg2: spec.Args[1]}
	var frame []Row
	for pos := 0; pos < len(idxs); pos++ {
		lo, hi := los[pos], his[pos]
		if lo > hi {
			result[idxs[pos]] = Null()
			continue
		}
		frame = frame[:0]
		for j := lo; j <= hi; j++ {
			frame = append(frame, rows[idxs[j]])
		}
		v, err := computeFirstLast(name, agg, frame, eval)
		if err != nil {
			return err
		}
		result[idxs[pos]] = v
	}
	return nil
}

// computeLOCF implements LOCF(x) — "last observation carried forward": each
// row's x, or when x is NULL the most recent non-NULL x earlier in the
// partition's window order. It is the gap-filling companion to a
// generate_series LEFT JOIN (see the DIALECT.md recipe); leading NULLs (no
// prior observation) stay NULL.
func computeLOCF(spec WindowSpec, idxs []int, rows []Row, eval Evaluator, result []Value) error {
	if len(spec.Args) == 0 {
		return fmt.Errorf("LOCF requires an argument, e.g. LOCF(value)")
	}
	carry := Null()
	for _, ri := range idxs {
		v, err := eval.Eval(spec.Args[0], rows[ri])
		if err != nil {
			return err
		}
		if !v.IsNull() {
			carry = v
		}
		result[ri] = carry
	}
	return nil
}

// computeDeltaRate implements DELTA(x) — the difference from the previous
// row's x in window order — and RATE(x, ts) — the per-second rate of increase
// between the previous row and this one, with Prometheus-style counter-reset
// handling: a drop in x is treated as a counter reset, so the increase is the
// new value rather than a negative delta. The first row of each partition is
// NULL, as is any pair with a NULL input or (for RATE) a non-positive time
// step. ts may be a timestamp (rate per second) or a number (rate per unit).
func computeDeltaRate(spec WindowSpec, idxs []int, rows []Row, eval Evaluator, result []Value, name string) error {
	if len(spec.Args) == 0 {
		return fmt.Errorf("%s requires an argument, e.g. %s(value)", name, name)
	}
	if name == "RATE" && len(spec.Args) < 2 {
		return fmt.Errorf("RATE expects 2 args, e.g. RATE(counter, ts)")
	}
	// seconds converts an ordering value to a float axis position: timestamps
	// to epoch seconds, numbers as-is.
	seconds := func(v Value) (float64, bool) {
		if t, ok := v.V.(time.Time); ok {
			return float64(t.UnixNano()) / 1e9, true
		}
		return v.AsFloat()
	}
	for pos, ri := range idxs {
		result[ri] = Null()
		if pos == 0 {
			continue
		}
		prev := idxs[pos-1]
		cv, err := eval.Eval(spec.Args[0], rows[ri])
		if err != nil {
			return err
		}
		pv, err := eval.Eval(spec.Args[0], rows[prev])
		if err != nil {
			return err
		}
		if cv.IsNull() || pv.IsNull() {
			continue
		}
		cur, ok1 := cv.AsFloat()
		prv, ok2 := pv.AsFloat()
		if !ok1 || !ok2 {
			return fmt.Errorf("%s requires a numeric argument", name)
		}
		if name == "DELTA" {
			result[ri] = FloatVal(cur - prv)
			continue
		}
		ctv, err := eval.Eval(spec.Args[1], rows[ri])
		if err != nil {
			return err
		}
		ptv, err := eval.Eval(spec.Args[1], rows[prev])
		if err != nil {
			return err
		}
		if ctv.IsNull() || ptv.IsNull() {
			continue
		}
		ct, ok1 := seconds(ctv)
		pt, ok2 := seconds(ptv)
		if !ok1 || !ok2 {
			return fmt.Errorf("RATE requires a timestamp or numeric second argument")
		}
		dt := ct - pt
		if dt <= 0 {
			continue // duplicate or out-of-order timestamps: no meaningful rate
		}
		inc := cur - prv
		if inc < 0 {
			inc = cur // counter reset: assume the counter restarted from zero
		}
		result[ri] = FloatVal(inc / dt)
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
