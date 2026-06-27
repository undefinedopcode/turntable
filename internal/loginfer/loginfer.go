// Package loginfer infers structure from unstructured log lines. It is a
// Drain-style template miner: tokenize each line, mask tokens to typed
// placeholders, cluster similar lines into templates, then emit, per template,
// a regular expression (named capture groups) and column metadata that the
// `log` connector's `pattern` option can consume directly.
//
// The mining core (tokenizer, masker, miner, finalize, align, naming) is adapted
// from an experiment by the project author; Infer/emit are the library surface.
package loginfer

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/april/turntable/internal/sql"
)

// ---------- token model ----------

type VarType string

const (
	VNone  VarType = ""
	VTS    VarType = "TS"
	VDATE  VarType = "DATE"
	VTIME  VarType = "TIME"
	VMON   VarType = "MON"
	VIP    VarType = "IP"
	VIP6   VarType = "IP6"
	VUUID  VarType = "UUID"
	VNUM   VarType = "NUM"
	VHEX   VarType = "HEX"
	VPATH  VarType = "PATH"
	VURL   VarType = "URL"
	VQSTR  VarType = "QSTR"
	VLEVEL VarType = "LEVEL"
	VBRACK VarType = "BRACKET"
	VPAREN VarType = "PAREN"
)

type Token struct {
	Raw    string
	Value  string
	Name   string // field-name hint (kv key), else ""
	Type   VarType
	Masked string // "<TYPE>", "key=<TYPE>", or the literal text
}

func (t Token) isVar() bool { return t.Type != VNone }

// ---------- maskers ----------

var (
	reRFC3339   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?$`)
	reCLFTS     = regexp.MustCompile(`^\d{1,2}/[A-Za-z]{3}/\d{4}:\d{2}:\d{2}:\d{2} [+-]\d{4}$`)
	reDate      = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	reTime      = regexp.MustCompile(`^\d{2}:\d{2}:\d{2}(?:\.\d+)?$`)
	reIP        = regexp.MustCompile(`^\d{1,3}(?:\.\d{1,3}){3}$`)
	reIP6       = regexp.MustCompile(`^(?:[0-9a-fA-F]{0,4}:){2,}[0-9a-fA-F]{0,4}$`)
	reUUID      = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	reNum       = regexp.MustCompile(`^-?\d+(?:\.\d+)?$`)
	reHex       = regexp.MustCompile(`^(?:0x)?[0-9a-fA-F]{6,}$`)
	rePath      = regexp.MustCompile(`^/[^\s]*$`)
	reURL       = regexp.MustCompile(`^https?://[^\s]+$`)
	reKV        = regexp.MustCompile(`^([A-Za-z_][\w.\-]*)=(.*)$`)
	reWordish   = regexp.MustCompile(`^[A-Za-z_][\w.\-]*$`)
	reHasLetter = regexp.MustCompile(`[a-fA-F]`)
	reHasDigit  = regexp.MustCompile(`\d`)
)

var months = map[string]bool{"Jan": true, "Feb": true, "Mar": true, "Apr": true, "May": true, "Jun": true,
	"Jul": true, "Aug": true, "Sep": true, "Oct": true, "Nov": true, "Dec": true}

var levels = map[string]bool{"TRACE": true, "DEBUG": true, "INFO": true, "NOTICE": true, "WARN": true,
	"WARNING": true, "ERROR": true, "ERR": true, "CRIT": true, "CRITICAL": true, "FATAL": true, "PANIC": true}

func maskValue(v string) (VarType, string) {
	switch {
	case reRFC3339.MatchString(v):
		return VTS, "<TS>"
	case reUUID.MatchString(v):
		return VUUID, "<UUID>"
	case reURL.MatchString(v):
		return VURL, "<URL>"
	case reIP.MatchString(v):
		return VIP, "<IP>"
	case rePath.MatchString(v):
		return VPATH, "<PATH>"
	case reDate.MatchString(v):
		return VDATE, "<DATE>"
	case reTime.MatchString(v):
		return VTIME, "<TIME>"
	case reNum.MatchString(v):
		return VNUM, "<NUM>"
	case reIP6.MatchString(v):
		return VIP6, "<IP6>"
	case reHex.MatchString(v) && reHasLetter.MatchString(v) && reHasDigit.MatchString(v):
		return VHEX, "<HEX>"
	}
	if levels[strings.ToUpper(v)] {
		return VLEVEL, "<LEVEL>"
	}
	if months[v] {
		return VMON, "<MON>"
	}
	return VNone, ""
}

func unquote(s string) string { return strings.Trim(s, `"'`) }

func maskToken(raw string) Token {
	if len(raw) >= 2 && raw[0] == '[' && raw[len(raw)-1] == ']' {
		inner := raw[1 : len(raw)-1]
		if reCLFTS.MatchString(inner) || reRFC3339.MatchString(inner) {
			return Token{raw, raw, "", VTS, "<TS>"}
		}
		if reWordish.MatchString(inner) {
			return Token{Raw: raw, Value: raw, Masked: raw}
		}
		return Token{raw, inner, "", VBRACK, "<BRACKET>"}
	}
	if len(raw) >= 2 && raw[0] == '(' && raw[len(raw)-1] == ')' {
		return Token{raw, raw[1 : len(raw)-1], "", VPAREN, "<PAREN>"}
	}
	if len(raw) >= 2 && (raw[0] == '"' || raw[0] == '\'') {
		return Token{raw, unquote(raw), "", VQSTR, "<QSTR>"}
	}
	if m := reKV.FindStringSubmatch(raw); m != nil {
		key, val := m[1], unquote(m[2])
		vt, mk := maskValue(val)
		if vt == VNone {
			return Token{Raw: raw, Value: val, Name: key, Type: VNone, Masked: key + "=<*>"}
		}
		return Token{Raw: raw, Value: val, Name: key, Type: vt, Masked: key + "=" + mk}
	}
	if vt, mk := maskValue(raw); vt != VNone {
		return Token{Raw: raw, Value: raw, Type: vt, Masked: mk}
	}
	return Token{Raw: raw, Value: raw, Masked: raw}
}

// ---------- tokenizer ----------

func splitTokens(line string) []string {
	var toks []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			toks = append(toks, b.String())
			b.Reset()
		}
	}
	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == ' ' || c == '\t':
			flush()
		case (c == '"' || c == '\'') && (b.Len() == 0 || strings.HasSuffix(b.String(), "=")):
			standalone := b.Len() == 0
			b.WriteRune(c)
			for i++; i < len(rs); i++ {
				b.WriteRune(rs[i])
				if rs[i] == c {
					break
				}
			}
			if standalone {
				flush()
			}
		case c == '[' && b.Len() == 0:
			depth := 0
			for ; i < len(rs); i++ {
				if rs[i] == '[' {
					depth++
				} else if rs[i] == ']' {
					depth--
				}
				b.WriteRune(rs[i])
				if depth == 0 {
					break
				}
			}
			flush()
		default:
			b.WriteRune(c)
		}
	}
	flush()
	return toks
}

func tokenize(line string) []Token {
	parts := splitTokens(line)
	toks := make([]Token, len(parts))
	for i, p := range parts {
		toks[i] = maskToken(p)
	}
	return mergeTimestamps(toks)
}

func mergeTimestamps(in []Token) []Token {
	var out []Token
	for i := 0; i < len(in); i++ {
		if i+1 < len(in) && in[i].Type == VDATE && in[i+1].Type == VTIME {
			v := in[i].Raw + " " + in[i+1].Raw
			out = append(out, Token{Raw: v, Value: v, Type: VTS, Masked: "<TS>"})
			i++
			continue
		}
		if i+2 < len(in) && in[i].Type == VMON && in[i+1].Type == VNUM && in[i+2].Type == VTIME {
			v := in[i].Raw + " " + in[i+1].Raw + " " + in[i+2].Raw
			out = append(out, Token{Raw: v, Value: v, Type: VTS, Masked: "<TS>"})
			i += 2
			continue
		}
		out = append(out, in[i])
	}
	return out
}

// ---------- miner ----------

type cluster struct {
	id       int
	template []string
	count    int
}

type miner struct {
	buckets  map[string][]*cluster
	all      []*cluster
	byID     map[int]*cluster
	redirect map[int]int
	st       float64
}

func newMiner(st float64) *miner {
	return &miner{buckets: map[string][]*cluster{}, byID: map[int]*cluster{}, redirect: map[int]int{}, st: st}
}

// reDigitRun collapses digit sequences so numeric-variant tokens share a key.
var reDigitRun = regexp.MustCompile(`\d+`)

// bucketStem normalizes a token for the bucket key: digit runs become '#', so
// worker-3 / worker-1 / worker-42 land in the same bucket and can cluster (the
// similarity threshold still gates the actual merge, so this only widens what is
// compared). Masked placeholders (<TS>, key=<NUM>) carry no bare digits and pass
// through unchanged.
func bucketStem(tok string) string {
	return reDigitRun.ReplaceAllString(tok, "#")
}

func (m *miner) key(masked []string) string {
	first := ""
	if len(masked) > 0 {
		first = bucketStem(masked[0])
	}
	return fmt.Sprintf("%d\x00%s", len(masked), first)
}

func sim(tmpl, masked []string) float64 {
	if len(tmpl) != len(masked) {
		return -1
	}
	if len(tmpl) == 0 {
		return 1
	}
	match := 0
	for i := range tmpl {
		if tmpl[i] == masked[i] || tmpl[i] == "<*>" {
			match++
		}
	}
	return float64(match) / float64(len(tmpl))
}

func (m *miner) add(masked []string) *cluster {
	k := m.key(masked)
	var best *cluster
	bestSim := -1.0
	for _, c := range m.buckets[k] {
		if s := sim(c.template, masked); s > bestSim {
			bestSim, best = s, c
		}
	}
	if best != nil && bestSim >= m.st {
		for i := range best.template {
			if best.template[i] != masked[i] {
				best.template[i] = "<*>"
			}
		}
		best.count++
		return best
	}
	c := &cluster{id: len(m.all) + 1, template: append([]string(nil), masked...), count: 1}
	m.buckets[k] = append(m.buckets[k], c)
	m.all = append(m.all, c)
	m.byID[c.id] = c
	return c
}

func (m *miner) resolve(id int) int {
	for {
		r, ok := m.redirect[id]
		if !ok {
			return id
		}
		id = r
	}
}

// ---------- finalize ----------

func isPlaceholder(s string) bool { return strings.HasPrefix(s, "<") || strings.Contains(s, "=<") }

func structured(s string) bool { return s != "<*>" && s != "<MSG>" }

func coalesce(t []string) []string {
	var out []string
	for i := 0; i < len(t); {
		if t[i] == "<*>" {
			j := i
			for j < len(t) && t[j] == "<*>" {
				j++
			}
			if j-i >= 2 {
				out = append(out, "<MSG>")
			} else {
				out = append(out, t[i:j]...)
			}
			i = j
		} else {
			out = append(out, t[i])
			i++
		}
	}
	return out
}

func fillable(mid []string) bool {
	for _, t := range mid {
		if t == "<MSG>" || t == "<*>" {
			continue
		}
		if isPlaceholder(t) {
			return false
		}
	}
	return true
}

func commonPrefix(a, b []string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

func commonSuffix(a, b []string, cap int) int {
	n := 0
	for n < len(a)-cap && n < len(b)-cap && a[len(a)-1-n] == b[len(b)-1-n] {
		n++
	}
	return n
}

func mergeTemplates(a, b []string) ([]string, bool) {
	p := commonPrefix(a, b)
	s := commonSuffix(a, b, p)
	midA, midB := a[p:len(a)-s], b[p:len(b)-s]
	if len(midA) == 0 && len(midB) == 0 {
		return nil, false
	}
	if !fillable(midA) || !fillable(midB) {
		return nil, false
	}
	textLike := func(m []string) bool {
		if len(m) > 1 {
			return true
		}
		for _, t := range m {
			if t == "<MSG>" || t == "<*>" {
				return true
			}
		}
		return false
	}
	if !textLike(midA) && !textLike(midB) {
		return nil, false
	}
	out := append([]string{}, a[:p]...)
	out = append(out, "<MSG>")
	out = append(out, a[len(a)-s:]...)
	return out, true
}

func (m *miner) finalize() {
	for _, c := range m.all {
		c.template = coalesce(c.template)
	}
	live := append([]*cluster(nil), m.all...)
	for {
		merged := false
		for i := 0; i < len(live); i++ {
			for j := i + 1; j < len(live); j++ {
				if t, ok := mergeTemplates(live[i].template, live[j].template); ok {
					live[i].template = t
					live[i].count += live[j].count
					m.redirect[live[j].id] = live[i].id
					live = append(live[:j], live[j+1:]...)
					merged = true
					j--
				}
			}
		}
		if !merged {
			break
		}
	}
}

func (m *miner) templates() []*cluster {
	var live []*cluster
	for _, c := range m.all {
		if _, gone := m.redirect[c.id]; !gone {
			live = append(live, c)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].id < live[j].id })
	return live
}

// ---------- field naming ----------

var vocab = map[string]string{"for": "user", "from": "source", "to": "dest", "by": "actor", "user": "user"}

var defaultNames = map[VarType]string{VTS: "time", VDATE: "date", VTIME: "time", VIP: "ip", VIP6: "ip",
	VUUID: "uuid", VNUM: "num", VHEX: "hash", VPATH: "path", VURL: "url", VQSTR: "text", VLEVEL: "level",
	VBRACK: "tag", VPAREN: "detail", VNone: "value"}

func sanitize(s string) string {
	s = strings.ToLower(strings.TrimFunc(s, func(r rune) bool {
		return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9')
	}))
	if s == "" {
		return "field"
	}
	return s
}

// safeIdent keeps a generated column name out of the SQL dialect's reserved
// words, so it can be referenced bare in a query (e.g. a field named after the
// literal "in" or "count" becomes "in_"/"count_"). The user can still rename it.
func safeIdent(name string) string {
	for sql.IsKeyword(name) {
		name += "_"
	}
	return name
}

func nameFor(tmpl []string, tk Token, idx int) string {
	if tk.Name != "" {
		return sanitize(tk.Name)
	}
	leftLit := ""
	if idx > 0 && structured(tmpl[idx-1]) && !isPlaceholder(tmpl[idx-1]) {
		leftLit = sanitize(strings.Trim(tmpl[idx-1], "[]"))
	}
	if tk.isVar() {
		if v, ok := vocab[leftLit]; ok {
			return v
		}
		if leftLit != "" && reWordish.MatchString(leftLit) && len(leftLit) <= 24 {
			return leftLit
		}
		return defaultNames[tk.Type]
	}
	if v, ok := vocab[leftLit]; ok {
		return v
	}
	if leftLit != "" {
		return leftLit
	}
	return fmt.Sprintf("field_%d", idx)
}

// ---------- alignment ----------

type slot struct {
	toks  []Token
	tIdx  int
	isMsg bool
}

func align(tmpl []string, toks []Token) []slot {
	var slots []slot
	ti := 0
	for k := 0; k < len(tmpl); k++ {
		if tmpl[k] == "<MSG>" {
			ai := -1
			for x := k + 1; x < len(tmpl); x++ {
				if structured(tmpl[x]) {
					ai = x
					break
				}
			}
			start := ti
			if ai < 0 {
				ti = len(toks)
			} else {
				anchor := tmpl[ai]
				for ti < len(toks) && toks[ti].Masked != anchor {
					ti++
				}
			}
			slots = append(slots, slot{toks: toks[start:ti], tIdx: k, isMsg: true})
		} else {
			if ti < len(toks) {
				slots = append(slots, slot{toks: toks[ti : ti+1], tIdx: k})
				ti++
			}
		}
	}
	return slots
}

// ---------- public API: Infer + pattern emission ----------

// Column is one named, typed output column of an inferred template.
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// Template is one inferred log structure: a readable label, the share of lines
// it covers, a ready-to-use regular expression (named groups) for the `log`
// connector's `pattern` option, the resulting columns, and a parsed sample row.
type Template struct {
	Label      string   `json:"label"`
	Count      int      `json:"count"`
	Pattern    string   `json:"pattern"`
	Columns    []Column `json:"columns"`
	Sample     []string `json:"sample"`
	SampleLine string   `json:"sample_line"`
	// Common marks the synthesized "matches (almost) all rows" template — the
	// shared leading structure with the variable tail as one message field.
	Common bool `json:"common"`
}

// Infer mines the lines into templates and emits a regex + columns for each,
// ordered by coverage (most lines first). Templates whose emitted pattern does
// not match their own sample are dropped.
func Infer(lines []string) []Template {
	m := newMiner(0.5)
	type rec struct {
		raw  string
		toks []Token
		cid  int
	}
	var recs []rec
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		toks := tokenize(ln)
		masked := make([]string, len(toks))
		for i, t := range toks {
			masked[i] = t.Masked
		}
		c := m.add(masked)
		recs = append(recs, rec{ln, toks, c.id})
	}
	m.finalize()

	repToks := map[int][]Token{}
	repLine := map[int]string{}
	for _, r := range recs {
		sid := m.resolve(r.cid)
		if _, ok := repToks[sid]; !ok {
			repToks[sid] = r.toks
			repLine[sid] = r.raw
		}
	}

	var out []Template
	for _, c := range m.templates() {
		pat, cols, sample := emit(c.template, repToks[c.id])
		if pat == "" {
			continue
		}
		re, err := regexp.Compile(pat)
		if err != nil || re.FindStringSubmatch(repLine[c.id]) == nil {
			continue // self-check: a pattern that can't parse its own line is useless
		}
		out = append(out, Template{
			Label:      strings.Join(c.template, " "),
			Count:      c.count,
			Pattern:    pat,
			Columns:    cols,
			Sample:     sample,
			SampleLine: repLine[c.id],
		})
	}
	// Synthesize a single "matches (almost) all rows" template from the structure
	// the lines share — the common case for regular logs (a fixed header + a
	// free-form message tail) that the per-bucket miner over-splits. Offer it
	// (first, since it covers the most) only when it covers a majority AND beats
	// every mined template on coverage — i.e. when the miner over-split. If a
	// single mined template already covers everything, the catch-all adds nothing.
	bestMined := 0
	for _, t := range out {
		if t.Count > bestMined {
			bestMined = t.Count
		}
	}
	if tmpl, repToks, repLn, cov, ok := commonTemplate(lines); ok && cov > bestMined && cov*2 >= len(recs) {
		if pat, cols, sample := emit(tmpl, repToks); pat != "" {
			if re, err := regexp.Compile(pat); err == nil && re.FindStringSubmatch(repLn) != nil {
				common := Template{
					Label: strings.Join(tmpl, " "), Count: cov, Pattern: pat,
					Columns: cols, Sample: sample, SampleLine: repLn, Common: true,
				}
				// Drop any mined template whose pattern the common one duplicates.
				deduped := out[:0]
				for _, t := range out {
					if t.Pattern != pat {
						deduped = append(deduped, t)
					}
				}
				out = append([]Template{common}, deduped...)
			}
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

// commonTemplate finds the leading structure shared by (almost) all lines: the
// longest run of positions where they agree on a token class — a fixed type
// (<TS>/<NUM>/…), a constant literal, or "some single word" (a varying field) —
// with everything after collapsed into <MSG>. This is the "all rows" pattern for
// regular logs. It tolerates a minority of outliers (stack traces etc.); ok is
// false when no broad common prefix exists. Returns the template, a
// representative line's tokens + text, and how many lines it covers.
func commonTemplate(lines []string) ([]string, []Token, string, int, bool) {
	type rec struct {
		raw  string
		toks []Token
	}
	var recs []rec
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		recs = append(recs, rec{ln, tokenize(ln)})
	}
	total := len(recs)
	if total == 0 {
		return nil, nil, "", 0, false
	}
	classOf := func(t Token) string {
		switch {
		case t.Type != VNone:
			return "t:" + string(t.Type)
		case t.Name != "":
			return "k:" + t.Name
		default:
			return "w" // a plain word (constant or varying)
		}
	}

	active := make([]int, total)
	for i := range active {
		active[i] = i
	}
	var tmpl []string
	for pos := 0; ; pos++ {
		counts := map[string]int{}
		for _, li := range active {
			if pos < len(recs[li].toks) {
				counts[classOf(recs[li].toks[pos])]++
			}
		}
		dom, domN := "", 0
		for c, n := range counts {
			if n > domN {
				dom, domN = c, n
			}
		}
		// Stop once the shared structure no longer covers most lines.
		if domN*100 < total*85 {
			break
		}
		var next []int
		sameVal, first, firstVal := true, true, ""
		for _, li := range active {
			if pos < len(recs[li].toks) && classOf(recs[li].toks[pos]) == dom {
				next = append(next, li)
				v := recs[li].toks[pos].Value
				if first {
					firstVal, first = v, false
				} else if v != firstVal {
					sameVal = false
				}
			}
		}
		active = next
		rep := recs[active[0]].toks[pos]
		switch {
		case dom == "w" && sameVal:
			tmpl = append(tmpl, rep.Raw) // constant word → literal anchor
		case dom == "w":
			tmpl = append(tmpl, "<*>") // varying word → field
		default:
			tmpl = append(tmpl, rep.Masked) // typed / key=value
		}
	}
	if len(tmpl) == 0 {
		return nil, nil, "", 0, false
	}
	for _, li := range active {
		if len(recs[li].toks) > len(tmpl) {
			tmpl = append(tmpl, "<MSG>")
			break
		}
	}
	return tmpl, recs[active[0]].toks, recs[active[0]].raw, len(active), true
}

// emit turns a template (aligned to a representative line's tokens) into a regex
// with named groups, the column list, and the sample values.
func emit(tmpl []string, toks []Token) (string, []Column, []string) {
	if len(toks) == 0 {
		return "", nil, nil
	}
	var frags []string
	var cols []Column
	var sample []string
	used := map[string]int{}
	uniq := func(n string) string {
		used[n]++
		if used[n] == 1 {
			return n
		}
		return fmt.Sprintf("%s_%d", n, used[n])
	}
	for _, sl := range align(tmpl, toks) {
		mask := tmpl[sl.tIdx]
		if sl.isMsg {
			name := uniq("message")
			frags = append(frags, fmt.Sprintf(`(?P<%s>.*)`, name))
			cols = append(cols, Column{name, "string"})
			parts := make([]string, len(sl.toks))
			for i, t := range sl.toks {
				parts[i] = t.Raw
			}
			sample = append(sample, strings.Join(parts, " "))
			continue
		}
		if len(sl.toks) == 0 {
			continue
		}
		// A constant literal token becomes a fixed anchor (no column).
		if structured(mask) && !isPlaceholder(mask) {
			frags = append(frags, regexp.QuoteMeta(sl.toks[0].Raw))
			continue
		}
		tk := sl.toks[0]
		name := uniq(safeIdent(sanitize(nameFor(tmpl, tk, sl.tIdx))))
		frag, typ := groupRegex(tk, name)
		frags = append(frags, frag)
		cols = append(cols, Column{name, typ})
		sample = append(sample, tk.Value)
	}
	if len(cols) == 0 {
		return "", nil, nil
	}
	return "^" + strings.Join(frags, `\s+`) + "$", cols, sample
}

// groupRegex returns a named-capture regex fragment and a display type for a
// variable token. A key=value pair anchors the literal `key=` before the value
// capture.
func groupRegex(tk Token, name string) (string, string) {
	frag, typ := valueRegex(tk, name)
	if tk.Name != "" {
		return regexp.QuoteMeta(tk.Name) + "=" + frag, typ
	}
	return frag, typ
}

// valueRegex emits the typed capture for a token's value (no kv key prefix).
func valueRegex(tk Token, name string) (string, string) {
	switch tk.Type {
	case VTS:
		if strings.HasPrefix(tk.Raw, "[") {
			return fmt.Sprintf(`\[(?P<%s>[^\]]+)\]`, name), "time"
		}
		// A merged timestamp (DATE TIME, or MON DAY TIME) spans several
		// space-separated fields; match that many so the group covers it.
		fields := strings.Count(tk.Raw, " ") + 1
		parts := make([]string, fields)
		for i := range parts {
			parts[i] = `\S+`
		}
		return fmt.Sprintf(`(?P<%s>%s)`, name, strings.Join(parts, ` `)), "time"
	case VNUM:
		return fmt.Sprintf(`(?P<%s>-?\d+(?:\.\d+)?)`, name), "number"
	case VPAREN:
		return fmt.Sprintf(`\((?P<%s>[^)]*)\)`, name), "string"
	case VBRACK:
		return fmt.Sprintf(`\[(?P<%s>[^\]]*)\]`, name), "string"
	case VQSTR:
		return fmt.Sprintf(`"(?P<%s>[^"]*)"`, name), "string"
	case VLEVEL:
		return fmt.Sprintf(`(?P<%s>\w+)`, name), "string"
	}
	return fmt.Sprintf(`(?P<%s>\S+)`, name), "string"
}

// RenameColumn returns a copy of pattern with capture group `from` renamed to
// `to` — used by the UI when a user edits an inferred column name. Names are
// unique, so a single textual replacement is exact.
func RenameColumn(pattern, from, to string) string {
	return strings.Replace(pattern, "(?P<"+from+">", "(?P<"+to+">", 1)
}
