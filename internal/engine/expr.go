package engine

import (
	"fmt"
	"strings"
)

// BExpr is a bound, typed expression tree. Unlike the parser's AST, every node
// here knows its result type, and column references are resolved to ordinals.
// Rewrite rules operate on this layer; the AST is never mutated after binding.
type BExpr interface {
	Ty() Type
	Pos() Pos
	String() string
}

// ---------------------------------------------------------------------------
// Nodes
// ---------------------------------------------------------------------------

// ColRef points at a column of an input row. Depth 0 is the current query
// block's row; depth n>0 reaches outward through correlated query blocks.
type ColRef struct {
	Depth     int
	Index     int
	Qualifier string
	Name      string
	T         Type
	Posn      Pos
}

func (e *ColRef) Ty() Type { return e.T }
func (e *ColRef) Pos() Pos { return e.Posn }
func (e *ColRef) String() string {
	if e.Qualifier != "" {
		return e.Qualifier + "." + e.Name
	}
	return e.Name
}

// Lit is a constant. T may be wider than the literal's natural type (for
// example NULL typed by context).
type Lit struct {
	V    Value
	T    Type
	Posn Pos
}

func (e *Lit) Ty() Type { return e.T }
func (e *Lit) Pos() Pos { return e.Posn }
func (e *Lit) String() string {
	if e.T.Kind == KNull || e.V.Null {
		return "NULL"
	}
	switch e.T.Kind {
	case KDec:
		if e.V.Kind == KDec {
			if d, ok := e.V.Dec.rescale(e.T.Scale); ok {
				return d.String()
			}
		}
	case KDate:
		return "DATE '" + FormatDate(e.V.Date) + "'"
	case KClock:
		return "TICK '" + formatTick(e.V.Clock) + "'"
	case KString:
		return "'" + strings.ReplaceAll(e.V.Str, "'", "''") + "'"
	case KBool:
		if e.V.Bool {
			return "TRUE"
		}
		return "FALSE"
	case KInt:
		return fmt.Sprintf("%d", e.V.Int)
	}
	return e.V.String()
}

type Un struct {
	Op   string
	X    BExpr
	T    Type
	Posn Pos
}

func (e *Un) Ty() Type { return e.T }
func (e *Un) Pos() Pos { return e.Posn }
func (e *Un) String() string {
	if e.Op == "not" {
		return "NOT " + exprPrec(e.X, 3)
	}
	return e.Op + exprPrec(e.X, 7)
}

type Bin struct {
	Op   string
	L    BExpr
	R    BExpr
	T    Type
	Posn Pos
}

func (e *Bin) Ty() Type { return e.T }
func (e *Bin) Pos() Pos { return e.Posn }
func (e *Bin) String() string {
	if e.Op == "and" || e.Op == "or" {
		prec := 1
		if e.Op == "and" {
			prec = 2
		}
		return exprPrec(e.L, prec) + " " + strings.ToUpper(e.Op) + " " + exprPrec(e.R, prec+1)
	}
	prec := binPrec(e.Op)
	return exprPrec(e.L, prec) + " " + opSymbol(e.Op) + " " + exprPrec(e.R, prec+1)
}

func opSymbol(op string) string {
	switch op {
	case "eq":
		return "="
	case "ne":
		return "<>"
	case "lt":
		return "<"
	case "le":
		return "<="
	case "gt":
		return ">"
	case "ge":
		return ">="
	case "and":
		return "AND"
	case "or":
		return "OR"
	}
	return op
}

func binPrec(op string) int {
	switch op {
	case "and":
		return 2
	case "or":
		return 1
	case "eq", "ne", "lt", "le", "gt", "ge":
		return 4
	case "add", "sub":
		return 5
	case "mul", "div", "mod":
		return 6
	}
	return 4
}

type Fn struct {
	Name string
	Args []BExpr
	T    Type
	Posn Pos
}

func (e *Fn) Ty() Type { return e.T }
func (e *Fn) Pos() Pos { return e.Posn }
func (e *Fn) String() string {
	parts := make([]string, len(e.Args))
	for i, a := range e.Args {
		parts[i] = a.String()
	}
	return e.Name + "(" + strings.Join(parts, ", ") + ")"
}

type Cast struct {
	X    BExpr
	T    Type
	Posn Pos
}

func (e *Cast) Ty() Type { return e.T }
func (e *Cast) Pos() Pos { return e.Posn }
func (e *Cast) String() string {
	return "CAST(" + e.X.String() + " AS " + e.T.String() + ")"
}

type When struct {
	Cond    BExpr
	Then    BExpr
	Posn    Pos
	Operand bool // cond compares to the CASE operand
}

type Case struct {
	Operand BExpr
	Whens   []When
	Else    BExpr
	T       Type
	Posn    Pos
}

func (e *Case) Ty() Type { return e.T }
func (e *Case) Pos() Pos { return e.Posn }
func (e *Case) String() string {
	var b strings.Builder
	b.WriteString("CASE")
	if e.Operand != nil {
		b.WriteString(" " + e.Operand.String())
	}
	for _, w := range e.Whens {
		if w.Operand {
			b.WriteString(" WHEN " + w.Cond.String())
		} else {
			b.WriteString(" WHEN " + w.Cond.String())
		}
		b.WriteString(" THEN " + w.Then.String())
	}
	if e.Else != nil {
		b.WriteString(" ELSE " + e.Else.String())
	}
	b.WriteString(" END")
	return b.String()
}

type IsNull struct {
	X    BExpr
	Not  bool
	Posn Pos
}

func (e *IsNull) Ty() Type { return BoolT }
func (e *IsNull) Pos() Pos { return e.Posn }
func (e *IsNull) String() string {
	if e.Not {
		return e.X.String() + " IS NOT NULL"
	}
	return e.X.String() + " IS NULL"
}

type Between struct {
	X    BExpr
	Lo   BExpr
	Hi   BExpr
	Not  bool
	Posn Pos
}

func (e *Between) Ty() Type { return BoolT }
func (e *Between) Pos() Pos { return e.Posn }
func (e *Between) String() string {
	n := ""
	if e.Not {
		n = "NOT "
	}
	return e.X.String() + " " + n + "BETWEEN " + e.Lo.String() + " AND " + e.Hi.String()
}

type InList struct {
	X    BExpr
	List []BExpr
	Not  bool
	Posn Pos
}

func (e *InList) Ty() Type { return BoolT }
func (e *InList) Pos() Pos { return e.Posn }
func (e *InList) String() string {
	n := ""
	if e.Not {
		n = "NOT "
	}
	parts := make([]string, len(e.List))
	for i, x := range e.List {
		parts[i] = x.String()
	}
	return e.X.String() + " " + n + "IN (" + strings.Join(parts, ", ") + ")"
}

type InSub struct {
	X    BExpr
	Plan *PlanNode
	Not  bool
	T    Type
	Posn Pos
}

func (e *InSub) Ty() Type { return e.T }
func (e *InSub) Pos() Pos { return e.Posn }
func (e *InSub) String() string {
	n := ""
	if e.Not {
		n = "NOT "
	}
	return e.X.String() + " " + n + "IN (" + e.Plan.planSummary() + ")"
}

type Like struct {
	X    BExpr
	Pat  BExpr
	Not  bool
	Posn Pos
}

func (e *Like) Ty() Type { return BoolT }
func (e *Like) Pos() Pos { return e.Posn }
func (e *Like) String() string {
	n := ""
	if e.Not {
		n = "NOT "
	}
	return e.X.String() + " " + n + "LIKE " + e.Pat.String()
}

// ScalarSub is a scalar subquery. Its plan returns exactly one column; at
// runtime more than one row is an error.
type ScalarSub struct {
	Plan *PlanNode
	T    Type
	Posn Pos
}

func (e *ScalarSub) Ty() Type { return e.T }
func (e *ScalarSub) Pos() Pos { return e.Posn }
func (e *ScalarSub) String() string {
	return "(" + e.Plan.planSummary() + ")"
}

type Exists struct {
	Plan *PlanNode
	Posn Pos
}

func (e *Exists) Ty() Type { return BoolT }
func (e *Exists) Pos() Pos { return e.Posn }
func (e *Exists) String() string {
	return "EXISTS (" + e.Plan.planSummary() + ")"
}

// AggRef reads a computed aggregate from the aggregate node's output row.
type AggRef struct {
	Index int
	Name  string
	T     Type
	Posn  Pos
}

func (e *AggRef) Ty() Type       { return e.T }
func (e *AggRef) Pos() Pos       { return e.Posn }
func (e *AggRef) String() string { return e.Name }

// ---------------------------------------------------------------------------
// Printing with minimal parentheses
// ---------------------------------------------------------------------------

func exprPrec(e BExpr, minPrec int) string {
	switch x := e.(type) {
	case *Bin:
		var prec int
		if x.Op == "and" {
			prec = 2
		} else if x.Op == "or" {
			prec = 1
		} else {
			prec = binPrec(x.Op)
		}
		s := x.String()
		if prec < minPrec {
			return "(" + s + ")"
		}
		return s
	case *Un:
		prec := 3
		if x.Op != "not" {
			prec = 7
		}
		if prec < minPrec {
			return "(" + x.String() + ")"
		}
		return x.String()
	}
	return e.String()
}

// ---------------------------------------------------------------------------
// Traversal helpers
// ---------------------------------------------------------------------------

// children returns the direct subexpressions, including plans' root
// expressions? No: plans are traversed separately.
func exprChildren(e BExpr) []BExpr {
	switch x := e.(type) {
	case *Un:
		return []BExpr{x.X}
	case *Bin:
		return []BExpr{x.L, x.R}
	case *Fn:
		return x.Args
	case *Cast:
		return []BExpr{x.X}
	case *Case:
		out := []BExpr{}
		if x.Operand != nil {
			out = append(out, x.Operand)
		}
		for _, w := range x.Whens {
			out = append(out, w.Cond, w.Then)
		}
		if x.Else != nil {
			out = append(out, x.Else)
		}
		return out
	case *IsNull:
		return []BExpr{x.X}
	case *Between:
		return []BExpr{x.X, x.Lo, x.Hi}
	case *InList:
		return append([]BExpr{x.X}, x.List...)
	case *InSub:
		return []BExpr{x.X}
	case *Like:
		return []BExpr{x.X, x.Pat}
	}
	return nil
}

// mapExpr applies f bottom-up to every node of e. If f returns a different
// node, the node's rebuilt parents see the replacement.
func mapExpr(e BExpr, f func(BExpr) BExpr) BExpr {
	if e == nil {
		return nil
	}
	switch x := e.(type) {
	case *Un:
		n := *x
		n.X = mapExpr(x.X, f)
		return f(&n)
	case *Bin:
		n := *x
		n.L = mapExpr(x.L, f)
		n.R = mapExpr(x.R, f)
		return f(&n)
	case *Fn:
		n := *x
		n.Args = make([]BExpr, len(x.Args))
		for i, a := range x.Args {
			n.Args[i] = mapExpr(a, f)
		}
		return f(&n)
	case *Cast:
		n := *x
		n.X = mapExpr(x.X, f)
		return f(&n)
	case *Case:
		n := *x
		if x.Operand != nil {
			n.Operand = mapExpr(x.Operand, f)
		}
		n.Whens = make([]When, len(x.Whens))
		for i, w := range x.Whens {
			n.Whens[i] = w
			n.Whens[i].Cond = mapExpr(w.Cond, f)
			n.Whens[i].Then = mapExpr(w.Then, f)
		}
		if x.Else != nil {
			n.Else = mapExpr(x.Else, f)
		}
		return f(&n)
	case *IsNull:
		n := *x
		n.X = mapExpr(x.X, f)
		return f(&n)
	case *Between:
		n := *x
		n.X = mapExpr(x.X, f)
		n.Lo = mapExpr(x.Lo, f)
		n.Hi = mapExpr(x.Hi, f)
		return f(&n)
	case *InList:
		n := *x
		n.X = mapExpr(x.X, f)
		n.List = make([]BExpr, len(x.List))
		for i, v := range x.List {
			n.List[i] = mapExpr(v, f)
		}
		return f(&n)
	case *InSub:
		n := *x
		n.X = mapExpr(x.X, f)
		return f(&n)
	case *Like:
		n := *x
		n.X = mapExpr(x.X, f)
		n.Pat = mapExpr(x.Pat, f)
		return f(&n)
	}
	return f(e)
}

// walkExpr visits e without rebuilding.
func walkExpr(e BExpr, f func(BExpr)) {
	if e == nil {
		return
	}
	f(e)
	for _, c := range exprChildren(e) {
		walkExpr(c, f)
	}
}

func exprHasColumn(e BExpr) bool {
	found := false
	walkExpr(e, func(x BExpr) {
		switch x.(type) {
		case *ColRef, *AggRef:
			found = true
		}
	})
	return found
}

func exprHasSubquery(e BExpr) bool {
	found := false
	walkExpr(e, func(x BExpr) {
		switch x.(type) {
		case *ScalarSub, *InSub, *Exists:
			found = true
		}
	})
	return found
}

// exprHasAggRef reports whether a post-aggregation expression still reads an
// aggregate slot (used to validate placement).
func exprHasAggRef(e BExpr) bool {
	found := false
	walkExpr(e, func(x BExpr) {
		if _, ok := x.(*AggRef); ok {
			found = true
		}
	})
	return found
}

// exprEqual compares two expressions structurally, including resolved column
// ordinals. Used to match GROUP BY keys.
func exprEqual(a, b BExpr) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return exprKey(a) == exprKey(b)
}

func exprKey(e BExpr) string {
	if e == nil {
		return "<nil>"
	}
	switch x := e.(type) {
	case *ColRef:
		return fmt.Sprintf("col(%d,%d,%s)", x.Depth, x.Index, x.T.String())
	case *Lit:
		if x.V.Null {
			return "null(" + x.T.String() + ")"
		}
		return "lit(" + x.T.String() + ":" + x.V.String() + ")"
	case *AggRef:
		return fmt.Sprintf("agg(%d,%s)", x.Index, x.T.String())
	case *ScalarSub:
		return "sub(" + x.Plan.fingerprint() + ")"
	case *InSub:
		return "insub(" + exprKey(x.X) + "," + x.Plan.fingerprint() + fmt.Sprint(x.Not) + ")"
	case *Exists:
		return "exists(" + x.Plan.fingerprint() + ")"
	}
	kids := exprChildren(e)
	parts := make([]string, len(kids))
	for i, k := range kids {
		parts[i] = exprKey(k)
	}
	return fmt.Sprintf("%T(%s)[%s]", e, e.String(), strings.Join(parts, "|"))
}

// containsSide reports whether e references a depth-0 column whose ordinal
// falls in [lo, hi).
func containsSide(e BExpr, lo, hi int) bool {
	found := false
	walkExpr(e, func(x BExpr) {
		if c, ok := x.(*ColRef); ok && c.Depth == 0 && c.Index >= lo && c.Index < hi {
			found = true
		}
	})
	return found
}

// PlanString renders a subquery plan one line for expression printing.
func (n *PlanNode) planSummary() string {
	if n == nil {
		return "<nil>"
	}
	return n.label()
}
