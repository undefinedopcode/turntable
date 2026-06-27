package engine

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	// Embed the IANA time-zone database so CONVERT_TZ/FROM_TZ resolve named
	// zones (e.g. "America/Los_Angeles") without relying on system zoneinfo,
	// which may be absent (minimal containers, Windows). Pure Go, no CGO.
	_ "time/tzdata"
)

// reCache memoizes compiled patterns so the REGEXP_* functions (called once per
// row, usually with a constant literal pattern) don't recompile every call.
var reCache sync.Map // pattern string -> *regexp.Regexp

func compileRe(pattern string) (*regexp.Regexp, error) {
	if v, ok := reCache.Load(pattern); ok {
		return v.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	reCache.Store(pattern, re)
	return re, nil
}

// ScalarFunc is a scalar SQL function implementation. It receives the
// already-evaluated argument values and returns a result. Returning an error
// surfaces a message to the user.
type ScalarFunc func(args []Value) (Value, error)

// FuncRegistry holds named scalar functions. Aggregate functions are handled
// separately by the Aggregate operator, not here.
type FuncRegistry struct {
	funcs map[string]ScalarFunc
}

// NewFuncRegistry returns a registry pre-populated with the v0.1 scalar
// function library.
func NewFuncRegistry() *FuncRegistry {
	r := &FuncRegistry{funcs: map[string]ScalarFunc{}}
	r.registerDefaults()
	return r
}

// Lookup returns the function with the given (case-insensitive) name, or nil.
func (r *FuncRegistry) Lookup(name string) ScalarFunc {
	return r.funcs[strings.ToUpper(name)]
}

// Register adds a function under an uppercased name.
func (r *FuncRegistry) Register(name string, fn ScalarFunc) {
	r.funcs[strings.ToUpper(name)] = fn
}

// Names returns every registered scalar function name, sorted. Aliases (e.g.
// LEN and LENGTH) are listed individually so any callable name is discoverable.
func (r *FuncRegistry) Names() []string {
	out := make([]string, 0, len(r.funcs))
	for n := range r.funcs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Aggregates returns the supported aggregate function names. They are handled by
// the Aggregate operator, not the scalar registry; listed here for discovery.
func Aggregates() []string {
	return []string{
		"AVG", "COUNT", "MAX", "MEDIAN", "MIN", "STDDEV", "STDDEV_POP",
		"STDDEV_SAMP", "STRING_AGG", "SUM", "VARIANCE", "VAR_POP", "VAR_SAMP",
	}
}

func (r *FuncRegistry) registerDefaults() {
	r.Register("COALESCE", funcCoalesce)
	r.Register("LOWER", funcLower)
	r.Register("UPPER", funcUpper)
	r.Register("LENGTH", funcLength)
	r.Register("LEN", funcLength)
	r.Register("SUBSTR", funcSubstr)
	r.Register("SUBSTRING", funcSubstr)
	r.Register("TRIM", funcTrim)
	r.Register("LTRIM", funcLTrim)
	r.Register("RTRIM", funcRTrim)
	r.Register("CONCAT", funcConcat)
	r.Register("ABS", funcAbs)
	r.Register("ROUND", funcRound)
	r.Register("FLOOR", funcFloor)
	r.Register("CEIL", funcCeil)
	r.Register("CEILING", funcCeil)
	r.Register("REPLACE", funcReplace)
	r.Register("NOW", funcNow)
	r.Register("CURRENT_TIMESTAMP", funcNow)

	// v0.3 string functions.
	r.Register("LEFT", funcLeft)
	r.Register("RIGHT", funcRight)
	r.Register("STRPOS", funcStrpos)
	r.Register("INSTR", funcStrpos)
	r.Register("SPLIT_PART", funcSplitPart)
	r.Register("REGEXP_REPLACE", funcRegexpReplace)
	r.Register("REGEXP_MATCHES", funcRegexpMatches)
	r.Register("REGEXP_EXTRACT", funcRegexpExtract)
	r.Register("REPEAT", funcRepeat)
	r.Register("REVERSE", funcReverse)
	r.Register("INITCAP", funcInitcap)
	r.Register("LPAD", funcLpad)
	r.Register("RPAD", funcRpad)

	// v0.3 time functions.
	r.Register("DATE_TRUNC", funcDateTrunc)
	r.Register("DATE_ADD", funcDateAdd)
	r.Register("AGE", funcAge)
	r.Register("TO_TIMESTAMP", funcToTimestamp)
	r.Register("DATE", funcDate)
	r.Register("STRFTIME", funcStrftime)
	r.Register("CURRENT_DATE", funcCurrentDate)
	r.Register("CONVERT_TZ", funcConvertTZ)
	r.Register("FROM_TZ", funcFromTZ)
}

func funcCoalesce(args []Value) (Value, error) {
	for _, a := range args {
		if !a.IsNull() {
			return a, nil
		}
	}
	return Null(), nil
}

func funcLower(args []Value) (Value, error) {
	if len(args) != 1 {
		return Value{}, fmt.Errorf("LOWER expects 1 arg")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	return StringVal(strings.ToLower(args[0].AsString())), nil
}

func funcUpper(args []Value) (Value, error) {
	if len(args) != 1 {
		return Value{}, fmt.Errorf("UPPER expects 1 arg")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	return StringVal(strings.ToUpper(args[0].AsString())), nil
}

func funcLength(args []Value) (Value, error) {
	if len(args) != 1 {
		return Value{}, fmt.Errorf("LENGTH expects 1 arg")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	switch args[0].Type {
	case TypeString:
		return IntVal(int64(len(args[0].V.(string)))), nil
	case TypeBytes:
		return IntVal(int64(len(args[0].V.([]byte)))), nil
	}
	return IntVal(int64(len(args[0].AsString()))), nil
}

func funcSubstr(args []Value) (Value, error) {
	if len(args) < 2 || len(args) > 3 {
		return Value{}, fmt.Errorf("SUBSTR expects 2 or 3 args")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	s := args[0].AsString()
	start, ok := args[1].AsInt()
	if !ok {
		return Value{}, fmt.Errorf("SUBSTR start must be integer")
	}
	// SQL is 1-based; negative or 0 clamps to start.
	if start < 1 {
		start = 1
	}
	start--
	runes := []rune(s)
	if int(start) > len(runes) {
		return StringVal(""), nil
	}
	end := int64(len(runes))
	if len(args) == 3 {
		ln, ok := args[2].AsInt()
		if !ok {
			return Value{}, fmt.Errorf("SUBSTR length must be integer")
		}
		end = start + ln
	}
	if end < start {
		end = start
	}
	if end > int64(len(runes)) {
		end = int64(len(runes))
	}
	return StringVal(string(runes[start:end])), nil
}

func funcTrim(args []Value) (Value, error) {
	if len(args) != 1 {
		return Value{}, fmt.Errorf("TRIM expects 1 arg")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	return StringVal(strings.TrimSpace(args[0].AsString())), nil
}

func funcLTrim(args []Value) (Value, error) {
	if len(args) != 1 {
		return Value{}, fmt.Errorf("LTRIM expects 1 arg")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	return StringVal(strings.TrimLeft(args[0].AsString(), " \t\n\r")), nil
}

func funcRTrim(args []Value) (Value, error) {
	if len(args) != 1 {
		return Value{}, fmt.Errorf("RTRIM expects 1 arg")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	return StringVal(strings.TrimRight(args[0].AsString(), " \t\n\r")), nil
}

func funcConcat(args []Value) (Value, error) {
	var b strings.Builder
	for _, a := range args {
		if a.IsNull() {
			continue
		}
		b.WriteString(a.AsString())
	}
	return StringVal(b.String()), nil
}

func funcAbs(args []Value) (Value, error) {
	if len(args) != 1 {
		return Value{}, fmt.Errorf("ABS expects 1 arg")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	if n, ok := args[0].AsInt(); ok && args[0].Type == TypeInt {
		if n < 0 {
			n = -n
		}
		return IntVal(n), nil
	}
	if f, ok := args[0].AsFloat(); ok {
		return FloatVal(absFloat(f)), nil
	}
	return Value{}, fmt.Errorf("ABS requires numeric")
}

func absFloat(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func funcRound(args []Value) (Value, error) {
	if len(args) < 1 || len(args) > 2 {
		return Value{}, fmt.Errorf("ROUND expects 1 or 2 args")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	f, ok := args[0].AsFloat()
	if !ok {
		return Value{}, fmt.Errorf("ROUND requires numeric")
	}
	if len(args) == 1 {
		return FloatVal(roundAt(f, 0)), nil
	}
	d, ok := args[1].AsInt()
	if !ok {
		return Value{}, fmt.Errorf("ROUND digits must be integer")
	}
	return FloatVal(roundAt(f, int(d))), nil
}

func roundAt(f float64, digits int) float64 {
	pow := 1.0
	for i := 0; i < digits; i++ {
		pow *= 10
	}
	return float64(int64(f*pow+0.5)) / pow
}

func funcFloor(args []Value) (Value, error) {
	if len(args) != 1 {
		return Value{}, fmt.Errorf("FLOOR expects 1 arg")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	f, ok := args[0].AsFloat()
	if !ok {
		return Value{}, fmt.Errorf("FLOOR requires numeric")
	}
	return FloatVal(float64(int64(f))), nil
}

func funcCeil(args []Value) (Value, error) {
	if len(args) != 1 {
		return Value{}, fmt.Errorf("CEIL expects 1 arg")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	f, ok := args[0].AsFloat()
	if !ok {
		return Value{}, fmt.Errorf("CEIL requires numeric")
	}
	return FloatVal(float64(int64(f) + 1)), nil
}

func funcReplace(args []Value) (Value, error) {
	if len(args) != 3 {
		return Value{}, fmt.Errorf("REPLACE expects 3 args")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	return StringVal(strings.ReplaceAll(args[0].AsString(), args[1].AsString(), args[2].AsString())), nil
}

func funcNow(args []Value) (Value, error) {
	if len(args) != 0 {
		return Value{}, fmt.Errorf("NOW expects 0 args")
	}
	return TimeVal(time.Now()), nil
}

// ---- v0.3 string functions ---------------------------------------------------

func funcLeft(args []Value) (Value, error) {
	if len(args) != 2 {
		return Value{}, fmt.Errorf("LEFT expects 2 args")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	n, ok := args[1].AsInt()
	if !ok {
		return Value{}, fmt.Errorf("LEFT length must be integer")
	}
	s := []rune(args[0].AsString())
	if n < 0 {
		// Negative n counts from the end: LEFT(s, -2) = s[:len-2].
		if -n >= int64(len(s)) {
			return StringVal(""), nil
		}
		return StringVal(string(s[:int64(len(s))+n])), nil
	}
	if n > int64(len(s)) {
		n = int64(len(s))
	}
	return StringVal(string(s[:n])), nil
}

func funcRight(args []Value) (Value, error) {
	if len(args) != 2 {
		return Value{}, fmt.Errorf("RIGHT expects 2 args")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	n, ok := args[1].AsInt()
	if !ok {
		return Value{}, fmt.Errorf("RIGHT length must be integer")
	}
	s := []rune(args[0].AsString())
	if n < 0 {
		if -n >= int64(len(s)) {
			return StringVal(""), nil
		}
		return StringVal(string(s[-n:])), nil
	}
	if n > int64(len(s)) {
		n = int64(len(s))
	}
	return StringVal(string(s[int64(len(s))-n:])), nil
}

func funcStrpos(args []Value) (Value, error) {
	if len(args) != 2 {
		return Value{}, fmt.Errorf("STRPOS expects 2 args")
	}
	if args[0].IsNull() || args[1].IsNull() {
		return Null(), nil
	}
	idx := strings.Index(args[0].AsString(), args[1].AsString())
	return IntVal(int64(idx + 1)), nil
}

func funcSplitPart(args []Value) (Value, error) {
	if len(args) != 3 {
		return Value{}, fmt.Errorf("SPLIT_PART expects 3 args")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	n, ok := args[2].AsInt()
	if !ok {
		return Value{}, fmt.Errorf("SPLIT_PART part must be integer")
	}
	parts := strings.Split(args[0].AsString(), args[1].AsString())
	if n < 1 || n > int64(len(parts)) {
		return StringVal(""), nil
	}
	return StringVal(parts[n-1]), nil
}

func funcRegexpReplace(args []Value) (Value, error) {
	if len(args) < 3 || len(args) > 4 {
		return Value{}, fmt.Errorf("REGEXP_REPLACE expects 3 or 4 args")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	re, err := compileRe(args[1].AsString())
	if err != nil {
		return Value{}, fmt.Errorf("REGEXP_REPLACE: %v", err)
	}
	repl := args[2].AsString()
	global := len(args) == 4 && strings.Contains(args[3].AsString(), "g")
	src := args[0].AsString()
	if global {
		return StringVal(re.ReplaceAllString(src, repl)), nil
	}
	// Single replacement: replace only the first match.
	loc := re.FindStringIndex(src)
	if loc == nil {
		return StringVal(src), nil
	}
	return StringVal(src[:loc[0]] + repl + src[loc[1]:]), nil
}

func funcRegexpMatches(args []Value) (Value, error) {
	if len(args) != 2 {
		return Value{}, fmt.Errorf("REGEXP_MATCHES expects 2 args")
	}
	return funcRegexpExtract(args)
}

// funcRegexpExtract pulls a substring out of a string by regular expression.
// REGEXP_EXTRACT(s, pattern)        -> the first capturing group, or the whole
//
//	match if the pattern has no group (NULL if no match)
//
// REGEXP_EXTRACT(s, pattern, group) -> the n-th group (0 = whole match);
//
//	NULL if no match or the group is out of range / didn't participate.
func funcRegexpExtract(args []Value) (Value, error) {
	if len(args) < 2 || len(args) > 3 {
		return Value{}, fmt.Errorf("REGEXP_EXTRACT expects 2 or 3 args (string, pattern[, group])")
	}
	if args[0].IsNull() || args[1].IsNull() {
		return Null(), nil
	}
	re, err := compileRe(args[1].AsString())
	if err != nil {
		return Value{}, fmt.Errorf("REGEXP_EXTRACT: %v", err)
	}
	m := re.FindStringSubmatch(args[0].AsString())
	if m == nil {
		return Null(), nil
	}
	if len(args) == 3 {
		if args[2].IsNull() {
			return Null(), nil
		}
		g, ok := args[2].AsInt()
		if !ok || g < 0 {
			return Value{}, fmt.Errorf("REGEXP_EXTRACT: group must be a non-negative integer")
		}
		if int(g) >= len(m) {
			return Null(), nil
		}
		return StringVal(m[g]), nil
	}
	// Default: the first capturing group, or the whole match if none.
	if len(m) > 1 {
		return StringVal(m[1]), nil
	}
	return StringVal(m[0]), nil
}

func funcRepeat(args []Value) (Value, error) {
	if len(args) != 2 {
		return Value{}, fmt.Errorf("REPEAT expects 2 args")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	n, ok := args[1].AsInt()
	if !ok || n < 0 {
		return Value{}, fmt.Errorf("REPEAT count must be non-negative integer")
	}
	return StringVal(strings.Repeat(args[0].AsString(), int(n))), nil
}

func funcReverse(args []Value) (Value, error) {
	if len(args) != 1 {
		return Value{}, fmt.Errorf("REVERSE expects 1 arg")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	r := []rune(args[0].AsString())
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return StringVal(string(r)), nil
}

func funcInitcap(args []Value) (Value, error) {
	if len(args) != 1 {
		return Value{}, fmt.Errorf("INITCAP expects 1 arg")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	// Capitalize the first letter of each whitespace-delimited word.
	return StringVal(strings.Title(strings.ToLower(args[0].AsString()))), nil
}

func padString(s string, n int64, pad string, left bool) string {
	r := []rune(s)
	if int64(len(r)) >= n {
		if left {
			return string(r[:n])
		}
		return string(r[int64(len(r))-n:])
	}
	need := n - int64(len(r))
	pr := []rune(pad)
	if len(pr) == 0 {
		pr = []rune(" ")
	}
	fill := strings.Repeat(string(pr), int(need/int64(len(pr)))+1)
	fillRunes := []rune(fill)[:need]
	if left {
		return string(fillRunes) + string(r)
	}
	return string(r) + string(fillRunes)
}

func funcLpad(args []Value) (Value, error) {
	if len(args) < 2 || len(args) > 3 {
		return Value{}, fmt.Errorf("LPAD expects 2 or 3 args")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	n, ok := args[1].AsInt()
	if !ok {
		return Value{}, fmt.Errorf("LPAD length must be integer")
	}
	pad := " "
	if len(args) == 3 && !args[2].IsNull() {
		pad = args[2].AsString()
	}
	return StringVal(padString(args[0].AsString(), n, pad, true)), nil
}

func funcRpad(args []Value) (Value, error) {
	if len(args) < 2 || len(args) > 3 {
		return Value{}, fmt.Errorf("RPAD expects 2 or 3 args")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	n, ok := args[1].AsInt()
	if !ok {
		return Value{}, fmt.Errorf("RPAD length must be integer")
	}
	pad := " "
	if len(args) == 3 && !args[2].IsNull() {
		pad = args[2].AsString()
	}
	return StringVal(padString(args[0].AsString(), n, pad, false)), nil
}

// ---- v0.3 time functions -----------------------------------------------------

// asTimeVal coerces a Value to a time.Time, returning a Null Value on failure.
func asTimeVal(v Value) (time.Time, bool) {
	if v.IsNull() {
		return time.Time{}, false
	}
	if v.Type == TypeTime {
		return v.V.(time.Time), true
	}
	// Try parsing the string, then unix seconds.
	s := strings.TrimSpace(v.AsString())
	if t, err := parseTime(s); err == nil {
		return t, true
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(n, 0), true
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Unix(int64(f), int64((f-float64(int64(f)))*1e9)), true
	}
	return time.Time{}, false
}

// parseInterval parses a Postgres-style interval literal given as a string
// like "1 day", "3 hours", "30 minutes". Returns the duration. Units: second(s),
// minute(s), hour(s), day(s), week(s), month(s, 30d), year(s, 365d).
func parseInterval(s string) (time.Duration, error) {
	parts := strings.Fields(strings.TrimSpace(s))
	if len(parts) == 0 {
		return 0, fmt.Errorf("empty interval")
	}
	// Accept either "N unit" pairs or a single number (seconds).
	if len(parts) == 1 {
		if f, err := strconv.ParseFloat(parts[0], 64); err == nil {
			return time.Duration(f * float64(time.Second)), nil
		}
		return 0, fmt.Errorf("bad interval %q", s)
	}
	n, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0, fmt.Errorf("bad interval amount %q", parts[0])
	}
	unit := strings.ToLower(strings.TrimSuffix(parts[1], "s"))
	switch unit {
	case "second":
		return time.Duration(n * float64(time.Second)), nil
	case "minute":
		return time.Duration(n * float64(time.Minute)), nil
	case "hour":
		return time.Duration(n * float64(time.Hour)), nil
	case "day":
		return time.Duration(n * float64(24 * time.Hour)), nil
	case "week":
		return time.Duration(n * float64(7 * 24 * time.Hour)), nil
	case "month":
		return time.Duration(n * float64(30 * 24 * time.Hour)), nil
	case "year":
		return time.Duration(n * float64(365 * 24 * time.Hour)), nil
	}
	return 0, fmt.Errorf("unknown interval unit %q", parts[1])
}

func funcDateTrunc(args []Value) (Value, error) {
	if len(args) != 2 {
		return Value{}, fmt.Errorf("DATE_TRUNC expects 2 args")
	}
	if args[1].IsNull() {
		return Null(), nil
	}
	unit := strings.ToLower(args[0].AsString())
	t, ok := asTimeVal(args[1])
	if !ok {
		return Null(), nil
	}
	switch unit {
	case "year":
		return TimeVal(time.Date(t.Year(), 1, 1, 0, 0, 0, 0, t.Location())), nil
	case "month":
		return TimeVal(time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())), nil
	case "day":
		return TimeVal(time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())), nil
	case "hour":
		return TimeVal(time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location())), nil
	case "minute":
		return TimeVal(time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, t.Location())), nil
	case "second":
		return TimeVal(time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, t.Location())), nil
	case "quarter":
		m := ((int(t.Month())-1)/3)*3 + 1
		return TimeVal(time.Date(t.Year(), time.Month(m), 1, 0, 0, 0, 0, t.Location())), nil
	case "week":
		// Truncate to Monday.
		days := int(t.Weekday()) - 1
		if days < 0 {
			days = 6
		}
		base := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
		return TimeVal(base.AddDate(0, 0, -days)), nil
	}
	return Value{}, fmt.Errorf("DATE_TRUNC: unknown unit %q", unit)
}

func funcDateAdd(args []Value) (Value, error) {
	if len(args) != 2 {
		return Value{}, fmt.Errorf("DATE_ADD expects 2 args")
	}
	if args[0].IsNull() || args[1].IsNull() {
		return Null(), nil
	}
	t, ok := asTimeVal(args[0])
	if !ok {
		return Value{}, fmt.Errorf("DATE_ADD: first arg must be a timestamp")
	}
	d, err := parseInterval(args[1].AsString())
	if err != nil {
		return Value{}, fmt.Errorf("DATE_ADD: %v", err)
	}
	return TimeVal(t.Add(d)), nil
}

var tzOffsetRe = regexp.MustCompile(`^([+-])(\d{2}):?(\d{2})?$`)

// loadZone resolves a zone name to a *time.Location: an IANA name
// ("America/Los_Angeles", "UTC", "Local"), or a fixed numeric offset
// ("-07:00", "-0700", "+05:30", "Z").
func loadZone(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.EqualFold(name, "Z") {
		return time.UTC, nil
	}
	if m := tzOffsetRe.FindStringSubmatch(name); m != nil {
		h, _ := strconv.Atoi(m[2])
		min := 0
		if m[3] != "" {
			min, _ = strconv.Atoi(m[3])
		}
		secs := (h*60 + min) * 60
		if m[1] == "-" {
			secs = -secs
		}
		return time.FixedZone(name, secs), nil
	}
	return time.LoadLocation(name)
}

// funcConvertTZ renders an instant in a target zone: same instant, the displayed
// offset (and EXTRACT/DATE_TRUNC of it) becomes zone-local. CONVERT_TZ(ts, zone).
func funcConvertTZ(args []Value) (Value, error) {
	if len(args) != 2 {
		return Value{}, fmt.Errorf("CONVERT_TZ expects 2 args (timestamp, zone)")
	}
	if args[0].IsNull() || args[1].IsNull() {
		return Null(), nil
	}
	t, ok := asTimeVal(args[0])
	if !ok {
		return Value{}, fmt.Errorf("CONVERT_TZ: first arg must be a timestamp")
	}
	loc, err := loadZone(args[1].AsString())
	if err != nil {
		return Value{}, fmt.Errorf("CONVERT_TZ: %v", err)
	}
	return TimeVal(t.In(loc)), nil
}

// funcFromTZ reinterprets the wall-clock fields of ts as local time in `zone`,
// yielding the correct instant — use to fix a zone-less timestamp the parser
// assumed UTC. FROM_TZ(ts, zone).
func funcFromTZ(args []Value) (Value, error) {
	if len(args) != 2 {
		return Value{}, fmt.Errorf("FROM_TZ expects 2 args (timestamp, zone)")
	}
	if args[0].IsNull() || args[1].IsNull() {
		return Null(), nil
	}
	t, ok := asTimeVal(args[0])
	if !ok {
		return Value{}, fmt.Errorf("FROM_TZ: first arg must be a timestamp")
	}
	loc, err := loadZone(args[1].AsString())
	if err != nil {
		return Value{}, fmt.Errorf("FROM_TZ: %v", err)
	}
	r := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), loc)
	return TimeVal(r), nil
}

func funcAge(args []Value) (Value, error) {
	if len(args) == 1 {
		if args[0].IsNull() {
			return Null(), nil
		}
		t, ok := asTimeVal(args[0])
		if !ok {
			return Null(), nil
		}
		return TimeVal(t), nil
	}
	if len(args) != 2 {
		return Value{}, fmt.Errorf("AGE expects 1 or 2 args")
	}
	if args[0].IsNull() || args[1].IsNull() {
		return Null(), nil
	}
	a, ok := asTimeVal(args[0])
	if !ok {
		return Null(), nil
	}
	b, ok := asTimeVal(args[1])
	if !ok {
		return Null(), nil
	}
	// Return the difference as a duration (TypeDuration).
	return Value{Type: TypeDuration, V: a.Sub(b)}, nil
}

func funcToTimestamp(args []Value) (Value, error) {
	if len(args) != 1 {
		return Value{}, fmt.Errorf("TO_TIMESTAMP expects 1 arg")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	f, ok := args[0].AsFloat()
	if !ok {
		return Value{}, fmt.Errorf("TO_TIMESTAMP requires numeric epoch")
	}
	sec := int64(f)
	nsec := int64((f - float64(sec)) * 1e9)
	return TimeVal(time.Unix(sec, nsec)), nil
}

func funcDate(args []Value) (Value, error) {
	if len(args) != 1 {
		return Value{}, fmt.Errorf("DATE expects 1 arg")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	t, ok := asTimeVal(args[0])
	if !ok {
		return Null(), nil
	}
	return TimeVal(time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())), nil
}

func funcStrftime(args []Value) (Value, error) {
	if len(args) != 2 {
		return Value{}, fmt.Errorf("STRFTIME expects 2 args")
	}
	if args[0].IsNull() || args[1].IsNull() {
		return Null(), nil
	}
	t, ok := asTimeVal(args[1])
	if !ok {
		return Null(), nil
	}
	layout := strftimeToGo(args[0].AsString())
	return StringVal(t.Format(layout)), nil
}

// strftimeToGo translates common strftime/%-specifiers to a Go reference layout.
// If the string contains no % specifiers it is returned unchanged so that Go
// reference layouts ("2006-01-02") work too.
var strftimeRepl = strings.NewReplacer(
	"%Y", "2006", "%y", "06",
	"%m", "01", "%d", "02",
	"%H", "15", "%M", "04", "%S", "05",
	"%p", "PM", "%I", "03",
	"%A", "Monday", "%a", "Mon",
	"%B", "January", "%b", "Jan",
	"%j", "002", "%z", "-0700", "%Z", "MST",
	"%%", "%",
)

func strftimeToGo(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	return strftimeRepl.Replace(s)
}

func funcCurrentDate(args []Value) (Value, error) {
	if len(args) != 0 {
		return Value{}, fmt.Errorf("CURRENT_DATE expects 0 args")
	}
	now := time.Now()
	return TimeVal(time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())), nil
}

// IsAggregate reports whether name is a recognized aggregate function.
func IsAggregate(name string) bool {
	switch strings.ToUpper(name) {
	case "COUNT", "SUM", "AVG", "MIN", "MAX",
		"MEDIAN", "STDDEV", "STDDEV_SAMP", "STDDEV_POP",
		"VARIANCE", "VAR_SAMP", "VAR_POP", "STRING_AGG":
		return true
	}
	return false
}