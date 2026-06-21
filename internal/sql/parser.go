package sql

import (
	"fmt"
	"strconv"
	"strings"
)

// Parser converts a token stream into an AST. This is a skeleton parser that
// handles the SELECT shape and a subset of expressions; the full grammar is
// completed in the v0.1 milestone.
type Parser struct {
	toks []Token
	pos  int
}

// NewParser returns a Parser over the given tokens.
func NewParser(toks []Token) *Parser { return &Parser{toks: toks} }

// Parse lexes and parses src into a Statement.
func Parse(src string) (Statement, error) {
	toks, err := Lex(src)
	if err != nil {
		return nil, err
	}
	return NewParser(toks).ParseStatement()
}

func (p *Parser) cur() Token    { return p.toks[p.pos] }
func (p *Parser) peek() Token    { return p.toks[p.pos+1] }
func (p *Parser) advance() Token { t := p.toks[p.pos]; p.pos++; return t }

func (p *Parser) atEOF() bool { return p.cur().Kind == TKEOF }

func (p *Parser) kw(word string) bool {
	t := p.cur()
	return t.Kind == TKKeyword && strings.EqualFold(t.Value, word)
}

func (p *Parser) op(s string) bool {
	t := p.cur()
	return t.Kind == TKOperator && t.Value == s
}

func (p *Parser) expectOp(s string) error {
	if !p.op(s) {
		return p.errf("expected %q", s)
	}
	p.advance()
	return nil
}

func (p *Parser) expectKW(word string) error {
	if !p.kw(word) {
		return p.errf("expected keyword %q", word)
	}
	p.advance()
	return nil
}

func (p *Parser) errf(format string, args ...any) error {
	t := p.cur()
	return fmt.Errorf("parse error at offset %d (token %q): %s",
		t.Pos, t.Value, fmt.Sprintf(format, args...))
}

// ParseStatement parses a single (currently SELECT) statement.
func (p *Parser) ParseStatement() (Statement, error) {
	if !p.kw("SELECT") {
		return nil, p.errf("expected SELECT")
	}
	return p.parseSelect()
}

func (p *Parser) parseSelect() (*SelectStmt, error) {
	if err := p.expectKW("SELECT"); err != nil {
		return nil, err
	}
	s := &SelectStmt{}
	if p.kw("DISTINCT") {
		s.Distinct = true
		p.advance()
	}

	// optional DISTINCT handled in v0.1
	items, err := p.parseSelectList()
	if err != nil {
		return nil, err
	}
	s.Items = SelectList{Items: items}

	// FROM is optional: "SELECT 1+1" yields a single row over a synthetic
	// empty relation (useful in the REPL for scratch expressions).
	if p.kw("FROM") {
		p.advance()
		from, err := p.parseTableRef()
		if err != nil {
			return nil, err
		}
		s.From = from
	} else {
		s.NoFrom = true
	}

	// JOINs
	for p.kw("INNER") || p.kw("LEFT") || p.kw("JOIN") {
		j, err := p.parseJoin()
		if err != nil {
			return nil, err
		}
		s.Joins = append(s.Joins, j)
	}

	if p.kw("WHERE") {
		p.advance()
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		s.Where = e
	}

	if p.kw("GROUP") {
		p.advance()
		if err := p.expectKW("BY"); err != nil {
			return nil, err
		}
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			s.GroupBy = append(s.GroupBy, e)
			if !p.op(",") {
				break
			}
			p.advance()
		}
	}

	if p.kw("HAVING") {
		p.advance()
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		s.Having = e
	}

	if p.kw("ORDER") {
		p.advance()
		if err := p.expectKW("BY"); err != nil {
			return nil, err
		}
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			ot := OrderTerm{Expr: e}
			if p.kw("DESC") {
				ot.Desc = true
				p.advance()
			} else if p.kw("ASC") {
				p.advance()
			}
			s.OrderBy = append(s.OrderBy, ot)
			if !p.op(",") {
				break
			}
			p.advance()
		}
	}

	if p.kw("LIMIT") {
		p.advance()
		n, err := p.parseInt()
		if err != nil {
			return nil, err
		}
		s.Limit = &n
	}

	if p.kw("OFFSET") {
		p.advance()
		n, err := p.parseInt()
		if err != nil {
			return nil, err
		}
		s.Offset = &n
	}

	return s, nil
}

func (p *Parser) parseSelectList() ([]SelectItem, error) {
	var items []SelectItem
	for {
		if p.op("*") {
			p.advance()
			items = append(items, SelectItem{Star: true})
		} else {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			item := SelectItem{Expr: e}
			if p.kw("AS") {
				p.advance()
				t := p.advance()
				if t.Kind != TKIdent && t.Kind != TKKeyword {
					return nil, p.errf("expected alias after AS")
				}
				item.As = t.Value
			}
			items = append(items, item)
		}
		if !p.op(",") {
			break
		}
		p.advance()
	}
	return items, nil
}

func (p *Parser) parseTableRef() (TableRef, error) {
	var tr TableRef
	t := p.advance()
	if t.Kind == TKOperator && t.Value == "(" {
		// subquery
		sub, err := p.parseSelect()
		if err != nil {
			return tr, err
		}
		if err := p.expectOp(")"); err != nil {
			return tr, err
		}
		tr.Subquery = sub
	} else if t.Kind == TKIdent || t.Kind == TKKeyword {
		// qualified "prefix:source" or bare name
		name := t.Value
		if p.op(":") {
			p.advance()
			src, err := p.parseSourceString()
			if err != nil {
				return tr, err
			}
			tr.Prefix = name
			tr.Source = src
		} else {
			tr.Name = name
		}
	} else {
		return tr, p.errf("expected table reference")
	}
	if p.kw("AS") {
		p.advance()
		a := p.advance()
		if a.Kind != TKIdent {
			return tr, p.errf("expected alias after AS")
		}
		tr.Alias = a.Value
	} else if p.cur().Kind == TKIdent {
		// implicit alias
		tr.Alias = p.advance().Value
	}
	return tr, nil
}

// parseSourceString reads the remainder of a qualified source spec until the
// next whitespace-delimited keyword/ident boundary. For v0.1 we accept a
// single ident/path token; richer path parsing is added with the file
// connectors.
func (p *Parser) parseSourceString() (string, error) {
	// collect a path-like token: idents, dots, slashes, digits
	var b strings.Builder
	for {
		t := p.cur()
		if t.Kind == TKIdent || t.Kind == TKInt || t.Kind == TKFloat {
			b.WriteString(t.Value)
			p.advance()
		} else if t.Kind == TKOperator && (t.Value == "." || t.Value == "/") {
			b.WriteString(t.Value)
			p.advance()
		} else {
			break
		}
	}
	if b.Len() == 0 {
		return "", p.errf("expected source after ':'")
	}
	return b.String(), nil
}

func (p *Parser) parseJoin() (Join, error) {
	j := Join{Kind: JoinInner}
	if p.kw("INNER") {
		p.advance()
	} else if p.kw("LEFT") {
		j.Kind = JoinLeft
		p.advance()
	}
	if err := p.expectKW("JOIN"); err != nil {
		return j, err
	}
	ref, err := p.parseTableRef()
	if err != nil {
		return j, err
	}
	j.Ref = ref
	if err := p.expectKW("ON"); err != nil {
		return j, err
	}
	on, err := p.parseExpr()
	if err != nil {
		return j, err
	}
	j.On = on
	return j, nil
}

func (p *Parser) parseInt() (int, error) {
	t := p.advance()
	if t.Kind != TKInt {
		return 0, p.errf("expected integer")
	}
	n, err := strconv.Atoi(t.Value)
	return n, err
}

// parseExpr is a placeholder that parses primary expressions and binary ops
// with minimal precedence. Full precedence climbing arrives in v0.1.
// ParseExpr lexes and parses a standalone expression (useful for tests and
// pushdown translators).
func ParseExpr(src string) (Expr, error) {
	toks, err := Lex(src)
	if err != nil {
		return nil, err
	}
	p := NewParser(toks)
	return p.parseExpr()
}

func (p *Parser) parseExpr() (Expr, error) {
	return p.parseOr()
}

func (p *Parser) parseOr() (Expr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.kw("OR") {
		p.advance()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &BinaryOp{Op: "OR", Left: left, Right: right}
	}
	return left, nil
}

func (p *Parser) parseAnd() (Expr, error) {
	left, err := p.parseCompare()
	if err != nil {
		return nil, err
	}
	for p.kw("AND") {
		p.advance()
		right, err := p.parseCompare()
		if err != nil {
			return nil, err
		}
		left = &BinaryOp{Op: "AND", Left: left, Right: right}
	}
	return left, nil
}

func (p *Parser) parseCompare() (Expr, error) {
	left, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	t := p.cur()
	if t.Kind == TKOperator {
		switch t.Value {
		case "=", "<>", "<", "<=", ">", ">=":
			p.advance()
			right, err := p.parseAdd()
			if err != nil {
				return nil, err
			}
			return &BinaryOp{Op: t.Value, Left: left, Right: right}, nil
		}
	}
	// IS [NOT] NULL
	if p.kw("IS") {
		p.advance()
		neg := false
		if p.kw("NOT") {
			neg = true
			p.advance()
		}
		if err := p.expectKW("NULL"); err != nil {
			return nil, err
		}
		return &IsNullExpr{Expr: left, Negate: neg}, nil
	}
	// [NOT] IN / BETWEEN / LIKE
	neg := false
	if p.kw("NOT") {
		// only if followed by IN/BETWEEN/LIKE
		if p.peek().Kind == TKKeyword && (strings.EqualFold(p.peek().Value, "IN") || strings.EqualFold(p.peek().Value, "BETWEEN") || strings.EqualFold(p.peek().Value, "LIKE")) {
			neg = true
			p.advance()
		}
	}
	if p.kw("IN") {
		p.advance()
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		var list []Expr
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			list = append(list, e)
			if !p.op(",") {
				break
			}
			p.advance()
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		return &InExpr{Expr: left, List: list, Negate: neg}, nil
	}
	if p.kw("BETWEEN") {
		p.advance()
		lo, err := p.parseAdd()
		if err != nil {
			return nil, err
		}
		// SQL allows `BETWEEN x AND y` — but AND is also boolean AND. Since we
		// are in parseCompare (below parseAnd), `AND` here terminates parseAdd,
		// so we consume the AND keyword explicitly.
		if err := p.expectKW("AND"); err != nil {
			return nil, err
		}
		hi, err := p.parseAdd()
		if err != nil {
			return nil, err
		}
		return &BetweenExpr{Expr: left, Low: lo, High: hi, Negate: neg}, nil
	}
	if p.kw("LIKE") {
		p.advance()
		pat, err := p.parseAdd()
		if err != nil {
			return nil, err
		}
		return &LikeExpr{Expr: left, Pat: pat, Negate: neg}, nil
	}
	return left, nil
}

func (p *Parser) parseAdd() (Expr, error) {
	left, err := p.parseMul()
	if err != nil {
		return nil, err
	}
	for p.op("+") || p.op("-") {
		op := p.advance().Value
		right, err := p.parseMul()
		if err != nil {
			return nil, err
		}
		left = &BinaryOp{Op: op, Left: left, Right: right}
	}
	return left, nil
}

func (p *Parser) parseMul() (Expr, error) {
	left, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for p.op("*") || p.op("/") {
		op := p.advance().Value
		right, err := p.parsePrimary()
		if err != nil {
			return nil, err
		}
		left = &BinaryOp{Op: op, Left: left, Right: right}
	}
	return left, nil
}

func (p *Parser) parsePrimary() (Expr, error) {
	t := p.cur()
	switch {
	case t.Kind == TKInt:
		p.advance()
		v, _ := strconv.ParseInt(t.Value, 10, 64)
		return &LitInt{V: v}, nil
	case t.Kind == TKFloat:
		p.advance()
		v, _ := strconv.ParseFloat(t.Value, 64)
		return &LitFloat{V: v}, nil
	case t.Kind == TKString:
		p.advance()
		return &LitString{V: t.Value}, nil
	case p.kw("TRUE"):
		p.advance()
		return &LitBool{V: true}, nil
	case p.kw("FALSE"):
		p.advance()
		return &LitBool{V: false}, nil
	case p.kw("NULL"):
		p.advance()
		return &LitNull{}, nil
	case p.kw("CASE"):
		return p.parseCase()
	case p.kw("CAST"):
		return p.parseCast()
	case p.kw("EXTRACT"):
		return p.parseExtract()
	case p.kw("POSITION"):
		return p.parsePosition()
	case p.op("("):
		p.advance()
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		return e, nil
	case t.Kind == TKIdent || (t.Kind == TKKeyword && isFuncKW(t.Value)):
		// function call or column ref
		name := p.advance().Value
		if p.op("(") {
			p.advance()
			fc := &FuncCall{Name: name}
			if p.op("*") {
				p.advance()
				fc.Args = []Expr{&ColRef{Name: "*"}}
			} else if !p.op(")") {
				for {
					e, err := p.parseExpr()
					if err != nil {
						return nil, err
					}
					fc.Args = append(fc.Args, e)
					if !p.op(",") {
						break
					}
					p.advance()
				}
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			return fc, nil
		}
		ref := &ColRef{Name: name}
		if p.op(".") {
			p.advance()
			// qualifier was name, field is next
			field := p.advance()
			ref.Qualifier = name
			ref.Name = field.Value
		}
		return ref, nil
	}
	return nil, p.errf("unexpected token in expression: %q", t.Value)
}

func isFuncKW(s string) bool {
	switch s {
	case "COUNT", "SUM", "AVG", "MIN", "MAX", "COALESCE",
		"LEFT", "RIGHT":
		return true
	}
	return false
}

// parseCase parses a CASE expression. Two SQL forms are supported:
//
//	CASE WHEN cond THEN val [WHEN ...] [ELSE val] END
//	CASE expr WHEN val THEN val [WHEN ...] [ELSE val] END   (simple form)
//
// The simple form desugars to WHEN (expr = val) comparisons.
func (p *Parser) parseCase() (Expr, error) {
	p.advance() // CASE
	ce := &CaseExpr{}

	// Optional simple-form subject: CASE <expr> WHEN ...
	var subject Expr
	if !p.kw("WHEN") {
		s, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		subject = s
	}

	for p.kw("WHEN") {
		p.advance()
		cond, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if subject != nil {
			// simple form: cond is the comparison value; build subject = cond
			cond = &BinaryOp{Op: "=", Left: subject, Right: cond}
		}
		if err := p.expectKW("THEN"); err != nil {
			return nil, err
		}
		then, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		ce.Whens = append(ce.Whens, CaseWhen{Cond: cond, Then: then})
	}

	if p.kw("ELSE") {
		p.advance()
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		ce.Else = e
	}
	if err := p.expectKW("END"); err != nil {
		return nil, err
	}
	return ce, nil
}

// parseCast parses CAST(expr AS type). The type name is an identifier or
// keyword (e.g. INT, TIMESTAMP) optionally parenthesized with a size, which
// we accept and ignore (e.g. VARCHAR(255)).
func (p *Parser) parseCast() (Expr, error) {
	p.advance() // CAST
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	e, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if err := p.expectKW("AS"); err != nil {
		return nil, err
	}
	t := p.cur()
	if t.Kind != TKIdent && t.Kind != TKKeyword {
		return nil, p.errf("expected type name after AS, got %q", t.Value)
	}
	p.advance()
	// Optional (n) or (n, m) size spec, e.g. VARCHAR(255) / DECIMAL(10,2).
	if p.op("(") {
		p.advance()
		for !p.op(")") && !p.atEOF() {
			p.advance()
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	return &CastExpr{Expr: e, Type: t.Value}, nil
}

// parseExtract parses EXTRACT(field FROM source). Field is one of the date-part
// keywords (YEAR, MONTH, ... EPOCH). The closing paren is required.
func (p *Parser) parseExtract() (Expr, error) {
	p.advance() // EXTRACT
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	field := p.cur()
	if field.Kind != TKIdent && field.Kind != TKKeyword {
		return nil, p.errf("expected date field in EXTRACT, got %q", field.Value)
	}
	p.advance()
	if err := p.expectKW("FROM"); err != nil {
		return nil, err
	}
	src, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	return &ExtractExpr{Field: field.Value, Source: src}, nil
}

// parsePosition parses POSITION(substr IN str).
func (p *Parser) parsePosition() (Expr, error) {
	p.advance() // POSITION
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	// Parse the substring at add-level precedence so the IN keyword isn't
	// consumed as part of an IN-list predicate.
	sub, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	if err := p.expectKW("IN"); err != nil {
		return nil, err
	}
	str, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	return &PositionExpr{Substr: sub, Str: str}, nil
}