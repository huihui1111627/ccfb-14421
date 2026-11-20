package engine

import (
	"strings"
)

// TokenKind enumerates lexical token classes.
type TokenKind int

const (
	TEOF TokenKind = iota
	TIdent
	TNumber
	TString
	TKeyword
	TOp
	TPunct
	TBad
)

func (k TokenKind) String() string {
	switch k {
	case TEOF:
		return "end of input"
	case TIdent:
		return "identifier"
	case TNumber:
		return "number"
	case TString:
		return "string"
	case TKeyword:
		return "keyword"
	case TOp:
		return "operator"
	case TPunct:
		return "punctuation"
	case TBad:
		return "invalid token"
	}
	return "?"
}

// Token is a lexed token with a 1-based rune position.
type Token struct {
	Kind TokenKind
	Text string // raw text; for strings the unquoted value
	Pos  Pos
	End  Pos
}

// reserved lists keywords that cannot be used as bare identifiers.
var reserved = map[string]bool{
	"select": true, "from": true, "where": true, "group": true, "by": true,
	"having": true, "order": true, "limit": true, "offset": true, "as": true,
	"join": true, "inner": true, "left": true, "right": true, "full": true,
	"outer": true, "cross": true, "on": true, "using": true, "union": true,
	"except": true, "intersect": true, "with": true, "recursive": true,
	"distinct": true, "case": true, "when": true, "then": true, "else": true,
	"end": true, "and": true, "or": true, "not": true, "null": true, "is": true,
	"true": true, "false": true, "exists": true, "in": true, "between": true,
	"like": true, "cast": true, "asc": true, "desc": true, "all": true,
	"lateral": true, "asof": true, "nulls": true, "first": true, "last": true,
	"over": true, "partition": true, "rows": true, "range": true,
}

// syncKeywords are tokens the parser skips to when recovering from a syntax
// error. They can begin a new clause or statement.
var syncKeywords = map[string]bool{
	"select": true, "from": true, "where": true, "group": true, "having": true,
	"order": true, "limit": true, "join": true, "inner": true, "left": true,
	"right": true, "full": true, "cross": true, "on": true, "union": true,
	"except": true, "intersect": true, "with": true, "asof": true,
}

// Lexer tokenizes SQL with positions. Errors are collected, not fatal: an
// unknown character becomes a TBad token so the parser can report and resync.
type Lexer struct {
	src   []rune
	off   int
	line  int
	col   int
	tokAt int
	errs  []*Error
	toks  []Token
}

func NewLexer(src string) *Lexer {
	return &Lexer{src: []rune(src), line: 1, col: 1}
}

func (l *Lexer) pos() Pos { return Pos{l.line, l.col} }

func (l *Lexer) peekAt(n int) rune {
	if l.off+n >= len(l.src) {
		return 0
	}
	return l.src[l.off+n]
}

func (l *Lexer) peek() rune { return l.peekAt(0) }

func (l *Lexer) advance() rune {
	r := l.src[l.off]
	l.off++
	if r == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return r
}

func (l *Lexer) text(from int) string { return string(l.src[from:l.off]) }

func (l *Lexer) emit(k TokenKind, text string, start Pos) {
	l.toks = append(l.toks, Token{Kind: k, Text: text, Pos: start, End: l.pos()})
}

func (l *Lexer) fail(p Pos, format string, args ...any) {
	l.errs = append(l.errs, errAt(CodeLex, p, format, args...))
}

// Tokenize runs the lexer to completion and returns tokens plus diagnostics.
func (l *Lexer) Tokenize() ([]Token, []*Error) {
	for l.off < len(l.src) {
		l.tokAt = l.off
		r := l.peek()
		if isSpace(r) {
			l.advance()
			continue
		}
		if r == '-' && l.peekAt(1) == '-' {
			for l.off < len(l.src) && l.peek() != '\n' {
				l.advance()
			}
			continue
		}
		if r == '/' && l.peekAt(1) == '*' {
			start := l.pos()
			l.advance()
			l.advance()
			closed := false
			for l.off < len(l.src) {
				if l.peek() == '*' && l.peekAt(1) == '/' {
					l.advance()
					l.advance()
					closed = true
					break
				}
				l.advance()
			}
			if !closed {
				l.fail(start, "unterminated block comment")
			}
			continue
		}
		start := l.pos()
		switch {
		case isDigit(r) || (r == '.' && isDigit(l.peekAt(1))):
			l.lexNumber(start)
		case r == '\'':
			l.lexString(start)
		case r == '"':
			l.lexQuotedIdent(start)
		case isIdentStart(r):
			l.lexIdent(start)
		default:
			l.lexOperator(start)
		}
	}
	l.toks = append(l.toks, Token{Kind: TEOF, Pos: l.pos(), End: l.pos()})
	return l.toks, l.errs
}

func (l *Lexer) lexNumber(start Pos) {
	hasDot := false
	seenDigit := false
	for l.off < len(l.src) {
		r := l.peek()
		if isDigit(r) {
			seenDigit = true
			l.advance()
			continue
		}
		if r == '.' && !hasDot {
			hasDot = true
			l.advance()
			continue
		}
		break
	}
	if !seenDigit {
		l.fail(start, "malformed number")
		l.emit(TBad, l.text(l.tokAt), start)
		return
	}
	if l.off < len(l.src) && (l.peek() == 'e' || l.peek() == 'E') {
		save := l.off
		l.advance()
		if l.peek() == '+' || l.peek() == '-' {
			l.advance()
		}
		if isDigit(l.peek()) {
			for l.off < len(l.src) && isDigit(l.peek()) {
				l.advance()
			}
		} else {
			l.off = save
		}
	}
	l.emit(TNumber, l.text(l.tokAt), start)
}

func (l *Lexer) lexString(start Pos) {
	l.advance()
	var b strings.Builder
	closed := false
	for l.off < len(l.src) {
		r := l.advance()
		if r == '\'' {
			if l.peek() == '\'' {
				l.advance()
				b.WriteRune('\'')
				continue
			}
			closed = true
			break
		}
		b.WriteRune(r)
	}
	if !closed {
		l.fail(start, "unterminated string literal")
	}
	l.toks = append(l.toks, Token{Kind: TString, Text: b.String(), Pos: start, End: l.pos()})
}

func (l *Lexer) lexQuotedIdent(start Pos) {
	l.advance()
	var b strings.Builder
	closed := false
	for l.off < len(l.src) {
		r := l.advance()
		if r == '"' {
			if l.peek() == '"' {
				l.advance()
				b.WriteRune('"')
				continue
			}
			closed = true
			break
		}
		b.WriteRune(r)
	}
	if !closed {
		l.fail(start, "unterminated quoted identifier")
	}
	l.toks = append(l.toks, Token{Kind: TIdent, Text: b.String(), Pos: start, End: l.pos()})
}

func (l *Lexer) lexIdent(start Pos) {
	for l.off < len(l.src) && (isIdentStart(l.peek()) || isDigit(l.peek())) {
		l.advance()
	}
	text := l.text(l.tokAt)
	lower := strings.ToLower(text)
	if reserved[lower] {
		l.toks = append(l.toks, Token{Kind: TKeyword, Text: lower, Pos: start, End: l.pos()})
		return
	}
	l.toks = append(l.toks, Token{Kind: TIdent, Text: text, Pos: start, End: l.pos()})
}

// operators are matched longest-first.
var operators = []string{"<=>", "::", "<=", ">=", "<>", "!=", "||", "->", "=>", "=", "<", ">", "+", "-", "*", "/", "%", "(", ")", ",", ".", ";"}

func (l *Lexer) lexOperator(start Pos) {
	rest := string(l.src[l.tokAt:])
	for _, op := range operators {
		if strings.HasPrefix(rest, op) {
			for range op {
				l.advance()
			}
			kind := TOp
			switch op {
			case "(", ")", ",", ".", ";":
				kind = TPunct
			}
			l.toks = append(l.toks, Token{Kind: kind, Text: op, Pos: start, End: l.pos()})
			return
		}
	}
	r := l.advance()
	l.fail(start, "unexpected character %q", string(r))
	l.emit(TBad, string(r), start)
}

func isSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\r' || r == '\n' }
func isDigit(r rune) bool { return r >= '0' && r <= '9' }
func isIdentStart(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r >= 0x80
}

// Lex tokenizes a whole source string.
func Lex(src string) ([]Token, []*Error) {
	return NewLexer(src).Tokenize()
}
