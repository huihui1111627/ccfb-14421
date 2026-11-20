package engine

import (
	"fmt"
	"strconv"
	"strings"
)

// Node is anything that carries a source position.
type Node interface {
	Pos() Pos
}

// Statement is a complete query: WITH + body + ORDER BY + LIMIT/OFFSET.
type Statement struct {
	With    *WithClause
	Body    QueryBody
	OrderBy []SortItem
	Limit   *int64
	Offset  *int64
	Posn    Pos
}

func (s *Statement) Pos() Pos { return s.Posn }

func (s *Statement) String() string {
	var b strings.Builder
	if s.With != nil {
		b.WriteString(s.With.String())
		b.WriteByte(' ')
	}
	b.WriteString(s.Body.String())
	if len(s.OrderBy) > 0 {
		b.WriteString(" ORDER BY ")
		for i, it := range s.OrderBy {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(it.String())
		}
	}
	if s.Limit != nil {
		fmt.Fprintf(&b, " LIMIT %d", *s.Limit)
	}
	if s.Offset != nil {
		fmt.Fprintf(&b, " OFFSET %d", *s.Offset)
	}
	return b.String()
}

// WithClause introduces common table expressions.
type WithClause struct {
	Recursive bool
	CTEs      []*CTE
	Posn      Pos
}

func (w *WithClause) Pos() Pos { return w.Posn }

func (w *WithClause) String() string {
	var b strings.Builder
	b.WriteString("WITH ")
	if w.Recursive {
		b.WriteString("RECURSIVE ")
	}
	for i, c := range w.CTEs {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(c.String())
	}
	return b.String()
}

// CTE is one named common table expression.
type CTE struct {
	Name    string
	Cols    []string
	Body    QueryBody
	Posn    Pos
	NamePos Pos
}

func (c *CTE) Pos() Pos { return c.Posn }

func (c *CTE) String() string {
	var b strings.Builder
	b.WriteString(c.Name)
	if len(c.Cols) > 0 {
		b.WriteString("(")
		b.WriteString(strings.Join(c.Cols, ", "))
		b.WriteString(")")
	}
	b.WriteString(" AS (")
	b.WriteString(c.Body.String())
	b.WriteString(")")
	return b.String()
}

// QueryBody is a SELECT core or a set operation.
type QueryBody interface {
	Node
	queryBody()
	String() string
}

// SelectCore is a single SELECT ... FROM ... WHERE ... GROUP BY ... HAVING.
type SelectCore struct {
	Distinct bool
	Items    []*SelectItem
	From     TableExpr
	Where    Expr
	GroupBy  []Expr
	Having   Expr
	Posn     Pos
}

func (*SelectCore) queryBody() {}

func (s *SelectCore) Pos() Pos { return s.Posn }

func (s *SelectCore) String() string {
	var b strings.Builder
	b.WriteString("SELECT ")
	if s.Distinct {
		b.WriteString("DISTINCT ")
	}
	for i, it := range s.Items {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(it.String())
	}
	if s.From != nil {
		b.WriteString(" FROM ")
		b.WriteString(s.From.String())
	}
	if s.Where != nil {
		b.WriteString(" WHERE ")
		b.WriteString(s.Where.String())
	}
	if len(s.GroupBy) > 0 {
		b.WriteString(" GROUP BY ")
		for i, g := range s.GroupBy {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(g.String())
		}
	}
	if s.Having != nil {
		b.WriteString(" HAVING ")
		b.WriteString(s.Having.String())
	}
	return b.String()
}

// SetOp combines two query bodies.
type SetOp struct {
	Op    string // UNION, EXCEPT, INTERSECT
	All   bool
	Left  QueryBody
	Right QueryBody
	Posn  Pos
}

func (*SetOp) queryBody() {}

func (s *SetOp) Pos() Pos { return s.Posn }

func (s *SetOp) String() string {
	out := s.Left.String() + " " + s.Op
	if s.All {
		out += " ALL"
	}
	return out + " " + s.Right.String()
}

// SelectItem is one projection entry.
type SelectItem struct {
	Expr     Expr
	Alias    string
	Star     bool
	Table    string // for t.*
	Posn     Pos
	AliasPos Pos
}

func (s *SelectItem) Pos() Pos { return s.Posn }

func (s *SelectItem) String() string {
	var out string
	if s.Star {
		if s.Table != "" {
			out = s.Table + ".*"
		} else {
			out = "*"
		}
	} else {
		out = s.Expr.String()
	}
	if s.Alias != "" {
		out += " AS " + s.Alias
	}
	return out
}

// SortItem is one ORDER BY entry.
type SortItem struct {
	Expr Expr
	Desc bool
	Posn Pos
}

func (s *SortItem) Pos() Pos { return s.Posn }

func (s *SortItem) String() string {
	out := s.Expr.String()
	if s.Desc {
		out += " DESC"
	}
	return out
}

// ---------------------------------------------------------------------------
// Table expressions
// ---------------------------------------------------------------------------

type TableExpr interface {
	Node
	tableExpr()
	String() string
}

type TableName struct {
	Name     string
	Alias    string
	Posn     Pos
	NamePos  Pos
	AliasPos Pos
}

func (*TableName) tableExpr() {}

func (t *TableName) Pos() Pos { return t.Posn }

func (t *TableName) String() string {
	out := t.Name
	if t.Alias != "" {
		out += " AS " + t.Alias
	}
	return out
}

type SubqueryTable struct {
	Body     *Statement
	Alias    string
	Lateral  bool
	Posn     Pos
	AliasPos Pos
}

func (*SubqueryTable) tableExpr() {}

func (t *SubqueryTable) Pos() Pos { return t.Posn }

func (t *SubqueryTable) String() string {
	out := "(" + t.Body.String() + ")"
	if t.Lateral {
		out = "LATERAL " + out
	}
	if t.Alias != "" {
		out += " AS " + t.Alias
	}
	return out
}

// JoinKind is the SQL join flavour.
type JoinKind int

const (
	JoinInner JoinKind = iota
	JoinLeft
	JoinRight
	JoinFull
	JoinCross
	JoinAsof
)

func (k JoinKind) String() string {
	switch k {
	case JoinLeft:
		return "LEFT JOIN"
	case JoinRight:
		return "RIGHT JOIN"
	case JoinFull:
		return "FULL JOIN"
	case JoinCross:
		return "CROSS JOIN"
	case JoinAsof:
		return "ASOF JOIN"
	}
	return "INNER JOIN"
}

// IsOuter reports whether unmatched rows on either side are preserved.
func (k JoinKind) IsOuter() bool {
	return k == JoinLeft || k == JoinRight || k == JoinFull
}

type JoinTable struct {
	Kind     JoinKind
	Left     TableExpr
	Right    TableExpr
	On       Expr
	Using    []string
	UsingPos []Pos
	Posn     Pos
	KindPos  Pos
}

func (*JoinTable) tableExpr() {}

func (t *JoinTable) Pos() Pos { return t.Posn }

func (t *JoinTable) String() string {
	out := t.Left.String() + " " + t.Kind.String() + " " + t.Right.String()
	if t.On != nil {
		out += " ON " + t.On.String()
	}
	if len(t.Using) > 0 {
		out += " USING (" + strings.Join(t.Using, ", ") + ")"
	}
	return out
}

// ---------------------------------------------------------------------------
// Expressions
// ---------------------------------------------------------------------------

type Expr interface {
	Node
	exprNode()
	String() string
}

type ELiteral struct {
	Kind Kind
	Int  int64
	Dec  Decimal
	Str  string
	Bool bool
	Date int32
	Posn Pos
}

func (*ELiteral) exprNode() {}

func (e *ELiteral) Pos() Pos { return e.Posn }

func (e *ELiteral) String() string {
	switch e.Kind {
	case KInt:
		return strconv.FormatInt(e.Int, 10)
	case KDec:
		return e.Dec.String()
	case KString:
		return "'" + strings.ReplaceAll(e.Str, "'", "''") + "'"
	case KBool:
		if e.Bool {
			return "TRUE"
		}
		return "FALSE"
	case KDate:
		return "DATE '" + FormatDate(e.Date) + "'"
	case KClock:
		return "TICK '" + formatTick(int32(e.Date)) + "'"
	case KNull:
		return "NULL"
	}
	return "?"
}

type EColumn struct {
	Parts []string
	Posn  Pos
}

func (*EColumn) exprNode() {}

func (e *EColumn) Pos() Pos { return e.Posn }

func (e *EColumn) String() string { return strings.Join(e.Parts, ".") }

type EStar struct {
	Table string
	Posn  Pos
}

func (*EStar) exprNode() {}

func (e *EStar) Pos() Pos { return e.Posn }

func (e *EStar) String() string {
	if e.Table != "" {
		return e.Table + ".*"
	}
	return "*"
}

type EUnary struct {
	Op    string // -, +, not
	X     Expr
	Posn  Pos
	OpPos Pos
}

func (*EUnary) exprNode() {}

func (e *EUnary) Pos() Pos { return e.Posn }

func (e *EUnary) String() string {
	if e.Op == "not" {
		return "NOT " + formatExpr(e.X, 3)
	}
	return e.Op + formatExpr(e.X, 7)
}

type EBinary struct {
	Op    string
	L     Expr
	R     Expr
	Posn  Pos
	OpPos Pos
}

func (*EBinary) exprNode() {}

func (e *EBinary) Pos() Pos { return e.Posn }

func binaryPrec(op string) int {
	switch op {
	case "or":
		return 1
	case "and":
		return 2
	case "=", "<>", "<", ">", "<=", ">=":
		return 4
	case "+", "-":
		return 5
	case "*", "/", "%":
		return 6
	}
	return 4
}

func (e *EBinary) String() string {
	prec := binaryPrec(e.Op)
	return formatExpr(e.L, prec) + " " + strings.ToUpper(e.Op) + " " + formatExpr(e.R, prec+1)
}

type EFunc struct {
	Name     string
	Args     []Expr
	Star     bool
	Distinct bool
	Posn     Pos
	NamePos  Pos
}

func (*EFunc) exprNode() {}

func (e *EFunc) Pos() Pos { return e.Posn }

func (e *EFunc) String() string {
	var b strings.Builder
	b.WriteString(e.Name)
	b.WriteString("(")
	if e.Star {
		b.WriteString("*")
	} else {
		if e.Distinct {
			b.WriteString("DISTINCT ")
		}
		for i, a := range e.Args {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(a.String())
		}
	}
	b.WriteString(")")
	return b.String()
}

type EStarArg struct{ Posn Pos }

func (*EStarArg) exprNode() {}

func (e *EStarArg) Pos() Pos { return e.Posn }

func (e *EStarArg) String() string { return "*" }

type ECast struct {
	X    Expr
	Type Type
	Posn Pos
}

func (*ECast) exprNode() {}

func (e *ECast) Pos() Pos { return e.Posn }

func (e *ECast) String() string { return "CAST(" + e.X.String() + " AS " + e.Type.String() + ")" }

type ECase struct {
	Operand Expr
	Whens   []CaseWhen
	Else    Expr
	Posn    Pos
}

type CaseWhen struct {
	Cond Expr
	Then Expr
	Posn Pos
}

func (*ECase) exprNode() {}

func (e *ECase) Pos() Pos { return e.Posn }

func (e *ECase) String() string {
	var b strings.Builder
	b.WriteString("CASE")
	if e.Operand != nil {
		b.WriteString(" " + e.Operand.String())
	}
	for _, w := range e.Whens {
		b.WriteString(" WHEN " + w.Cond.String() + " THEN " + w.Then.String())
	}
	if e.Else != nil {
		b.WriteString(" ELSE " + e.Else.String())
	}
	b.WriteString(" END")
	return b.String()
}

type EIs struct {
	X     Expr
	What  string // null, true, false
	Not   bool
	Posn  Pos
	OpPos Pos
}

func (*EIs) exprNode() {}

func (e *EIs) Pos() Pos { return e.Posn }

func (e *EIs) String() string {
	out := e.X.String() + " IS "
	if e.Not {
		out += "NOT "
	}
	return out + strings.ToUpper(e.What)
}

type EBetween struct {
	X     Expr
	Lo    Expr
	Hi    Expr
	Not   bool
	Posn  Pos
	OpPos Pos
}

func (*EBetween) exprNode() {}

func (e *EBetween) Pos() Pos { return e.Posn }

func (e *EBetween) String() string {
	not := ""
	if e.Not {
		not = "NOT "
	}
	return e.X.String() + " " + not + "BETWEEN " + e.Lo.String() + " AND " + e.Hi.String()
}

type EInList struct {
	X     Expr
	List  []Expr
	Not   bool
	Posn  Pos
	OpPos Pos
}

func (*EInList) exprNode() {}

func (e *EInList) Pos() Pos { return e.Posn }

func (e *EInList) String() string {
	not := ""
	if e.Not {
		not = "NOT "
	}
	parts := make([]string, len(e.List))
	for i, x := range e.List {
		parts[i] = x.String()
	}
	return e.X.String() + " " + not + "IN (" + strings.Join(parts, ", ") + ")"
}

type EInSubquery struct {
	X     Expr
	Body  *Statement
	Not   bool
	Posn  Pos
	OpPos Pos
}

func (*EInSubquery) exprNode() {}

func (e *EInSubquery) Pos() Pos { return e.Posn }

func (e *EInSubquery) String() string {
	not := ""
	if e.Not {
		not = "NOT "
	}
	return e.X.String() + " " + not + "IN (" + e.Body.String() + ")"
}

type ELike struct {
	X     Expr
	Pat   Expr
	Not   bool
	Posn  Pos
	OpPos Pos
}

func (*ELike) exprNode() {}

func (e *ELike) Pos() Pos { return e.Posn }

func (e *ELike) String() string {
	not := ""
	if e.Not {
		not = "NOT "
	}
	return e.X.String() + " " + not + "LIKE " + e.Pat.String()
}

type EScalarSubquery struct {
	Body *Statement
	Posn Pos
}

func (*EScalarSubquery) exprNode() {}

func (e *EScalarSubquery) Pos() Pos { return e.Posn }

func (e *EScalarSubquery) String() string { return "(" + e.Body.String() + ")" }

type EExists struct {
	Body *Statement
	Posn Pos
}

func (*EExists) exprNode() {}

func (e *EExists) Pos() Pos { return e.Posn }

func (e *EExists) String() string { return "EXISTS (" + e.Body.String() + ")" }

type EParen struct {
	X    Expr
	Posn Pos
}

func (*EParen) exprNode() {}

func (e *EParen) Pos() Pos { return e.Posn }

func (e *EParen) String() string { return "(" + e.X.String() + ")" }

// formatExpr prints an expression with parentheses only where precedence
// requires them.
func formatExpr(e Expr, minPrec int) string {
	if e == nil {
		return "<nil>"
	}
	switch x := e.(type) {
	case *EBinary:
		prec := binaryPrec(x.Op)
		s := formatExpr(x.L, prec) + " " + strings.ToUpper(x.Op) + " " + formatExpr(x.R, prec+1)
		if prec < minPrec {
			return "(" + s + ")"
		}
		return s
	case *EUnary:
		prec := 3
		if x.Op != "not" {
			prec = 7
		}
		s := x.String()
		if prec < minPrec {
			return "(" + s + ")"
		}
		return s
	case *EParen:
		inner := formatExpr(x.X, 0)
		if minPrec > 8 {
			return "(" + inner + ")"
		}
		return "(" + inner + ")"
	}
	return e.String()
}
