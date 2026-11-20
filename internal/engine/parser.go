package engine

import (
	"fmt"
	"strings"
)

// Parser is a hand-written recursive-descent parser with precedence climbing
// for expressions. It never stops at the first syntax error: each clause and
// each select item / join is parsed under a recovery guard that reports the
// error, skips to the next synchronization point, and continues, so one pass
// yields every independent syntax error in the statement.
type Parser struct {
	toks []Token
	i    int
	errs []*Error

	// recoveries counts error-recovery skips; a runaway statement is cut off.
	recoveries int
	// errLine/errCol record the farthest source position damaged by a syntax
	// error, so the binder can suppress errors caused by the damage.
	errLine int
	errCol  int
	// parenDepth tracks open parentheses across the whole statement. Error
	// recovery uses it to avoid treating commas inside a damaged call as item
	// separators.
	parenDepth int
	// lastErrCtx is the token index of the last reported syntax error. Errors
	// that recovery produces at the very same spot are echoes and are not
	// reported twice.
	lastErrCtx int
}

// redundantAt reports whether a diagnostic at token index idx duplicates the
// error that triggered the recovery currently in progress.
func (p *Parser) redundantAt(idx int) bool { return idx == p.lastErrCtx }

// ParseResult is everything the front end produced before binding.
type ParseResult struct {
	Stmt *Statement
	// If Stmt is non-nil it is a best-effort tree; Errs is non-empty then.
	Errs []*Error
	toks []Token
	// DamageLine/DamageCol mark the farthest position reached by syntax
	// error recovery. Binder errors before this point are unreliable.
	DamageLine int
	DamageCol  int
}

const maxRecoveries = 24

func Parse(src string) *ParseResult {
	toks, lexErrs := Lex(src)
	p := &Parser{toks: toks}
	stmt := p.parseStatement()
	// Trailing tokens after a statement are an error. After an earlier syntax
	// error this is almost always the same damage surfacing again, so it is
	// only reported when the statement otherwise parsed cleanly. Otherwise
	// rescue the remainder so further independent faults are still found.
	if p.cur().Kind != TEOF {
		if len(p.errs) == 0 && len(lexErrs) == 0 {
			p.errAt(p.cur().Pos, "unexpected %s %q after end of statement", p.cur().Kind, p.cur().Text)
		}
		p.rescueExprWithin(0, false)
		p.skipToEnd()
	}
	errs := sortErrors(append(lexErrs, p.errs...))
	return &ParseResult{Stmt: stmt, Errs: errs, toks: toks, DamageLine: p.errLine, DamageCol: p.errCol}
}

func (p *Parser) cur() Token { return p.toks[p.i] }
func (p *Parser) peekN(n int) Token {
	if p.i+n >= len(p.toks) {
		return p.toks[len(p.toks)-1]
	}
	return p.toks[p.i+n]
}

func (p *Parser) adv() Token {
	t := p.toks[p.i]
	if p.i < len(p.toks)-1 {
		p.i++
	}
	if t.Kind == TPunct {
		switch t.Text {
		case "(":
			p.parenDepth++
		case ")":
			if p.parenDepth > 0 {
				p.parenDepth--
			}
		}
	}
	return t
}

func (p *Parser) atEOF() bool { return p.cur().Kind == TEOF }

func (p *Parser) isKw(kw string) bool {
	return p.cur().Kind == TKeyword && p.cur().Text == kw
}

func (p *Parser) isOp(op string) bool { return p.cur().Kind == TOp && p.cur().Text == op }

func (p *Parser) isPunct(s string) bool { return p.cur().Kind == TPunct && p.cur().Text == s }

func (p *Parser) acceptKw(kw string) bool {
	if p.isKw(kw) {
		p.adv()
		return true
	}
	return false
}

func (p *Parser) acceptOp(op string) bool {
	if p.isOp(op) {
		p.adv()
		return true
	}
	return false
}

func (p *Parser) acceptPunct(s string) bool {
	if p.isPunct(s) {
		p.adv()
		return true
	}
	return false
}

func (p *Parser) errAt(pos Pos, format string, args ...any) {
	p.errs = append(p.errs, errAt(CodeSyntax, pos, format, args...))
	p.lastErrCtx = p.i
	p.noteDamage(pos)
}

// noteDamage records the end of the damaged region. Expression-level recovery
// extends this mark to where parsing resumed, which is the boundary past
// which binder errors are trustworthy.
func (p *Parser) noteDamage(pos Pos) {
	if pos.Line > p.errLine || (pos.Line == p.errLine && pos.Col > p.errCol) {
		p.errLine, p.errCol = pos.Line, pos.Col
	}
}

func (p *Parser) expectKw(kw string) bool {
	if p.acceptKw(kw) {
		return true
	}
	p.errAt(p.cur().Pos, "expected %s, found %s %q", strings.ToUpper(kw), p.cur().Kind, p.cur().Text)
	return false
}

func (p *Parser) expectPunct(s string) bool {
	if p.acceptPunct(s) {
		return true
	}
	p.errAt(p.cur().Pos, "expected %q, found %s %q", s, p.cur().Kind, p.cur().Text)
	return false
}

func (p *Parser) expectOp(op string) bool {
	if p.acceptOp(op) {
		return true
	}
	p.errAt(p.cur().Pos, "expected %q, found %s %q", op, p.cur().Kind, p.cur().Text)
	return false
}

// expectIdent consumes an identifier. Keywords are not identifiers.
func (p *Parser) expectIdent(what string) (string, Pos, bool) {
	if p.cur().Kind == TIdent {
		t := p.adv()
		return t.Text, t.Pos, true
	}
	p.errAt(p.cur().Pos, "expected %s, found %s %q", what, p.cur().Kind, p.cur().Text)
	return "", p.cur().Pos, false
}

// atSync reports whether the current token can begin a new clause.
func (p *Parser) atSync() bool {
	if p.atEOF() {
		return true
	}
	if p.cur().Kind == TKeyword && syncKeywords[p.cur().Text] {
		return true
	}
	// A top-level ',' or ')' closes enclosing constructs.
	return p.isPunct(",") || p.isPunct(")")
}

// skipToSync advances past the current token and then to the next token that
// starts a clause or closes the enclosing construct. Used after a syntax
// error inside a clause to re-enter at a known-good boundary.
func (p *Parser) skipToSync() {
	p.recoveries++
	if p.recoveries > maxRecoveries {
		p.skipToEnd()
		return
	}
	if !p.atEOF() {
		p.adv()
	}
	for !p.atSync() {
		p.adv()
	}
}

func (p *Parser) skipToEnd() {
	for !p.atEOF() {
		p.adv()
	}
}

// guarded parses one unit (clause, item, join) and on failure records the
// error once, resyncs, and reports false so the caller can move on.
func (p *Parser) guarded(fn func() bool) bool {
	startIdx := p.i
	startErrs := len(p.errs)
	if fn() {
		return true
	}
	if len(p.errs) == startErrs {
		p.errAt(p.cur().Pos, "syntax error near %s %q", p.cur().Kind, p.cur().Text)
	}
	if p.i == startIdx {
		p.skipToSync()
	}
	return false
}

// ---------------------------------------------------------------------------
// Statement / query body
// ---------------------------------------------------------------------------

func (p *Parser) parseStatement() *Statement {
	st := &Statement{Posn: p.cur().Pos}
	st.With = p.parseWith()
	st.Body = p.parseQueryBody(0)
	if st.Body == nil {
		// Manufacture an empty select so downstream phases can still run.
		st.Body = &SelectCore{Posn: st.Posn}
	}
	if p.isKw("order") {
		p.adv()
		p.expectKw("by")
		st.OrderBy = p.parseOrderBy()
	}
	if p.acceptKw("limit") {
		if v, ok := p.parseIntLiteral(); ok {
			st.Limit = &v
		}
	}
	if p.acceptKw("offset") {
		if v, ok := p.parseIntLiteral(); ok {
			st.Offset = &v
		}
	}
	return st
}

func (p *Parser) parseIntLiteral() (int64, bool) {
	if p.cur().Kind != TNumber {
		p.errAt(p.cur().Pos, "expected an integer, found %s %q", p.cur().Kind, p.cur().Text)
		return 0, false
	}
	t := p.adv()
	d, ip, ok := ParseDecimal(t.Text)
	if !ok || d.Scale != 0 || !isAllDigits(t.Text) || ip > 18 {
		p.errAt(t.Pos, "expected a non-negative integer, found %q", t.Text)
		return 0, false
	}
	if d.Unscaled < 0 {
		p.errAt(t.Pos, "expected a non-negative integer, found %q", t.Text)
		return 0, false
	}
	return d.Unscaled, true
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func (p *Parser) parseWith() *WithClause {
	if !p.isKw("with") {
		return nil
	}
	w := &WithClause{Posn: p.cur().Pos}
	p.adv()
	w.Recursive = p.acceptKw("recursive")
	for {
		ok := p.guarded(func() bool {
			c := &CTE{Posn: p.cur().Pos}
			name, pos, ok := p.expectIdent("CTE name")
			if !ok {
				return false
			}
			c.Name, c.NamePos = name, pos
			if p.isPunct("(") {
				p.adv()
				for {
					col, _, ok := p.expectIdent("column name")
					if !ok {
						break
					}
					c.Cols = append(c.Cols, col)
					if !p.acceptPunct(",") {
						break
					}
				}
				p.expectPunct(")")
			}
			if !p.expectKw("as") {
				return false
			}
			if !p.expectPunct("(") {
				return false
			}
			c.Body = p.parseQueryBody(0)
			if c.Body == nil {
				p.errAt(p.cur().Pos, "expected a query inside the CTE")
				return false
			}
			if !p.expectPunct(")") {
				return false
			}
			w.CTEs = append(w.CTEs, c)
			return true
		})
		if !ok || !p.acceptPunct(",") {
			break
		}
	}
	if len(w.CTEs) == 0 {
		return nil
	}
	return w
}

// parseQueryBody parses SELECT cores joined by set operators. minPrec orders
// INTERSECT tighter than UNION/EXCEPT.
func (p *Parser) parseQueryBody(minPrec int) QueryBody {
	left := p.parseSelectCore()
	if left == nil {
		return nil
	}
	for {
		var op string
		prec := 0
		if p.isKw("union") {
			op, prec = "union", 1
		} else if p.isKw("except") {
			op, prec = "except", 1
		} else if p.isKw("intersect") {
			op, prec = "intersect", 2
		}
		if op == "" || prec < minPrec {
			return left
		}
		opTok := p.adv()
		all := p.acceptKw("all")
		if !all {
			p.acceptKw("distinct")
		}
		right := p.parseQueryBody(prec + 1)
		if right == nil {
			p.errAt(p.cur().Pos, "expected a query after %s", strings.ToUpper(op))
			return left
		}
		left = &SetOp{Op: strings.ToUpper(op), All: all, Left: left, Right: right, Posn: opTok.Pos}
	}
}

func (p *Parser) parseSelectCore() QueryBody {
	if !p.isKw("select") {
		p.errAt(p.cur().Pos, "expected SELECT, found %s %q", p.cur().Kind, p.cur().Text)
		return nil
	}
	sel := &SelectCore{Posn: p.cur().Pos}
	p.adv()
	if p.acceptKw("distinct") {
		sel.Distinct = true
	} else {
		p.acceptKw("all")
	}
	sel.Items = p.parseSelectItems()
	if sel.Items == nil {
		sel.Items = []*SelectItem{}
	}
	if p.acceptKw("from") {
		from := p.parseFromClause()
		sel.From = from
	}
	if p.isKw("where") {
		p.adv()
		sel.Where = p.parseExprClause("WHERE")
	}
	if p.isKw("group") {
		p.adv()
		p.expectKw("by")
		sel.GroupBy = p.parseExprListClause("GROUP BY")
	}
	if p.isKw("having") {
		p.adv()
		sel.Having = p.parseExprClause("HAVING")
	}
	return sel
}

func (p *Parser) parseSelectItems() []*SelectItem {
	var items []*SelectItem
	for {
		mark := len(p.errs)
		ok := p.guarded(func() bool {
			it := p.parseSelectItem()
			if it == nil {
				return false
			}
			items = append(items, it)
			return true
		})
		if !ok {
			if len(p.errs) > mark {
				p.noteDamage(p.cur().Pos)
			}
			if p.acceptPunct(",") {
				continue
			}
			break
		}
		if len(p.errs) > mark {
			p.noteDamage(p.cur().Pos)
		}
		if !p.acceptPunct(",") {
			break
		}
	}
	return items
}

func (p *Parser) parseSelectItem() *SelectItem {
	start := p.cur().Pos
	// t.* form
	if p.cur().Kind == TIdent && p.peekN(1).Kind == TPunct && p.peekN(1).Text == "." &&
		p.peekN(2).Kind == TOp && p.peekN(2).Text == "*" {
		tab := p.adv().Text
		p.adv()
		p.adv()
		return &SelectItem{Star: true, Table: tab, Posn: start}
	}
	if p.isOp("*") {
		p.adv()
		return &SelectItem{Star: true, Posn: start}
	}
	e := p.parseExpr(0)
	if e == nil {
		return nil
	}
	it := &SelectItem{Expr: e, Posn: start}
	if p.acceptKw("as") {
		name, pos, ok := p.expectIdent("column alias")
		if !ok {
			return it
		}
		it.Alias, it.AliasPos = name, pos
	} else if p.cur().Kind == TIdent {
		t := p.adv()
		it.Alias, it.AliasPos = t.Text, t.Pos
	} else if p.cur().Kind == TString {
		t := p.adv()
		it.Alias = t.Text
	}
	return it
}

func (p *Parser) parseOrderBy() []SortItem {
	var out []SortItem
	for {
		ok := p.guarded(func() bool {
			start := p.cur().Pos
			e := p.parseExpr(0)
			if e == nil {
				return false
			}
			it := SortItem{Expr: e, Posn: start}
			if p.acceptKw("desc") {
				it.Desc = true
			} else {
				p.acceptKw("asc")
			}
			out = append(out, it)
			return true
		})
		if !ok || !p.acceptPunct(",") {
			break
		}
	}
	return out
}

func (p *Parser) parseFromClause() TableExpr {
	left := p.parseTablePrimary()
	if left == nil {
		return nil
	}
	for {
		kind, ok := p.joinKind()
		if !ok {
			break
		}
		kindPos := p.cur().Pos
		// Consume the join keyword sequence.
		for p.cur().Kind == TKeyword {
			switch p.cur().Text {
			case "join", "inner", "left", "right", "full", "outer", "cross", "asof":
				p.adv()
				continue
			}
			break
		}
		right := p.parseTablePrimary()
		if right == nil {
			p.errAt(p.cur().Pos, "expected a table after %s", kind)
			p.skipToSync()
			break
		}
		j := &JoinTable{Kind: kind, Left: left, Right: right, Posn: left.Pos(), KindPos: kindPos}
		if p.acceptKw("on") {
			mark := len(p.errs)
			j.On = p.parseExpr(0)
			if len(p.errs) > mark {
				p.noteDamage(p.cur().Pos)
			}
		} else if p.isKw("using") {
			p.adv()
			p.expectPunct("(")
			for {
				name, pos, ok := p.expectIdent("column name")
				if !ok {
					break
				}
				j.Using = append(j.Using, name)
				j.UsingPos = append(j.UsingPos, pos)
				if !p.acceptPunct(",") {
					break
				}
			}
			p.expectPunct(")")
		} else if kind != JoinCross {
			p.errAt(p.cur().Pos, "%s requires an ON or USING clause", kind)
		}
		left = j
	}
	return left
}

func (p *Parser) joinKind() (JoinKind, bool) {
	if p.cur().Kind != TKeyword {
		return 0, false
	}
	switch p.cur().Text {
	case "join":
		return JoinInner, true
	case "inner":
		if p.peekN(1).Kind == TKeyword && p.peekN(1).Text == "join" {
			return JoinInner, true
		}
	case "left":
		if p.peekN(1).Kind == TKeyword && (p.peekN(1).Text == "join" || p.peekN(1).Text == "outer") {
			return JoinLeft, true
		}
	case "right":
		if p.peekN(1).Kind == TKeyword && (p.peekN(1).Text == "join" || p.peekN(1).Text == "outer") {
			return JoinRight, true
		}
	case "full":
		if p.peekN(1).Kind == TKeyword && (p.peekN(1).Text == "join" || p.peekN(1).Text == "outer") {
			return JoinFull, true
		}
	case "cross":
		if p.peekN(1).Kind == TKeyword && p.peekN(1).Text == "join" {
			return JoinCross, true
		}
	case "asof":
		if p.peekN(1).Kind == TKeyword && p.peekN(1).Text == "join" {
			return JoinAsof, true
		}
	}
	return 0, false
}

func (p *Parser) parseTablePrimary() TableExpr {
	switch {
	case p.isPunct("("):
		start := p.cur().Pos
		p.adv()
		// Could be a parenthesized join or a subquery/derived table.
		if p.isKw("select") || p.isKw("with") {
			body := p.parseStatement()
			p.expectPunct(")")
			t := &SubqueryTable{Body: body, Posn: start}
			p.parseTableAlias(t)
			return t
		}
		inner := p.parseFromClause()
		p.expectPunct(")")
		return inner
	case p.isKw("lateral"):
		start := p.cur().Pos
		p.adv()
		if !p.expectPunct("(") {
			return nil
		}
		body := p.parseStatement()
		p.expectPunct(")")
		t := &SubqueryTable{Body: body, Lateral: true, Posn: start}
		p.parseTableAlias(t)
		return t
	case p.cur().Kind == TIdent:
		tok := p.adv()
		t := &TableName{Name: tok.Text, Posn: tok.Pos, NamePos: tok.Pos}
		p.parseTableAlias(t)
		return t
	}
	p.errAt(p.cur().Pos, "expected a table name or subquery, found %s %q", p.cur().Kind, p.cur().Text)
	return nil
}

func (p *Parser) parseTableAlias(t TableExpr) {
	hasAs := p.acceptKw("as")
	if p.cur().Kind == TIdent {
		tok := p.adv()
		switch x := t.(type) {
		case *TableName:
			x.Alias, x.AliasPos = tok.Text, tok.Pos
		case *SubqueryTable:
			x.Alias, x.AliasPos = tok.Text, tok.Pos
		}
		return
	}
	if hasAs {
		p.errAt(p.cur().Pos, "expected a table alias after AS")
	}
}

// parseExprClause parses one clause-level expression. After syntax damage it
// rescues within the predicate: it skips the damaged fragment to the next
// AND/OR (or stray comma), parses the following operand in a clean attempt,
// and repeats. Each damaged conjunct therefore yields one error and the next
// independent one is still found. The clause's damaged span is recorded so
// the binder suppresses semantic errors that depend on the damaged fragment.
func (p *Parser) parseExprClause(what string) Expr {
	mark := len(p.errs)
	start := p.i
	var e Expr
	p.guarded(func() bool {
		e = p.parseExpr(0)
		return e != nil
	})
	if len(p.errs) == mark {
		_ = what
		return e
	}
	if p.i == start {
		// A failed cast may have left the cursor on the offending token (it
		// consumed nothing). Step over it so the rescue loop can progress.
		p.adv()
	}
	p.rescueExprWithin(p.parenDepth, true)
	p.noteDamage(p.cur().Pos)
	return e
}

// rescueExprWithin skips damaged tokens at paren depth base and, at each
// AND/OR (or comma when acceptComma), makes a fresh attempt to parse the next
// operand. With clause set, a clause keyword also terminates the rescue. This
// is what turns several independent syntax faults in one predicate into
// several diagnostics instead of one.
func (p *Parser) rescueExprWithin(base int, clause bool) {
	for {
		if p.atEOF() {
			return
		}
		t := p.cur()
		atBase := p.parenDepth == base
		if atBase && t.Kind == TPunct && t.Text == ")" {
			return
		}
		if atBase && clause && t.Kind == TKeyword && syncKeywords[t.Text] && t.Text != "and" {
			return
		}
		if atBase && ((t.Kind == TPunct && t.Text == ",") || (t.Kind == TKeyword && (t.Text == "and" || t.Text == "or"))) {
			p.adv()
			mark := len(p.errs)
			before := p.i
			p.parseExpr(0)
			if len(p.errs) > mark {
				// The fresh attempt failed; continue skipping from wherever it
				// stopped, making sure we always advance.
				if p.i == before && !p.atEOF() {
					p.adv()
				}
				continue
			}
			// Success: the parse may have consumed the rest of the predicate.
			continue
		}
		if p.parenDepth < base {
			return
		}
		p.adv()
	}
}

func (p *Parser) parseExprListClause(what string) []Expr {
	var out []Expr
	for {
		var e Expr
		mark := len(p.errs)
		ok := p.guarded(func() bool {
			e = p.parseExpr(0)
			return e != nil
		})
		if len(p.errs) > mark {
			p.recoverExpr()
		}
		if ok {
			out = append(out, e)
		}
		if !p.acceptPunct(",") {
			break
		}
	}
	if len(out) == 0 {
		p.errAt(p.cur().Pos, "expected an expression list after %s", what)
	}
	return out
}

// ---------------------------------------------------------------------------
// Expressions: precedence climbing
// ---------------------------------------------------------------------------

// binary op precedence levels (higher binds tighter):
//
//	1 OR
//	2 AND
//	3 NOT (prefix)
//	4 IS / comparison / LIKE / IN / BETWEEN
//	5 + -
//	6 * / %
//	7 unary + -
//	8 primary
func (p *Parser) parseExpr(minPrec int) Expr {
	startIdx := p.i
	left := p.parseUnary(minPrec)
	if left == nil {
		// If the operand failed without consuming anything, skip the offending
		// token so recovery does not re-enter on it forever.
		if p.i == startIdx && !p.atEOF() {
			p.adv()
		}
		return nil
	}
	for {
		op, prec, ntok := p.peekBinaryOp()
		if op == "" || prec < minPrec {
			return left
		}
		opPos := p.cur().Pos
		for i := 0; i < ntok; i++ {
			p.adv()
		}
		mark := len(p.errs)
		switch op {
		case "is":
			not := p.acceptKw("not")
			var what string
			if p.acceptKw("null") {
				what = "null"
			} else if p.acceptKw("true") {
				what = "true"
			} else if p.acceptKw("false") {
				what = "false"
			} else {
				p.errAt(p.cur().Pos, "expected NULL, TRUE or FALSE after IS")
				return left
			}
			left = &EIs{X: left, What: what, Not: not, Posn: left.Pos(), OpPos: opPos}
		case "between":
			lo := p.parseExpr(prec + 1)
			if lo == nil {
				if len(p.errs) == mark {
					p.errAt(p.cur().Pos, "expected a lower bound after BETWEEN")
				}
				p.recoverExpr()
				return left
			}
			if !p.expectKw("and") {
				return left
			}
			hi := p.parseExpr(prec + 1)
			if hi == nil {
				if len(p.errs) == mark {
					p.errAt(p.cur().Pos, "expected an upper bound after BETWEEN ... AND")
				}
				p.recoverExpr()
				return left
			}
			left = &EBetween{X: left, Lo: lo, Hi: hi, Posn: left.Pos(), OpPos: opPos}
		case "notin", "notlike", "notbetween":
			// The NOT was consumed as part of the operator; re-dispatch with
			// the negation recorded.
			p.cur()
			switch op {
			case "notin":
				if !p.expectPunct("(") {
					return left
				}
				left = p.finishIn(left, opPos, true)
				continue
			case "notlike":
				r := p.parseExpr(prec + 1)
				if r == nil {
					p.errAt(p.cur().Pos, "expected a pattern after NOT LIKE")
					return left
				}
				return &ELike{X: left, Pat: r, Not: true, Posn: left.Pos(), OpPos: opPos}
			default:
				lo := p.parseExpr(prec + 1)
				if lo == nil {
					p.errAt(p.cur().Pos, "expected a lower bound after NOT BETWEEN")
					return left
				}
				if !p.expectKw("and") {
					return left
				}
				hi := p.parseExpr(prec + 1)
				if hi == nil {
					p.errAt(p.cur().Pos, "expected an upper bound after NOT BETWEEN ... AND")
					return left
				}
				return &EBetween{X: left, Lo: lo, Hi: hi, Not: true, Posn: left.Pos(), OpPos: opPos}
			}
		case "in":
			if !p.expectPunct("(") {
				return left
			}
			left = p.finishIn(left, opPos, false)
			continue
		case "like":
			r := p.parseExpr(prec + 1)
			if r == nil {
				if len(p.errs) == mark {
					p.errAt(p.cur().Pos, "expected a pattern after LIKE")
				}
				p.recoverExpr()
				return left
			}
			left = &ELike{X: left, Pat: r, Posn: left.Pos(), OpPos: opPos}
		default:
			r := p.parseExpr(prec + 1)
			if r == nil {
				if len(p.errs) == mark {
					p.errAt(p.cur().Pos, "expected an operand after %q", strings.ToUpper(op))
				}
				p.recoverExpr()
				return left
			}
			left = &EBinary{Op: op, L: left, R: r, Posn: left.Pos(), OpPos: opPos}
		}
	}
}

// finishIn parses the parenthesised right side of IN / NOT IN. The '(' has
// already been consumed.
func (p *Parser) finishIn(left Expr, opPos Pos, not bool) Expr {
	if p.isKw("select") || p.isKw("with") {
		body := p.parseStatement()
		p.expectPunct(")")
		return &EInSubquery{X: left, Body: body, Not: not, Posn: left.Pos(), OpPos: opPos}
	}
	var list []Expr
	for {
		e := p.parseExpr(0)
		if e == nil {
			break
		}
		list = append(list, e)
		if !p.acceptPunct(",") {
			break
		}
	}
	p.expectPunct(")")
	return &EInList{X: left, List: list, Not: not, Posn: left.Pos(), OpPos: opPos}
}

// peekBinaryOp recognises a binary/infix operator at the cursor and returns
// its canonical name, precedence and token count.
func (p *Parser) peekBinaryOp() (string, int, int) {
	t := p.cur()
	if t.Kind == TKeyword {
		switch t.Text {
		case "or":
			return "or", 1, 1
		case "and":
			return "and", 2, 1
		case "is":
			return "is", 4, 1
		case "between":
			return "between", 4, 1
		case "in":
			return "in", 4, 1
		case "like":
			return "like", 4, 1
		case "not":
			if p.peekN(1).Kind == TKeyword && (p.peekN(1).Text == "in" || p.peekN(1).Text == "like" || p.peekN(1).Text == "between") {
				switch p.peekN(1).Text {
				case "in":
					return "notin", 4, 2
				case "like":
					return "notlike", 4, 2
				case "between":
					return "notbetween", 4, 2
				}
			}
		}
		return "", 0, 0
	}
	if t.Kind == TOp {
		switch t.Text {
		case "=":
			return "=", 4, 1
		case "<>", "!=":
			return "<>", 4, 1
		case "<":
			return "<", 4, 1
		case ">":
			return ">", 4, 1
		case "<=":
			return "<=", 4, 1
		case ">=":
			return ">=", 4, 1
		case "+":
			return "+", 5, 1
		case "-":
			return "-", 5, 1
		case "*":
			return "*", 6, 1
		case "/":
			return "/", 6, 1
		case "%":
			return "%", 6, 1
		}
	}
	return "", 0, 0
}

func (p *Parser) parseUnary(minPrec int) Expr {
	t := p.cur()
	if t.Kind == TKeyword && t.Text == "not" && minPrec <= 3 {
		p.adv()
		mark := len(p.errs)
		x := p.parseExpr(3)
		if x == nil {
			if len(p.errs) == mark {
				p.errAt(p.cur().Pos, "expected an expression after NOT")
			}
			return nil
		}
		return &EUnary{Op: "not", X: x, Posn: t.Pos, OpPos: t.Pos}
	}
	if t.Kind == TOp && (t.Text == "-" || t.Text == "+") {
		p.adv()
		mark := len(p.errs)
		x := p.parseExpr(7)
		if x == nil {
			if len(p.errs) == mark {
				p.errAt(p.cur().Pos, "expected an operand after %q", t.Text)
			}
			return nil
		}
		if t.Text == "+" {
			return x
		}
		return &EUnary{Op: "-", X: x, Posn: t.Pos, OpPos: t.Pos}
	}
	return p.parsePrimary()
}

func (p *Parser) parsePrimary() Expr {
	t := p.cur()
	switch t.Kind {
	case TNumber:
		p.adv()
		lit := p.numberLiteral(t)
		for p.isOp("::") {
			p.adv()
			ty, ok := p.parseTypeName()
			if !ok {
				p.skipBadTypeName()
				return lit
			}
			lit = &ECast{X: lit, Type: ty, Posn: t.Pos}
		}
		return lit
	case TString:
		p.adv()
		return &ELiteral{Kind: KString, Str: t.Text, Posn: t.Pos}
	case TKeyword:
		switch t.Text {
		case "null":
			p.adv()
			return &ELiteral{Kind: KNull, Posn: t.Pos}
		case "true", "false":
			p.adv()
			return &ELiteral{Kind: KBool, Bool: t.Text == "true", Posn: t.Pos}
		case "case":
			return p.parseCase()
		case "cast":
			return p.parseCast()
		case "exists":
			p.adv()
			if !p.expectPunct("(") {
				return nil
			}
			body := p.parseStatement()
			p.expectPunct(")")
			return &EExists{Body: body, Posn: t.Pos}
		}
		p.errAt(t.Pos, "unexpected keyword %q in expression", strings.ToUpper(t.Text))
		p.adv()
		// Retry at the next token: the junk keyword is consumed and the rest
		// of the expression keeps its alignment, so later independent errors
		// in the same statement are still found.
		return p.parsePrimary()
	case TOp:
		p.errAt(t.Pos, "unexpected operator %q in expression", t.Text)
		p.adv()
		return p.parsePrimary()
	case TPunct:
		if t.Text == "(" {
			p.adv()
			if p.isKw("select") || p.isKw("with") {
				body := p.parseStatement()
				p.expectPunct(")")
				return &EScalarSubquery{Body: body, Posn: t.Pos}
			}
			mark := len(p.errs)
			inner := p.parseExpr(0)
			if inner == nil {
				if len(p.errs) == mark {
					p.errAt(p.cur().Pos, "expected an expression after '('")
				}
				p.recoverToCloseParen()
				return nil
			}
			if len(p.errs) > mark {
				// Rescue the remaining conjuncts of the group so later
				// independent faults are still reported.
				p.rescueExprWithin(p.parenDepth, false)
				if p.isPunct(")") {
					p.adv()
				} else {
					p.recoverToCloseParen()
				}
			} else if !p.expectPunct(")") {
				p.recoverToCloseParen()
			}
			var out Expr = &EParen{X: inner, Posn: t.Pos}
			for p.isOp("::") {
				p.adv()
				ty, ok := p.parseTypeName()
				if !ok {
					return out
				}
				out = &ECast{X: out, Type: ty, Posn: t.Pos}
			}
			return out
		}
		if t.Text == ")" {
			// Let the enclosing construct decide; it has the matching '('.
			p.errAt(t.Pos, "unexpected \")\" in expression")
			return nil
		}
		p.errAt(t.Pos, "unexpected %q in expression", t.Text)
		p.adv()
		return p.parsePrimary()
	case TIdent:
		// Typed literals: DATE '2024-01-01' and TICK '95.50'.
		switch strings.ToLower(t.Text) {
		case "date":
			if p.peekN(1).Kind == TString {
				p.adv()
				str := p.adv()
				d, ok := ParseDate(str.Text)
				if !ok {
					p.errAt(str.Pos, "invalid date literal %q (expected YYYY-MM-DD)", str.Text)
					return &ELiteral{Kind: KNull, Posn: t.Pos}
				}
				return &ELiteral{Kind: KDate, Date: d, Posn: t.Pos}
			}
		case "tick":
			if p.peekN(1).Kind == TString {
				p.adv()
				str := p.adv()
				tk, ok := ParseTick(str.Text)
				if !ok {
					p.errAt(str.Pos, "invalid clock literal %q (expected 0.00..100.00)", str.Text)
					return &ELiteral{Kind: KNull, Posn: t.Pos}
				}
				return &ELiteral{Kind: KClock, Date: tk, Posn: t.Pos}
			}
		}
		// Function call?
		if p.peekN(1).Kind == TPunct && p.peekN(1).Text == "(" {
			return p.parseFunctionCall()
		}
		// Postfix ::type cast, e.g. 95.50::tick.
		x := p.parseColumnOrLiteral(t)
		for p.isOp("::") {
			p.adv()
			ty, ok := p.parseTypeName()
			if !ok {
				p.skipBadTypeName()
				return x
			}
			x = &ECast{X: x, Type: ty, Posn: t.Pos}
		}
		return x
	}
	if !p.redundantAt(p.i) {
		p.errAt(t.Pos, "expected an expression, found %s %q", t.Kind, t.Text)
	}
	if t.Kind != TEOF {
		p.adv()
	}
	return nil
}

// recoverCallArg skips a damaged argument up to the next argument separator or
// the call's closing paren. It reports whether the call ended (true) or a new
// argument follows a consumed comma (false).
func (p *Parser) recoverCallArg(oldCtx int, name string) bool {
	base := p.parenDepth
	for !p.atEOF() {
		if p.parenDepth == base {
			if p.isPunct(",") {
				p.adv()
				if p.canStartExpr() {
					return false
				}
				// Another bad argument; keep going.
				continue
			}
			if p.isPunct(")") {
				p.adv()
				return true
			}
			if p.cur().Kind == TKeyword && syncKeywords[p.cur().Text] {
				return true
			}
		}
		if p.parenDepth < base {
			return true
		}
		p.adv()
	}
	return true
}

// canStartExpr reports whether the current token can begin an expression.
func (p *Parser) canStartExpr() bool {
	switch p.cur().Kind {
	case TNumber, TString, TIdent:
		return true
	case TKeyword:
		switch p.cur().Text {
		case "null", "true", "false", "case", "cast", "exists", "not":
			return true
		}
	case TPunct:
		return p.cur().Text == "("
	case TOp:
		if p.cur().Text != "-" && p.cur().Text != "+" {
			return false
		}
		// Unary sign still needs an operand.
		n := p.peekN(1)
		switch n.Kind {
		case TNumber, TString, TIdent:
			return true
		case TPunct:
			return n.Text == "("
		case TKeyword:
			switch n.Text {
			case "null", "true", "false", "case", "cast", "exists", "not":
				return true
			}
		}
		return false
	}
	return false
}

// parseColumnOrLiteral parses a qualified column reference starting at t.
func (p *Parser) parseColumnOrLiteral(t Token) Expr {
	{
		parts := []string{t.Text}
		p.adv()
		for p.isPunct(".") {
			if p.peekN(1).Kind != TIdent {
				p.errAt(p.cur().Pos, "expected a column name after \".\"")
				p.adv()
				if p.atEOF() || p.isPunct(")") || p.isPunct(",") {
					return nil
				}
				if !p.canStartExpr() {
					p.adv()
				}
				break
			}
			p.adv()
			parts = append(parts, p.adv().Text)
		}
		if len(parts) > 3 {
			p.errAt(t.Pos, "column reference has too many parts: %s", strings.Join(parts, "."))
			parts = parts[len(parts)-3:]
		}
		return &EColumn{Parts: parts, Posn: t.Pos}
	}
}

// recoverExpr skips tokens after an expression-level syntax error until the
// next clause keyword, top-level comma, or the ')' that closes the enclosing
// construct. It prevents one damaged operand from turning the rest of the
// clause into a cascade of spurious errors.
func (p *Parser) recoverExpr() {
	base := p.parenDepth
	for !p.atEOF() {
		t := p.cur()
		if p.parenDepth <= base {
			if t.Kind == TPunct && t.Text == "," {
				return
			}
			if t.Kind == TKeyword && syncKeywords[t.Text] {
				return
			}
			// A stray ')' at the recovery level closes the enclosing construct.
			if t.Kind == TPunct && t.Text == ")" {
				return
			}
		}
		p.adv()
	}
	// Errors in the skipped region are consequences of the reported one.
	p.noteDamage(p.cur().Pos)
}

// recoverToCloseParen skips to the ')' matching the innermost open call. It
// starts at the parser's current paren depth so nested constructs are not
// closed early.
func (p *Parser) recoverToCloseParen() {
	base := p.parenDepth - 1
	if base < 0 {
		base = 0
	}
	for !p.atEOF() {
		if p.isPunct(")") && p.parenDepth <= base+1 {
			p.adv()
			return
		}
		p.adv()
	}
}

func (p *Parser) numberLiteral(t Token) Expr {
	if !isAllDigits(t.Text) {
		// Exponent or fraction form: use exact decimal parsing, rejecting
		// exponents we cannot represent exactly.
		if strings.ContainsAny(t.Text, "eE") {
			p.errAt(t.Pos, "exponent notation is not supported; write the literal in full")
			return &ELiteral{Kind: KNull, Posn: t.Pos}
		}
	}
	d, _, ok := ParseDecimal(t.Text)
	if !ok {
		p.errAt(t.Pos, "numeric literal out of range: %s", t.Text)
		return &ELiteral{Kind: KNull, Posn: t.Pos}
	}
	if d.Scale == 0 && isAllDigits(t.Text) {
		if d.Unscaled > 1<<62 {
			p.errAt(t.Pos, "integer literal out of range: %s", t.Text)
			return &ELiteral{Kind: KNull, Posn: t.Pos}
		}
		return &ELiteral{Kind: KInt, Int: d.Unscaled, Posn: t.Pos}
	}
	// Precision is the number of significant written digits.
	digits := strings.ReplaceAll(strings.TrimLeft(t.Text, "0"), ".", "")
	if digits == "" {
		digits = "0"
	}
	pn := len(digits)
	if pn < d.Scale {
		pn = d.Scale + 1
	}
	if strings.HasPrefix(strings.TrimLeft(t.Text, "0"), ".") {
		pn = d.Scale
	}
	return &ELiteral{Kind: KDec, Dec: d, Posn: t.Pos}
}

func (p *Parser) parseFunctionCall() Expr {
	oldCtx := p.lastErrCtx
	nameTok := p.adv()
	p.adv() // '('
	name := strings.ToLower(nameTok.Text)
	fn := &EFunc{Name: name, Posn: nameTok.Pos, NamePos: nameTok.Pos}
	if p.isOp("*") {
		p.adv()
		fn.Star = true
		p.expectPunct(")")
		return fn
	}
	if p.acceptKw("distinct") {
		fn.Distinct = true
	}
	if p.acceptPunct(")") {
		return fn
	}
	for {
		e := p.parseExpr(0)
		if e == nil {
			if len(fn.Args) == 0 && (p.atSync() || p.isPunct(",")) {
				// `sum(stems FROM ...`: the ')' is missing. Report only that.
				if !p.redundantAt(p.i) {
					p.errAt(p.cur().Pos, "expected \")\" to close call to %s", name)
				}
				return fn
			}
			if !p.redundantAt(p.i) {
				p.errAt(p.cur().Pos, "expected an argument in call to %s", name)
			}
			// Rescue: skip the damaged argument, then continue with the next
			// argument if there is one, so independent faults inside the same
			// call are all reported.
			done := p.recoverCallArg(oldCtx, name)
			if done {
				return fn
			}
			continue
		}
		fn.Args = append(fn.Args, e)
		if !p.acceptPunct(",") {
			break
		}
		if !p.canStartExpr() {
			// A comma followed by something that cannot begin an expression is
			// always its own syntactic fault, independent of any earlier one in
			// the same call.
			if !p.redundantAt(p.i) {
				p.errAt(p.cur().Pos, "expression expected after ',' in call to %s", name)
			}
			if p.recoverCallArg(oldCtx, name) {
				return fn
			}
			continue
		}
	}
	if !p.acceptPunct(")") {
		// Report the missing ')' once, then resynchronise. Do not treat the
		// partial argument list as complete: the binder sees what was parsed
		// and reports only if the count is genuinely wrong.
		p.errAt(p.cur().Pos, "expected \")\" to close call to %s, found %s %q", name, p.cur().Kind, p.cur().Text)
		// Do not consume the sync keyword: the caller's clause loop needs it.
	}
	return fn
}

func (p *Parser) parseCase() Expr {
	t := p.adv() // CASE
	c := &ECase{Posn: t.Pos}
	if !p.isKw("when") {
		c.Operand = p.parseExpr(0)
	}
	for p.acceptKw("when") {
		wpos := p.cur().Pos
		cond := p.parseExpr(0)
		if cond == nil {
			p.errAt(p.cur().Pos, "expected a condition after WHEN")
			break
		}
		if !p.expectKw("then") {
			break
		}
		then := p.parseExpr(0)
		if then == nil {
			p.errAt(p.cur().Pos, "expected an expression after THEN")
			break
		}
		c.Whens = append(c.Whens, CaseWhen{Cond: cond, Then: then, Posn: wpos})
	}
	if len(c.Whens) == 0 {
		p.errAt(t.Pos, "CASE requires at least one WHEN branch")
	}
	if p.acceptKw("else") {
		c.Else = p.parseExpr(0)
		if c.Else == nil {
			p.errAt(p.cur().Pos, "expected an expression after ELSE")
		}
	}
	if !p.expectKw("end") {
		p.errAt(p.cur().Pos, "unterminated CASE expression")
	}
	return c
}

func (p *Parser) parseCast() Expr {
	t := p.adv() // CAST
	p.expectPunct("(")
	x := p.parseExpr(0)
	if x == nil {
		p.errAt(p.cur().Pos, "expected an expression inside CAST")
	}
	if !p.expectKw("as") {
		p.recoverToCloseParen()
		return x
	}
	ty, ok := p.parseTypeName()
	if !ok {
		p.skipBadTypeName()
		return x
	}
	if !p.atStmtEnd() {
		p.expectPunct(")")
	}
	return &ECast{X: x, Type: ty, Posn: t.Pos}
}

// parseTypeName parses a type name used in CAST and column declarations.
func (p *Parser) parseTypeName() (Type, bool) {
	if p.cur().Kind != TIdent {
		p.errAt(p.cur().Pos, "expected a type name, found %s %q", p.cur().Kind, p.cur().Text)
		return Type{}, false
	}
	name := strings.ToLower(p.adv().Text)
	switch name {
	case "int", "integer":
		return IntT, true
	case "decimal", "numeric":
		prec, scale := 18, 6
		if p.acceptPunct("(") {
			pv, ok := p.parseIntLiteral()
			if !ok {
				return Type{}, false
			}
			prec = int(pv)
			scale = 0
			if p.acceptPunct(",") {
				sv, ok := p.parseIntLiteral()
				if !ok {
					return Type{}, false
				}
				scale = int(sv)
			}
			p.expectPunct(")")
		}
		if prec < 1 || prec > 18 {
			p.errAt(p.cur().Pos, "decimal precision must be between 1 and 18, got %d", prec)
			return Type{}, false
		}
		if scale < 0 || scale > prec {
			p.errAt(p.cur().Pos, "decimal scale must be between 0 and the precision (%d), got %d", prec, scale)
			return Type{}, false
		}
		return DecType(prec, scale), true
	case "string", "text", "varchar":
		return StrT, true
	case "bool", "boolean":
		return BoolT, true
	case "date":
		return DateT, true
	case "tick", "clock", "price":
		return ClockT, true
	case "null":
		return NullT, true
	}
	p.errAt(p.cur().Pos, "unknown type %q", name)
	return Type{}, false
}

// skipBadTypeName resynchronises after a malformed type name: to the next
// argument separator or closing paren at the current nesting level, so the
// enclosing call or clause can continue.
func (p *Parser) skipBadTypeName() {
	base := p.parenDepth
	for !p.atEOF() {
		// Never run past the ')' that closes the enclosing group: it belongs
		// to the outer construct, and consuming it corrupts the rest of the
		// statement.
		if p.parenDepth < base {
			return
		}
		if p.parenDepth == base {
			if p.isPunct(",") || p.isPunct(")") {
				return
			}
			if p.cur().Kind == TKeyword && syncKeywords[p.cur().Text] {
				return
			}
		}
		// AND/OR separate independent predicate terms even inside a damaged
		// group; stopping there lets the rescue parser find later faults.
		if p.cur().Kind == TKeyword && (p.cur().Text == "and" || p.cur().Text == "or") {
			return
		}
		p.adv()
	}
}

// atStmtEnd reports whether the cursor is at the end of the statement or a
// clause boundary, where further expression recovery would be pointless.
func (p *Parser) atStmtEnd() bool {
	if p.atEOF() {
		return true
	}
	return p.cur().Kind == TKeyword && syncKeywords[p.cur().Text]
}

var _ = fmt.Sprintf
