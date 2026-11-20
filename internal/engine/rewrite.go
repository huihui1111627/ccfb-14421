package engine

import (
	"strings"
	"sync"
)

// Rule rewrites a plan tree bottom-up. Apply must return a new tree (it must
// not mutate the input) or the same pointer when nothing changed. Every rule
// is schema-preserving: the driver checks the root's output schema after each
// application and discards an application that violates it.
type Rule interface {
	Name() string
	// Doc is a one-line description shown in the trace UI.
	Doc() string
	Apply(n *PlanNode, rc *RuleCtx) *PlanNode
}

// RuleCtx is scratch state shared across one rule application.
type RuleCtx struct {
	IDs *idAlloc
	// Depth is the nesting depth of the node currently being visited.
	Depth int
	// Warnings collected by rules (e.g. skipped CNF).
	Warnings []*Error
	// Note lets a rule attach an explanation to the change it makes.
	Note string
	// CTERefs counts CTEScan references by CTE name in the tree currently
	// being rewritten. It is recomputed once per rule application.
	CTERefs map[string]int
	// Checked and Rejected are per-rule-application counters a rule updates
	// as it evaluates its preconditions. The driver reports them per step.
	Checked  int
	Rejected int
}

// NodeChange records a single node rewrite inside one rule application.
type NodeChange struct {
	NodeID int
	Depth  int
	Before string
	After  string
	Note   string
}

// RuleStep records one rule's application in one pass. Before/After are the
// whole-tree renderings, which is what the inspection page displays.
type RuleStep struct {
	Pass     int
	Rule     string
	Doc      string
	Before   string
	After    string
	Changed  int
	Checked  int
	Rejected int
	// SkipReason explains why a checked precondition refused the rewrite.
	SkipReason string
	Changes    []NodeChange
}

// PassSnapshot is a plan tree at a pass boundary.
type PassSnapshot struct {
	Pass  int
	Tree  string
	Steps int // number of rule steps in this pass
}

// RewardResult is the full outcome of rewriting.
type RewriteResult struct {
	Root      *PlanNode
	Steps     []RuleStep
	Passes    int
	Counts    map[string]int
	Checks    map[string]int
	Rejected  map[string]int
	Warnings  []*Error
	Snapshots []PassSnapshot
}

// DefaultRules is the rewrite pipeline. Order matters: folds first expose
// false predicates, outer-to-inner runs before simplification, CTE inlining
// precedes CNF so the inlined body participates.
func DefaultRules() []Rule {
	return []Rule{
		ConstantFold{},
		OuterToInner{},
		SimplifyFalse{},
		InlineCTE{},
		CNF{},
	}
}

// maxPasses bounds the fixpoint loop; reaching it means rules are not
// converging and the result is reported as such.
const maxPasses = 12

// Rewrite runs rules to a fixpoint. In every pass each rule is applied
// bottom-up over the whole tree; the pass repeats while any rule changed a
// node. Schema preservation is enforced per rule application. New nodes take
// IDs from alloc, which the caller seeds above the tree's existing IDs.
func Rewrite(root *PlanNode, rules []Rule, alloc *idAlloc) *RewriteResult {
	res := &RewriteResult{
		Root:     root,
		Counts:   map[string]int{},
		Checks:   map[string]int{},
		Rejected: map[string]int{},
	}
	cur := root
	lastGood := root
	for pass := 1; pass <= maxPasses; pass++ {
		changed := false
		ctx := &RuleCtx{IDs: alloc}
		for _, r := range rules {
			before := cur
			next, changes, checked, rejected, note := applyRule(cur, r, ctx)
			if len(ctx.Warnings) > 0 {
				res.Warnings = append(res.Warnings, ctx.Warnings...)
				ctx.Warnings = nil
			}
			step := RuleStep{
				Pass: pass, Rule: r.Name(), Doc: r.Doc(),
				Before: FormatTree(before), After: FormatTree(next),
				Checked: checked, Rejected: rejected, SkipReason: note,
			}
			step.Changes = changes
			step.Changed = len(changes)
			res.Checks[r.Name()] += checked
			res.Rejected[r.Name()] += rejected
			if step.Changed > 0 {
				res.Counts[r.Name()] += step.Changed
				changed = true
			}
			res.Steps = append(res.Steps, step)
			cur = next
		}
		// The output contract is checked once per pass, on the root schema:
		// a rule may reorganise the tree freely below the root, but the
		// columns the executor receives must not change. A pass that breaks
		// the contract is discarded and reported.
		if !schemaEqual(root.Schema, cur.Schema) || !schemaMarkersEqual(root.Schema, cur.Schema) {
			res.Warnings = append(res.Warnings, errAt(CodeRewrite, Pos{},
				"pass %d: rules changed the output schema of the plan root; pass discarded", pass))
			cur = lastGood
		} else {
			lastGood = cur
		}
		res.Snapshots = append(res.Snapshots, PassSnapshot{Pass: pass, Tree: FormatTree(cur), Steps: len(rules)})
		res.Passes = pass
		if !changed {
			break
		}
	}
	cur.ID = root.ID
	res.Root = cur
	return res
}

// schemaMarkersEqual compares schema markers (declared names/types) without
// the plan-derivable Nullable flag, which rules may tighten when an outer join
// becomes inner. Declared markers are what the executor's output contract
// depends on.
func schemaMarkersEqual(a, b []OutputCol) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i].Name, b[i].Name) || !a[i].Type.Same(b[i].Type) ||
			!strings.EqualFold(a[i].Qualifier, b[i].Qualifier) {
			return false
		}
	}
	return true
}

func countCTEScans(n *PlanNode, out map[string]int) {
	if n == nil {
		return
	}
	if n.Op == OpCTEScan {
		out[n.CTEName]++
	}
	for _, c := range n.Children {
		countCTEScans(c, out)
	}
	for _, e := range n.Exprs {
		walkSubqueryPlans(e, func(p *PlanNode) { countCTEScans(p, out) })
	}
	for _, a := range n.Aggs {
		if a.Arg != nil {
			walkSubqueryPlans(a.Arg, func(p *PlanNode) { countCTEScans(p, out) })
		}
	}
	for _, g := range n.GroupBy {
		walkSubqueryPlans(g, func(p *PlanNode) { countCTEScans(p, out) })
	}
}

// walkSubqueryPlans visits subquery plans embedded in an expression.
func walkSubqueryPlans(e BExpr, f func(*PlanNode)) {
	walkExpr(e, func(x BExpr) {
		switch v := x.(type) {
		case *ScalarSub:
			f(v.Plan)
		case *InSub:
			f(v.Plan)
		case *Exists:
			f(v.Plan)
		}
	})
}

// applyRule walks bottom-up. It counts precondition checks and node changes.
func applyRule(root *PlanNode, r Rule, rc *RuleCtx) (*PlanNode, []NodeChange, int, int, string) {
	var changes []NodeChange
	checked := 0
	rejected := 0
	var skipNote string
	rc.CTERefs = map[string]int{}
	countCTEScans(root, rc.CTERefs)
	var visit func(n *PlanNode, depth int) *PlanNode
	visit = func(n *PlanNode, depth int) *PlanNode {
		if n == nil {
			return nil
		}
		kids := make([]*PlanNode, len(n.Children))
		changedChild := false
		for i, c := range n.Children {
			nc := visit(c, depth+1)
			kids[i] = nc
			if nc != c {
				changedChild = true
			}
		}
		if changedChild {
			cp := *n
			cp.Children = kids
			n = &cp
		}
		rc.Depth = depth
		rc.Note = ""
		rc.Checked, rc.Rejected = 0, 0
		beforeFP := n.fingerprint()
		out := r.Apply(n, rc)
		if out == nil {
			out = n
		}
		checked += rc.Checked
		rejected += rc.Rejected
		afterFP := out.fingerprint()
		if beforeFP != afterFP {
			changes = append(changes, NodeChange{
				NodeID: n.ID, Depth: depth,
				// Short human-readable labels rather than structural
				// fingerprints; the full trees are in the step's Before/After.
				Before: n.label(), After: out.label(), Note: rc.Note,
			})
			checked++
		}
		if rc.Note != "" && skipNote == "" && beforeFP == afterFP {
			skipNote = rc.Note
		}
		return out
	}
	out := visit(root, 0)
	if len(changes) == 0 && skipNote != "" {
		checked++
	}
	return out, changes, checked, rejected, skipNote
}

// RewritePlan rewrites p in place (root pointer updated) and updates MaxID.
// Rules allocate node IDs from the plan's id allocator; this keeps them above
// every ID already in the tree.
func (p *Plan) RewritePlan(rules []Rule) *RewriteResult {
	n := p.MaxID
	alloc := &idAlloc{n: n}
	rr := Rewrite(p.Root, rules, alloc)
	p.MaxID = alloc.n
	if p.MaxID < rr.Root.ID {
		p.MaxID = rr.Root.ID
	}
	p.Root = rr.Root
	p.Output = rr.Root.Schema
	return rr
}

// ---------------------------------------------------------------------------
// Expression utilities shared by rules
// ---------------------------------------------------------------------------

// conjuncts flattens a predicate into AND-separated terms.
func conjuncts(e BExpr) []BExpr {
	if e == nil {
		return nil
	}
	if b, ok := e.(*Bin); ok && b.Op == "and" {
		return append(conjuncts(b.L), conjuncts(b.R)...)
	}
	return []BExpr{e}
}

// disjuncts flattens a predicate into OR-separated terms.
func disjuncts(e BExpr) []BExpr {
	if e == nil {
		return nil
	}
	if b, ok := e.(*Bin); ok && b.Op == "or" {
		return append(disjuncts(b.L), disjuncts(b.R)...)
	}
	return []BExpr{e}
}

// conjoin builds a left-deep AND chain.
func conjoin(es []BExpr) BExpr {
	return foldBool(es, "and")
}

func disjoin(es []BExpr) BExpr {
	return foldBool(es, "or")
}

func foldBool(es []BExpr, op string) BExpr {
	if len(es) == 0 {
		return nil
	}
	out := es[0]
	for _, e := range es[1:] {
		out = &Bin{Op: op, L: out, R: e, T: BoolT, Posn: out.Pos()}
	}
	return out
}

// isFalsePred reports whether e is provably FALSE for every row.
func isFalsePred(e BExpr) bool {
	v, ok := constEvalBool(e)
	return ok && !v
}

// isTruePred reports whether e is provably TRUE for every row.
func isTruePred(e BExpr) bool {
	v, ok := constEvalBool(e)
	return ok && v
}

// constEvalBool evaluates a predicate that has no column, subquery or
// aggregate references. A NULL result is "not known".
func constEvalBool(e BExpr) (bool, bool) {
	if e == nil || exprHasColumn(e) || exprHasSubquery(e) || exprHasAggRef(e) {
		return false, false
	}
	ev := &Evaluator{reg: DefaultRegistry()}
	v, err := ev.eval(e, &Env{})
	if err != nil || v.Null || v.Kind != KBool {
		return false, false
	}
	return v.Bool, true
}

func constEvalValue(e BExpr) (Value, *Error) {
	if e == nil || exprHasColumn(e) || exprHasSubquery(e) || exprHasAggRef(e) {
		return Value{}, errAt(CodeEval, e.Pos(), "not constant")
	}
	ev := &Evaluator{reg: DefaultRegistry()}
	return ev.eval(e, &Env{})
}

var (
	sharedRegistry *Registry
	registryOnce   sync.Once
)

// DefaultRegistry returns the process-wide registry with all builtins and
// auction functions installed. Constant folding uses it, so folded results
// match interpreter results exactly.
func DefaultRegistry() *Registry {
	registryOnce.Do(func() {
		sharedRegistry = NewRegistry()
	})
	return sharedRegistry
}
