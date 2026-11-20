package engine

import (
	"fmt"
	"sort"
	"strings"
)

// Row is one tuple of values.
type Row []Value

// Table is a materialized relation with named columns.
type Table struct {
	Name string
	Cols []string
	Rows []Row
}

// Dataset is the input to the interpreter.
type Dataset struct {
	Tables map[string]*Table
}

func NewDataset() *Dataset { return &Dataset{Tables: map[string]*Table{}} }

func (d *Dataset) Add(t *Table) { d.Tables[strings.ToLower(t.Name)] = t }

func (d *Dataset) Table(name string) (*Table, bool) {
	t, ok := d.Tables[strings.ToLower(name)]
	return t, ok
}

// Env is the evaluation environment: the current row plus enclosing rows for
// correlated references. outer is ordered outermost first.
type Env struct {
	Row   Row
	Outer []Row
}

func (e *Env) lookup(c *ColRef) (Value, *Error) {
	if c.Depth == 0 {
		if c.Index < 0 || c.Index >= len(e.Row) {
			return Value{}, errAt(CodeInternal, c.Posn, "internal: column index %d out of range", c.Index)
		}
		return e.Row[c.Index], nil
	}
	if c.Depth > len(e.Outer) {
		return Value{}, errAt(CodeInternal, c.Posn, "internal: correlation depth %d exceeds %d", c.Depth, len(e.Outer))
	}
	r := e.Outer[len(e.Outer)-c.Depth]
	if c.Index < 0 || c.Index >= len(r) {
		return Value{}, errAt(CodeInternal, c.Posn, "internal: correlated column index %d out of range", c.Index)
	}
	return r[c.Index], nil
}

// Evaluator runs plan trees naively. It is the reference semantics: every
// rewrite must yield identical rows (as a multiset) under it.
type Evaluator struct {
	reg *Registry
	ds  *Dataset
	// Steps limits work so a pathological generated query cannot hang the
	// harness. Zero means unlimited.
	maxRows int
}

func NewEvaluator(ds *Dataset, reg *Registry) *Evaluator {
	return &Evaluator{reg: reg, ds: ds}
}

func (ev *Evaluator) Run(n *PlanNode) ([]Row, *Error) {
	return ev.run(n, nil)
}

func (ev *Evaluator) run(n *PlanNode, outer []Row) ([]Row, *Error) {
	if n == nil {
		return nil, errAt(CodeInternal, Pos{}, "internal: nil plan node")
	}
	switch n.Op {
	case OpValues:
		rows := make([]Row, len(n.Rows))
		for i, r := range n.Rows {
			rows[i] = Row(r)
		}
		return rows, nil
	case OpEmpty:
		return nil, nil
	case OpTableScan:
		t, ok := ev.ds.Table(n.Table)
		if !ok {
			return nil, errAt(CodeInternal, Pos{}, "internal: table %q missing from dataset", n.Table)
		}
		if len(t.Cols) != len(n.Schema) {
			return nil, errAt(CodeInternal, Pos{}, "internal: table %q has %d columns, plan expects %d", n.Table, len(t.Cols), len(n.Schema))
		}
		out := make([]Row, len(t.Rows))
		for i, r := range t.Rows {
			rr := make(Row, len(r))
			for j := range r {
				rr[j] = coerceToType(r[j], n.Schema[j].Type)
			}
			out[i] = rr
		}
		return out, nil
	case OpCTEScan:
		if n.CTEPlan == nil {
			return nil, errAt(CodeInternal, Pos{}, "internal: CTE %q has no plan", n.CTEName)
		}
		rows, e := ev.run(n.CTEPlan, outer)
		if e != nil {
			return nil, e
		}
		return renameRows(rows, n.Schema), nil
	case OpRename:
		if len(n.Children) != 1 {
			return nil, errAt(CodeInternal, Pos{}, "internal: Rename expects one child")
		}
		rows, e := ev.run(n.Children[0], outer)
		if e != nil {
			return nil, e
		}
		if len(n.Children[0].Schema) != len(n.Schema) {
			return nil, errAt(CodeInternal, Pos{}, "internal: Rename schema mismatch")
		}
		return rows, nil
	case OpFilter:
		rows, e := ev.run(n.Children[0], outer)
		if e != nil {
			return nil, e
		}
		out := make([]Row, 0, len(rows))
		for _, r := range rows {
			env := &Env{Row: r, Outer: outer}
			v, e := ev.eval(n.Exprs[0], env)
			if e != nil {
				return nil, e
			}
			if v.Null || v.Kind != KBool {
				continue
			}
			if v.Bool {
				out = append(out, r)
			}
		}
		return out, nil
	case OpProject:
		rows, e := ev.run(n.Children[0], outer)
		if e != nil {
			return nil, e
		}
		out := make([]Row, len(rows))
		for i, r := range rows {
			env := &Env{Row: r, Outer: outer}
			nr := make(Row, len(n.Exprs))
			for j, x := range n.Exprs {
				v, e := ev.eval(x, env)
				if e != nil {
					return nil, e
				}
				nr[j] = coerceToType(v, n.Schema[j].Type)
			}
			out[i] = nr
		}
		return out, nil
	case OpJoin:
		return ev.runJoin(n, outer)
	case OpAggregate:
		return ev.runAggregate(n, outer)
	case OpSort:
		rows, e := ev.run(n.Children[0], outer)
		if e != nil {
			return nil, e
		}
		keyed := make([]struct {
			row  Row
			keys []Value
		}, len(rows))
		for i, r := range rows {
			env := &Env{Row: r, Outer: outer}
			kv := make([]Value, len(n.Exprs))
			for j, x := range n.Exprs {
				v, e := ev.eval(x, env)
				if e != nil {
					return nil, e
				}
				kv[j] = v
			}
			keyed[i] = struct {
				row  Row
				keys []Value
			}{r, kv}
		}
		sort.SliceStable(keyed, func(a, b int) bool {
			for j := range n.Exprs {
				c := cmpForSort(keyed[a].keys[j], keyed[b].keys[j])
				if c == 0 {
					continue
				}
				if n.SortDir[j] {
					return c > 0
				}
				return c < 0
			}
			return false
		})
		out := make([]Row, len(rows))
		for i := range keyed {
			out[i] = keyed[i].row
		}
		return out, nil
	case OpLimit:
		rows, e := ev.run(n.Children[0], outer)
		if e != nil {
			return nil, e
		}
		start := int(n.Offset)
		if start > len(rows) {
			start = len(rows)
		}
		end := len(rows)
		if n.Limit > 0 && start+int(n.Limit) < end {
			end = start + int(n.Limit)
		}
		return rows[start:end], nil
	case OpDistinct:
		rows, e := ev.run(n.Children[0], outer)
		if e != nil {
			return nil, e
		}
		seen := map[string]bool{}
		out := rows[:0]
		for _, r := range rows {
			k := rowKey(r)
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, r)
		}
		return out, nil
	case OpSetOp:
		l, e := ev.run(n.Children[0], outer)
		if e != nil {
			return nil, e
		}
		r, e := ev.run(n.Children[1], outer)
		if e != nil {
			return nil, e
		}
		return setOpRows(n.SetOp, n.SetAll, l, r)
	}
	return nil, errAt(CodeInternal, Pos{}, "internal: unhandled plan op %s", n.Op)
}

func renameRows(rows []Row, schema []OutputCol) []Row {
	if len(schema) == 0 {
		return rows
	}
	out := make([]Row, len(rows))
	for i, r := range rows {
		nr := make(Row, len(r))
		for j := range r {
			nr[j] = coerceToType(r[j], schema[j].Type)
		}
		out[i] = nr
	}
	return out
}

func (ev *Evaluator) runJoin(n *PlanNode, outer []Row) ([]Row, *Error) {
	l, e := ev.run(n.Children[0], outer)
	if e != nil {
		return nil, e
	}
	// A lateral / correlated right side depends on the current left row: it
	// must be evaluated once per left row with that row appended to the outer
	// context. An uncorrelated right side is evaluated once, as usual.
	dependent := planIsDependent(n.Children[1], 1)
	var r []Row
	if !dependent {
		var err *Error
		r, err = ev.run(n.Children[1], outer)
		if err != nil {
			return nil, err
		}
	}
	var out []Row
	// rightMatched tracks unmatched right rows for RIGHT/FULL joins. A
	// dependent right side is re-evaluated per left row, so its rows are not
	// stable identities; a RIGHT join with a correlated right side falls back
	// to treating every evaluated right row as matched.
	rightMatched := make([]bool, len(r))
	if dependent && (n.Join.Kind == JoinRight || n.Join.Kind == JoinFull) {
		return nil, errAt(CodeEval, Pos{}, "correlated right side is not supported under %s", n.Join.Kind)
	}
	for _, lr := range l {
		rows := r
		ctx := outer
		if dependent {
			ctx = appendOuterRows(outer, lr)
			var err *Error
			rows, err = ev.run(n.Children[1], ctx)
			if err != nil {
				return nil, err
			}
			if rightMatched != nil && len(rows) > len(rightMatched) {
				rightMatched = append(rightMatched, make([]bool, len(rows)-len(rightMatched))...)
			}
		}
		matched := false
		for ri, rr := range rows {
			combined := make(Row, 0, len(lr)+len(rr))
			combined = append(combined, lr...)
			combined = append(combined, rr...)
			ok, e := ev.joinMatches(n, combined, ctx)
			if e != nil {
				return nil, e
			}
			if !ok {
				continue
			}
			matched = true
			if ri < len(rightMatched) {
				rightMatched[ri] = true
			}
			out = append(out, combined)
			if ev.maxRows > 0 && len(out) > ev.maxRows {
				return nil, errAt(CodeEval, Pos{}, "execution exceeded %d rows", ev.maxRows)
			}
		}
		if !matched && (n.Join.Kind == JoinLeft || n.Join.Kind == JoinFull) {
			combined := make(Row, len(lr)+len(n.Children[1].Schema))
			copy(combined, lr)
			for i := len(lr); i < len(combined); i++ {
				combined[i] = NullValue(n.Children[1].Schema[i-len(lr)].Type.Kind)
			}
			out = append(out, combined)
		}
	}
	// Only an outer join emits unmatched rows from either side. (An inner
	// join with no matches has already produced nothing.)
	if !dependent && (n.Join.Kind == JoinRight || n.Join.Kind == JoinFull) {
		for ri, rr := range r {
			if rightMatched[ri] {
				continue
			}
			combined := make(Row, len(n.Children[0].Schema)+len(rr))
			for i := 0; i < len(n.Children[0].Schema); i++ {
				combined[i] = NullValue(n.Children[0].Schema[i].Type.Kind)
			}
			copy(combined[len(n.Children[0].Schema):], rr)
			out = append(out, combined)
		}
	}
	return out, nil
}

// planIsDependent reports whether the plan tree contains a column reference at
// the given depth (or deeper), i.e. it reads a row from an enclosing query
// block. Correlated/lateral substrees must be evaluated per outer row.
func planIsDependent(n *PlanNode, minDepth int) bool {
	if n == nil {
		return false
	}
	found := false
	check := func(e BExpr) {
		if found || e == nil {
			return
		}
		walkExpr(e, func(x BExpr) {
			if c, ok := x.(*ColRef); ok && c.Depth >= minDepth {
				found = true
			}
		})
		walkSubqueryPlans(e, func(p *PlanNode) {
			if planIsDependent(p, minDepth) {
				found = true
			}
		})
	}
	for _, e := range n.Exprs {
		check(e)
	}
	for _, g := range n.GroupBy {
		check(g)
	}
	for _, a := range n.Aggs {
		check(a.Arg)
	}
	for _, c := range n.Children {
		if planIsDependent(c, minDepth) {
			return true
		}
	}
	return found
}

// appendOuterRows appends r to outer, producing the context for evaluating a
// substree that may reference one more enclosing row.
func appendOuterRows(outer []Row, r Row) []Row {
	out := make([]Row, len(outer)+1)
	copy(out, outer)
	out[len(outer)] = r
	return out
}

func (ev *Evaluator) joinMatches(n *PlanNode, combined Row, outer []Row) (bool, *Error) {
	if n.Join.Kind == JoinCross || len(n.Exprs) == 0 {
		return true, nil
	}
	v, e := ev.eval(n.Exprs[0], &Env{Row: combined, Outer: outer})
	if e != nil {
		return false, e
	}
	return !v.Null && v.Kind == KBool && v.Bool, nil
}

func (ev *Evaluator) runAggregate(n *PlanNode, outer []Row) ([]Row, *Error) {
	rows, e := ev.run(n.Children[0], outer)
	if e != nil {
		return nil, e
	}
	type group struct {
		keys []Value
		rows []Row
	}
	var groups []*group
	index := map[string]*group{}
	for _, r := range rows {
		env := &Env{Row: r, Outer: outer}
		kv := make([]Value, len(n.GroupBy))
		for j, g := range n.GroupBy {
			v, e := ev.eval(g, env)
			if e != nil {
				return nil, e
			}
			kv[j] = v
		}
		k := rowKey(kv)
		g, ok := index[k]
		if !ok {
			g = &group{keys: kv}
			index[k] = g
			groups = append(groups, g)
		}
		g.rows = append(g.rows, r)
	}
	if len(n.GroupBy) == 0 && len(groups) == 0 {
		// Aggregates without GROUP BY always produce one row.
		groups = append(groups, &group{})
	}
	out := make([]Row, 0, len(groups))
	for _, g := range groups {
		row := make(Row, 0, len(n.GroupBy)+len(n.Aggs))
		for _, k := range g.keys {
			row = append(row, k)
		}
		for i := range n.Aggs {
			v, e := ev.evalAgg(&n.Aggs[i], g.rows, outer)
			if e != nil {
				return nil, e
			}
			row = append(row, coerceToType(v, n.Aggs[i].Type))
		}
		out = append(out, row)
	}
	return out, nil
}

func (ev *Evaluator) evalAgg(a *AggDef, rows []Row, outer []Row) (Value, *Error) {
	name := strings.ToLower(a.Fn)
	if name == "count" {
		if a.Distinct {
			seen := map[string]bool{}
			n := int64(0)
			for _, r := range rows {
				v, e := ev.aggArg(a, r, outer)
				if e != nil {
					return Value{}, e
				}
				if v.Null {
					continue
				}
				k := valueKey(v)
				if !seen[k] {
					seen[k] = true
					n++
				}
			}
			return Value{Kind: KInt, Int: n}, nil
		}
		if a.Star || a.Arg == nil {
			return Value{Kind: KInt, Int: int64(len(rows))}, nil
		}
		n := int64(0)
		for _, r := range rows {
			v, e := ev.aggArg(a, r, outer)
			if e != nil {
				return Value{}, e
			}
			if !v.Null {
				n++
			}
		}
		return Value{Kind: KInt, Int: n}, nil
	}
	var vals []Value
	seen := map[string]bool{}
	for _, r := range rows {
		v, e := ev.aggArg(a, r, outer)
		if e != nil {
			return Value{}, e
		}
		if v.Null {
			continue
		}
		if a.Distinct {
			k := valueKey(v)
			if seen[k] {
				continue
			}
			seen[k] = true
		}
		vals = append(vals, v)
	}
	switch name {
	case "min", "max":
		if len(vals) == 0 {
			return NullValue(a.Type.Kind), nil
		}
		best := vals[0]
		for _, v := range vals[1:] {
			c, ok := compareValues(v, best)
			if !ok {
				continue
			}
			if (name == "min" && c < 0) || (name == "max" && c > 0) {
				best = v
			}
		}
		return coerceToType(best, a.Type), nil
	case "sum":
		if len(vals) == 0 {
			return NullValue(a.Type.Kind), nil
		}
		if a.Type.Kind == KInt {
			var s int64
			for _, v := range vals {
				x := v.Int
				ns := s + x
				if (x > 0 && ns < s) || (x < 0 && ns > s) {
					return Value{}, errAt(CodeEval, a.Pos0(), "sum overflow")
				}
				s = ns
			}
			return Value{Kind: KInt, Int: s}, nil
		}
		scale := 0
		for _, v := range vals {
			if toDec(v).Scale > scale {
				scale = toDec(v).Scale
			}
		}
		var acc int64
		for _, v := range vals {
			d := toDec(v)
			r, ok := d.rescale(scale)
			if !ok {
				return Value{}, errAt(CodeEval, a.Pos0(), "sum overflow")
			}
			ns := acc + r.Unscaled
			if (r.Unscaled > 0 && ns < acc) || (r.Unscaled < 0 && ns > acc) {
				return Value{}, errAt(CodeEval, a.Pos0(), "sum overflow")
			}
			acc = ns
		}
		out, ok := (Decimal{acc, scale}).rescale(a.Type.Scale)
		if !ok {
			return Value{}, errAt(CodeEval, a.Pos0(), "sum overflow")
		}
		return Value{Kind: KDec, Dec: out}, nil
	case "avg":
		if len(vals) == 0 {
			return NullValue(a.Type.Kind), nil
		}
		scale := 0
		for _, v := range vals {
			if toDec(v).Scale > scale {
				scale = toDec(v).Scale
			}
		}
		var acc int64
		for _, v := range vals {
			d := toDec(v)
			r, ok := d.rescale(scale)
			if !ok {
				return Value{}, errAt(CodeEval, a.Pos0(), "avg overflow")
			}
			acc += r.Unscaled
		}
		out, ok := DecDiv(Decimal{acc, scale}, Decimal{int64(len(vals)), 0}, a.Type.Scale)
		if !ok {
			return Value{}, errAt(CodeEval, a.Pos0(), "avg overflow")
		}
		return Value{Kind: KDec, Dec: out}, nil
	}
	return Value{}, errAt(CodeInternal, a.Pos0(), "internal: unknown aggregate %s", a.Fn)
}

func (a *AggDef) Pos0() Pos {
	if a.Arg != nil {
		return a.Arg.Pos()
	}
	return Pos{}
}

func (ev *Evaluator) aggArg(a *AggDef, r Row, outer []Row) (Value, *Error) {
	if a.Star || a.Arg == nil {
		return Value{Kind: KInt, Int: 1}, nil
	}
	return ev.eval(a.Arg, &Env{Row: r, Outer: outer})
}

// ---------------------------------------------------------------------------
// Expression evaluation
// ---------------------------------------------------------------------------

func (ev *Evaluator) eval(e BExpr, env *Env) (Value, *Error) {
	switch x := e.(type) {
	case *Lit:
		return coerceToType(x.V, x.T), nil
	case *ColRef:
		v, err := env.lookup(x)
		if err != nil {
			return Value{}, err
		}
		return coerceToType(v, x.T), nil
	case *AggRef:
		if x.Index < 0 || x.Index >= len(env.Row) {
			return Value{}, errAt(CodeInternal, x.Posn, "internal: aggregate slot %d out of range", x.Index)
		}
		return coerceToType(env.Row[x.Index], x.T), nil
	case *Un:
		v, err := ev.eval(x.X, env)
		if err != nil {
			return Value{}, err
		}
		if x.Op == "not" {
			if v.Null {
				return Value{Kind: KBool, Null: true}, nil
			}
			if v.Kind != KBool {
				return Value{}, errAt(CodeEval, x.Posn, "NOT operand is not boolean")
			}
			return Value{Kind: KBool, Bool: !v.Bool}, nil
		}
		if v.Null {
			return NullValue(x.T.Kind), nil
		}
		switch v.Kind {
		case KInt:
			if v.Int == -1<<63 {
				return Value{}, errAt(CodeEval, x.Posn, "integer negation overflow")
			}
			return Value{Kind: KInt, Int: -v.Int}, nil
		case KDec:
			return Value{Kind: KDec, Dec: Decimal{-v.Dec.Unscaled, v.Dec.Scale}}, nil
		}
		return Value{}, errAt(CodeEval, x.Posn, "cannot negate %s", v.Kind)
	case *Bin:
		if x.Op == "and" || x.Op == "or" {
			return ev.evalLogic(x, env)
		}
		l, err := ev.eval(x.L, env)
		if err != nil {
			return Value{}, err
		}
		r, err := ev.eval(x.R, env)
		if err != nil {
			return Value{}, err
		}
		return ev.evalBinary(x, l, r)
	case *Fn:
		args := make([]Value, len(x.Args))
		for i, a := range x.Args {
			v, err := ev.eval(a, env)
			if err != nil {
				return Value{}, err
			}
			args[i] = v
		}
		return ev.reg.EvalCall(x.Posn, x.Name, args)
	case *Cast:
		v, err := ev.eval(x.X, env)
		if err != nil {
			return Value{}, err
		}
		return castValue(v, x.T, x.Posn)
	case *Case:
		var operand Value
		if x.Operand != nil {
			v, err := ev.eval(x.Operand, env)
			if err != nil {
				return Value{}, err
			}
			operand = v
		}
		for _, w := range x.Whens {
			v, err := ev.eval(w.Cond, env)
			if err != nil {
				return Value{}, err
			}
			hit := false
			if x.Operand != nil {
				eq, known := operand.Equal(v)
				hit = known && eq
			} else {
				hit = !v.Null && v.Kind == KBool && v.Bool
			}
			if hit {
				tv, err := ev.eval(w.Then, env)
				if err != nil {
					return Value{}, err
				}
				return coerceToType(tv, x.T), nil
			}
		}
		if x.Else != nil {
			v, err := ev.eval(x.Else, env)
			if err != nil {
				return Value{}, err
			}
			return coerceToType(v, x.T), nil
		}
		return NullValue(x.T.Kind), nil
	case *IsNull:
		v, err := ev.eval(x.X, env)
		if err != nil {
			return Value{}, err
		}
		isNull := v.Null
		if x.Not {
			isNull = !isNull
		}
		return Value{Kind: KBool, Bool: isNull}, nil
	case *Between:
		xv, err := ev.eval(x.X, env)
		if err != nil {
			return Value{}, err
		}
		lo, err := ev.eval(x.Lo, env)
		if err != nil {
			return Value{}, err
		}
		hi, err := ev.eval(x.Hi, env)
		if err != nil {
			return Value{}, err
		}
		res, known := evalBetween(xv, lo, hi)
		if !known {
			return Value{Kind: KBool, Null: true}, nil
		}
		if x.Not {
			res = !res
		}
		return Value{Kind: KBool, Bool: res}, nil
	case *InList:
		xv, err := ev.eval(x.X, env)
		if err != nil {
			return Value{}, err
		}
		res, known, err := ev.evalInList(xv, x.List, env)
		if err != nil {
			return Value{}, err
		}
		if !known {
			return Value{Kind: KBool, Null: true}, nil
		}
		if x.Not {
			res = !res
		}
		return Value{Kind: KBool, Bool: res}, nil
	case *InSub:
		// Three-valued membership: TRUE if a match exists, UNKNOWN if there
		// is no match but the list contains NULL (or the probe is NULL),
		// otherwise FALSE. NOT IN negates the whole three-valued result.
		xv, err := ev.eval(x.X, env)
		if err != nil {
			return Value{}, err
		}
		rows, err := ev.run(x.Plan, appendOuter(env))
		if err != nil {
			return Value{}, err
		}
		if xv.Null {
			return Value{Kind: KBool, Null: true}, nil
		}
		sawNull := false
		for _, r := range rows {
			if len(r) == 0 {
				continue
			}
			if r[0].Null {
				sawNull = true
				continue
			}
			if eq, k := xv.Equal(r[0]); k && eq {
				if x.Not {
					return Value{Kind: KBool, Bool: false}, nil
				}
				return Value{Kind: KBool, Bool: true}, nil
			}
		}
		if sawNull {
			return Value{Kind: KBool, Null: true}, nil
		}
		return Value{Kind: KBool, Bool: x.Not}, nil
	case *Like:
		xv, err := ev.eval(x.X, env)
		if err != nil {
			return Value{}, err
		}
		pv, err := ev.eval(x.Pat, env)
		if err != nil {
			return Value{}, err
		}
		if xv.Null || pv.Null {
			return Value{Kind: KBool, Null: true}, nil
		}
		res, e := likeMatch(xv.Str, pv.Str)
		if e != nil {
			return Value{}, errAt(CodeEval, x.Posn, "LIKE: %v", e)
		}
		if x.Not {
			res = !res
		}
		return Value{Kind: KBool, Bool: res}, nil
	case *ScalarSub:
		rows, err := ev.run(x.Plan, appendOuter(env))
		if err != nil {
			return Value{}, err
		}
		if len(rows) == 0 {
			return NullValue(x.T.Kind), nil
		}
		if len(rows) > 1 {
			return Value{}, errAt(CodeCardinality, x.Posn, "scalar subquery returned %d rows; expected at most one", len(rows))
		}
		if len(rows[0]) != 1 {
			return Value{}, errAt(CodeCardinality, x.Posn, "scalar subquery returned %d columns", len(rows[0]))
		}
		return coerceToType(rows[0][0], x.T), nil
	case *Exists:
		rows, err := ev.run(x.Plan, appendOuter(env))
		if err != nil {
			return Value{}, err
		}
		return Value{Kind: KBool, Bool: len(rows) > 0}, nil
	}
	return Value{}, errAt(CodeInternal, e.Pos(), "internal: cannot evaluate %T", e)
}

func appendOuter(env *Env) []Row {
	out := make([]Row, len(env.Outer)+1)
	copy(out, env.Outer)
	out[len(env.Outer)] = env.Row
	return out
}

func (ev *Evaluator) evalLogic(x *Bin, env *Env) (Value, *Error) {
	l, err := ev.eval(x.L, env)
	if err != nil {
		return Value{}, err
	}
	if x.Op == "and" {
		if !l.Null && l.Kind == KBool && !l.Bool {
			return Value{Kind: KBool, Bool: false}, nil
		}
		r, err := ev.eval(x.R, env)
		if err != nil {
			return Value{}, err
		}
		if !r.Null && r.Kind == KBool && !r.Bool {
			return Value{Kind: KBool, Bool: false}, nil
		}
		if l.Null || r.Null {
			return Value{Kind: KBool, Null: true}, nil
		}
		return Value{Kind: KBool, Bool: true}, nil
	}
	// OR
	if !l.Null && l.Kind == KBool && l.Bool {
		return Value{Kind: KBool, Bool: true}, nil
	}
	r, err := ev.eval(x.R, env)
	if err != nil {
		return Value{}, err
	}
	if !r.Null && r.Kind == KBool && r.Bool {
		return Value{Kind: KBool, Bool: true}, nil
	}
	if l.Null || r.Null {
		return Value{Kind: KBool, Null: true}, nil
	}
	return Value{Kind: KBool, Bool: false}, nil
}

func (ev *Evaluator) evalBinary(x *Bin, l, r Value) (Value, *Error) {
	switch x.Op {
	case "eq", "ne", "lt", "le", "gt", "ge":
		if l.Null || r.Null {
			return Value{Kind: KBool, Null: true}, nil
		}
		c, ok := compareValues(l, r)
		if !ok {
			return Value{}, errAt(CodeEval, x.Posn, "cannot compare %s with %s", l.Kind, r.Kind)
		}
		var b bool
		switch x.Op {
		case "eq":
			b = c == 0
		case "ne":
			b = c != 0
		case "lt":
			b = c < 0
		case "le":
			b = c <= 0
		case "gt":
			b = c > 0
		case "ge":
			b = c >= 0
		}
		return Value{Kind: KBool, Bool: b}, nil
	}
	if l.Null || r.Null {
		return NullValue(x.T.Kind), nil
	}
	if x.T.Kind == KInt {
		return evalIntArith(x, l, r)
	}
	return evalDecArith(x, l, r)
}

func evalIntArith(x *Bin, l, r Value) (Value, *Error) {
	a, b := l.Int, r.Int
	switch x.Op {
	case "add":
		s := a + b
		if (b > 0 && s < a) || (b < 0 && s > a) {
			return Value{}, errAt(CodeEval, x.Posn, "integer overflow in +")
		}
		return Value{Kind: KInt, Int: s}, nil
	case "sub":
		s := a - b
		if (b < 0 && s < a) || (b > 0 && s > a) {
			return Value{}, errAt(CodeEval, x.Posn, "integer overflow in -")
		}
		return Value{Kind: KInt, Int: s}, nil
	case "mul":
		if a != 0 && (b > 1<<62/abs64(a) || b < -(1<<62)/abs64(a)) {
			return Value{}, errAt(CodeEval, x.Posn, "integer overflow in *")
		}
		return Value{Kind: KInt, Int: a * b}, nil
	case "div":
		if b == 0 {
			return Value{}, errAt(CodeEval, x.Posn, "division by zero")
		}
		if a == -1<<63 && b == -1 {
			return Value{}, errAt(CodeEval, x.Posn, "integer overflow in /")
		}
		return Value{Kind: KInt, Int: a / b}, nil
	case "mod":
		if b == 0 {
			return Value{}, errAt(CodeEval, x.Posn, "division by zero")
		}
		return Value{Kind: KInt, Int: a % b}, nil
	}
	return Value{}, errAt(CodeInternal, x.Posn, "internal: bad int op %s", x.Op)
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

func evalDecArith(x *Bin, l, r Value) (Value, *Error) {
	a, b := toDec(l), toDec(r)
	scale := x.T.Scale
	var d Decimal
	var ok bool
	switch x.Op {
	case "add":
		d, ok = DecAdd(a, b, scale)
	case "sub":
		d, ok = DecSub(a, b, scale)
	case "mul":
		d, ok = DecMul(a, b, scale)
	case "div":
		if b.Unscaled == 0 {
			return Value{}, errAt(CodeEval, x.Posn, "division by zero")
		}
		d, ok = DecDiv(a, b, scale)
	default:
		return Value{}, errAt(CodeInternal, x.Posn, "internal: bad decimal op %s", x.Op)
	}
	if !ok {
		return Value{}, errAt(CodeEval, x.Posn, "fixed-point overflow or scale loss in %s", x.Op)
	}
	return Value{Kind: KDec, Dec: d}, nil
}

func evalBetween(x, lo, hi Value) (bool, bool) {
	if x.Null || lo.Null || hi.Null {
		return false, false
	}
	c1, ok1 := compareValues(x, lo)
	c2, ok2 := compareValues(x, hi)
	if !ok1 || !ok2 {
		return false, false
	}
	return c1 >= 0 && c2 <= 0, true
}

func (ev *Evaluator) evalInList(x Value, list []BExpr, env *Env) (bool, bool, *Error) {
	if x.Null {
		return false, false, nil
	}
	sawNull := false
	for _, it := range list {
		v, err := ev.eval(it, env)
		if err != nil {
			return false, false, err
		}
		if v.Null {
			sawNull = true
			continue
		}
		eq, known := x.Equal(v)
		if known && eq {
			return true, true, nil
		}
		_ = known
	}
	if sawNull {
		return false, false, nil
	}
	return false, true, nil
}

func likeMatch(s, pat string) (bool, error) {
	sr, pr := []rune(s), []rune(pat)
	var rec func(i, j int) (bool, error)
	rec = func(i, j int) (bool, error) {
		for j < len(pr) {
			switch pr[j] {
			case '%':
				for k := i; k <= len(sr); k++ {
					ok, err := rec(k, j+1)
					if err != nil {
						return false, err
					}
					if ok {
						return true, nil
					}
				}
				return false, nil
			case '_':
				if i >= len(sr) {
					return false, nil
				}
				i++
				j++
			default:
				if i >= len(sr) || sr[i] != pr[j] {
					return false, nil
				}
				i++
				j++
			}
		}
		return i == len(sr), nil
	}
	return rec(0, 0)
}

// ---------------------------------------------------------------------------
// Casts and coercion
// ---------------------------------------------------------------------------

func coerceToType(v Value, t Type) Value {
	if v.Null || t.Kind == KNull || t.Kind == KInvalid {
		if v.Null && t.Kind != KNull && t.Kind != KInvalid {
			return NullValue(t.Kind)
		}
		return v
	}
	switch t.Kind {
	case KDec:
		if v.Kind == KInt {
			if d, ok := (Decimal{v.Int, 0}).rescale(t.Scale); ok {
				return Value{Kind: KDec, Dec: d}
			}
			return v
		}
		if v.Kind == KDec {
			if d, ok := v.Dec.rescale(t.Scale); ok {
				return Value{Kind: KDec, Dec: d}
			}
		}
	case KInt:
		if v.Kind == KInt {
			return v
		}
	case KClock:
		if v.Kind == KClock {
			return v
		}
	case KDate:
		if v.Kind == KDate {
			return v
		}
	}
	return v
}

func castValue(v Value, t Type, p Pos) (Value, *Error) {
	if v.Null {
		return NullValue(t.Kind), nil
	}
	switch t.Kind {
	case KInt:
		switch v.Kind {
		case KInt:
			return v, nil
		case KDec:
			d, ok := v.Dec.rescale(0)
			if !ok {
				return Value{}, errAt(CodeEval, p, "cast overflow")
			}
			return Value{Kind: KInt, Int: d.Unscaled}, nil
		case KString:
			n, e := parseIntStrict(v.Str)
			if e != nil {
				return Value{}, errAt(CodeEval, p, "cannot cast %q to int", v.Str)
			}
			return Value{Kind: KInt, Int: n}, nil
		case KBool:
			if v.Bool {
				return Value{Kind: KInt, Int: 1}, nil
			}
			return Value{Kind: KInt, Int: 0}, nil
		case KClock:
			return Value{Kind: KInt, Int: int64(v.Clock)}, nil
		}
	case KDec:
		switch v.Kind {
		case KDec:
			d, ok := v.Dec.rescale(t.Scale)
			if !ok {
				return Value{}, errAt(CodeEval, p, "cast overflow")
			}
			return Value{Kind: KDec, Dec: d}, nil
		case KInt:
			d, ok := (Decimal{v.Int, 0}).rescale(t.Scale)
			if !ok {
				return Value{}, errAt(CodeEval, p, "cast overflow")
			}
			return Value{Kind: KDec, Dec: d}, nil
		case KString:
			d, _, ok := ParseDecimal(v.Str)
			if !ok {
				return Value{}, errAt(CodeEval, p, "cannot cast %q to decimal", v.Str)
			}
			dd, ok := d.rescale(t.Scale)
			if !ok {
				return Value{}, errAt(CodeEval, p, "cast overflow")
			}
			return Value{Kind: KDec, Dec: dd}, nil
		case KClock:
			d, ok := (Decimal{int64(v.Clock), 2}).rescale(t.Scale)
			if !ok {
				return Value{}, errAt(CodeEval, p, "cast overflow")
			}
			return Value{Kind: KDec, Dec: d}, nil
		}
	case KString:
		return Value{Kind: KString, Str: v.String()}, nil
	case KBool:
		switch v.Kind {
		case KBool:
			return v, nil
		case KInt:
			return Value{Kind: KBool, Bool: v.Int != 0}, nil
		case KString:
			switch strings.ToLower(v.Str) {
			case "true":
				return Value{Kind: KBool, Bool: true}, nil
			case "false":
				return Value{Kind: KBool, Bool: false}, nil
			}
			return Value{}, errAt(CodeEval, p, "cannot cast %q to bool", v.Str)
		}
	case KDate:
		if v.Kind == KString {
			d, ok := ParseDate(v.Str)
			if !ok {
				return Value{}, errAt(CodeEval, p, "cannot cast %q to date", v.Str)
			}
			return Value{Kind: KDate, Date: d}, nil
		}
	case KClock:
		switch v.Kind {
		case KString:
			tk, ok := ParseTick(v.Str)
			if !ok {
				return Value{}, errAt(CodeEval, p, "cannot cast %q to tick", v.Str)
			}
			return Value{Kind: KClock, Clock: tk}, nil
		case KInt:
			if v.Int < 0 || v.Int > TickMax {
				return Value{}, errAt(CodeEval, p, "tick %d out of range", v.Int)
			}
			return Value{Kind: KClock, Clock: int32(v.Int)}, nil
		case KDec:
			d, ok := v.Dec.rescale(2)
			if !ok || d.Unscaled < 0 || d.Unscaled > TickMax {
				return Value{}, errAt(CodeEval, p, "tick out of range")
			}
			return Value{Kind: KClock, Clock: int32(d.Unscaled)}, nil
		}
	}
	return Value{}, errAt(CodeEval, p, "cannot cast %s to %s", v.Kind, t)
}

func parseIntStrict(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	neg := false
	if s[0] == '-' || s[0] == '+' {
		neg = s[0] == '-'
		s = s[1:]
	}
	if !isAllDigits(s) {
		return 0, fmt.Errorf("not an integer")
	}
	var n int64
	for i := 0; i < len(s); i++ {
		d := int64(s[i] - '0')
		if n > (1<<62)/10 {
			return 0, fmt.Errorf("overflow")
		}
		n = n*10 + d
	}
	if neg {
		n = -n
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Keys and sorting
// ---------------------------------------------------------------------------

func valueKey(v Value) string {
	if v.Null {
		return "N"
	}
	switch v.Kind {
	case KInt:
		return "i" + fmt.Sprint(v.Int)
	case KDec:
		d, ok := v.Dec.rescale(10)
		if !ok {
			return "d" + v.Dec.String()
		}
		return "d" + fmt.Sprint(d.Unscaled)
	case KString:
		return "s" + v.Str
	case KBool:
		return "b" + fmt.Sprint(v.Bool)
	case KDate:
		return "D" + fmt.Sprint(v.Date)
	case KClock:
		return "t" + fmt.Sprint(v.Clock)
	}
	return "?"
}

func rowKey(r Row) string {
	var b strings.Builder
	for i, v := range r {
		if i > 0 {
			b.WriteByte('|')
		}
		b.WriteString(valueKey(v))
	}
	return b.String()
}

func cmpForSort(a, b Value) int {
	if a.Null && b.Null {
		return 0
	}
	// NULLs sort last regardless of direction (stable, documented).
	if a.Null {
		return 1
	}
	if b.Null {
		return -1
	}
	c, ok := compareValues(a, b)
	if !ok {
		return strings.Compare(valueKey(a), valueKey(b))
	}
	return c
}

func setOpRows(op string, all bool, l, r []Row) ([]Row, *Error) {
	key := func(r Row) string { return rowKey(r) }
	switch strings.ToUpper(op) {
	case "UNION":
		out := append([]Row{}, l...)
		out = append(out, r...)
		if all {
			return out, nil
		}
		seen := map[string]bool{}
		var ded []Row
		for _, row := range out {
			k := key(row)
			if seen[k] {
				continue
			}
			seen[k] = true
			ded = append(ded, row)
		}
		return ded, nil
	case "EXCEPT":
		rcount := map[string]int{}
		for _, row := range r {
			rcount[key(row)]++
		}
		var out []Row
		seen := map[string]bool{}
		for _, row := range l {
			k := key(row)
			if all {
				if rcount[k] > 0 {
					rcount[k]--
					continue
				}
				out = append(out, row)
				continue
			}
			if rcount[k] > 0 || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, row)
		}
		return out, nil
	case "INTERSECT":
		rcount := map[string]int{}
		for _, row := range r {
			rcount[key(row)]++
		}
		var out []Row
		seen := map[string]bool{}
		for _, row := range l {
			k := key(row)
			if rcount[k] <= 0 {
				continue
			}
			if !all {
				if seen[k] {
					continue
				}
				seen[k] = true
			} else {
				rcount[k]--
			}
			out = append(out, row)
		}
		return out, nil
	}
	return nil, errAt(CodeInternal, Pos{}, "internal: unknown set op %q", op)
}

// RowSetString renders rows deterministically for comparison.
func RowSetString(rows []Row) string {
	keys := make([]string, len(rows))
	for i, r := range rows {
		keys[i] = rowKey(r)
	}
	sort.Strings(keys)
	return strings.Join(keys, "\n")
}

// RowsEqual compares two result multisets exactly, including decimal values
// and NULLs.
func RowsEqual(a, b []Row) bool {
	return RowSetString(a) == RowSetString(b)
}
