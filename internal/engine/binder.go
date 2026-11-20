package engine

import (
	"fmt"
	"strings"
)

// Binder turns a parsed AST into a typed logical plan. It resolves names
// (including CTEs, correlated subqueries and lateral derived tables), checks
// types, lifts aggregates, and reports every independent name/type error it
// finds rather than stopping at the first.
type Binder struct {
	cat     *Catalog
	reg     *Registry
	ids     *idAlloc
	errs    []*Error
	cteEnv  map[string]*cteBinding
	cteInfo map[string]*CTEInfo
	refs    map[string]int
	blockID int
	// suppressExprErrors suppresses errors that are consequences of an
	// already-reported syntax error.
	suppressExprErrors bool
	// damageLine/damageCol delimit the region damaged by syntax errors. A
	// semantic error before that point may be spurious; one after it is real.
	damageLine int
	damageCol  int
}

type cteBinding struct {
	name  string
	cols  []string
	types []Type
	plan  *PlanNode
	// node is the CTEScan prototype; rules may clone body or keep CTEScan.
	recursive bool
}

// scope is one query block's row descriptor plus a pointer to its
// correlatable parent.
type scope struct {
	names    []string
	quals    []string
	types    []Type
	nullable []bool
	parent   *scope
	// lateral marks that the parent scope is a lateral level; correlated refs
	// through a lateral are still allowed and are handled by the evaluator.
	depth int
}

func newScope(parent *scope) *scope {
	d := 0
	if parent != nil {
		d = parent.depth + 1
	}
	return &scope{parent: parent, depth: d}
}

func (s *scope) add(name, qual string, t Type, nullable bool) {
	s.names = append(s.names, name)
	s.quals = append(s.quals, qual)
	s.types = append(s.types, t)
	s.nullable = append(s.nullable, nullable)
}

func (s *scope) addAll(cols []OutputCol) int {
	start := len(s.names)
	for _, c := range cols {
		s.add(c.Name, c.Qualifier, c.Type, c.Nullable)
	}
	return start
}

// NewBinder creates a binder over a catalog and function registry.
func NewBinder(cat *Catalog, reg *Registry) *Binder {
	return &Binder{
		cat:     cat,
		reg:     reg,
		ids:     &idAlloc{},
		cteEnv:  map[string]*cteBinding{},
		cteInfo: map[string]*CTEInfo{},
		refs:    map[string]int{},
	}
}

// Bind resolves a parsed statement. The returned plan is nil only if binding
// could not produce a usable tree at all; errs lists every diagnostic found.
func (b *Binder) Bind(stmt *Statement) (*Plan, []*Error) {
	root := b.bindStatement(stmt, nil)
	plan := &Plan{Root: root, MaxID: b.ids.n, CTEInfo: b.cteInfo, RefCounts: b.refs}
	if root != nil {
		plan.Output = root.Schema
	}
	errs := sortErrors(b.errs)
	return plan, errs
}

func (b *Binder) errorf(code Code, p Pos, format string, args ...any) {
	b.errs = append(b.errs, errAt(code, p, format, args...))
}

func (b *Binder) errorHints(code Code, p Pos, msg string, hints []string) {
	b.errs = append(b.errs, errWithHints(code, p, msg, hints))
}

func (b *Binder) bindStatement(stmt *Statement, outer *scope) *PlanNode {
	if stmt == nil {
		return nil
	}
	// WITH is visible to the body and to itself for RECURSIVE.
	if stmt.With != nil {
		for _, c := range stmt.With.CTEs {
			if c.Name == "" {
				continue
			}
			key := strings.ToLower(c.Name)
			if _, dup := b.cteEnv[key]; dup {
				b.errorf(CodeName, c.NamePos, "CTE %q is defined more than once", c.Name)
			}
			// Bind the body with the CTE environment as it stands (excluding
			// itself unless RECURSIVE).
			body := b.bindQueryBody(c.Body, outer)
			if body == nil {
				continue
			}
			types := make([]Type, len(body.Schema))
			names := make([]string, len(body.Schema))
			for i, col := range body.Schema {
				types[i] = col.Type
				names[i] = col.Name
				if i < len(c.Cols) {
					names[i] = c.Cols[i]
				}
			}
			if len(c.Cols) > len(body.Schema) {
				b.errorf(CodeName, c.NamePos, "CTE %q declares %d columns but its query produces %d",
					c.Name, len(c.Cols), len(body.Schema))
			}
			// Apply declared column names.
			if len(c.Cols) > 0 {
				body = b.renameSchema(body, names, c.Name)
			}
			b.cteEnv[key] = &cteBinding{name: c.Name, cols: names, types: types, plan: body, recursive: stmt.With.Recursive}
			b.cteInfo[c.Name] = &CTEInfo{Name: c.Name, Cols: names, Plan: body}
		}
	}
	body := b.bindQueryBody(stmt.Body, outer)
	if body == nil {
		return nil
	}
	// ORDER BY resolves against the output columns of the body (plus hidden
	// source columns where unambiguous).
	if len(stmt.OrderBy) > 0 {
		body = b.bindOrderBy(body, stmt, outer)
	}
	if stmt.Limit != nil || stmt.Offset != nil {
		n := &PlanNode{Op: OpLimit, Children: []*PlanNode{body}, Schema: body.Schema, ID: b.ids.next(), Origin: "LIMIT"}
		if stmt.Limit != nil {
			n.Limit = *stmt.Limit
		}
		if stmt.Offset != nil {
			n.Offset = *stmt.Offset
		}
		body = n
	}
	return body
}

// renameSchema wraps a plan in a Rename node so output names (and optionally
// qualifiers) match a CTE/derived-table declaration.
func (b *Binder) renameSchema(n *PlanNode, names []string, qual string) *PlanNode {
	schema := make([]OutputCol, len(n.Schema))
	for i, c := range n.Schema {
		schema[i] = c
		if i < len(names) && names[i] != "" {
			schema[i].Name = names[i]
		}
		if qual != "" {
			schema[i].Qualifier = qual
		}
	}
	return &PlanNode{Op: OpRename, Children: []*PlanNode{n}, Schema: schema, Sub: n, ID: b.ids.next(), Origin: "alias"}
}

func (b *Binder) bindQueryBody(qb QueryBody, outer *scope) *PlanNode {
	switch q := qb.(type) {
	case *SelectCore:
		return b.bindSelect(q, outer)
	case *SetOp:
		return b.bindSetOp(q, outer)
	}
	return nil
}

func (b *Binder) bindSetOp(s *SetOp, outer *scope) *PlanNode {
	l := b.bindQueryBody(s.Left, outer)
	r := b.bindQueryBody(s.Right, outer)
	if l == nil || r == nil {
		return l
	}
	if len(l.Schema) != len(r.Schema) {
		b.errorf(CodeType, s.Posn, "%s branches must have the same number of columns, got %d and %d",
			s.Op, len(l.Schema), len(r.Schema))
	}
	n := len(l.Schema)
	if len(r.Schema) < n {
		n = len(r.Schema)
	}
	for i := 0; i < n; i++ {
		if !l.Schema[i].Type.Same(r.Schema[i].Type) {
			b.errorf(CodeType, s.Posn, "%s column %d has type %s on the left and %s on the right",
				s.Op, i+1, l.Schema[i].Type, r.Schema[i].Type)
		}
	}
	schema := make([]OutputCol, n)
	for i := 0; i < n; i++ {
		schema[i] = l.Schema[i]
		schema[i].Qualifier = ""
		schema[i].Nullable = l.Schema[i].Nullable || r.Schema[i].Nullable
	}
	return &PlanNode{Op: OpSetOp, SetOp: s.Op, SetAll: s.All, Children: []*PlanNode{l, r},
		Schema: schema, ID: b.ids.next(), Origin: "set operation"}
}

// ---------------------------------------------------------------------------
// SELECT
// ---------------------------------------------------------------------------

type selectCtx struct {
	b        *Binder
	sel      *SelectCore
	sc       *scope
	from     *PlanNode
	fromPos  Pos
	aliases  []string // select-list output names
	aggList  []AggDef
	aggTypes []Type
	// projection expressions with aggregates replaced by AggRef and with
	// column refs resolved against the pre-aggregation row.
	project  []BExpr
	hasAgg   bool
	groupBy  []BExpr
	groupPos []Pos
	distinct bool
}

func (b *Binder) bindSelect(sel *SelectCore, outer *scope) *PlanNode {
	if sel == nil {
		return nil
	}
	ctx := &selectCtx{b: b, sel: sel, sc: newScope(outer), distinct: sel.Distinct}
	if sel.From != nil {
		ctx.from = b.bindTableExpr(sel.From, ctx.sc, outer)
	} else {
		// SELECT without FROM: a single empty row.
		ctx.from = &PlanNode{Op: OpValues, Rows: [][]Value{{}}, Schema: nil, ID: b.ids.next(), Origin: "no FROM"}
	}
	// bindTableExpr registers each relation's columns into ctx.sc as it goes,
	// so correlated and lateral references can see the partially built scope.
	if ctx.from == nil {
		return nil
	}
	// WHERE
	var where BExpr
	if sel.Where != nil {
		where = b.bindExpr(sel.Where, ctx.sc, true)
		if where != nil {
			where = b.requireBool(where, "WHERE")
		}
	}
	// GROUP BY
	if len(sel.GroupBy) > 0 {
		for _, g := range sel.GroupBy {
			ge := b.bindExpr(g, ctx.sc, false)
			if ge == nil {
				continue
			}
			if exprHasAggRef(ge) {
				b.errorf(CodeType, ge.Pos(), "aggregate functions are not allowed in GROUP BY")
				continue
			}
			ctx.groupBy = append(ctx.groupBy, ge)
		}
	}
	// HAVING
	var having BExpr
	if sel.Having != nil {
		having = b.bindAggExpr(sel.Having, ctx)
		if having != nil {
			having = b.requireBool(having, "HAVING")
		}
	}
	// SELECT items
	var proj []BExpr
	var outNames []string
	var outTypes []Type
	var outQuals []string
	var outNullable []bool
	for _, it := range sel.Items {
		if it.Star {
			if it.Table != "" {
				idx := -1
				for i, q := range ctx.sc.quals {
					if strings.EqualFold(q, it.Table) {
						idx = i
						break
					}
				}
				if idx < 0 {
					b.errorf(CodeName, it.Posn, "unknown table %q in %s.*", it.Table, it.Table)
					continue
				}
				for i := range ctx.sc.names {
					if !strings.EqualFold(ctx.sc.quals[i], it.Table) {
						continue
					}
					proj = append(proj, &ColRef{Depth: 0, Index: i, Qualifier: ctx.sc.quals[i], Name: ctx.sc.names[i], T: ctx.sc.types[i], Posn: it.Posn})
					outNames = append(outNames, ctx.sc.names[i])
					outTypes = append(outTypes, ctx.sc.types[i])
					outQuals = append(outQuals, ctx.sc.quals[i])
					outNullable = append(outNullable, ctx.sc.nullable[i])
				}
				continue
			}
			if ctx.hasAgg {
				b.errorf(CodeName, it.Posn, "* cannot be combined with aggregate expressions")
				continue
			}
			for i := range ctx.sc.names {
				proj = append(proj, &ColRef{Depth: 0, Index: i, Qualifier: ctx.sc.quals[i], Name: ctx.sc.names[i], T: ctx.sc.types[i], Posn: it.Posn})
				outNames = append(outNames, ctx.sc.names[i])
				outTypes = append(outTypes, ctx.sc.types[i])
				outQuals = append(outQuals, ctx.sc.quals[i])
				outNullable = append(outNullable, ctx.sc.nullable[i])
			}
			continue
		}
		e := b.bindAggExpr(it.Expr, ctx)
		if e == nil {
			// Keep arity stable for error recovery.
			e = &Lit{V: NullValue(KNull), T: NullT, Posn: it.Posn}
		}
		name := it.Alias
		if name == "" {
			name = deriveName(it.Expr)
		}
		proj = append(proj, e)
		outNames = append(outNames, name)
		t := e.Ty()
		outTypes = append(outTypes, t)
		outQuals = append(outQuals, "")
		outNullable = append(outNullable, exprNullable(e))
	}
	if len(proj) == 0 {
		// SELECT with only errors: produce a one-column plan for recovery.
		proj = append(proj, &Lit{V: NullValue(KNull), T: NullT, Posn: sel.Posn})
		outNames = append(outNames, "?column?")
		outTypes = append(outTypes, NullT)
		outQuals = append(outQuals, "")
		outNullable = append(outNullable, true)
	}
	if err := b.checkAggregation(ctx, proj, having); err != nil {
		b.errs = append(b.errs, err)
	}
	// Build the plan: FROM -> Filter -> Aggregate -> Having(Filter) -> Project.
	n := ctx.from
	if where != nil {
		n = &PlanNode{Op: OpFilter, Children: []*PlanNode{n}, Exprs: []BExpr{where}, Schema: n.Schema, ID: b.ids.next(), Origin: "WHERE"}
	}
	aggInput := n
	// Pre-aggregate column space for AggDef.Arg references and the aggregate
	// node's output schema (group keys first, then aggregates).
	if ctx.hasAgg || len(ctx.groupBy) > 0 {
		aggSchema := make([]OutputCol, 0, len(ctx.groupBy)+len(ctx.aggList))
		for i, g := range ctx.groupBy {
			aggSchema = append(aggSchema, OutputCol{Name: groupName(g, i), Type: g.Ty(), Nullable: exprNullable(g)})
		}
		for _, a := range ctx.aggList {
			aggSchema = append(aggSchema, OutputCol{Name: aggName(a), Type: a.Type, Nullable: true})
		}
		aggNode := &PlanNode{Op: OpAggregate, Children: []*PlanNode{aggInput},
			GroupBy: ctx.groupBy, Aggs: ctx.aggList, Schema: aggSchema, ID: b.ids.next(), Origin: "GROUP BY"}
		n = aggNode
		// Projection and HAVING both read the aggregate node's output: group
		// keys are slots 0..n-1 and aggregate i is slot n+i. Rebase both.
		proj = rebaseOntoAggregate(proj, ctx)
		if having != nil {
			having = rebaseOntoAggregate([]BExpr{having}, ctx)[0]
			n = &PlanNode{Op: OpFilter, Children: []*PlanNode{n}, Exprs: []BExpr{having}, Schema: n.Schema, ID: b.ids.next(), Origin: "HAVING"}
		}
	}
	projSchema := make([]OutputCol, len(proj))
	for i := range proj {
		qual := outQuals[i]
		// A plain column keeps its table qualifier in the output schema, so
		// ORDER BY can still name it as t.col after projection.
		if c, ok := proj[i].(*ColRef); ok && qual == "" {
			qual = c.Qualifier
		}
		projSchema[i] = OutputCol{Name: outNames[i], Type: proj[i].Ty(), Qualifier: qual, Nullable: outNullable[i]}
	}
	projNode := &PlanNode{Op: OpProject, Children: []*PlanNode{n}, Exprs: proj, Schema: projSchema, ID: b.ids.next(), Origin: "SELECT list"}
	// DISTINCT applies to the projection's output, not to the pre-projection
	// row, so it must sit above the Project node.
	if ctx.distinct {
		projNode = &PlanNode{Op: OpDistinct, Children: []*PlanNode{projNode}, Schema: projSchema, ID: b.ids.next(), Origin: "DISTINCT"}
	}
	return projNode
}

func deriveName(e Expr) string {
	switch x := e.(type) {
	case *EColumn:
		return x.Parts[len(x.Parts)-1]
	case *EFunc:
		return x.Name
	case *ECast:
		return "cast"
	case *ELiteral:
		return "?column?"
	}
	return "?column?"
}

func groupName(e BExpr, i int) string {
	if c, ok := e.(*ColRef); ok {
		return c.Name
	}
	return fmt.Sprintf("group%d", i+1)
}

func aggName(a AggDef) string {
	if a.Arg != nil {
		return a.Fn + "(" + a.Arg.String() + ")"
	}
	return a.Fn + "(*)"
}

// rebaseOntoAggregate rewrites projection/HAVING expressions so that group
// keys and aggregate calls read the aggregate node's output row.
func rebaseOntoAggregate(proj []BExpr, ctx *selectCtx) []BExpr {
	// Map group key expression key -> index; aggregates were appended in
	// order of discovery, so AggRef indices are offset by group count.
	nGroups := len(ctx.groupBy)
	// Walk each expression bottom-up. Because bindAggExpr already replaced
	// aggregate calls with AggRef (with a temporary index), fix indices here.
	out := make([]BExpr, len(proj))
	for i, e := range proj {
		out[i] = mapExpr(e, func(x BExpr) BExpr {
			switch v := x.(type) {
			case *AggRef:
				n := *v
				// Stored index counts aggregates from 0; shift by groups.
				n.Index = nGroups + v.Index
				return &n
			case *ColRef:
				if v.Depth != 0 {
					return v
				}
				// If it corresponds to a group key, replace it with the key
				// slot. Otherwise it is an error already reported by
				// checkAggregation.
				for gi, g := range ctx.groupBy {
					if exprKey(g) == exprKey(v) {
						return &ColRef{Depth: 0, Index: gi, Name: groupName(g, gi), T: g.Ty(), Posn: v.Posn}
					}
				}
				return v
			}
			return x
		})
	}
	return out
}

// bindAggExpr binds an expression in the SELECT/HAVING context, lifting
// aggregate calls into ctx.aggList and replacing them with AggRef nodes.
func (b *Binder) bindAggExpr(ast Expr, ctx *selectCtx) BExpr {
	switch x := ast.(type) {
	case *EFunc:
		if isAggregateName(x.Name) {
			return b.bindAggregate(x, ctx)
		}
		// Scalar function over aggregate results (round_to(avg(x), 2)) is
		// allowed: its arguments are hoisted like any other SELECT expression.
		// Only an aggregate directly inside another aggregate's argument is
		// nesting, and bindAggregate handles that case.
		args := make([]BExpr, len(x.Args))
		for i, a := range x.Args {
			args[i] = b.bindAggExpr(a, ctx)
			if args[i] == nil {
				args[i] = &Lit{V: NullValue(KNull), T: NullT, Posn: a.Pos()}
			}
		}
		argTypes := make([]Type, len(args))
		for i := range args {
			argTypes[i] = args[i].Ty()
		}
		t := b.checkCall(x.Posn, x.Name, argTypes)
		return &Fn{Name: x.Name, Args: args, T: t, Posn: x.Posn}
	}
	// For all other shapes, rewrite the AST into a bound expr, but recursively
	// honoring aggregate calls. The generic binder cannot see aggregates, so
	// pre-process: bind with a special flag.
	return b.bindExprMode(ast, ctx.sc, false, ctx)
}

func isAggCall(e Expr) bool {
	f, ok := e.(*EFunc)
	return ok && isAggregateName(f.Name)
}

// aggCallInside reports whether e contains an aggregate call anywhere below
// its own top node, e.g. sum(x) inside sum(x) + 1. Such an aggregate used as
// another aggregate's argument is illegal nesting; an aggregate used inside
// an ordinary scalar expression is fine.
func aggCallInside(ast Expr) (Pos, bool) {
	var found Pos
	var walk func(Expr)
	walk = func(e Expr) {
		if e == nil || found.Line != 0 {
			return
		}
		if isAggCall(e) {
			found = e.Pos()
			return
		}
		for _, c := range astChildren(e) {
			walk(c)
		}
	}
	for _, c := range astChildren(ast) {
		walk(c)
	}
	return found, found.Line != 0
}

// astChildren lists the direct subexpressions of a parser AST node.
func astChildren(e Expr) []Expr {
	switch x := e.(type) {
	case *EUnary:
		return []Expr{x.X}
	case *EBinary:
		return []Expr{x.L, x.R}
	case *EFunc:
		return x.Args
	case *ECast:
		return []Expr{x.X}
	case *ECase:
		out := []Expr{}
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
	case *EIs:
		return []Expr{x.X}
	case *EBetween:
		return []Expr{x.X, x.Lo, x.Hi}
	case *EInList:
		return append([]Expr{x.X}, x.List...)
	case *EInSubquery:
		return []Expr{x.X}
	case *ELike:
		return []Expr{x.X, x.Pat}
	case *EParen:
		return []Expr{x.X}
	}
	return nil
}

func (b *Binder) bindAggregate(x *EFunc, ctx *selectCtx) BExpr {
	if x.Star && !strings.EqualFold(x.Name, "count") {
		b.errorf(CodeType, x.Posn, "%s(*) is invalid; only count(*) takes *", x.Name)
	}
	var arg BExpr
	var argT = NullT
	nestedPos, nested := Pos{}, false
	if !x.Star && len(x.Args) > 0 {
		// A direct aggregate argument is nesting; an aggregate inside a
		// scalar expression is also nesting for the aggregate's argument.
		if isAggCall(x.Args[0]) {
			nestedPos, nested = x.Args[0].Pos(), true
		} else {
			nestedPos, nested = aggCallInside(x.Args[0])
		}
	}
	if nested {
		// Report nesting once, at the inner call, and produce a typed NULL so
		// the enclosing aggregate does not also fail arity/type checks.
		b.errorf(CodeType, nestedPos, "aggregate functions cannot be nested")
		inner := &Lit{V: NullValue(KNull), T: NullT, Posn: nestedPos}
		t, _ := aggReturnType(x.Posn, x.Name, inner.Ty(), true, false, x.Distinct)
		def := AggDef{Fn: strings.ToLower(x.Name), Arg: inner, Type: t}
		ctx.aggList = append(ctx.aggList, def)
		ctx.hasAgg = true
		return &AggRef{Index: len(ctx.aggList) - 1, Name: aggName(def), T: t, Posn: x.Posn}
	}
	if !x.Star {
		if len(x.Args) != 1 && !b.suppressExprErrors {
			b.errorf(CodeArity, x.Posn, "%s takes exactly one argument", x.Name)
		}
		if len(x.Args) > 0 {
			arg = b.bindExprMode(x.Args[0], ctx.sc, true, nil)
			if arg != nil {
				argT = arg.Ty()
			}
		}
	}
	t, e := aggReturnType(x.Posn, x.Name, argT, arg != nil && !x.Star, x.Star, x.Distinct)
	if e != nil {
		b.errs = append(b.errs, e)
		t = NullT
	}
	def := AggDef{Fn: strings.ToLower(x.Name), Arg: arg, Star: x.Star, Distinct: x.Distinct, Type: t}
	ctx.aggList = append(ctx.aggList, def)
	ctx.hasAgg = true
	idx := len(ctx.aggList) - 1
	return &AggRef{Index: idx, Name: aggName(def), T: t, Posn: x.Posn}
}

// bindExprMode is the general binder for expressions. When ctx is non-nil
// (SELECT/HAVING) aggregates at the top level of subexpressions are hoisted.
func (b *Binder) bindExprMode(ast Expr, sc *scope, allowAgg bool, ctx *selectCtx) BExpr {
	// Handle aggregate subexpressions inside larger expressions by
	// intercepting EFunc aggregate nodes recursively.
	if ctx != nil {
		return b.bindExprWithAgg(ast, sc, ctx)
	}
	return b.bindExpr(ast, sc, allowAgg)
}

func (b *Binder) bindExprWithAgg(ast Expr, sc *scope, ctx *selectCtx) BExpr {
	switch x := ast.(type) {
	case *EFunc:
		if isAggregateName(x.Name) {
			return b.bindAggregate(x, ctx)
		}
		args := make([]BExpr, len(x.Args))
		argTypes := make([]Type, len(x.Args))
		for i, a := range x.Args {
			args[i] = b.bindExprWithAgg(a, sc, ctx)
			if args[i] == nil {
				args[i] = &Lit{V: NullValue(KNull), T: NullT, Posn: a.Pos()}
			}
			argTypes[i] = args[i].Ty()
		}
		t := b.checkCall(x.Posn, x.Name, argTypes)
		return &Fn{Name: x.Name, Args: args, T: t, Posn: x.Posn}
	case *EBinary:
		l := b.bindExprWithAgg(x.L, sc, ctx)
		r := b.bindExprWithAgg(x.R, sc, ctx)
		return b.combineBinary(x, l, r)
	case *EUnary:
		xb := b.bindExprWithAgg(x.X, sc, ctx)
		return b.combineUnary(x, xb)
	case *ECase:
		c := &Case{Posn: x.Posn}
		if x.Operand != nil {
			c.Operand = b.bindExprWithAgg(x.Operand, sc, ctx)
		}
		for _, w := range x.Whens {
			cond := b.bindExprWithAgg(w.Cond, sc, ctx)
			then := b.bindExprWithAgg(w.Then, sc, ctx)
			c.Whens = append(c.Whens, When{Cond: cond, Then: then, Posn: w.Posn, Operand: x.Operand != nil})
		}
		if x.Else != nil {
			c.Else = b.bindExprWithAgg(x.Else, sc, ctx)
		}
		return b.finishCase(c)
	case *EParen:
		return b.bindExprWithAgg(x.X, sc, ctx)
	}
	// No aggregates possible below (literals, columns, subqueries already
	// bound in their own blocks).
	base := b.bindExpr(ast, sc, false)
	if base != nil {
		if exprHasAggRef(base) && ctx == nil {
			b.errorf(CodeType, base.Pos(), "aggregate function not allowed here")
		}
	}
	return base
}

// ---------------------------------------------------------------------------
// General expression binder
// ---------------------------------------------------------------------------

// afterDamage reports whether p is past the last syntax damage (or whether
// suppression is off).
func (b *Binder) afterDamage(p Pos) bool {
	if !b.suppressExprErrors {
		return true
	}
	if p.Line > b.damageLine {
		return true
	}
	if p.Line < b.damageLine {
		return false
	}
	return p.Col > b.damageCol
}

func (b *Binder) bindExpr(ast Expr, sc *scope, allowAgg bool) BExpr {
	if ast == nil {
		return nil
	}
	switch x := ast.(type) {
	case *ELiteral:
		return b.literal(x)
	case *EColumn:
		return b.bindColumn(x, sc)
	case *EStar:
		b.errorf(CodeSyntax, x.Posn, "* is only allowed in a select list or count(*)")
		return nil
	case *EParen:
		return b.bindExpr(x.X, sc, allowAgg)
	case *EUnary:
		xb := b.bindExpr(x.X, sc, allowAgg)
		return b.combineUnary(x, xb)
	case *EBinary:
		l := b.bindExpr(x.L, sc, allowAgg)
		r := b.bindExpr(x.R, sc, allowAgg)
		return b.combineBinary(x, l, r)
	case *EFunc:
		if isAggregateName(x.Name) {
			b.errorf(CodeType, x.Posn, "aggregate function %s is not allowed here", x.Name)
			return nil
		}
		args := make([]BExpr, len(x.Args))
		argTypes := make([]Type, len(x.Args))
		for i, a := range x.Args {
			args[i] = b.bindExpr(a, sc, allowAgg)
			if args[i] == nil {
				args[i] = &Lit{V: NullValue(KNull), T: NullT, Posn: a.Pos()}
			}
			argTypes[i] = args[i].Ty()
		}
		if x.Star {
			b.errorf(CodeType, x.Posn, "%s(*) is not valid; only count(*) takes *", x.Name)
		}
		t := b.checkCall(x.Posn, x.Name, argTypes)
		return &Fn{Name: x.Name, Args: args, T: t, Posn: x.Posn}
	case *ECast:
		xb := b.bindExpr(x.X, sc, allowAgg)
		if xb == nil {
			return nil
		}
		cast, e := b.checkCast(xb, x.Type, x.Posn)
		if e != nil {
			b.errs = append(b.errs, e)
			return &Cast{X: xb, T: x.Type, Posn: x.Posn}
		}
		return cast
	case *ECase:
		c := &Case{Posn: x.Posn}
		if x.Operand != nil {
			c.Operand = b.bindExpr(x.Operand, sc, allowAgg)
		}
		for _, w := range x.Whens {
			c.Whens = append(c.Whens, When{
				Cond:    b.bindExpr(w.Cond, sc, allowAgg),
				Then:    b.bindExpr(w.Then, sc, allowAgg),
				Posn:    w.Posn,
				Operand: x.Operand != nil,
			})
		}
		if x.Else != nil {
			c.Else = b.bindExpr(x.Else, sc, allowAgg)
		}
		return b.finishCase(c)
	case *EIs:
		xb := b.bindExpr(x.X, sc, allowAgg)
		if xb == nil {
			return nil
		}
		return b.bindIs(x, xb)
	case *EBetween:
		xb := b.bindExpr(x.X, sc, allowAgg)
		lo := b.bindExpr(x.Lo, sc, allowAgg)
		hi := b.bindExpr(x.Hi, sc, allowAgg)
		if xb == nil || lo == nil || hi == nil {
			return nil
		}
		b.checkComparable(x.OpPos, xb, lo, "BETWEEN")
		b.checkComparable(x.OpPos, xb, hi, "BETWEEN")
		return &Between{X: xb, Lo: lo, Hi: hi, Not: x.Not, Posn: x.Posn}
	case *EInList:
		xb := b.bindExpr(x.X, sc, allowAgg)
		list := make([]BExpr, 0, len(x.List))
		for _, it := range x.List {
			e := b.bindExpr(it, sc, allowAgg)
			if e == nil {
				continue
			}
			if xb != nil {
				b.checkComparable(x.OpPos, xb, e, "IN")
			}
			list = append(list, e)
		}
		if xb == nil {
			return nil
		}
		return &InList{X: xb, List: list, Not: x.Not, Posn: x.Posn}
	case *EInSubquery:
		xb := b.bindExpr(x.X, sc, allowAgg)
		sub := b.bindStatement(x.Body, sc)
		if xb == nil || sub == nil {
			return nil
		}
		if len(sub.Schema) != 1 {
			b.errorf(CodeCardinality, x.Posn, "IN subquery must return exactly one column, got %d", len(sub.Schema))
			return nil
		}
		b.checkComparable(x.OpPos, xb, &Lit{V: NullValue(sub.Schema[0].Type.Kind), T: sub.Schema[0].Type, Posn: x.Posn}, "IN")
		return &InSub{X: xb, Plan: sub, Not: x.Not, T: BoolT, Posn: x.Posn}
	case *ELike:
		xb := b.bindExpr(x.X, sc, allowAgg)
		pat := b.bindExpr(x.Pat, sc, allowAgg)
		if xb == nil || pat == nil {
			return nil
		}
		if !xb.Ty().Same(StrT) {
			b.errorf(CodeType, x.OpPos, "LIKE requires string operands, got %s", xb.Ty())
		}
		if !pat.Ty().Same(StrT) {
			b.errorf(CodeType, x.OpPos, "LIKE pattern must be a string, got %s", pat.Ty())
		}
		return &Like{X: xb, Pat: pat, Not: x.Not, Posn: x.Posn}
	case *EScalarSubquery:
		sub := b.bindStatement(x.Body, sc)
		if sub == nil {
			return nil
		}
		if len(sub.Schema) != 1 {
			b.errorf(CodeCardinality, x.Posn, "scalar subquery must return exactly one column, got %d", len(sub.Schema))
			return nil
		}
		return &ScalarSub{Plan: sub, T: sub.Schema[0].Type, Posn: x.Posn}
	case *EExists:
		sub := b.bindStatement(x.Body, sc)
		if sub == nil {
			return nil
		}
		return &Exists{Plan: sub, Posn: x.Posn}
	case *EStarArg:
		return nil
	}
	b.errorf(CodeInternal, ast.Pos(), "internal: unhandled expression %T", ast)
	return nil
}

func (b *Binder) literal(x *ELiteral) BExpr {
	switch x.Kind {
	case KNull:
		return &Lit{V: NullValue(KNull), T: NullT, Posn: x.Posn}
	case KInt:
		return &Lit{V: Value{Kind: KInt, Int: x.Int}, T: IntT, Posn: x.Posn}
	case KDec:
		pn := x.Dec.Scale + 1
		if ip, ok := intPartDigits(x.Dec); ok && ip > 0 {
			pn = ip + x.Dec.Scale
		}
		if pn < x.Dec.Scale+1 {
			pn = x.Dec.Scale + 1
		}
		if pn > 18 {
			pn = 18
		}
		return &Lit{V: Value{Kind: KDec, Dec: x.Dec}, T: DecType(pn, x.Dec.Scale), Posn: x.Posn}
	case KString:
		return &Lit{V: Value{Kind: KString, Str: x.Str}, T: StrT, Posn: x.Posn}
	case KBool:
		return &Lit{V: Value{Kind: KBool, Bool: x.Bool}, T: BoolT, Posn: x.Posn}
	case KDate:
		return &Lit{V: Value{Kind: KDate, Date: x.Date}, T: DateT, Posn: x.Posn}
	case KClock:
		return &Lit{V: Value{Kind: KClock, Clock: int32(x.Date)}, T: ClockT, Posn: x.Posn}
	}
	return nil
}

func intPartDigits(d Decimal) (int, bool) {
	u := d.Unscaled
	if u < 0 {
		u = -u
	}
	n := 1
	for u >= 10 {
		u /= 10
		n++
	}
	n -= d.Scale
	if n < 0 {
		n = 0
	}
	return n, true
}

func (b *Binder) combineUnary(x *EUnary, xb BExpr) BExpr {
	if xb == nil {
		return nil
	}
	if x.Op == "not" {
		if !xb.Ty().Same(BoolT) {
			b.errorf(CodeType, x.OpPos, "NOT requires a boolean operand, got %s", xb.Ty())
			return &Un{Op: "not", X: xb, T: BoolT, Posn: x.Posn}
		}
		return &Un{Op: "not", X: xb, T: BoolT, Posn: x.Posn}
	}
	t := xb.Ty()
	if !t.IsNumeric() {
		b.errorf(CodeType, x.OpPos, "unary %s requires a numeric operand, got %s", x.Op, t)
		return &Un{Op: x.Op, X: xb, T: NullT, Posn: x.Posn}
	}
	if lit, ok := xb.(*Lit); ok && lit.T.Kind == KInt {
		n := *lit
		n.V.Int = -n.V.Int
		return &n
	}
	return &Un{Op: "-", X: xb, T: t, Posn: x.Posn}
}

func (b *Binder) combineBinary(x *EBinary, l, r BExpr) BExpr {
	if l == nil || r == nil {
		return nil
	}
	if l.Ty().Kind == KNull && r.Ty().Kind == KNull {
		return &Bin{Op: binCanon(opName(x.Op)), L: l, R: r, T: NullT, Posn: x.Posn}
	}
	op := x.Op
	switch op {
	case "and", "or":
		if !l.Ty().Same(BoolT) && !l.Ty().IsNull() && b.afterDamage(x.OpPos) {
			b.errorf(CodeType, x.OpPos, "%s requires boolean operands, got %s", strings.ToUpper(op), l.Ty())
		}
		if !r.Ty().Same(BoolT) && !r.Ty().IsNull() && b.afterDamage(x.OpPos) {
			b.errorf(CodeType, x.OpPos, "%s requires boolean operands, got %s", strings.ToUpper(op), r.Ty())
		}
		return &Bin{Op: op, L: l, R: r, T: BoolT, Posn: x.Posn}
	case "=", "<>", "<", ">", "<=", ">=":
		b.checkComparable(x.OpPos, l, r, opSymbol(op))
		return &Bin{Op: cmpOpName(op), L: l, R: r, T: BoolT, Posn: x.Posn}
	case "+", "-", "*", "/", "%":
		if !l.Ty().IsNumeric() && !l.Ty().IsNull() && b.afterDamage(x.OpPos) {
			b.errorf(CodeType, x.OpPos, "%s requires numeric operands, got %s", op, l.Ty())
		}
		if !r.Ty().IsNumeric() && !r.Ty().IsNull() && b.afterDamage(x.OpPos) {
			b.errorf(CodeType, x.OpPos, "%s requires numeric operands, got %s", op, r.Ty())
		}
		if !l.Ty().IsNumeric() || !r.Ty().IsNumeric() {
			return &Bin{Op: opName(op), L: l, R: r, T: NullT, Posn: x.Posn}
		}
		if op == "%" && (l.Ty().Kind == KDec || r.Ty().Kind == KDec) && !l.Ty().IsNull() && !r.Ty().IsNull() && b.afterDamage(x.OpPos) {
			b.errorf(CodeType, x.OpPos, "%% is only defined for integer operands; use round_to for fixed-point remainders")
			return &Bin{Op: "mod", L: l, R: r, T: NullT, Posn: x.Posn}
		}
		t := arithType(op, l.Ty(), r.Ty())
		return &Bin{Op: opName(op), L: l, R: r, T: t, Posn: x.Posn}
	}
	b.errorf(CodeInternal, x.Posn, "internal: unknown binary operator %q", op)
	return nil
}

func binCanon(op string) string { return op }

func cmpOpName(op string) string {
	switch op {
	case "=":
		return "eq"
	case "<>":
		return "ne"
	case "<":
		return "lt"
	case ">":
		return "gt"
	case "<=":
		return "le"
	case ">=":
		return "ge"
	}
	return op
}

func opName(op string) string {
	switch op {
	case "+":
		return "add"
	case "-":
		return "sub"
	case "*":
		return "mul"
	case "/":
		return "div"
	case "%":
		return "mod"
	}
	return op
}

// arithType applies fixed-point typing rules:
//
//	+/-   dec(p,s): p = max(p1-s1,p2-s2)+max(s1,s2)+1, s = max(s1,s2)
//	*     dec(p,s): p = p1+p2, s = s1+s2, capped at 18 digits with rounding
//	/     dec(p,s): s = max(6, s1, s2), p = 18; integer/integer stays integer
//	%     integer only
func arithType(op string, a, bt Type) Type {
	if a.IsNull() && bt.IsNull() {
		return NullT
	}
	if op == "%" {
		return IntT
	}
	if a.Kind == KInt && bt.Kind == KInt {
		// Integer arithmetic stays integral; / is truncating integer division,
		// mirroring the interpreter. All but / can overflow and are checked at
		// runtime.
		return IntT
	}
	ad, bd := toDecType(a), toDecType(bt)
	switch op {
	case "+", "-":
		ip := ad.Precision - ad.Scale
		if x := bd.Precision - bd.Scale; x > ip {
			ip = x
		}
		s := maxInt(ad.Scale, bd.Scale)
		p := ip + s + 1
		if p > 18 {
			p = 18
		}
		return DecType(p, s)
	case "*":
		s := ad.Scale + bd.Scale
		p := ad.Precision + bd.Precision
		if s > 10 {
			s = 10
		}
		if p > 18 {
			p = 18
		}
		return DecType(p, s)
	case "/":
		s := maxInt(6, maxInt(ad.Scale, bd.Scale))
		if s > 10 {
			s = 10
		}
		return DecType(18, s)
	}
	return NullT
}

func toDecType(t Type) Type {
	if t.Kind == KInt {
		return DecType(19, 0)
	}
	if t.Kind == KNull {
		return DecType(19, 0)
	}
	return t
}

// assignable reports whether a value of type from may be used where to is
// expected without an explicit CAST. Clock/date conversions are never
// implicit: mixing them (or a clock with a plain number) is an error.
func assignable(from, to Type) bool {
	if from.Kind == KNull || to.Kind == KNull {
		return true
	}
	if from.Kind == to.Kind {
		return true
	}
	if from.IsNumeric() && to.IsNumeric() {
		return true
	}
	return false
}

// checkAssignable reports an implicit-conversion error when a value of type
// from is used in a slot expecting to.
func (b *Binder) checkAssignable(p Pos, from, to Type, what string) bool {
	if assignable(from, to) {
		return true
	}
	if !b.afterDamage(p) {
		return false
	}
	b.errorHints(CodeImplicit, p,
		fmt.Sprintf("%s expects %s, got %s", what, to, from), convHints)
	return false
}

// checkComparable validates that two operands can be compared. Clock and date
// do not mix; string/number do not mix.
func (b *Binder) checkComparable(p Pos, l, r BExpr, what string) bool {
	lt, rt := l.Ty(), r.Ty()
	if lt.IsNull() || rt.IsNull() {
		return true
	}
	if lt.Kind == rt.Kind {
		if lt.Kind == KDec && rt.Kind == KDec {
			return true
		}
		return true
	}
	if lt.IsNumeric() && rt.IsNumeric() {
		return true
	}
	if b.afterDamage(p) {
		b.errorHints(CodeImplicit, p, fmt.Sprintf("cannot compare %s with %s in %s", lt, rt, what), convHints)
	}
	return false
}

// bindColumn resolves a column reference against the scope chain. It reports
// ambiguity with candidate qualifiers.
func (b *Binder) bindColumn(x *EColumn, sc *scope) BExpr {
	parts := x.Parts
	var colName, qual string
	if len(parts) >= 2 {
		colName = parts[len(parts)-1]
		qual = parts[len(parts)-2]
		if len(parts) == 3 {
			qual = parts[1] + "." + parts[2]
			colName = parts[2]
			// fall back to two-part matching
			qual = parts[1]
		}
	} else {
		colName = parts[0]
	}
	// Outside the FROM/WHERE/ON part of a statement, a failed name certainly
	// reflects syntax damage: e.g. after `FROM lots` failed to parse, every
	// column reference loses its scope. Suppress those during recovery.
	if sc == nil {
		if b.suppressExprErrors {
			return &Lit{V: NullValue(KNull), T: NullT, Posn: x.Posn}
		}
	}
	// Search depth-first outward; a match at the innermost scope wins even if
	// outer scopes also match (SQL scoping). Ambiguity is within one scope.
	depth := 0
	for s := sc; s != nil; s = s.parent {
		var hits []int
		for i := range s.names {
			if !strings.EqualFold(s.names[i], colName) {
				continue
			}
			if qual != "" && !strings.EqualFold(s.quals[i], qual) {
				continue
			}
			hits = append(hits, i)
		}
		if len(hits) == 1 {
			i := hits[0]
			return &ColRef{Depth: depth, Index: i, Name: s.names[i], Qualifier: s.quals[i], T: s.types[i], Posn: x.Posn}
		}
		if len(hits) > 1 {
			var cands []string
			for _, i := range hits {
				q := s.quals[i]
				if q == "" {
					q = "?"
				}
				cands = append(cands, q+"."+s.names[i])
			}
			if !b.afterDamage(x.Posn) {
				return nil
			}
			b.errorHints(CodeAmbiguous, x.Posn,
				fmt.Sprintf("column reference %q is ambiguous", colName),
				[]string{"candidates: " + strings.Join(cands, ", "), "qualify the reference with one of the candidate table aliases"})
			return nil
		}
		depth++
	}
	if b.suppressExprErrors {
		return &Lit{V: NullValue(KNull), T: NullT, Posn: x.Posn}
	}
	if qual != "" {
		b.errorf(CodeName, x.Posn, "unknown column %q (no table alias %q in scope)", colName, qual)
	} else {
		b.errorf(CodeName, x.Posn, "unknown column %q", colName)
	}
	return nil
}

func (b *Binder) bindIs(x *EIs, xb BExpr) BExpr {
	switch x.What {
	case "null":
		return &IsNull{X: xb, Not: x.Not, Posn: x.Posn}
	case "true", "false":
		if !xb.Ty().Same(BoolT) && !xb.Ty().IsNull() {
			b.errorf(CodeType, x.OpPos, "IS %s requires a boolean operand, got %s", strings.ToUpper(x.What), xb.Ty())
			return &IsNull{X: xb, Not: x.Not, Posn: x.Posn}
		}
		want := x.What == "true"
		if x.Not {
			want = !want
		}
		if want {
			return xb
		}
		return &Un{Op: "not", X: xb, T: BoolT, Posn: x.Posn}
	}
	return nil
}

func (b *Binder) finishCase(c *Case) BExpr {
	// Determine the result type by unifying branches, with NULL in each
	// position taking the type of the other branches.
	var types []Type
	for _, w := range c.Whens {
		if w.Cond == nil || w.Then == nil {
			return nil
		}
		if !w.Cond.Ty().Same(BoolT) {
			b.errorf(CodeType, w.Posn, "CASE WHEN condition must be boolean, got %s", w.Cond.Ty())
		}
		types = append(types, w.Then.Ty())
	}
	if c.Else != nil {
		types = append(types, c.Else.Ty())
	}
	res := NullT
	for _, t := range types {
		if t.IsNull() {
			continue
		}
		if res.IsNull() {
			res = t
			continue
		}
		if res.Kind == KDec && t.Kind == KDec {
			s := maxInt(res.Scale, t.Scale)
			p := maxInt(res.Precision-res.Scale, t.Precision-t.Scale) + s
			if p < s {
				p = s + 1
			}
			res = DecType(p, s)
			continue
		}
		if res.IsNumeric() && t.IsNumeric() {
			u, _ := unifyNumeric(res, t)
			res = u
			continue
		}
		if !res.Same(t) {
			b.errorHints(CodeType, c.Posn, fmt.Sprintf("CASE branches have incompatible types %s and %s", res, t), convHints)
			return nil
		}
	}
	c.T = res
	return c
}

// checkAggregation validates that every non-aggregated column reference in the
// projection/HAVING is functionally determined by a GROUP BY key (syntactic
// matching on the whole expression).
func (b *Binder) checkAggregation(ctx *selectCtx, proj []BExpr, having BExpr) *Error {
	grouped := ctx.hasAgg || len(ctx.groupBy) > 0
	if !grouped {
		// No aggregation: nothing to check, but disallow AggRef without any agg.
		return nil
	}
	check := func(e BExpr, where string) *Error {
		if b.suppressExprErrors && !b.afterDamage(e.Pos()) {
			return nil
		}
		var bad *Error
		walkExpr(e, func(x BExpr) {
			if bad != nil {
				return
			}
			c, ok := x.(*ColRef)
			if !ok || c.Depth != 0 {
				return
			}
			for _, g := range ctx.groupBy {
				if exprKey(g) == exprKey(c) {
					return
				}
			}
			bad = errWithHints(CodeType, c.Posn,
				fmt.Sprintf("column %s must appear in GROUP BY or be used in an aggregate (%s)", c, where),
				[]string{"add the column to GROUP BY", "or wrap it in an aggregate such as min()/max()"})
		})
		return bad
	}
	for _, e := range proj {
		if err := check(e, "SELECT list"); err != nil {
			return err
		}
	}
	if having != nil {
		if err := check(having, "HAVING"); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// FROM
// ---------------------------------------------------------------------------

func (b *Binder) bindTableExpr(te TableExpr, sc *scope, outer *scope) *PlanNode {
	switch t := te.(type) {
	case *TableName:
		return b.bindTableName(t, sc)
	case *SubqueryTable:
		return b.bindSubqueryTable(t, sc, outer)
	case *JoinTable:
		return b.bindJoin(t, sc, outer)
	}
	return nil
}

func (b *Binder) bindTableName(t *TableName, sc *scope) *PlanNode {
	// CTE first, then catalog.
	if cb, ok := b.cteEnv[strings.ToLower(t.Name)]; ok {
		alias := t.Alias
		if alias == "" {
			alias = t.Name
		}
		schema := make([]OutputCol, len(cb.plan.Schema))
		for i, c := range cb.plan.Schema {
			schema[i] = c
			if i < len(cb.cols) && cb.cols[i] != "" {
				schema[i].Name = cb.cols[i]
			}
			schema[i].Qualifier = alias
		}
		n := &PlanNode{Op: OpCTEScan, Table: t.Name, CTEName: t.Name, CTEPlan: cb.plan,
			Schema: schema, ID: b.ids.next(), Origin: "CTE reference"}
		b.refs[cb.name]++
		if info := b.cteInfo[cb.name]; info != nil {
			info.Refs = b.refs[cb.name]
		}
		sc.addAll(schema)
		return n
	}
	def, ok := b.cat.Get(t.Name)
	if !ok {
		b.errorf(CodeName, t.NamePos, "unknown table %q", t.Name)
		// Register nothing; caller keeps going.
		return nil
	}
	alias := t.Alias
	if alias == "" {
		alias = t.Name
	}
	schema := make([]OutputCol, len(def.Columns))
	cols := make([]string, len(def.Columns))
	for i, c := range def.Columns {
		schema[i] = OutputCol{Name: c.Name, Type: c.Type, Qualifier: alias, Nullable: c.Nullable}
		cols[i] = c.Name
	}
	n := &PlanNode{Op: OpTableScan, Table: def.Name, Columns: cols, Schema: schema, ID: b.ids.next(), Origin: "table"}
	sc.addAll(schema)
	return n
}

func (b *Binder) bindSubqueryTable(t *SubqueryTable, sc *scope, outer *scope) *PlanNode {
	// Non-lateral derived tables see only the outer scope, not siblings in the
	// same FROM. Lateral ones see the current scope.
	parent := outer
	if t.Lateral {
		parent = sc
	}
	sub := b.bindStatement(t.Body, parent)
	if sub == nil {
		return nil
	}
	alias := t.Alias
	if alias == "" {
		b.errorf(CodeName, t.Posn, "a derived table must have an alias")
		alias = "subquery"
	}
	schema := make([]OutputCol, len(sub.Schema))
	for i, c := range sub.Schema {
		schema[i] = c
		schema[i].Qualifier = alias
		schema[i].Name = c.Name
		if schema[i].Name == "" {
			schema[i].Name = fmt.Sprintf("col%d", i+1)
		}
	}
	n := &PlanNode{Op: OpRename, Sub: sub, Children: []*PlanNode{sub}, Schema: schema,
		ID: b.ids.next(), Table: alias, Origin: "derived table"}
	sc.addAll(schema)
	return n
}

func (b *Binder) bindJoin(t *JoinTable, sc *scope, outer *scope) *PlanNode {
	if t.Kind == JoinAsof {
		b.errorf(CodeType, t.KindPos, "ASOF JOIN is not supported by this front end")
	}
	// Each side registers its columns into sc as it binds; remember the
	// registration counts so we can rebuild a clean combined scope for ON.
	base := len(sc.names)
	left := b.bindTableExpr(t.Left, sc, outer)
	if left == nil {
		return nil
	}
	leftNames := append([]string(nil), sc.names...)
	leftQuals := append([]string(nil), sc.quals...)
	leftTypes := append([]Type(nil), sc.types...)
	leftNull := append([]bool(nil), sc.nullable...)
	right := b.bindTableExpr(t.Right, sc, outer)
	if right == nil {
		// Truncate the scope back to what the left side registered.
		sc.names, sc.quals, sc.types, sc.nullable = leftNames[:base], leftQuals[:base], leftTypes[:base], leftNull[:base]
		return left
	}
	_ = leftTypes
	var on BExpr
	if t.On != nil {
		on = b.bindExpr(t.On, sc, false)
		if on != nil {
			on = b.requireBool(on, "ON")
		}
	}
	if len(t.Using) > 0 {
		on = b.bindUsing(t, sc, left, right, on, len(left.Schema))
	}
	schema := make([]OutputCol, 0, len(left.Schema)+len(right.Schema))
	for _, c := range left.Schema {
		cc := c
		if t.Kind == JoinRight || t.Kind == JoinFull {
			cc.Nullable = true
		}
		schema = append(schema, cc)
	}
	for _, c := range right.Schema {
		cc := c
		if t.Kind == JoinLeft || t.Kind == JoinFull {
			cc.Nullable = true
		}
		schema = append(schema, cc)
	}
	// The join output replaces both sides in the scope: rebuild sc so the
	// combined column list matches the join schema exactly once. This keeps
	// the sc/schema correspondence the rest of binding relies on.
	sc.names = sc.names[:base]
	sc.quals = sc.quals[:base]
	sc.types = sc.types[:base]
	sc.nullable = sc.nullable[:base]
	sc.addAll(schema)
	n := &PlanNode{Op: OpJoin, Join: &JoinInfo{Kind: t.Kind, LeftCols: len(left.Schema), RightCols: len(right.Schema)},
		Children: []*PlanNode{left, right}, Schema: schema, ID: b.ids.next(), Origin: t.Kind.String()}
	if on != nil {
		n.Exprs = []BExpr{on}
	}
	return n
}

func (b *Binder) bindUsing(t *JoinTable, sc *scope, left, right *PlanNode, prior BExpr, leftLen int) BExpr {
	var conds []BExpr
	for i, name := range t.Using {
		li := findCol(left.Schema, name)
		ri := findCol(right.Schema, name)
		if li < 0 || ri < 0 {
			b.errorf(CodeName, t.UsingPos[i], "USING column %q must exist on both sides of the join", name)
			continue
		}
		if !left.Schema[li].Type.Same(right.Schema[ri].Type) {
			b.errorf(CodeType, t.UsingPos[i], "USING column %q has type %s on the left and %s on the right",
				name, left.Schema[li].Type, right.Schema[ri].Type)
			continue
		}
		l := &ColRef{Depth: 0, Index: li, Qualifier: left.Schema[li].Qualifier, Name: name, T: left.Schema[li].Type, Posn: t.UsingPos[i]}
		r := &ColRef{Depth: 0, Index: leftLen + ri, Qualifier: right.Schema[ri].Qualifier, Name: name, T: right.Schema[ri].Type, Posn: t.UsingPos[i]}
		conds = append(conds, &Bin{Op: "eq", L: l, R: r, T: BoolT, Posn: t.UsingPos[i]})
	}
	var out BExpr
	for _, c := range conds {
		if out == nil {
			out = c
		} else {
			out = &Bin{Op: "and", L: out, R: c, T: BoolT, Posn: t.UsingPos[0]}
		}
	}
	if prior != nil {
		if out == nil {
			return prior
		}
		return &Bin{Op: "and", L: prior, R: out, T: BoolT, Posn: t.UsingPos[0]}
	}
	return out
}

func findCol(s []OutputCol, name string) int {
	for i, c := range s {
		if strings.EqualFold(c.Name, name) {
			return i
		}
	}
	return -1
}

func (b *Binder) requireBool(e BExpr, what string) BExpr {
	if e == nil {
		return e
	}
	if !e.Ty().Same(BoolT) {
		if !b.afterDamage(e.Pos()) {
			return e
		}
		b.errorHints(CodeType, e.Pos(), fmt.Sprintf("%s requires a boolean expression, got %s", what, e.Ty()),
			[]string{"use a comparison such as x = 1 or IS NULL"})
	}
	return e
}

// ---------------------------------------------------------------------------
// ORDER BY
// ---------------------------------------------------------------------------

// bindOrderBy attaches a Sort node. Sort keys may reference output aliases,
// output ordinals, or (when the body is a plain SELECT) source columns.
func (b *Binder) bindOrderBy(body *PlanNode, stmt *Statement, outer *scope) *PlanNode {
	// Build a scope over the output to resolve aliases and ordinals.
	outScope := newScope(nil)
	outScope.addAll(body.Schema)
	var keys []BExpr
	var dirs []bool
	for _, it := range stmt.OrderBy {
		// Bare integer ordinal.
		if lit, ok := it.Expr.(*ELiteral); ok && lit.Kind == KInt {
			idx := int(lit.Int) - 1
			if idx < 0 || idx >= len(body.Schema) {
				b.errorf(CodeName, lit.Posn, "ORDER BY position %d is out of range (1..%d)", lit.Int, len(body.Schema))
				continue
			}
			keys = append(keys, &ColRef{Depth: 0, Index: idx, Name: body.Schema[idx].Name, T: body.Schema[idx].Type, Posn: lit.Posn})
			dirs = append(dirs, it.Desc)
			continue
		}
		// Bare column name matching an output alias.
		if col, ok := it.Expr.(*EColumn); ok && len(col.Parts) == 1 {
			if idx := findCol(body.Schema, col.Parts[0]); idx >= 0 {
				keys = append(keys, &ColRef{Depth: 0, Index: idx, Name: body.Schema[idx].Name, T: body.Schema[idx].Type, Posn: col.Posn})
				dirs = append(dirs, it.Desc)
				continue
			}
		}
		// General expression over output columns.
		e := b.bindExpr(it.Expr, outScope, false)
		if e == nil {
			continue
		}
		keys = append(keys, e)
		dirs = append(dirs, it.Desc)
	}
	if len(keys) == 0 {
		return body
	}
	return &PlanNode{Op: OpSort, Children: []*PlanNode{body}, Exprs: keys, SortDir: dirs,
		Schema: body.Schema, ID: b.ids.next(), Origin: "ORDER BY"}
}

// ---------------------------------------------------------------------------
// CAST
// ---------------------------------------------------------------------------

func (b *Binder) checkCast(x BExpr, to Type, p Pos) (BExpr, *Error) {
	from := x.Ty()
	if from.Kind == KNull {
		return &Cast{X: x, T: to, Posn: p}, nil
	}
	// A cast of a literal is a compile-time constant: evaluate it now so an
	// out-of-range value (e.g. 100.15::tick, whose maximum is 100.00) is a
	// bind-time error rather than a row-order-dependent runtime error.
	if from.Kind == to.Kind {
		return &Cast{X: x, T: to, Posn: p}, nil
	}
	// Explicit CASTs are permitted between numeric and clock types (the
	// write-side representation) but date<->clock still requires the named
	// conversion functions; nothing converts implicitly.
	allowed := map[Kind]map[Kind]bool{
		KInt:    {KDec: true, KString: true, KClock: true, KBool: true},
		KDec:    {KInt: true, KString: true, KClock: true},
		KString: {KInt: true, KDec: true, KDate: true, KClock: true, KBool: true},
		KDate:   {KString: true},
		KClock:  {KString: true, KInt: true, KDec: true},
		KBool:   {KString: true, KInt: true},
	}
	if allowed[from.Kind][to.Kind] {
		// A cast of a literal is a compile-time constant: evaluate it now so
		// an out-of-range value (100.15::tick, whose maximum is 100.00) is a
		// bind-time error rather than a row-order-dependent runtime error.
		if lit, ok := x.(*Lit); ok {
			if cv, err := castValue(lit.V, to, p); err == nil {
				// Fold the cast to a typed literal of the target type. This
				// makes the value range-checked here and lets later rules see
				// a constant (100.00::tick behaves as a typed tick literal).
				return &Lit{V: cv, T: to, Posn: p}, nil
			} else {
				err.Code = CodeImplicit
				return &Cast{X: x, T: to, Posn: p}, err
			}
		}
		return &Cast{X: x, T: to, Posn: p}, nil
	}
	return nil, errWithHints(CodeImplicit, p,
		fmt.Sprintf("cannot cast %s to %s", from, to),
		[]string{"date and clock values convert only through from_clock()/to_clock(); string conversions are explicit"})
}

// exprNullable is a conservative nullability estimate for a bound expression.
func exprNullable(e BExpr) bool {
	switch x := e.(type) {
	case *Lit:
		return x.V.Null
	case *ColRef:
		return true
	case *AggRef:
		return true
	case *Bin:
		if x.Op == "eq" || x.Op == "ne" || x.Op == "lt" || x.Op == "le" || x.Op == "gt" || x.Op == "ge" ||
			x.Op == "and" || x.Op == "or" {
			return true
		}
		return exprNullable(x.L) || exprNullable(x.R)
	case *Un:
		return exprNullable(x.X)
	case *Fn:
		for _, a := range x.Args {
			if exprNullable(a) {
				return true
			}
		}
		return true
	case *Case:
		if x.Else == nil {
			return true
		}
		for _, w := range x.Whens {
			if exprNullable(w.Then) {
				return true
			}
		}
		return exprNullable(x.Else)
	case *Cast:
		return exprNullable(x.X)
	case *IsNull, *Between, *InList, *Like:
		return true
	case *InSub:
		return true
	case *ScalarSub:
		return true
	case *Exists:
		return false
	}
	return true
}

// BindOne is a convenience wrapper used by the engine entry point.
func BindOne(stmt *Statement, cat *Catalog, reg *Registry) (*Plan, []*Error) {
	return NewBinder(cat, reg).Bind(stmt)
}

// BindAfterErrors binds a best-effort tree produced by a parser that already
// reported syntax errors. Context errors that are likely consequences of the
// syntax damage are suppressed; definite errors (unknown names, unknown
// functions, arity) are still reported.
func BindAfterErrors(stmt *Statement, cat *Catalog, reg *Registry, damageLine, damageCol int) (*Plan, []*Error) {
	b := NewBinder(cat, reg)
	b.suppressExprErrors = true
	b.damageLine, b.damageCol = damageLine, damageCol
	return b.Bind(stmt)
}

// checkCall validates a function call with the registry's signature. Type
// mismatches are reported with the same CodeImplicit vocabulary as operator
// errors so callers see a consistent diagnostic, and clock/date mix-ups get
// the explicit-conversion hint.
func (b *Binder) checkCall(p Pos, name string, args []Type) Type {
	d, ok := b.reg.Lookup(name)
	if !ok {
		if b.afterDamage(p) {
			b.errorf(CodeUnknownFn, p, "unknown function %q", name)
		}
		return NullT
	}
	t, e := d.Sig.CheckArgs(p, args)
	if e != nil {
		// Calls inside a syntactically damaged region have lost arguments;
		// their arity is not meaningful.
		if b.afterDamage(p) && !(b.suppressExprErrors && e.Code == CodeArity) {
			b.errs = append(b.errs, e)
		}
		return NullT
	}
	return t
}
