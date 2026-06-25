// Package logc is a file connector that reads plain-text log files, detecting
// the format automatically (JSON lines, Apache/nginx access logs, syslog,
// logfmt, or a generic timestamped/leveled line) and exposing parsed, typed
// rows. The format can be forced with a `format` option, or a custom regular
// expression supplied via `pattern` (named groups become columns).
package logc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

type Connector struct{}

func New() *Connector { return &Connector{} }

func (Connector) Name() string { return "log" }

func (Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) {
	return nil, nil
}

func (Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	f, err := detect(ds.Source, ds.Options)
	if err != nil {
		return engine.Schema{}, err
	}
	return f.schema, nil
}

func (Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	f, err := detect(req.Dataset.Source, req.Dataset.Options)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(req.Dataset.Source)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", req.Dataset.Source, err)
	}
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024) // tolerate long lines
	return &logIter{file: file, sc: sc, fmt: f}, nil
}

// ---- format -----------------------------------------------------------------

// format is a detected (or forced) log layout: a schema plus a per-line parser.
// rawCol is the column index that receives the original text of a line the
// parser cannot handle (a "message"/"line" column), or -1 to drop it.
type format struct {
	name   string
	schema engine.Schema
	parse  func(line string) ([]engine.Value, bool)
	rawCol int
}

func (f *format) unmatched(line string) []engine.Value {
	vals := make([]engine.Value, len(f.schema.Columns))
	for i := range vals {
		vals[i] = engine.Null()
	}
	if f.rawCol >= 0 {
		vals[f.rawCol] = engine.StringVal(line)
	}
	return vals
}

type logIter struct {
	file   *os.File
	sc     *bufio.Scanner
	fmt    *format
	closed bool
}

func (it *logIter) Next() (engine.Row, bool, error) {
	for it.sc.Scan() {
		line := it.sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		vals, ok := it.fmt.parse(line)
		if !ok {
			vals = it.fmt.unmatched(line)
		}
		return engine.Row{Values: vals}, true, nil
	}
	return engine.Row{}, false, it.sc.Err()
}

func (it *logIter) Close() error {
	if it.closed {
		return nil
	}
	it.closed = true
	return it.file.Close()
}

// ---- detection --------------------------------------------------------------

// detectThreshold is the minimum fraction of sampled lines a structured format
// must parse to be selected over the raw fallback.
const detectThreshold = 0.7

// detect chooses the format for a file: an explicit `pattern`, then a forced
// `format`, else the best-matching auto-detected format (raw if none clear the
// threshold).
func detect(path string, opts map[string]any) (*format, error) {
	if pat := stringOpt(opts, "pattern"); pat != "" {
		return patternFormat(pat)
	}
	sample, err := readSample(path, 200)
	if err != nil {
		return nil, err
	}
	if forced := stringOpt(opts, "format"); forced != "" && forced != "auto" {
		f := buildFormat(forced, sample)
		if f == nil {
			return nil, fmt.Errorf("unknown log format %q (json|logfmt|clf|combined|syslog|bracketed|leveled|raw)", forced)
		}
		return f, nil
	}
	// Specificity order: the first format that parses enough of the sample wins.
	for _, name := range []string{"json", "combined", "common", "syslog", "bracketed", "logfmt", "leveled"} {
		f := buildFormat(name, sample)
		if f != nil && matchRatio(f, sample) >= detectThreshold {
			return f, nil
		}
	}
	return buildFormat("raw", sample), nil
}

func matchRatio(f *format, sample []string) float64 {
	if len(sample) == 0 {
		return 0
	}
	n := 0
	for _, line := range sample {
		if _, ok := f.parse(line); ok {
			n++
		}
	}
	return float64(n) / float64(len(sample))
}

func readSample(path string, n int) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	defer file.Close()
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var out []string
	for sc.Scan() && len(out) < n {
		if line := sc.Text(); strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out, sc.Err()
}

func buildFormat(name string, sample []string) *format {
	switch name {
	case "json":
		return jsonFormat(sample)
	case "logfmt":
		return logfmtFormat(sample)
	case "combined":
		return regexFormat("combined", combinedRe, combinedCols)
	case "common", "clf":
		return regexFormat("common", commonRe, commonCols)
	case "syslog":
		return syslogFormat()
	case "bracketed":
		return bracketedFormat()
	case "leveled":
		return leveledFormat()
	case "raw":
		return rawFormat()
	}
	return nil
}

// ---- JSON lines -------------------------------------------------------------

func jsonFormat(sample []string) *format {
	order, seen := []string{}, map[string]bool{}
	for _, line := range sample {
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) != nil {
			continue
		}
		for k := range obj {
			if !seen[k] {
				seen[k], order = true, append(order, k)
			}
		}
	}
	sort.Strings(order)
	cols := make([]engine.Column, len(order))
	for i, k := range order {
		cols[i] = engine.Column{Name: k, Type: engine.TypeAny, Nullable: true}
	}
	rawCol := indexOf(order, "message")
	parse := func(line string) ([]engine.Value, bool) {
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) != nil {
			return nil, false
		}
		vals := make([]engine.Value, len(order))
		for i, k := range order {
			if v, ok := obj[k]; ok {
				vals[i] = connector.FromAny(v)
			} else {
				vals[i] = engine.Null()
			}
		}
		return vals, true
	}
	return &format{name: "json", schema: engine.Schema{Columns: cols}, parse: parse, rawCol: rawCol}
}

// ---- logfmt -----------------------------------------------------------------

var logfmtPair = regexp.MustCompile(`([A-Za-z0-9_.\-]+)=(?:"((?:[^"\\]|\\.)*)"|(\S*))`)

func logfmtPairs(line string) map[string]string {
	out := map[string]string{}
	for _, m := range logfmtPair.FindAllStringSubmatch(line, -1) {
		val := m[3] // unquoted value
		// A quoted value puts a `"` right after the `=`; use the unescaped group.
		if eq := strings.IndexByte(m[0], '='); eq >= 0 && eq+1 < len(m[0]) && m[0][eq+1] == '"' {
			val = strings.ReplaceAll(m[2], `\"`, `"`)
		}
		out[m[1]] = val
	}
	return out
}

func logfmtFormat(sample []string) *format {
	order, seen := []string{}, map[string]bool{}
	pairy := 0
	for _, line := range sample {
		p := logfmtPairs(line)
		if len(p) >= 2 {
			pairy++
		}
		for k := range p {
			if !seen[k] {
				seen[k], order = true, append(order, k)
			}
		}
	}
	if len(order) == 0 {
		return nil
	}
	sort.Strings(order)
	cols := make([]engine.Column, len(order))
	for i, k := range order {
		cols[i] = engine.Column{Name: k, Type: engine.TypeAny, Nullable: true}
	}
	parse := func(line string) ([]engine.Value, bool) {
		p := logfmtPairs(line)
		if len(p) < 2 {
			return nil, false
		}
		vals := make([]engine.Value, len(order))
		for i, k := range order {
			if s, ok := p[k]; ok {
				vals[i] = coerceScalar(s)
			} else {
				vals[i] = engine.Null()
			}
		}
		return vals, true
	}
	return &format{name: "logfmt", schema: engine.Schema{Columns: cols}, parse: parse, rawCol: indexOf(order, "msg")}
}

// ---- Apache/nginx access logs ----------------------------------------------

var (
	combinedRe = regexp.MustCompile(`^(\S+) (\S+) (\S+) \[([^\]]+)\] "([^"]*)" (\d{3}) (\S+) "([^"]*)" "([^"]*)"`)
	commonRe   = regexp.MustCompile(`^(\S+) (\S+) (\S+) \[([^\]]+)\] "([^"]*)" (\d{3}) (\S+)\s*$`)

	combinedCols = []string{"host", "ident", "user", "time", "method", "path", "protocol", "status", "bytes", "referer", "user_agent"}
	commonCols   = []string{"host", "ident", "user", "time", "method", "path", "protocol", "status", "bytes"}
)

// regexFormat builds the CLF/combined parser. The captured request string is
// split into method/path/protocol; status/bytes are typed; time is parsed.
func regexFormat(name string, re *regexp.Regexp, cols []string) *format {
	colType := func(c string) engine.Type {
		switch c {
		case "status", "bytes":
			return engine.TypeInt
		case "time":
			return engine.TypeTime
		default:
			return engine.TypeString
		}
	}
	schemaCols := make([]engine.Column, len(cols))
	for i, c := range cols {
		schemaCols[i] = engine.Column{Name: c, Type: colType(c), Nullable: true}
	}
	combined := name == "combined"
	parse := func(line string) ([]engine.Value, bool) {
		m := re.FindStringSubmatch(line)
		if m == nil {
			return nil, false
		}
		method, path, proto := splitRequest(m[5])
		vals := []engine.Value{
			engine.StringVal(m[1]), nullDash(m[2]), nullDash(m[3]),
			timeOrNull(m[4]), strOrNull(method), strOrNull(path), strOrNull(proto),
			intOrNull(m[6]), nullDashInt(m[7]),
		}
		if combined {
			vals = append(vals, nullDash(m[8]), nullDash(m[9]))
		}
		return vals, true
	}
	return &format{name: name, schema: engine.Schema{Columns: schemaCols}, parse: parse, rawCol: -1}
}

func splitRequest(req string) (method, path, proto string) {
	parts := strings.SplitN(req, " ", 3)
	switch len(parts) {
	case 3:
		return parts[0], parts[1], parts[2]
	case 2:
		return parts[0], parts[1], ""
	case 1:
		return "", parts[0], ""
	}
	return "", "", ""
}

// ---- syslog (RFC3164) -------------------------------------------------------

var syslogRe = regexp.MustCompile(`^(\w{3}\s+\d{1,2} \d{2}:\d{2}:\d{2}) (\S+) ([^:\[\s]+)(?:\[(\d+)\])?: (.*)$`)

func syslogFormat() *format {
	cols := []engine.Column{
		{Name: "time", Type: engine.TypeTime, Nullable: true},
		{Name: "host", Type: engine.TypeString, Nullable: true},
		{Name: "tag", Type: engine.TypeString, Nullable: true},
		{Name: "pid", Type: engine.TypeInt, Nullable: true},
		{Name: "message", Type: engine.TypeString, Nullable: true},
	}
	parse := func(line string) ([]engine.Value, bool) {
		m := syslogRe.FindStringSubmatch(line)
		if m == nil {
			return nil, false
		}
		return []engine.Value{
			timeOrNull(m[1]), strOrNull(m[2]), strOrNull(m[3]), nullDashInt(m[4]), strOrNull(m[5]),
		}, true
	}
	return &format{name: "syslog", schema: engine.Schema{Columns: cols}, parse: parse, rawCol: 4}
}

// ---- bracketed (pacman/ALPM and similar) ------------------------------------

// bracketedRe matches `[timestamp] [component] message`, used by pacman/ALPM,
// some installers, and other tools, e.g.
//
//	[2026-05-25T20:44:11-0700] [ALPM] installed glib2 (2.88.1-1)
var bracketedRe = regexp.MustCompile(`^\[([^\]]+)\] \[([^\]]+)\] (.*)$`)

func bracketedFormat() *format {
	cols := []engine.Column{
		{Name: "time", Type: engine.TypeTime, Nullable: true},
		{Name: "component", Type: engine.TypeString, Nullable: true},
		{Name: "message", Type: engine.TypeString, Nullable: true},
	}
	parse := func(line string) ([]engine.Value, bool) {
		m := bracketedRe.FindStringSubmatch(line)
		if m == nil {
			return nil, false
		}
		// Require the first bracket to be a real timestamp so detection doesn't
		// misfire on arbitrary `[x] [y] z` lines.
		tv := timeOrNull(m[1])
		if tv.IsNull() {
			return nil, false
		}
		return []engine.Value{tv, strOrNull(m[2]), strOrNull(m[3])}, true
	}
	return &format{name: "bracketed", schema: engine.Schema{Columns: cols}, parse: parse, rawCol: 2}
}

// ---- generic leveled / raw --------------------------------------------------

var leadingTSRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?|\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2})\s+(.*)$`)

var levels = map[string]bool{
	"TRACE": true, "DEBUG": true, "INFO": true, "NOTICE": true, "WARN": true,
	"WARNING": true, "ERROR": true, "ERR": true, "CRIT": true, "CRITICAL": true,
	"FATAL": true, "PANIC": true,
}

// leadingTimestamp splits a leading timestamp off a line, returning the
// timestamp text and the remainder.
func leadingTimestamp(line string) (ts, rest string, ok bool) {
	m := leadingTSRe.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// leadingLevel pulls an optional leading level keyword (bracketed or bare) off
// the remainder.
func leadingLevel(rest string) (level, msg string) {
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", rest
	}
	w := strings.ToUpper(strings.Trim(fields[0], "[]:"))
	if levels[w] {
		return w, strings.TrimSpace(rest[len(fields[0]):])
	}
	return "", rest
}

func leveledFormat() *format {
	cols := []engine.Column{
		{Name: "time", Type: engine.TypeTime, Nullable: true},
		{Name: "level", Type: engine.TypeString, Nullable: true},
		{Name: "message", Type: engine.TypeString, Nullable: true},
	}
	parse := func(line string) ([]engine.Value, bool) {
		ts, rest, ok := leadingTimestamp(line)
		if !ok {
			return nil, false
		}
		level, msg := leadingLevel(strings.TrimSpace(rest))
		return []engine.Value{timeOrNull(ts), strOrNull(level), strOrNull(msg)}, true
	}
	return &format{name: "leveled", schema: engine.Schema{Columns: cols}, parse: parse, rawCol: 2}
}

func rawFormat() *format {
	cols := []engine.Column{
		{Name: "line", Type: engine.TypeString, Nullable: true},
		{Name: "time", Type: engine.TypeTime, Nullable: true},
		{Name: "level", Type: engine.TypeString, Nullable: true},
	}
	parse := func(line string) ([]engine.Value, bool) {
		var tv engine.Value = engine.Null()
		if ts, _, ok := leadingTimestamp(line); ok {
			tv = timeOrNull(ts)
		}
		level := ""
		for _, f := range strings.Fields(line) {
			if levels[strings.ToUpper(strings.Trim(f, "[]:"))] {
				level = strings.ToUpper(strings.Trim(f, "[]:"))
				break
			}
		}
		return []engine.Value{engine.StringVal(line), tv, strOrNull(level)}, true
	}
	return &format{name: "raw", schema: engine.Schema{Columns: cols}, parse: parse, rawCol: 0}
}

// ---- custom pattern ---------------------------------------------------------

// patternFormat builds a format from a user regular expression: each named
// capture group becomes a column (in declaration order), with scalar coercion.
func patternFormat(pat string) (*format, error) {
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern: %w", err)
	}
	var names []string
	idx := map[string]int{}
	for i, n := range re.SubexpNames() {
		if n != "" {
			if _, dup := idx[n]; !dup {
				idx[n] = i
				names = append(names, n)
			}
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("pattern has no named groups, e.g. (?P<level>\\w+)")
	}
	cols := make([]engine.Column, len(names))
	for i, n := range names {
		t := engine.TypeAny
		if n == "time" || n == "timestamp" {
			t = engine.TypeTime
		}
		cols[i] = engine.Column{Name: n, Type: t, Nullable: true}
	}
	parse := func(line string) ([]engine.Value, bool) {
		m := re.FindStringSubmatch(line)
		if m == nil {
			return nil, false
		}
		vals := make([]engine.Value, len(names))
		for i, n := range names {
			s := m[idx[n]]
			if n == "time" || n == "timestamp" {
				vals[i] = timeOrNull(s)
			} else {
				vals[i] = coerceScalar(s)
			}
		}
		return vals, true
	}
	return &format{name: "pattern", schema: engine.Schema{Columns: cols}, parse: parse, rawCol: indexOf(names, "message")}, nil
}

// ---- value helpers ----------------------------------------------------------

var timeLayouts = []string{
	time.RFC3339Nano, time.RFC3339,
	"2006-01-02T15:04:05-0700",     // pacman/ALPM (numeric zone, no colon)
	"2006-01-02T15:04:05.999-0700", // ditto, with fractional seconds
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05.999999",
	"2006-01-02 15:04:05",
	"2006/01/02 15:04:05",
	"02/Jan/2006:15:04:05 -0700", // CLF
	"Jan _2 15:04:05",            // syslog (no year)
	"Jan 2 15:04:05",
	time.RFC1123Z, time.RFC1123, time.ANSIC, time.UnixDate,
}

func parseTime(s string) (time.Time, bool) {
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			if t.Year() == 0 {
				// Year-less formats (syslog): assume the current year.
				t = t.AddDate(time.Now().Year(), 0, 0)
			}
			return t, true
		}
	}
	return time.Time{}, false
}

func timeOrNull(s string) engine.Value {
	if t, ok := parseTime(strings.TrimSpace(s)); ok {
		return engine.TimeVal(t)
	}
	return engine.Null()
}

func strOrNull(s string) engine.Value {
	if s == "" {
		return engine.Null()
	}
	return engine.StringVal(s)
}

// nullDash maps the access-log "-" placeholder to NULL.
func nullDash(s string) engine.Value {
	if s == "" || s == "-" {
		return engine.Null()
	}
	return engine.StringVal(s)
}

func intOrNull(s string) engine.Value {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return engine.IntVal(n)
	}
	return engine.Null()
}

func nullDashInt(s string) engine.Value {
	if s == "" || s == "-" {
		return engine.Null()
	}
	return intOrNull(s)
}

// coerceScalar types a bare string value: int, then float, then string.
func coerceScalar(s string) engine.Value {
	if s == "" {
		return engine.Null()
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return engine.IntVal(n)
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return engine.FloatVal(f)
	}
	if s == "true" || s == "false" {
		return engine.BoolVal(s == "true")
	}
	return engine.StringVal(s)
}

func stringOpt(opts map[string]any, key string) string {
	if v, ok := opts[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func indexOf(xs []string, s string) int {
	for i, x := range xs {
		if x == s {
			return i
		}
	}
	return -1
}
