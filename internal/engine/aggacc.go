package engine

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/april/turntable/internal/sql"
)

// aggAccumulator folds child rows for one aggregate of one group, holding only
// bounded running state — so a GROUP BY no longer buffers whole input rows.
// The algebraic aggregates (COUNT/SUM/AVG/MIN/MAX, the CORR/COVAR/REGR family,
// FIRST/LAST) keep O(1) state per group; the inherently holistic ones (MEDIAN,
// PERCENTILE_*/QUANTILE, VARIANCE/STDDEV, STRING_AGG, and any DISTINCT variant)
// must still retain values, but only the single argument value per row, never
// the whole row. A fresh accumulator is built per group in AggregateIter.
type aggAccumulator interface {
	Add(Row) error
	Result() (Value, error)
}

// newAggAccumulator builds the accumulator for one aggregate spec. It is the
// single source of truth for aggregate semantics: computeAgg (the batch API)
// folds rows through it, and AggregateIter folds one group's rows through one
// per group. Unknown functions and arity errors surface here.
func newAggAccumulator(spec AggSpec, eval Evaluator) (aggAccumulator, error) {
	name := strings.ToUpper(spec.Func)
	// COUNT(*) counts rows regardless of nullness; DISTINCT is meaningless on it.
	if name == "COUNT" {
		if cr, ok := spec.Arg.(*sql.ColRef); ok && cr.Name == "*" && cr.Qualifier == "" {
			return &countStarAcc{}, nil
		}
	}
	switch name {
	case "COUNT":
		return &countAcc{arg: spec.Arg, eval: eval, seen: newSeen(spec.Distinct)}, nil
	case "SUM":
		return &sumAcc{arg: spec.Arg, eval: eval, seen: newSeen(spec.Distinct)}, nil
	case "AVG":
		return &avgAcc{arg: spec.Arg, eval: eval, seen: newSeen(spec.Distinct)}, nil
	case "MIN", "MAX":
		return &minmaxAcc{arg: spec.Arg, eval: eval, isMin: name == "MIN"}, nil
	case "STRING_AGG":
		return &stringAggAcc{spec: spec, eval: eval, seen: newSeen(spec.Distinct)}, nil
	case "MEDIAN", "PERCENTILE_CONT", "PERCENTILE_DISC", "QUANTILE":
		return &percentileAcc{name: name, spec: spec, eval: eval, seen: newSeen(spec.Distinct)}, nil
	case "VARIANCE", "VAR_SAMP", "VAR_POP", "STDDEV", "STDDEV_SAMP", "STDDEV_POP":
		return &varianceAcc{name: name, arg: spec.Arg, eval: eval, seen: newSeen(spec.Distinct)}, nil
	case "CORR", "COVAR_POP", "COVAR_SAMP", "REGR_SLOPE", "REGR_INTERCEPT",
		"REGR_R2", "REGR_COUNT", "REGR_AVGX", "REGR_AVGY":
		if spec.Arg2 == nil {
			return nil, fmt.Errorf("%s expects 2 args, e.g. %s(y, x)", name, name)
		}
		return &regrAcc{name: name, y: spec.Arg, x: spec.Arg2, eval: eval}, nil
	case "FIRST", "LAST":
		if spec.Arg2 == nil {
			return nil, fmt.Errorf("%s expects 2 args, e.g. %s(value, ts)", name, name)
		}
		return &firstLastAcc{name: name, arg: spec.Arg, ord: spec.Arg2, eval: eval}, nil
	}
	return nil, fmt.Errorf("unknown aggregate %s", spec.Func)
}

// newSeen returns a dedupe set for DISTINCT aggregates, else nil.
func newSeen(distinct bool) map[string]bool {
	if distinct {
		return map[string]bool{}
	}
	return nil
}

// aggValue evaluates arg over r for a value-based aggregate, reporting keep=false
// when the value is NULL or (with DISTINCT, seen != nil) already seen — the same
// null-skip + dedupe the old batch path applied before folding.
func aggValue(arg sql.Expr, eval Evaluator, r Row, seen map[string]bool) (Value, bool, error) {
	v, err := eval.Eval(arg, r)
	if err != nil {
		return Value{}, false, err
	}
	if v.IsNull() {
		return Value{}, false, nil
	}
	if seen != nil {
		k := keyString(v)
		if seen[k] {
			return Value{}, false, nil
		}
		seen[k] = true
	}
	return v, true, nil
}

// ---- COUNT(*) ---------------------------------------------------------------

type countStarAcc struct{ n int64 }

func (a *countStarAcc) Add(Row) error          { a.n++; return nil }
func (a *countStarAcc) Result() (Value, error) { return IntVal(a.n), nil }

// ---- COUNT(x) ---------------------------------------------------------------

type countAcc struct {
	arg  sql.Expr
	eval Evaluator
	seen map[string]bool
	n    int64
}

func (a *countAcc) Add(r Row) error {
	_, keep, err := aggValue(a.arg, a.eval, r, a.seen)
	if err != nil {
		return err
	}
	if keep {
		a.n++
	}
	return nil
}
func (a *countAcc) Result() (Value, error) { return IntVal(a.n), nil }

// ---- SUM --------------------------------------------------------------------

type sumAcc struct {
	arg  sql.Expr
	eval Evaluator
	seen map[string]bool
	sum  float64
	any  bool
}

func (a *sumAcc) Add(r Row) error {
	v, keep, err := aggValue(a.arg, a.eval, r, a.seen)
	if err != nil || !keep {
		return err
	}
	f, ok := v.AsFloat()
	if !ok {
		return fmt.Errorf("SUM requires numeric arg")
	}
	a.sum += f
	a.any = true
	return nil
}
func (a *sumAcc) Result() (Value, error) {
	if !a.any {
		return Null(), nil
	}
	return FloatVal(a.sum), nil
}

// ---- AVG --------------------------------------------------------------------

type avgAcc struct {
	arg  sql.Expr
	eval Evaluator
	seen map[string]bool
	sum  float64
	n    int64
}

func (a *avgAcc) Add(r Row) error {
	v, keep, err := aggValue(a.arg, a.eval, r, a.seen)
	if err != nil || !keep {
		return err
	}
	f, ok := v.AsFloat()
	if !ok {
		return fmt.Errorf("AVG requires numeric arg")
	}
	a.sum += f
	a.n++
	return nil
}
func (a *avgAcc) Result() (Value, error) {
	if a.n == 0 {
		return Null(), nil
	}
	return FloatVal(a.sum / float64(a.n)), nil
}

// ---- MIN / MAX --------------------------------------------------------------

type minmaxAcc struct {
	arg   sql.Expr
	eval  Evaluator
	isMin bool
	best  Value
	found bool
}

func (a *minmaxAcc) Add(r Row) error {
	// DISTINCT cannot change an extremum, so no dedupe set is kept.
	v, err := a.eval.Eval(a.arg, r)
	if err != nil {
		return err
	}
	if v.IsNull() {
		return nil
	}
	if !a.found {
		a.best, a.found = v, true
		return nil
	}
	c := Compare(v, a.best)
	if (a.isMin && c < 0) || (!a.isMin && c > 0) {
		a.best = v
	}
	return nil
}
func (a *minmaxAcc) Result() (Value, error) {
	if !a.found {
		return Null(), nil
	}
	return a.best, nil
}

// ---- STRING_AGG -------------------------------------------------------------

type stringAggAcc struct {
	spec     AggSpec
	eval     Evaluator
	seen     map[string]bool
	parts    []string
	probe    Row
	hasProbe bool
}

func (a *stringAggAcc) Add(r Row) error {
	if !a.hasProbe { // first row of the group carries the (constant) separator
		a.probe, a.hasProbe = r, true
	}
	v, keep, err := aggValue(a.spec.Arg, a.eval, r, a.seen)
	if err != nil || !keep {
		return err
	}
	a.parts = append(a.parts, v.AsString())
	return nil
}
func (a *stringAggAcc) Result() (Value, error) {
	if len(a.parts) == 0 {
		return Null(), nil
	}
	sep, err := aggSeparator(a.spec, a.probe, a.eval)
	if err != nil {
		return Value{}, err
	}
	return StringVal(strings.Join(a.parts, sep)), nil
}

// ---- MEDIAN / PERCENTILE_* / QUANTILE ---------------------------------------

type percentileAcc struct {
	name     string
	spec     AggSpec
	eval     Evaluator
	seen     map[string]bool
	nums     []float64
	probe    Row
	hasProbe bool
}

func (a *percentileAcc) Add(r Row) error {
	if !a.hasProbe { // first row carries the (constant) percentile fraction
		a.probe, a.hasProbe = r, true
	}
	v, keep, err := aggValue(a.spec.Arg, a.eval, r, a.seen)
	if err != nil || !keep {
		return err
	}
	f, ok := v.AsFloat()
	if !ok {
		return fmt.Errorf("%s requires numeric arg", a.name)
	}
	a.nums = append(a.nums, f)
	return nil
}
func (a *percentileAcc) Result() (Value, error) {
	if len(a.nums) == 0 {
		return Null(), nil
	}
	s := append([]float64(nil), a.nums...)
	sort.Float64s(s)
	n := len(s)
	if a.name == "MEDIAN" {
		if n%2 == 1 {
			return FloatVal(s[n/2]), nil
		}
		return FloatVal((s[n/2-1] + s[n/2]) / 2), nil
	}
	p, err := aggPercentile(a.spec, a.probe, a.eval)
	if err != nil {
		return Value{}, err
	}
	if a.name == "PERCENTILE_DISC" {
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

// ---- VARIANCE / STDDEV ------------------------------------------------------

// varianceAcc buffers the argument values and defers to the two-pass variance
// helper at Result — keeping results bit-identical to the previous batch path
// (a streaming one-pass form would perturb the low-order floating-point bits).
type varianceAcc struct {
	name string
	arg  sql.Expr
	eval Evaluator
	seen map[string]bool
	nums []float64
}

func (a *varianceAcc) Add(r Row) error {
	v, keep, err := aggValue(a.arg, a.eval, r, a.seen)
	if err != nil || !keep {
		return err
	}
	f, ok := v.AsFloat()
	if !ok {
		return fmt.Errorf("%s requires numeric arg", a.name)
	}
	a.nums = append(a.nums, f)
	return nil
}
func (a *varianceAcc) Result() (Value, error) {
	pop := a.name == "VAR_POP" || a.name == "STDDEV_POP"
	v, ok := variance(a.nums, pop)
	if !ok {
		return Null(), nil
	}
	if strings.HasPrefix(a.name, "STDDEV") {
		return FloatVal(math.Sqrt(v)), nil
	}
	return FloatVal(v), nil
}

// ---- CORR / COVAR_* / REGR_* ------------------------------------------------

// regrAcc streams the paired sums the two-argument statistical aggregates need
// (Σx, Σy, Σx², Σy², Σxy, n), so they cost O(1) state per group. The final
// formulas match the previous batch computeRegr exactly. Argument order follows
// SQL: y (dependent) is spec.Arg, x is spec.Arg2 — e.g. CORR(y, x).
type regrAcc struct {
	name                  string
	y, x                  sql.Expr
	eval                  Evaluator
	n                     int
	sx, sy, sxx, syy, sxy float64
}

func (a *regrAcc) Add(r Row) error {
	yv, err := a.eval.Eval(a.y, r)
	if err != nil {
		return err
	}
	xv, err := a.eval.Eval(a.x, r)
	if err != nil {
		return err
	}
	if yv.IsNull() || xv.IsNull() {
		return nil
	}
	y, ok1 := yv.AsFloat()
	x, ok2 := xv.AsFloat()
	if !ok1 || !ok2 {
		return fmt.Errorf("%s requires numeric args", a.name)
	}
	a.n++
	a.sx, a.sy, a.sxx, a.syy, a.sxy = a.sx+x, a.sy+y, a.sxx+x*x, a.syy+y*y, a.sxy+x*y
	return nil
}
func (a *regrAcc) Result() (Value, error) {
	if a.name == "REGR_COUNT" {
		return IntVal(int64(a.n)), nil
	}
	if a.n == 0 {
		return Null(), nil
	}
	fn := float64(a.n)
	mx, my := a.sx/fn, a.sy/fn
	// Centered sums of squares / cross-products: Σ(x-x̄)², Σ(y-ȳ)², Σ(x-x̄)(y-ȳ).
	cxx := a.sxx - a.sx*a.sx/fn
	cyy := a.syy - a.sy*a.sy/fn
	cxy := a.sxy - a.sx*a.sy/fn
	switch a.name {
	case "REGR_AVGX":
		return FloatVal(mx), nil
	case "REGR_AVGY":
		return FloatVal(my), nil
	case "COVAR_POP":
		return FloatVal(cxy / fn), nil
	case "COVAR_SAMP":
		if a.n < 2 {
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
	return Value{}, fmt.Errorf("unknown aggregate %s", a.name)
}

// ---- FIRST / LAST -----------------------------------------------------------

// firstLastAcc streams the value paired with the smallest (FIRST) / largest
// (LAST) ordering value seen. Rows whose ordering value is NULL are skipped; the
// selected value may itself be NULL. On ties FIRST keeps the earliest input row
// and LAST the latest, matching their names.
type firstLastAcc struct {
	name             string // FIRST or LAST
	arg, ord         sql.Expr
	eval             Evaluator
	bestOrd, bestVal Value
	found            bool
}

func (a *firstLastAcc) Add(r Row) error {
	ov, err := a.eval.Eval(a.ord, r)
	if err != nil {
		return err
	}
	if ov.IsNull() {
		return nil
	}
	if a.found {
		c := Compare(ov, a.bestOrd)
		// FIRST replaces only on a strictly smaller ordering value; LAST replaces
		// on greater-or-equal (so a tie takes the later row).
		if (a.name == "FIRST" && c >= 0) || (a.name == "LAST" && c < 0) {
			return nil
		}
	}
	vv, err := a.eval.Eval(a.arg, r)
	if err != nil {
		return err
	}
	a.bestOrd, a.bestVal, a.found = ov, vv, true
	return nil
}
func (a *firstLastAcc) Result() (Value, error) {
	if !a.found {
		return Null(), nil
	}
	return a.bestVal, nil
}
