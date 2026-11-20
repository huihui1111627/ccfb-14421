package engine

import (
	"fmt"
	"strings"
)

// Op is a logical plan operator.
type Op string

const (
	OpTableScan Op = "TableScan"
	OpValues    Op = "Values"
	OpEmpty     Op = "Empty"
	OpFilter    Op = "Filter"
	OpProject   Op = "Project"
	OpJoin      Op = "Join"
	OpAggregate Op = "Aggregate"
	OpSort      Op = "Sort"
	OpLimit     Op = "Limit"
	OpDistinct  Op = "Distinct"
	OpSetOp     Op = "SetOp"
	OpCTEScan   Op = "CTEScan"
	OpRename    Op = "Rename"
)

// OutputCol is one column the plan exposes to its parent.
type OutputCol struct {
	Name      string
	Type      Type
	Qualifier string
	Nullable  bool
}

// JoinInfo describes how a Join node combines its inputs.
type JoinInfo struct {
	Kind      JoinKind
	LeftCols  int
	RightCols int
}

// PlanNode is a logical plan node. Children are in execution order. Schema
// is the node's output columns; rewrite rules must preserve it exactly
// (names, types, qualifiers and order).
type PlanNode struct {
	Op       Op
	ID       int
	Schema   []OutputCol
	Children []*PlanNode

	Table    string
	Columns  []string // projected/scan column references by name
	Exprs    []BExpr  // filter predicate, projection list, sort keys, set predicates
	Aggs     []AggDef
	GroupBy  []BExpr
	Join     *JoinInfo
	SortDir  []bool
	Limit    int64
	Offset   int64
	Distinct bool
	SetOp    string
	SetAll   bool
	Rows     [][]Value // for Values

	// CTEName/CTEPlan link a CTEScan to the bound CTE definition.
	CTEName string
	CTEPlan *PlanNode
	// Sub is the inner plan of a Rename (derived table) node.
	Sub *PlanNode
	// OuterScopes records the number of enclosing query-block rows a
	// correlated expression may depend on. Nodes with OuterScopes > 0 cannot
	// be hoisted into another block by a rule.
	OuterScopes int

	// Origin is used by the explain page: which AST construct produced it.
	Origin string
	// Rule is the last rule that rewrote this node (for the trace).
	Rule string
}

// AggDef is one aggregate computation.
type AggDef struct {
	Fn       string
	Arg      BExpr // nil for COUNT(*)
	Star     bool
	Distinct bool
	Type     Type
}

// ColIndex returns the index of a column in this node's schema.
func (n *PlanNode) ColIndex(name string) int {
	for i, c := range n.Schema {
		if strings.EqualFold(c.Name, name) {
			return i
		}
	}
	return -1
}

// fingerprint is a canonical rendering used for structural comparison.
func (n *PlanNode) fingerprint() string {
	if n == nil {
		return "<nil>"
	}
	var b strings.Builder
	b.WriteString(string(n.Op))
	b.WriteByte('[')
	for i, c := range n.Schema {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(c.Name)
		b.WriteByte(':')
		b.WriteString(c.Type.String())
		if c.Qualifier != "" {
			b.WriteByte('@')
			b.WriteString(c.Qualifier)
		}
	}
	b.WriteByte(']')
	if n.Table != "" {
		b.WriteString("{" + n.Table + "}")
	}
	if n.SetOp != "" {
		b.WriteString("{" + n.SetOp + fmt.Sprint(n.SetAll) + "}")
	}
	if n.Join != nil {
		b.WriteString("{" + n.Join.Kind.String() + fmt.Sprintf("%d/%d}", n.Join.LeftCols, n.Join.RightCols))
	}
	if n.Limit != 0 || n.Op == OpLimit {
		b.WriteString(fmt.Sprintf("{lim%d+%d}", n.Limit, n.Offset))
	}
	for _, e := range n.Exprs {
		b.WriteString("{e:" + exprKey(e) + "}")
	}
	for _, g := range n.GroupBy {
		b.WriteString("{g:" + exprKey(g) + "}")
	}
	for _, a := range n.Aggs {
		b.WriteString("{a:" + a.Fn)
		if a.Arg != nil {
			b.WriteString(":" + exprKey(a.Arg))
		}
		b.WriteString(fmt.Sprint(a.Distinct) + "}")
	}
	b.WriteByte('(')
	for i, c := range n.Children {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(c.fingerprint())
	}
	b.WriteByte(')')
	return b.String()
}

// FormatTree renders the plan as an indented tree with schemas.
func FormatTree(n *PlanNode) string {
	var b strings.Builder
	var rec func(n *PlanNode, indent string, last bool, prefix string)
	rec = func(n *PlanNode, indent string, last bool, prefix string) {
		if n == nil {
			return
		}
		b.WriteString(prefix)
		if indent != "" {
			if last {
				b.WriteString("└─ ")
			} else {
				b.WriteString("├─ ")
			}
		}
		b.WriteString(n.label())
		b.WriteByte('\n')
		childPrefix := prefix
		if indent != "" {
			if last {
				childPrefix += "   "
			} else {
				childPrefix += "│  "
			}
		}
		for i, c := range n.Children {
			rec(c, indent+"  ", i == len(n.Children)-1, childPrefix)
		}
	}
	rec(n, "", true, "")
	return b.String()
}

func (n *PlanNode) label() string {
	var b strings.Builder
	b.WriteString("#")
	b.WriteString(fmt.Sprint(n.ID))
	b.WriteByte(' ')
	b.WriteString(string(n.Op))
	switch n.Op {
	case OpTableScan, OpCTEScan:
		b.WriteString(" " + n.Table)
		if len(n.Columns) > 0 {
			b.WriteString("(" + strings.Join(n.Columns, ", ") + ")")
		}
	case OpJoin:
		b.WriteString(" " + n.Join.Kind.String())
	case OpLimit:
		fmt.Fprintf(&b, " limit=%d offset=%d", n.Limit, n.Offset)
	case OpSetOp:
		b.WriteString(" " + n.SetOp)
		if n.SetAll {
			b.WriteString(" ALL")
		}
	case OpAggregate:
		var ags []string
		for _, a := range n.Aggs {
			s := a.Fn + "("
			if a.Arg != nil {
				s += a.Arg.String()
			}
			if a.Star {
				s += "*"
			}
			s += ")"
			ags = append(ags, s)
		}
		b.WriteString(" [" + strings.Join(ags, ", ") + "]")
	}
	if len(n.Exprs) > 0 && n.Op != OpProject && n.Op != OpAggregate {
		var es []string
		for _, e := range n.Exprs {
			es = append(es, e.String())
		}
		b.WriteString(" [" + strings.Join(es, ", ") + "]")
	}
	if n.Op == OpProject {
		var es []string
		for _, e := range n.Exprs {
			es = append(es, e.String())
		}
		b.WriteString(" [" + strings.Join(es, ", ") + "]")
	}
	b.WriteString("  ⇒ (")
	for i, c := range n.Schema {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(c.Name)
		b.WriteString(" ")
		b.WriteString(c.Type.String())
	}
	b.WriteString(")")
	return b.String()
}

// CollectPlan renders every node label with its id, for the page.
func CollectPlan(n *PlanNode) []map[string]any {
	var out []map[string]any
	var rec func(n *PlanNode, parent int)
	rec = func(n *PlanNode, parent int) {
		if n == nil {
			return
		}
		cols := make([]map[string]any, 0, len(n.Schema))
		for _, c := range n.Schema {
			cols = append(cols, map[string]any{
				"name": c.Name, "type": c.Type.String(),
				"qualifier": c.Qualifier, "nullable": c.Nullable,
			})
		}
		exprs := make([]string, 0, len(n.Exprs))
		for _, e := range n.Exprs {
			exprs = append(exprs, e.String())
		}
		out = append(out, map[string]any{
			"id": n.ID, "parent": parent, "op": string(n.Op),
			"label": n.label(), "table": n.Table,
			"columns": cols, "exprs": exprs,
			"origin": n.Origin, "rule": n.Rule,
			"children": childIDs(n),
		})
		for _, c := range n.Children {
			rec(c, n.ID)
		}
	}
	rec(n, 0)
	return out
}

func childIDs(n *PlanNode) []int {
	out := make([]int, 0, len(n.Children))
	for _, c := range n.Children {
		out = append(out, c.ID)
	}
	return out
}

// CTEInfo describes a bound common table expression for display.
type CTEInfo struct {
	Name string
	Cols []string
	Plan *PlanNode
	Refs int
}

// Plan is a fully bound, rewritable query plan.
type Plan struct {
	Root   *PlanNode
	Output []OutputCol
	MaxID  int
	// CTEInfo carries the CTE definitions for the GUI, keyed by name as
	// declared in the outermost WITH.
	CTEInfo map[string]*CTEInfo
	// RefCounts counts how many CTEScan nodes reference each bound CTE.
	RefCounts map[string]int
	// Warnings are non-fatal diagnostics (e.g. CNF skipped).
	Warnings []*Error
}

// NextID allocates a plan node ID.
type idAlloc struct{ n int }

func (a *idAlloc) next() int {
	a.n++
	return a.n
}

// ---------------------------------------------------------------------------
// Schema helpers
// ---------------------------------------------------------------------------

func schemaEqual(a, b []OutputCol) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i].Name, b[i].Name) ||
			!a[i].Type.Same(b[i].Type) ||
			!strings.EqualFold(a[i].Qualifier, b[i].Qualifier) ||
			a[i].Nullable != b[i].Nullable {
			return false
		}
	}
	return true
}

func schemaFingerprint(s []OutputCol) string {
	parts := make([]string, len(s))
	for i, c := range s {
		parts[i] = fmt.Sprintf("%s:%s@%s:%v", c.Name, c.Type.String(), c.Qualifier, c.Nullable)
	}
	return strings.Join(parts, ",")
}

// remapCols rewrites ColRefs at a given depth, using a mapping from old
// ordinal to new ordinal. A negative target means the column is gone, which
// is an internal error (rules must preserve referenced columns).
func remapExpr(e BExpr, depth int, m map[int]int) (BExpr, *Error) {
	var failure *Error
	walkExpr(e, func(x BExpr) {
		if c, ok := x.(*ColRef); ok && c.Depth == depth {
			nv, ok := m[c.Index]
			if !ok || nv < 0 {
				if failure == nil {
					failure = errAt(CodeInternal, c.Posn, "internal: rewrite dropped referenced column %s", c)
				}
			}
		}
	})
	if failure != nil {
		return e, failure
	}
	return mapExpr(e, func(x BExpr) BExpr {
		if c, ok := x.(*ColRef); ok && c.Depth == depth {
			n := *c
			n.Index = m[c.Index]
			return &n
		}
		return x
	}), nil
}

// shiftDepths adjusts ColRef depths, used when a correlated subtree is spliced
// into another block.
func shiftDepths(e BExpr, from, by int) BExpr {
	return mapExpr(e, func(x BExpr) BExpr {
		if c, ok := x.(*ColRef); ok {
			if (from >= 0 && c.Depth >= from) || from < 0 {
				n := *c
				n.Depth += by
				return &n
			}
		}
		return x
	})
}

// clonePlan deep-copies a plan tree so that a rule can rewrite a CTE inlined
// twice without aliasing.
func clonePlan(n *PlanNode, ids *idAlloc) *PlanNode {
	if n == nil {
		return nil
	}
	c := *n
	c.ID = ids.next()
	c.Schema = append([]OutputCol(nil), n.Schema...)
	c.Exprs = append([]BExpr(nil), n.Exprs...)
	c.GroupBy = append([]BExpr(nil), n.GroupBy...)
	c.Aggs = append([]AggDef(nil), n.Aggs...)
	c.SortDir = append([]bool(nil), n.SortDir...)
	c.Columns = append([]string(nil), n.Columns...)
	c.Children = make([]*PlanNode, len(n.Children))
	for i, ch := range n.Children {
		c.Children[i] = clonePlan(ch, ids)
	}
	return &c
}
