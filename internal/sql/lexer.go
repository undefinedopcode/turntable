// Package sql implements the lexer, parser, and AST for the OctoParser SQL
// dialect (see DESIGN.md §4). This file holds the lexer.
package sql

import (
	"fmt"
	"strings"
	"unicode"
)

// TokenKind classifies a lexed token.
type TokenKind int

const (
	TKEOF TokenKind = iota
	TKError
	TKIdent
	TKKeyword
	TKInt
	TKFloat
	TKString
	TKOperator // = <> < <= > >= + - * / ( ) , .
)

// Token is a single lexed token.
type Token struct {
	Kind  TokenKind
	Value string
	Pos   int // byte offset in source
}

// keywords recognized by the lexer (uppercased on match).
var keywords = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "JOIN": true, "INNER": true,
	"LEFT": true, "ON": true, "GROUP": true, "BY": true, "HAVING": true,
	"ORDER": true, "ASC": true, "DESC": true, "LIMIT": true, "OFFSET": true,
	"AND": true, "OR": true, "NOT": true, "IN": true, "BETWEEN": true,
	"LIKE": true, "IS": true, "NULL": true, "CASE": true, "WHEN": true,
	"THEN": true, "ELSE": true, "END": true, "CAST": true, "AS": true,
	"DISTINCT": true, "COUNT": true, "SUM": true, "AVG": true, "MIN": true,
	"MAX": true, "TRUE": true, "FALSE": true, "INTERVAL": true,
}

// Lex tokenizes src into a slice of Tokens, including a trailing TKEOF.
// This is a stub suitable for the skeleton; the full scanner is implemented
// in the v0.1 milestone.
func Lex(src string) ([]Token, error) {
	var toks []Token
	i := 0
	n := len(src)
	for i < n {
		c := src[i]
		// skip whitespace
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			i++
			continue
		}
		// line comment --
		if c == '-' && i+1 < n && src[i+1] == '-' {
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		}
		// string literal '...'
		if c == '\'' {
			start := i
			i++
			var b strings.Builder
			for i < n {
				if src[i] == '\'' {
					if i+1 < n && src[i+1] == '\'' { // escaped ''
						b.WriteByte('\'')
						i += 2
						continue
					}
					i++
					toks = append(toks, Token{Kind: TKString, Value: b.String(), Pos: start})
					break
				}
				b.WriteByte(src[i])
				i++
			}
			if i > n {
				return nil, fmt.Errorf("unterminated string at %d", start)
			}
			continue
		}
		// number
		if unicode.IsDigit(rune(c)) {
			start := i
			isFloat := false
			for i < n && (unicode.IsDigit(rune(src[i])) || src[i] == '.') {
				if src[i] == '.' {
					isFloat = true
				}
				i++
			}
			kind := TKInt
			if isFloat {
				kind = TKFloat
			}
			toks = append(toks, Token{Kind: kind, Value: src[start:i], Pos: start})
			continue
		}
		// identifier / keyword
		if isIdentStart(c) {
			start := i
			for i < n && isIdentPart(src[i]) {
				i++
			}
			word := src[start:i]
			upper := strings.ToUpper(word)
			if keywords[upper] {
				toks = append(toks, Token{Kind: TKKeyword, Value: upper, Pos: start})
			} else {
				toks = append(toks, Token{Kind: TKIdent, Value: word, Pos: start})
			}
			continue
		}
		// multi-char operators
		if i+1 < n {
			two := src[i : i+2]
			switch two {
			case "<>", "<=", ">=":
				toks = append(toks, Token{Kind: TKOperator, Value: two, Pos: i})
				i += 2
				continue
			}
		}
		// single-char operators
		switch c {
		case '=', '<', '>', '+', '-', '*', '/', '(', ')', ',', '.', ':':
			toks = append(toks, Token{Kind: TKOperator, Value: string(c), Pos: i})
			i++
			continue
		}
		return nil, fmt.Errorf("unexpected character %q at %d", c, i)
	}
	toks = append(toks, Token{Kind: TKEOF, Pos: i})
	return toks, nil
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}