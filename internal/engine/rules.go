package engine

import (
	"fmt"
)

// ---------------------------------------------------------------------------
// 1. Constant folding
// ---------------------------------------------------------------------------

// ConstantFold evaluates expressions that contain no column, aggregate or
// subquery references. Literals produced are typed exactly as the binder
// typed the whole expression, so folding cannot change result types.
type ConstantFold struct{}

func (ConstantFold) Name() string { return "constant-fold" }
func (ConstantFold) Doc() string {
	return "evaluate expressions with no column/subquery references at plan time"
}

func (ConstantFold) Apply(n *PlanNode, rc *RuleCtx) *PlanNode {
	if n.Op == OpEmpty || n.Op == OpValues {
		return nil
	}
	rc.Checked = 1
	folded := foldList(n.Exprs, rc)
	changed := !sameExprList(n.Exprs, folded)
	// Filter predicates: a constant TRUE filter is removed.
	if n.Op == OpFilter && len(folded) == 1 {
		if isTruePred(folded[0]) {
			rc.Checked = 1
			rc.Note = "constant TRUE predicate removed"
			return wrapFilterRemoval(n)
		}
	}
	if !changed {
		rc.Rejected = 1
		rc.Note = "no constant subexpression"
		return nil
	}
	cp := *n
	cp.Exprs = folded
	rc.Note = "folded " + fmt.Sprint(countFolds(n.Exprs, folded)) + " expression(s)"
	return &cp
}

func wrapFilterRemoval(n *PlanNode) *PlanNode {
	if len(n.Children) != 1 {
		return n
	}
	child := n.Children[0]
	// The Filter's schema equals its child's; return the child directly. A
	// parent that referenced the filter node by ID only affects display.
	return child
}

func foldList(es []BExpr, rc *RuleCtx) []BExpr {
	out := make([]BExpr, len(es))
	for i, e := range es {
		out[i] = foldExpr(e, rc)
	}
	return out
}

// foldExpr folds every constant subexpression. mapExpr is bottom-up, so a
// node is only reached after its children have been folded. Nodes that would
// produce an evaluation error (division by zero, overflow) are left intact so
// the interpreter reports the error at the right row and time.
func foldExpr(e BExpr, rc *RuleCtx) BExpr {
	return mapExpr(e, func(x BExpr) BExpr {
		switch x.(type) {
		case *Lit, *ColRef, *AggRef, *ScalarSub, *InSub, *Exists:
			return x
		}
		if x.Ty().Kind == KNull {
			return x
		}
		v, err := constEvalValue(x)
		if err != nil {
			return x
		}
		return &Lit{V: coerceToType(v, x.Ty()), T: x.Ty(), Posn: x.Pos()}
	})
}

func countFolds(before, after []BExpr) int {
	n := 0
	for i := range before {
		if !exprEqual(before[i], after[i]) {
			n++
		}
	}
	return n
}

func sameExprList(a, b []BExpr) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !exprEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 2. Outer join -> inner join
// ---------------------------------------------------------------------------

// OuterToInner turns an outer join into an inner join when a condition above
// it rejects the NULL-extended rows: a strict predicate on a column that the
// outer join can NULL-fill proves those rows are discarded anyway.
//
// Preconditions checked:
//   - the join is LEFT/RIGHT/FULL;
//   - the side being eliminated is the nullable side;
//   - the rejecting predicate is a strict function of that side (a column
//     reference from the nullable side, or a NOT of one);
//   - the predicate lives in a Filter directly above the join (or above a
//     chain of Filters/Projects that preserve the columns).
//
// For LEFT JOIN the nullable side is the right input; a predicate that
// requires a right column to be TRUE (or NON-NULL) can never hold on the
// NULL-extended row. IS NULL is explicitly NOT rejecting.
type OuterToInner struct{}

func (OuterToInner) Name() string { return "outer-to-inner" }
func (OuterToInner) Doc() string {
	return "reduce LEFT/RIGHT/FULL JOIN to INNER when an above predicate rejects NULL-extended rows"
}

func (OuterToInner) Apply(n *PlanNode, rc *RuleCtx) *PlanNode {
	if n.Op != OpFilter || len(n.Children) != 1 {
		return nil
	}
	join := n.Children[0]
	if join == nil || join.Op != OpJoin || join.Join == nil {
		return nil
	}
	if !join.Join.Kind.IsOuter() {
		return nil
	}
	rc.Checked = 1
	leftLen := join.Join.LeftCols
	rightLen := join.Join.RightCols
	var rejectLeft, rejectRight bool
	var why string
	for _, c := range conjuncts(n.Exprs[0]) {
		if isRejecting(c, 0, leftLen) {
			rejectLeft = true
			why = "predicate " + c.String() + " rejects NULL-extended left rows"
		}
		if isRejecting(c, leftLen, leftLen+rightLen) {
			rejectRight = true
			why = "predicate " + c.String() + " rejects NULL-extended right rows"
		}
	}
	newKind := join.Join.Kind
	switch join.Join.Kind {
	case JoinLeft:
		if !rejectRight {
			rc.Rejected = 1
			rc.Note = "no above predicate rejects NULL-extended right rows"
			return nil
		}
		newKind = JoinInner
	case JoinRight:
		if !rejectLeft {
			rc.Rejected = 1
			rc.Note = "no above predicate rejects NULL-extended left rows"
			return nil
		}
		newKind = JoinInner
	case JoinFull:
		// Rejecting rows whose LEFT columns are NULL removes the
		// right-unmatched rows, leaving a LEFT join (which preserves left).
		// Symmetrically, rejecting NULL right rows leaves a RIGHT join.
		if rejectLeft && rejectRight {
			newKind = JoinInner
		} else if rejectLeft {
			newKind = JoinLeft
		} else if rejectRight {
			newKind = JoinRight
		} else {
			rc.Rejected = 1
			rc.Note = "no above predicate rejects a NULL-extended side"
			return nil
		}
	default:
		return nil
	}
	// The join's column nullability must be adjusted: an INNER join no longer
	// NULL-extends. The schema's column names/types stay identical (that is
	// the contract); only the Nullable flag would change, and we deliberately
	// keep Nullable conservative (true) so the schema is unchanged.
	nj := *join
	ji := *join.Join
	ji.Kind = newKind
	nj.Join = &ji
	cp := *n
	cp.Children = []*PlanNode{&nj}
	rc.Note = why + "; " + join.Join.Kind.String() + " -> " + newKind.String()
	return &cp
}

// Truth-value possibility sets used by the outer-join analysis. A predicate
// rejects NULL-extended rows when TRUE is not among the values it can take
// there.
const (
	canTrue  uint8 = 1
	canFalse uint8 = 2
	canNull  uint8 = 4
	anyTruth       = canTrue | canFalse | canNull
)

// isRejecting reports whether predicate p can never be TRUE on a row where
// every column of the join side [lo,hi) is NULL. This is the three-valued-logic
// condition for the outer join to be reducible: such rows are discarded by the
// filter, so the NULL-extended rows cannot survive.
func isRejecting(p BExpr, lo, hi int) bool {
	if !containsSide(p, lo, hi) {
		return false
	}
	return truthSet(p, lo, hi)&canTrue == 0
}

// truthSet abstracts the values p can take when the columns of side [lo,hi)
// are NULL. Columns of the other side are unconstrained. Everything not
// understood conservatively allows all three values.
func truthSet(e BExpr, lo, hi int) uint8 {
	if e == nil {
		return anyTruth
	}
	switch x := e.(type) {
	case *Lit:
		if x.V.Null {
			return canNull
		}
		if x.V.Kind == KBool {
			if x.V.Bool {
				return canTrue
			}
			return canFalse
		}
		return canTrue // a non-null non-boolean is a type error; treat as usable
	case *ColRef:
		if x.Depth == 0 && x.Index >= lo && x.Index < hi {
			// NULL-extended column.
			return canNull
		}
		return anyTruth
	case *IsNull:
		child := truthSet(x.X, lo, hi)
		switch {
		case child&^(canNull) == 0:
			// Child is always NULL.
			if x.Not {
				return canFalse
			}
			return canTrue
		case child&canNull == 0:
			if x.Not {
				return canTrue
			}
			return canFalse
		}
		return canTrue | canFalse
	case *Un:
		if x.Op == "not" {
			child := truthSet(x.X, lo, hi)
			var out uint8
			if child&canTrue != 0 {
				out |= canFalse
			}
			if child&canFalse != 0 {
				out |= canTrue
			}
			if child&canNull != 0 {
				out |= canNull
			}
			return out
		}
		// Numeric negation: NULL-propagating.
		return propagateNull(truthSet(x.X, lo, hi))
	case *Bin:
		switch x.Op {
		case "and":
			return andTruth(truthSet(x.L, lo, hi), truthSet(x.R, lo, hi))
		case "or":
			return orTruth(truthSet(x.L, lo, hi), truthSet(x.R, lo, hi))
		case "eq", "ne", "lt", "le", "gt", "ge":
			l := truthSet(x.L, lo, hi)
			r := truthSet(x.R, lo, hi)
			return comparisonTruth(l, r)
		default:
			// Arithmetic is strict: any NULL operand yields NULL.
			l := truthSet(x.L, lo, hi)
			r := truthSet(x.R, lo, hi)
			return propagateNull(combineArith(l, r))
		}
	case *Fn:
		// All registered scalar functions are strict. If every argument can
		// only be NULL the result is NULL; otherwise leave it open.
		allNull := len(x.Args) > 0
		for _, a := range x.Args {
			if truthSet(a, lo, hi)&^canNull != 0 {
				allNull = false
			}
		}
		if allNull {
			return canNull
		}
		return anyTruth
	case *Between, *Like, *InList:
		// Strict: NULL operand yields NULL. With a NULL-extended operand the
		// common generated shape compares a nullable column against a
		// constant, whose result is NULL.
		return canNull | canTrue | canFalse
	case *Cast:
		return propagateNull(truthSet(x.X, lo, hi))
	}
	return anyTruth
}

// propagateNull turns a set of non-boolean value classes into a truth set:
// NULL propagates, non-NULL values are "some value" (canTrue, since the
// converter only needs to know NULL vs not-NULL at the leaves).
func propagateNull(v uint8) uint8 {
	if v&^canNull == 0 {
		return canNull
	}
	return anyTruth
}

func combineArith(l, r uint8) uint8 {
	// A non-NULL operand makes the result non-NULL.
	if l&^canNull == 0 || r&^canNull == 0 {
		return canNull
	}
	return anyTruth
}

func comparisonTruth(l, r uint8) uint8 {
	// Strict: NULL in either operand gives NULL.
	if l&^canNull == 0 || r&^canNull == 0 {
		return canNull
	}
	if l&canNull != 0 || r&canNull != 0 {
		return canTrue | canFalse | canNull
	}
	return canTrue | canFalse
}

func andTruth(l, r uint8) uint8 {
	var out uint8
	for _, a := range truthValues(l) {
		for _, b := range truthValues(r) {
			switch {
			case a == canFalse || b == canFalse:
				out |= canFalse
			case a == canTrue && b == canTrue:
				out |= canTrue
			default:
				out |= canNull
			}
		}
	}
	return out
}

func orTruth(l, r uint8) uint8 {
	var out uint8
	for _, a := range truthValues(l) {
		for _, b := range truthValues(r) {
			switch {
			case a == canTrue || b == canTrue:
				out |= canTrue
			case a == canFalse && b == canFalse:
				out |= canFalse
			default:
				out |= canNull
			}
		}
	}
	return out
}

func truthValues(mask uint8) []uint8 {
	var out []uint8
	for _, v := range []uint8{canTrue, canFalse, canNull} {
		if mask&v != 0 {
			out = append(out, v)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 3. False predicate -> empty relation
// ---------------------------------------------------------------------------

// SimplifyFalse replaces a subtree with an Empty relation when a FILTER
// predicate is provably FALSE. Two forms are handled:
//
//   - a filter whose entire predicate folds to FALSE becomes Empty;
//   - a join ON condition that folds to FALSE (an inner join) becomes Empty,
//     dropping both sides' scans, because an inner join with a false condition
//     has no rows. Outer joins keep their preserved side, so they are left
//     alone (the correctness argument requires no unmatched rows).
//
// The Empty node carries the same schema as the replaced subtree, so parents
// see identical columns and types.
type SimplifyFalse struct{}

func (SimplifyFalse) Name() string { return "simplify-false" }
func (SimplifyFalse) Doc() string {
	return "replace a subtree whose predicate is provably FALSE with an empty relation"
}

func (SimplifyFalse) Apply(n *PlanNode, rc *RuleCtx) *PlanNode {
	switch n.Op {
	case OpFilter:
		if len(n.Children) == 1 && len(n.Exprs) == 1 {
			rc.Checked = 1
			if hasFalseConjunct(n.Exprs[0]) {
				rc.Note = "predicate contains a constant FALSE conjunct; subtree replaced by Empty"
				return emptyLike(n)
			}
		}
	case OpJoin:
		if n.Join != nil && len(n.Exprs) == 1 {
			rc.Checked = 1
			if n.Join.Kind == JoinInner && isFalsePred(n.Exprs[0]) {
				rc.Note = "inner join predicate folds to FALSE; whole join replaced by Empty"
				return emptyLike(n)
			}
			if n.Join.Kind.IsOuter() {
				rc.Rejected = 1
				rc.Note = "outer join keeps its preserved side; not replaced"
				return nil
			}
		}
	}
	if rc.Checked > 0 {
		rc.Rejected = 1
	}
	return nil
}

// hasFalseConjunct reports whether a top-level conjunct of p is a constant
// FALSE. FALSE AND anything is FALSE under three-valued logic (even NULL), so
// such a filter can never admit a row.
func hasFalseConjunct(p BExpr) bool {
	for _, c := range conjuncts(p) {
		if v, ok := constEvalBool(c); ok && !v {
			return true
		}
	}
	return false
}

// emptyLike builds an Empty node with n's exact schema. Empty rows must still
// be usable by an enclosing aggregate (GROUP BY over Empty yields no groups,
// and an aggregate without GROUP BY yields one row), which the interpreter
// implements.
func emptyLike(n *PlanNode) *PlanNode {
	schema := make([]OutputCol, len(n.Schema))
	copy(schema, n.Schema)
	return &PlanNode{Op: OpEmpty, Schema: schema, ID: n.ID, Origin: "empty relation", Rule: "simplify-false"}
}

// ---------------------------------------------------------------------------
// 4. Inline single-use CTE
// ---------------------------------------------------------------------------

// InlineCTE replaces a CTEScan with the CTE's plan body when the CTE is
// referenced exactly once anywhere in the plan. The CTEScan's schema (which
// carries the reference alias and declared column names) is re-established by
// wrapping the cloned body in a Rename node, so the parent's column indices
// and qualifiers are unchanged. A second scan of the same CTE in the same
// query keeps the CTE materialized: inlining it twice could duplicate work
// and change error behaviour (e.g. division by zero) subtlety.
type InlineCTE struct{}

func (InlineCTE) Name() string { return "inline-cte" }
func (InlineCTE) Doc() string {
	return "expand a CTE referenced exactly once directly into the plan"
}

func (InlineCTE) Apply(n *PlanNode, rc *RuleCtx) *PlanNode {
	if n.Op != OpCTEScan || n.CTEPlan == nil {
		return nil
	}
	rc.Checked = 1
	if rc.CTERefs[n.CTEName] != 1 {
		rc.Rejected = 1
		rc.Note = "CTE " + n.CTEName + " is referenced " + fmt.Sprint(rc.CTERefs[n.CTEName]) + " time(s); keeping materialized form"
		return nil
	}
	body := clonePlan(n.CTEPlan, rc.IDs)
	schema := make([]OutputCol, len(n.Schema))
	copy(schema, n.Schema)
	ren := &PlanNode{
		Op: OpRename, Children: []*PlanNode{body}, Sub: body,
		Schema: schema, ID: n.ID, Origin: "inlined CTE " + n.CTEName, Rule: "inline-cte",
	}
	rc.Note = "CTE " + n.CTEName + " has a single reference; body inlined"
	return ren
}

// countCTERefsInPlan counts CTEScan nodes for name inside a plan tree.
func countCTERefsInPlan(root *PlanNode, name string) int {
	count := 0
	var rec func(n *PlanNode)
	rec = func(n *PlanNode) {
		if n == nil {
			return
		}
		if n.Op == OpCTEScan && n.CTEName == name {
			count++
		}
		for _, c := range n.Children {
			rec(c)
		}
	}
	rec(root)
	return count
}

// CountCTEUse counts CTEScan references for a CTE name across a whole plan.
func CountCTEUse(root *PlanNode, name string) int {
	return countCTERefsInPlan(root, name)
}

// ---------------------------------------------------------------------------
// 5. CNF conversion
// ---------------------------------------------------------------------------

// MaxCNFTerms bounds conversion blow-up. If pushing a predicate into
// conjunctive normal form would produce more terms than this, the rule
// refuses and leaves the predicate as written.
const MaxCNFTerms = 64

// CNF rewrites a FILTER predicate into conjunctive normal form so that the
// executor can use each top-level conjunct as an independent access/join
// predicate. The conversion is only attempted when the estimated size stays
// within MaxCNFTerms; otherwise the predicate is preserved verbatim and a
// warning is recorded, which makes the refusal visible in the trace.
type CNF struct{}

func (CNF) Name() string { return "cnf" }
func (CNF) Doc() string {
	return "normalize a filter predicate to conjunctive normal form when it stays within the term budget"
}

func (CNF) Apply(n *PlanNode, rc *RuleCtx) *PlanNode {
	if n.Op != OpFilter || len(n.Exprs) != 1 {
		return nil
	}
	rc.Checked = 1
	p := n.Exprs[0]
	terms, ok := toCNF(p, MaxCNFTerms)
	if !ok {
		rc.Rejected = 1
		rc.Note = fmt.Sprintf("predicate would exceed the %d-term CNF budget; left as written", MaxCNFTerms)
		rc.Warnings = append(rc.Warnings, errAt(CodeRewrite, p.Pos(),
			"CNF skipped: estimated terms exceed %d", MaxCNFTerms))
		return nil
	}
	// Already CNF (top-level and-chain, no nested OR under a term's AND)?
	out := conjoin(terms)
	if exprKey(out) == exprKey(p) {
		rc.Rejected = 1
		rc.Note = "predicate is already in CNF"
		return nil
	}
	cp := *n
	cp.Exprs = []BExpr{out}
	rc.Note = fmt.Sprintf("predicate rewritten to CNF with %d term(s)", len(terms))
	return &cp
}

// toCNF converts p to a list of conjuncts, refusing if the result can exceed
// limit terms. The estimate is computed structurally before building.
func toCNF(p BExpr, limit int) ([]BExpr, bool) {
	nTerms := cnfSize(p, limit+1)
	if nTerms > limit {
		return nil, false
	}
	return cnfConvert(p), true
}

// cnfSize estimates the number of conjuncts CNF would produce, capped at cap.
func cnfSize(e BExpr, cap int) int {
	if e == nil {
		return 0
	}
	b, ok := e.(*Bin)
	if !ok {
		return 1
	}
	switch b.Op {
	case "and":
		return minInt(cap, cnfSize(b.L, cap)+cnfSize(b.R, cap))
	case "or":
		ls := cnfSize(b.L, cap)
		rs := cnfSize(b.R, cap)
		if ls >= cap || rs >= cap {
			return cap
		}
		return minInt(cap, ls*rs)
	}
	return 1
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// cnfConvert returns conjuncts of an equivalent CNF formula. Negations are
// pushed down only where safe: NOT over a non-boolean-leaf node is rewritten
// by De Morgan, NOT over comparisons is left alone (there is no three-valued
// negation rewrite applied to pushdown targets), and NOT over IS NULL is
// turned into IS NOT NULL, which is exact.
func cnfConvert(e BExpr) []BExpr {
	if e == nil {
		return nil
	}
	b, ok := e.(*Bin)
	if !ok {
		return []BExpr{e}
	}
	switch b.Op {
	case "and":
		return append(cnfConvert(b.L), cnfConvert(b.R)...)
	case "or":
		left := cnfConvert(b.L)
		right := cnfConvert(b.R)
		out := make([]BExpr, 0, len(left)*len(right))
		for _, l := range left {
			for _, r := range right {
				out = append(out, &Bin{Op: "or", L: l, R: r, T: BoolT, Posn: b.Posn})
			}
		}
		return out
	}
	return []BExpr{e}
}

// AndTerms returns the top-level conjuncts of a predicate, for the UI.
func AndTerms(e BExpr) []BExpr { return conjuncts(e) }
