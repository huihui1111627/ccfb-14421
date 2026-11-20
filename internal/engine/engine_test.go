package engine

import (
	"strings"
	"testing"
)

func analyze(t *testing.T, sql string) *Analysis {
	t.Helper()
	return NewEngine().Analyze(sql)
}

func TestParsePrecedence(t *testing.T) {
	cases := []struct{ in, want string }{
		{"SELECT 1 + 2 * 3", "SELECT 1 + 2 * 3"},
		{"SELECT (1 + 2) * 3", "SELECT (1 + 2) * 3"},
		{"SELECT 1 - 2 - 3", "SELECT 1 - 2 - 3"},
		{"SELECT 1 - (2 - 3)", "SELECT 1 - (2 - 3)"},
		{"SELECT NOT a = 1", "SELECT NOT a = 1"},
		{"SELECT a AND b OR c", "SELECT a AND b OR c"},
		{"SELECT a AND (b OR c)", "SELECT a AND (b OR c)"},
		{"SELECT -x * 2", "SELECT -x * 2"},
		{"SELECT 10 / 2 / 5", "SELECT 10 / 2 / 5"},
	}
	for _, c := range cases {
		pr := Parse(c.in)
		if len(pr.Errs) != 0 {
			t.Fatalf("%q: unexpected errors %v", c.in, pr.Errs)
		}
		got := pr.Stmt.String()
		if got != c.want {
			t.Errorf("%q: printed as %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMultipleSyntaxErrorsReported(t *testing.T) {
	cases := []struct {
		sql string
		// cols lists columns where an error must be reported, each within a
		// small window of the damaged token.
		cols []int
	}{
		{"SELECT lot_id, FROM lots WHERE stems > 0", []int{16}},
		{"SELECT stems FROM lots WHERE stems > 1 AND", []int{38}},
		{"SELECT a, FROM t WHERE b > AND c = ", []int{11, 28, 36}},
	}
	for _, c := range cases {
		a := analyze(t, c.sql)
		if len(a.Errors) < len(c.cols) {
			t.Errorf("%q: got %d errors %v, want at least %d", c.sql, len(a.Errors), a.Errors, len(c.cols))
			continue
		}
		for _, col := range c.cols {
			found := false
			for _, e := range a.Errors {
				if e.Line == 1 && e.Col >= col-1 && e.Col <= col+8 {
					found = true
				}
			}
			if !found {
				t.Errorf("%q: no error near column %d; got %v", c.sql, col, a.Errors)
			}
		}
	}
	// Errors on separate lines carry separate line numbers.
	a := analyze(t, "SELECT\n  a,\n  FROM t\nWHERE b > AND")
	if len(a.Errors) < 2 {
		t.Fatalf("want errors on two lines, got %v", a.Errors)
	}
	lines := map[int]bool{}
	for _, e := range a.Errors {
		lines[e.Line] = true
	}
	if !lines[3] || !lines[4] {
		t.Errorf("errors not spread over lines 3 and 4: %v", a.Errors)
	}
}

func TestErrorPositionsAreOneBasedRunes(t *testing.T) {
	a := analyze(t, "SELECT 花 FROM lots")
	if len(a.Errors) == 0 {
		t.Fatal("expected an unknown-column error")
	}
	e := a.Errors[0]
	if e.Line != 1 || e.Col != 8 {
		t.Errorf("position = %d:%d, want 1:8", e.Line, e.Col)
	}
}

func TestDecimalTyping(t *testing.T) {
	cases := []struct {
		expr string
		want string
	}{
		{"1 + 2", "int"},
		// Int operands are zero-scale decimals of precision 19, so a sum's
		// precision saturates at the 18-digit cap.
		{"1 + 2.50", "decimal(18,2)"},
		{"1.50 + 2.25", "decimal(4,2)"},
		{"1.50 * 2.25", "decimal(6,4)"},
		{"1.50 / 2.25", "decimal(18,6)"},
		{"stems + 1", "int"},
		{"stems + 1.5", "decimal(18,1)"},
		{"stems * 1.50", "decimal(18,2)"},
	}
	for _, c := range cases {
		a := analyze(t, "SELECT "+c.expr+" AS x FROM lots")
		if len(a.Errors) > 0 {
			t.Errorf("%q: unexpected errors %v", c.expr, a.Errors)
			continue
		}
		if len(a.Schema) != 1 || a.Schema[0].Type != c.want {
			t.Errorf("%q: type = %v, want %s", c.expr, a.Schema, c.want)
		}
	}
}

func TestClockConversionIsExplicit(t *testing.T) {
	for _, sql := range []string{
		"SELECT lot_id FROM lots WHERE base_tick = 95.00",
		"SELECT lot_id FROM lots WHERE sold_date = 1",
		"SELECT to_clock(sold_date) FROM lots",
		"SELECT tick_round(sold_date) FROM lots",
	} {
		a := analyze(t, sql)
		if len(a.Errors) == 0 {
			t.Errorf("%q: expected an error", sql)
			continue
		}
		if a.Errors[0].Code != "implicit-conversion" && a.Errors[0].Code != "arity" && a.Errors[0].Code != "type" {
			t.Errorf("%q: code = %s, want a conversion/type error", sql, a.Errors[0].Code)
		}
	}
	// The explicit form works.
	if a := analyze(t, "SELECT lot_id FROM lots WHERE base_tick = 95.00::tick"); len(a.Errors) > 0 {
		t.Errorf("explicit cast rejected: %v", a.Errors)
	}
}

func TestAmbiguityListsCandidates(t *testing.T) {
	a := analyze(t, "SELECT lot_id FROM lots l JOIN trades t ON t.lot_id = l.lot_id")
	if len(a.Errors) == 0 {
		t.Fatal("expected an ambiguity error")
	}
	e := a.Errors[0]
	if e.Code != "ambiguous" {
		t.Fatalf("code = %s, want ambiguous", e.Code)
	}
	joined := strings.Join(e.Hints, " ")
	if !strings.Contains(joined, "l.lot_id") || !strings.Contains(joined, "t.lot_id") {
		t.Errorf("hints do not list both candidates: %v", e.Hints)
	}
}

func TestCTEAndLateralBinding(t *testing.T) {
	for _, sql := range []string{
		"WITH a AS (SELECT lot_id FROM lots) SELECT lot_id FROM a",
		"SELECT l.lot_id, q.n FROM lots l JOIN LATERAL (SELECT count(*) AS n FROM trades t WHERE t.lot_id = l.lot_id) q ON q.n > 0",
		"SELECT l.lot_id FROM lots l WHERE EXISTS (SELECT 1 FROM trades t WHERE t.lot_id = l.lot_id)",
		"WITH RECURSIVE r AS (SELECT lot_id FROM lots) SELECT lot_id FROM r",
	} {
		if a := analyze(t, sql); len(a.Errors) > 0 {
			t.Errorf("%q: unexpected errors %v", sql, a.Errors)
		}
	}
}

func TestRewritePreservesSchema(t *testing.T) {
	sqls := []string{
		"SELECT l.lot_id, t.price_tick FROM lots l LEFT JOIN trades t ON t.lot_id = l.lot_id WHERE t.price_tick IS NOT NULL",
		"SELECT l.lot_id FROM lots l FULL JOIN trades t ON t.lot_id = l.lot_id WHERE t.stems IS NOT NULL AND l.stems IS NOT NULL",
		"WITH c AS (SELECT lot_id FROM lots) SELECT c.lot_id FROM c",
		"SELECT lot_id FROM lots WHERE 1 = 0",
		"SELECT lot_id FROM lots WHERE (stems > 1 OR grade = 'A')",
	}
	e := NewEngine()
	for _, sql := range sqls {
		pr := Parse(sql)
		if len(pr.Errs) > 0 {
			t.Fatalf("%q: parse errors %v", sql, pr.Errs)
		}
		plan, errs := BindOne(pr.Stmt, e.Cat, e.Reg)
		if len(errs) > 0 {
			t.Fatalf("%q: bind errors %v", sql, errs)
		}
		before := append([]OutputCol(nil), plan.Output...)
		rr := plan.RewritePlan(DefaultRules())
		if !schemaEqual(before, rr.Root.Schema) {
			t.Errorf("%q: schema changed\n before %v\n after  %v", sql, before, rr.Root.Schema)
		}
	}
}

func TestOuterToInnerOnlyWhenSound(t *testing.T) {
	sound := []string{
		"SELECT l.lot_id FROM lots l LEFT JOIN trades t ON t.lot_id = l.lot_id WHERE t.price_tick IS NOT NULL",
		"SELECT l.lot_id FROM lots l RIGHT JOIN trades t ON t.lot_id = l.lot_id WHERE l.stems > 0",
	}
	unsound := []string{
		// IS NULL keeps the NULL-extended rows: must stay a LEFT join.
		"SELECT l.lot_id FROM lots l LEFT JOIN trades t ON t.lot_id = l.lot_id WHERE t.price_tick IS NULL",
		// NOT(IS NOT NULL) also selects NULL-extended rows.
		"SELECT l.lot_id FROM lots l LEFT JOIN trades t ON t.lot_id = l.lot_id WHERE NOT (t.price_tick IS NOT NULL)",
		// A predicate only over the preserved side does not reject them.
		"SELECT l.lot_id FROM lots l LEFT JOIN trades t ON t.lot_id = l.lot_id WHERE l.stems > 0",
	}
	e := NewEngine()
	kindOf := func(sql string) JoinKind {
		pr := Parse(sql)
		plan, errs := BindOne(pr.Stmt, e.Cat, e.Reg)
		if len(errs) > 0 {
			t.Fatalf("%q: %v", sql, errs)
		}
		plan.RewritePlan(DefaultRules())
		var rec func(*PlanNode) (JoinKind, bool)
		rec = func(n *PlanNode) (JoinKind, bool) {
			if n.Op == OpJoin {
				return n.Join.Kind, true
			}
			for _, c := range n.Children {
				if k, ok := rec(c); ok {
					return k, true
				}
			}
			return 0, false
		}
		k, _ := rec(plan.Root)
		return k
	}
	for _, sql := range sound {
		if k := kindOf(sql); k != JoinInner {
			t.Errorf("sound case kept %s: %s", k, sql)
		}
	}
	for _, sql := range unsound {
		if k := kindOf(sql); k == JoinInner {
			t.Errorf("unsound case became INNER: %s", sql)
		}
	}
}

func TestCNFBudget(t *testing.T) {
	// 2^7 = 128 conjuncts exceeds the budget of 64: the rule must decline.
	var groups []string
	for i := 0; i < 7; i++ {
		groups = append(groups, "(stems > "+itoa(i)+" AND grade = 'A')")
	}
	sql := "SELECT lot_id FROM lots WHERE " + strings.Join(groups, " OR ")
	e := NewEngine()
	pr := Parse(sql)
	plan, errs := BindOne(pr.Stmt, e.Cat, e.Reg)
	if len(errs) > 0 {
		t.Fatalf("%v", errs)
	}
	rr := plan.RewritePlan(DefaultRules())
	if rr.Counts["cnf"] != 0 {
		t.Errorf("cnf applied %d times despite over-budget predicate", rr.Counts["cnf"])
	}
	found := false
	for _, w := range rr.Warnings {
		if strings.Contains(w.Msg, "CNF") {
			found = true
		}
	}
	if !found {
		t.Errorf("no CNF-budget warning recorded: %v", rr.Warnings)
	}
	// A modest predicate converts.
	sql2 := "SELECT lot_id FROM lots WHERE (stems > 1 AND grade = 'A') OR (stems > 2 AND grade = 'B')"
	pr2 := Parse(sql2)
	p2, _ := BindOne(pr2.Stmt, e.Cat, e.Reg)
	rr2 := p2.RewritePlan(DefaultRules())
	if rr2.Counts["cnf"] == 0 {
		t.Errorf("cnf did not fire on a modest predicate")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestInlineCTESingleUseOnly(t *testing.T) {
	e := NewEngine()
	once := "WITH c AS (SELECT lot_id FROM lots) SELECT c.lot_id FROM c"
	twice := "WITH c AS (SELECT lot_id FROM lots) SELECT a.lot_id FROM c a JOIN c b ON a.lot_id = b.lot_id"
	for sql, want := range map[string]bool{once: true, twice: false} {
		pr := Parse(sql)
		plan, errs := BindOne(pr.Stmt, e.Cat, e.Reg)
		if len(errs) > 0 {
			t.Fatalf("%q: %v", sql, errs)
		}
		rr := plan.RewritePlan(DefaultRules())
		got := rr.Counts["inline-cte"] > 0
		if got != want {
			t.Errorf("%q: inlined=%v, want %v", sql, got, want)
		}
	}
}

func TestConstantFoldSurvivesRewrite(t *testing.T) {
	e := NewEngine()
	sql := "SELECT lot_id FROM lots WHERE stems > 1 + 2 * 3 AND upper('a') = UPPER('a')"
	pr := Parse(sql)
	plan, errs := BindOne(pr.Stmt, e.Cat, e.Reg)
	if len(errs) > 0 {
		t.Fatalf("%v", errs)
	}
	rr := plan.RewritePlan(DefaultRules())
	if rr.Counts["constant-fold"] == 0 {
		t.Fatalf("constant folding did not fire")
	}
	after := FormatTree(rr.Root)
	if strings.Contains(after, "2 * 3") || strings.Contains(after, "UPPER('a')") {
		t.Errorf("unfolded constants remain in plan:\n%s", after)
	}
}

func TestFixedPointTerminates(t *testing.T) {
	e := NewEngine()
	sql := `WITH c AS (SELECT lot_id, stems FROM lots WHERE stems > 0)
SELECT c.lot_id FROM c LEFT JOIN trades t ON t.lot_id = c.lot_id
WHERE t.price_tick IS NOT NULL AND (2 + 2 = 4) AND (c.stems > 1 OR c.stems < 100)`
	pr := Parse(sql)
	plan, errs := BindOne(pr.Stmt, e.Cat, e.Reg)
	if len(errs) > 0 {
		t.Fatalf("%v", errs)
	}
	rr := plan.RewritePlan(DefaultRules())
	if rr.Passes >= maxPasses {
		t.Fatalf("rewrite did not converge in %d passes", rr.Passes)
	}
	if len(rr.Snapshots) < 2 || rr.Snapshots[len(rr.Snapshots)-1].Tree != rr.Snapshots[len(rr.Snapshots)-2].Tree {
		t.Errorf("final pass was not a no-op")
	}
}

func TestTokenPositionsAndLexErrors(t *testing.T) {
	toks, errs := Lex("SELECT 'abc")
	if len(errs) != 1 || errs[0].Code != "lex" {
		t.Fatalf("errs = %v", errs)
	}
	if toks[0].Pos.Line != 1 || toks[0].Pos.Col != 1 {
		t.Errorf("first token at %v", toks[0].Pos)
	}
	// Multiline: positions track lines and rune columns.
	toks, _ = Lex("SELECT a\n  FROM lots")
	for _, tk := range toks {
		if tk.Text == "from" && (tk.Pos.Line != 2 || tk.Pos.Col != 3) {
			t.Errorf("FROM at %v, want 2:3", tk.Pos)
		}
	}
}

func TestAuctionFunctionsRegisteredLikeBuiltins(t *testing.T) {
	r := DefaultRegistry()
	for _, n := range []string{"tick_round", "tick_bucket", "tick_span", "price_to_cent", "lower", "round_to"} {
		if _, ok := r.Lookup(n); !ok {
			t.Errorf("function %s missing", n)
		}
	}
	// A host can register its own function through the same path.
	r2 := NewRegistry()
	r2.RegisterFunction("my_fn", []FnArg{{"t", []Kind{KClock}}}, false,
		func(Pos, []Type) (Type, *Error) { return IntT, nil },
		func(a []Value) (Value, *Error) { return Value{Kind: KInt, Int: int64(a[0].Clock)}, nil })
	e := &Engine{Cat: AuctionCatalog(), Reg: r2}
	a := e.Analyze("SELECT my_fn(t.price_tick) AS x FROM trades t")
	if len(a.Errors) > 0 {
		t.Fatalf("custom function rejected: %v", a.Errors)
	}
	// The same type checker rejects a wrong argument type.
	a = e.Analyze("SELECT my_fn(t.stems) AS x FROM trades t")
	if len(a.Errors) == 0 || a.Errors[0].Code != "type" {
		t.Fatalf("custom function type check missing: %v", a.Errors)
	}
}
