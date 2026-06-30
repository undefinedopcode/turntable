package ttplugin

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Predicate is a decoded WHERE pushdown tree — the JSON subset documented in
// PLUGINS.md. The SDK decodes it from the scan request and, in automatic mode,
// evaluates it against each row for you. Plugins that push predicates down to
// their own backend (ManualPushdown) can walk this tree themselves.
type Predicate struct {
	Kind        string      `json:"kind"` // compare|and|or|not|in|between|like|isnull
	Op          string      `json:"op,omitempty"`
	Column      string      `json:"column,omitempty"`
	Value       *Literal    `json:"value,omitempty"`
	Values      []Literal   `json:"values,omitempty"`
	Low         *Literal    `json:"low,omitempty"`
	High        *Literal    `json:"high,omitempty"`
	Pattern     string      `json:"pattern,omitempty"`
	Insensitive bool        `json:"insensitive,omitempty"`
	Negate      bool        `json:"negate,omitempty"`
	Args        []Predicate `json:"args,omitempty"`
	Arg         *Predicate  `json:"arg,omitempty"`
}

// Literal is a typed constant in a predicate: type is int|float|string|bool|null.
type Literal struct {
	Type  string `json:"type"`
	Value any    `json:"value"`
}

// Eval reports whether a row satisfies the predicate. get returns the value of a
// column by name (nil for SQL NULL / unknown column). The SDK uses this for
// automatic filtering; it is exported so manual-pushdown plugins can reuse it.
func (p *Predicate) Eval(get func(column string) any) bool {
	switch p.Kind {
	case "and":
		for i := range p.Args {
			if !p.Args[i].Eval(get) {
				return false
			}
		}
		return true
	case "or":
		for i := range p.Args {
			if p.Args[i].Eval(get) {
				return true
			}
		}
		return false
	case "not":
		return p.Arg == nil || !p.Arg.Eval(get)
	case "isnull":
		return (get(p.Column) == nil) != p.Negate
	case "in":
		v := get(p.Column)
		for i := range p.Values {
			if compare(v, "=", p.Values[i]) {
				return !p.Negate
			}
		}
		return p.Negate
	case "between":
		v := get(p.Column)
		if p.Low == nil || p.High == nil {
			return false
		}
		in := compare(v, ">=", *p.Low) && compare(v, "<=", *p.High)
		return in != p.Negate
	case "like":
		v := get(p.Column)
		s, ok := v.(string)
		if !ok {
			if v == nil {
				return false
			}
			s = fmt.Sprint(v)
		}
		return likeMatch(s, p.Pattern, p.Insensitive) != p.Negate
	case "compare":
		if p.Value == nil {
			return false
		}
		return compare(get(p.Column), p.Op, *p.Value)
	}
	return false
}

// compare evaluates `cellValue OP literal`. NULL compares false to everything.
// Numbers compare numerically, times against an RFC3339/parseable string
// literal, everything else lexically.
func compare(v any, op string, lit Literal) bool {
	if v == nil || lit.Type == "null" {
		return false
	}
	if t, ok := v.(time.Time); ok {
		if s, ok := lit.Value.(string); ok {
			if lt, err := parseTime(s); err == nil {
				return numCmp(float64(t.UnixNano()), float64(lt.UnixNano()), op)
			}
		}
	}
	switch lit.Type {
	case "int", "float":
		a, aok := toFloat(v)
		b, bok := toFloat(lit.Value)
		if !aok || !bok {
			return false
		}
		return numCmp(a, b, op)
	case "bool":
		a, aok := v.(bool)
		b, bok := lit.Value.(bool)
		if !aok || !bok {
			return false
		}
		switch op {
		case "=":
			return a == b
		case "<>":
			return a != b
		}
		return false
	default:
		return strCmp(fmt.Sprint(v), fmt.Sprint(lit.Value), op)
	}
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

func numCmp(a, b float64, op string) bool {
	switch op {
	case "=":
		return a == b
	case "<>":
		return a != b
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	case ">=":
		return a >= b
	}
	return false
}

func strCmp(a, b, op string) bool {
	switch op {
	case "=":
		return a == b
	case "<>":
		return a != b
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	case ">=":
		return a >= b
	}
	return false
}

// likeMatch implements SQL LIKE: % matches any run, _ matches one character.
func likeMatch(s, pattern string, insensitive bool) bool {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	expr := b.String()
	if insensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return false
	}
	return re.MatchString(s)
}

func parseTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable time %q", s)
}
