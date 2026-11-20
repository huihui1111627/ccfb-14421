// Package generator produces random queries for differential testing: valid
// queries that exercise every rewrite rule, and invalid queries with a known
// list of expected error positions.
package generator

import (
	"fmt"
	"math/rand"
	"strings"

	"clockql/internal/engine"
)

// Column describes a catalog column the generator may reference.
type Col struct {
	Name     string
	Type     engine.Type
	Nullable bool
	Kind     string // "int","dec","str","date","tick","bool"
}

// Table is a generator view of a catalog table.
type Table struct {
	Name string
	Cols []Col
}

// Schema is the set of tables and functions available.
type Schema struct {
	Tables []Table
}

// AuctionSchema mirrors engine.AuctionCatalog with generator-friendly
// metadata.
func AuctionSchema() *Schema {
	dec := func(p, s int) engine.Type { return engine.DecType(p, s) }
	return &Schema{Tables: []Table{
		{Name: "lots", Cols: []Col{
			{Name: "lot_id", Type: engine.IntT, Kind: "int"},
			{Name: "grower", Type: engine.StrT, Kind: "str"},
			{Name: "variety", Type: engine.StrT, Kind: "str"},
			{Name: "stems", Type: engine.IntT, Kind: "int"},
			{Name: "grade", Type: engine.StrT, Kind: "str"},
			{Name: "base_tick", Type: engine.ClockT, Kind: "tick"},
			{Name: "sold_date", Type: engine.DateT, Kind: "date"},
			{Name: "stock", Type: engine.IntT, Nullable: true, Kind: "int"},
		}},
		{Name: "trades", Cols: []Col{
			{Name: "trade_id", Type: engine.IntT, Kind: "int"},
			{Name: "lot_id", Type: engine.IntT, Kind: "int"},
			{Name: "buyer", Type: engine.StrT, Kind: "str"},
			{Name: "price_tick", Type: engine.ClockT, Kind: "tick"},
			{Name: "stems", Type: engine.IntT, Kind: "int"},
			{Name: "trade_date", Type: engine.DateT, Kind: "date"},
			{Name: "discount", Type: dec(6, 2), Nullable: true, Kind: "dec"},
			{Name: "note", Type: engine.StrT, Nullable: true, Kind: "str"},
		}},
		{Name: "bidders", Cols: []Col{
			{Name: "buyer", Type: engine.StrT, Kind: "str"},
			{Name: "region", Type: engine.StrT, Kind: "str"},
			{Name: "credit", Type: dec(12, 2), Nullable: true, Kind: "dec"},
		}},
		{Name: "clock", Cols: []Col{
			{Name: "day", Type: engine.DateT, Kind: "date"},
			{Name: "open_tick", Type: engine.ClockT, Kind: "tick"},
			{Name: "close_tick", Type: engine.ClockT, Kind: "tick"},
			{Name: "floor_tick", Type: engine.ClockT, Kind: "tick"},
		}},
	}}
}

// Gen holds generation state.
type Gen struct {
	r         *rand.Rand
	schema    *Schema
	aliasSeed int
	// targeted selects a rewrite-rule stress mode per query.
	targeted int
}

func NewGen(seed int64) *Gen {
	return &Gen{r: rand.New(rand.NewSource(seed)), schema: AuctionSchema()}
}

func (g *Gen) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return g.r.Intn(n)
}

func (g *Gen) chance(p float64) bool { return g.r.Float64() < p }

func (g *Gen) pick(ss []string) string { return ss[g.intn(len(ss))] }

// tableByName returns a table definition.
func (g *Gen) tableByName(n string) *Table {
	for i := range g.schema.Tables {
		if g.schema.Tables[i].Name == n {
			return &g.schema.Tables[i]
		}
	}
	return nil
}

// scope describes what expressions may reference at a point in the query.
type scope struct {
	tables []string         // table aliases in FROM
	cols   map[string][]Col // alias -> cols
	cte    map[string][]Col
	// outer, when non-nil, is a correlatable parent scope.
	outer *scope
}

func (s *scope) clone() *scope {
	ns := &scope{cols: map[string][]Col{}, cte: map[string][]Col{}}
	ns.tables = append([]string{}, s.tables...)
	for k, v := range s.cols {
		ns.cols[k] = append([]Col{}, v...)
	}
	ns.cte = s.cte
	ns.outer = s.outer
	return ns
}

// ref is a concrete column reference expression.
type ref struct {
	text string
	col  Col
}

func (s *scope) allRefs() []ref {
	var out []ref
	for _, t := range s.tables {
		for _, c := range s.cols[t] {
			out = append(out, ref{text: t + "." + c.Name, col: c})
		}
	}
	return out
}

func (s *scope) refsOfKinds(kinds ...string) []ref {
	var out []ref
	for _, r := range s.allRefs() {
		for _, k := range kinds {
			if r.col.Kind == k {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Valid query generation
// ---------------------------------------------------------------------------

// Case is one generated valid query plus metadata.
type Case struct {
	SQL string
	// Tags name the rule(s) this case is designed to trigger.
	Tags []string
	// Seed permits reproduction.
	Seed int64
}

// Valid generates one valid query. It targets a rule family based on the
// index so the suite is balanced.
func (g *Gen) Valid(i int) Case {
	g.targeted = i % 12
	var sql string
	var tags []string
	switch g.targeted {
	case 0:
		sql, tags = g.genConstantFold(), []string{"constant-fold"}
	case 1:
		sql, tags = g.genOuterToInner(), []string{"outer-to-inner"}
	case 2:
		sql, tags = g.genFalsePredicate(), []string{"simplify-false"}
	case 3:
		sql, tags = g.genSingleCTE(), []string{"inline-cte"}
	case 4:
		sql, tags = g.genCNF(), []string{"cnf"}
	case 5:
		sql, tags = g.genAggregate(), []string{"constant-fold", "cnf"}
	case 6:
		sql, tags = g.genCorrelated(), []string{"inline-cte", "cnf"}
	case 7:
		sql, tags = g.genMixed(), []string{"outer-to-inner", "constant-fold", "cnf", "inline-cte"}
	case 8, 9, 10:
		// Three of twelve slots are correlated/lateral shapes: they are the
		// trickiest binding case and deserve proportionally more coverage.
		sql, tags = g.genCorrelated(), []string{"correlated", "cnf"}
	default:
		sql, tags = g.genCNFOverflow(), []string{"cnf-budget"}
	}
	return Case{SQL: sql, Tags: tags, Seed: g.r.Int63()}
}

// aggWanted builds a query over one table with random predicates and
// projections, always returning interesting types.
func (g *Gen) baseScope(algo string, talias string) *scope {
	t := g.tableByName(algo)
	sc := &scope{cols: map[string][]Col{}, cte: map[string][]Col{}}
	sc.tables = []string{talias}
	sc.cols[talias] = t.Cols
	return sc
}

func (g *Gen) litOfKind(kind string) string {
	switch kind {
	case "int":
		return fmt.Sprint(g.intn(300) - 20)
	case "dec":
		return fmt.Sprintf("%d.%02d", g.intn(40), g.intn(100))
	case "str":
		return "'" + g.pick([]string{"A", "B", "C", "rose", "tulip", "XL"}) + "'"
	case "date":
		return fmt.Sprintf("DATE '2024-0%d-%02d'", 1+g.intn(9), 1+g.intn(28))
	case "tick":
		v := g.intn(101)
		if v == 100 {
			return "100.00"
		}
		return fmt.Sprintf("%d.%02d", v, g.intn(100))
	}
	return "1"
}

// pred builds a predicate over sc. It avoids constants on both sides so
// every predicate references at least one column (so simplify-false has to
// find actual contradictions, not trivially constant terms).
func (g *Gen) pred(sc *scope, depth int, target string) string {
	if depth <= 0 {
		return g.atomPred(sc, target)
	}
	switch g.intn(6) {
	case 0:
		return "(" + g.pred(sc, depth-1, target) + " AND " + g.pred(sc, depth-1, target) + ")"
	case 1:
		return "(" + g.pred(sc, depth-1, target) + " OR " + g.pred(sc, depth-1, target) + ")"
	case 2:
		return "NOT (" + g.pred(sc, depth-1, target) + ")"
	case 3:
		if target == "cnf" {
			// Prefer chains that expand under CNF.
			return "(" + g.pred(sc, depth-1, target) + " OR (" + g.pred(sc, 0, target) + " AND " + g.pred(sc, 0, target) + "))"
		}
		return "(" + g.pred(sc, depth-1, target) + " AND " + g.pred(sc, depth-1, target) + ")"
	default:
		return g.atomPred(sc, target)
	}
}

func (g *Gen) atomPred(sc *scope, target string) string {
	if target == "false" {
		// Contradictions that constant folding can prove.
		if refs := sc.refsOfKinds("int"); len(refs) > 0 {
			r := refs[g.intn(len(refs))]
			n := 1 + g.intn(50)
			switch g.intn(4) {
			case 0:
				return fmt.Sprintf("%s > %d AND %s < %d", r.text, n+10, r.text, n)
			case 1:
				return fmt.Sprintf("%s = %d AND %s = %d", r.text, n, r.text, n+1)
			case 2:
				return fmt.Sprintf("NOT (%s >= %d) AND %s >= %d", r.text, n, r.text, n)
			default:
				return "1 = 0"
			}
		}
		return "1 = 0"
	}
	// Choose a column and an operator.
	all := sc.allRefs()
	if len(all) == 0 {
		return "1 = 1"
	}
	r := all[g.intn(len(all))]
	op := g.pick([]string{"=", "<>", "<", "<=", ">", ">="})
	rhs := ""
	if r.col.Kind == "tick" {
		if tickRefs := g.sameKindRefs(sc, "tick"); len(tickRefs) > 0 && g.chance(0.5) {
			return r.text + " " + op + " " + tickRefs[g.intn(len(tickRefs))]
		}
	}
	if g.chance(0.25) {
		// Compare to a constant of the same type. Clock constants need the
		// explicit ::tick cast, mirroring the language's conversion rule.
		rhs = g.litOfKind(r.col.Kind)
		if r.col.Kind == "tick" {
			rhs += "::tick"
		}
	} else if same := g.sameKindRefs(sc, r.col.Kind); len(same) > 0 {
		rhs = same[g.intn(len(same))]
	} else {
		rhs = g.litOfKind(r.col.Kind)
		if r.col.Kind == "tick" {
			rhs += "::tick"
		}
	}
	if g.chance(0.12) {
		if g.chance(0.5) {
			return r.text + " IS NULL"
		}
		return r.text + " IS NOT NULL"
	}
	if g.chance(0.10) && r.col.Kind == "str" {
		return r.text + " LIKE '" + g.pick([]string{"A%", "%l%", "_", "rose"}) + "'"
	}
	if g.chance(0.08) && (r.col.Kind == "int" || r.col.Kind == "dec") {
		return fmt.Sprintf("%s BETWEEN %s AND %s", r.text, g.litOfKind(r.col.Kind), g.litOfKind(r.col.Kind))
	}
	return r.text + " " + op + " " + rhs
}

func (g *Gen) sameKindRefs(sc *scope, kind string) []string {
	var out []string
	for _, r := range sc.allRefs() {
		if r.col.Kind == kind {
			out = append(out, r.text)
		}
	}
	return out
}

// numericExpr builds an arithmetic expression of a numeric type. Decimals
// appear often so fixed-point precision and rounding are exercised.
func (g *Gen) numericExpr(sc *scope) string {
	if g.chance(0.3) {
		return g.litOfKind(g.pick([]string{"int", "dec"}))
	}
	refs := sc.refsOfKinds("int", "dec")
	if len(refs) == 0 {
		return "1"
	}
	var build func(d int) string
	build = func(d int) string {
		if d <= 0 || g.chance(0.25) {
			r := refs[g.intn(len(refs))]
			return r.text
		}
		op := g.pick([]string{"+", "-", "*", "/"})
		l := build(d - 1)
		r := build(d - 1)
		if op == "/" {
			// Guard division by zero without a NULLIF function, and keep the
			// divisor integral so the guard compares like with like.
			return "(" + l + " / (CASE WHEN " + r + " = 0 THEN 1 ELSE " + r + " END))"
		}
		return "(" + l + " " + op + " " + r + ")"
	}
	return build(1 + g.intn(3))
}

func (g *Gen) scalarExpr(sc *scope) string {
	if g.chance(0.5) {
		return g.numericExpr(sc)
	}
	refs := sc.allRefs()
	if len(refs) == 0 {
		return "1"
	}
	r := refs[g.intn(len(refs))]
	switch r.col.Kind {
	case "str":
		return g.pick([]string{"upper(", "lower("}) + r.text + ")"
	case "tick":
		return "tick_round(" + r.text + ")"
	case "date":
		return "date_add(" + r.text + ", " + fmt.Sprint(g.intn(5)) + ")"
	}
	return r.text
}

// selectList builds a projection that is faithful to types.
func (g *Gen) selectList(sc *scope, aliasPrefix string) (string, []Col) {
	var items []string
	var cols []Col
	n := 1 + g.intn(4)
	for i := 0; i < n; i++ {
		switch g.intn(5) {
		case 0:
			refs := sc.allRefs()
			r := refs[g.intn(len(refs))]
			nm := fmt.Sprintf("%s_c%d", aliasPrefix, i)
			items = append(items, r.text+" AS "+nm)
			cols = append(cols, Col{Name: nm, Type: r.col.Type, Kind: r.col.Kind})
		case 1:
			items = append(items, g.numericExpr(sc)+" AS "+fmt.Sprintf("%s_n%d", aliasPrefix, i))
			cols = append(cols, Col{Name: fmt.Sprintf("%s_n%d", aliasPrefix, i), Type: engine.DecType(18, 6), Kind: "dec"})
		case 2:
			items = append(items, "tick_round("+g.tickExpr(sc)+") AS "+fmt.Sprintf("%s_t%d", aliasPrefix, i))
			cols = append(cols, Col{Name: fmt.Sprintf("%s_t%d", aliasPrefix, i), Type: engine.ClockT, Kind: "tick"})
		case 3:
			items = append(items, g.pred(sc, 1, "")+" AS "+fmt.Sprintf("%s_b%d", aliasPrefix, i))
			cols = append(cols, Col{Name: fmt.Sprintf("%s_b%d", aliasPrefix, i), Type: engine.BoolT, Kind: "bool"})
		default:
			items = append(items, "CASE WHEN "+g.pred(sc, 0, "")+" THEN "+g.litOfKind("int")+" ELSE "+g.litOfKind("int")+" END AS "+fmt.Sprintf("%s_k%d", aliasPrefix, i))
			cols = append(cols, Col{Name: fmt.Sprintf("%s_k%d", aliasPrefix, i), Type: engine.IntT, Kind: "int"})
		}
	}
	return strings.Join(items, ", "), cols
}

func (g *Gen) tickExpr(sc *scope) string {
	refs := sc.refsOfKinds("tick")
	if len(refs) == 0 {
		return "12.34"
	}
	return refs[g.intn(len(refs))].text
}

// ---------------------------------------------------------------------------
// Targeted generators
// ---------------------------------------------------------------------------

func (g *Gen) genConstantFold() string {
	var preds []string
	// Constant subexpressions in predicates and projections.
	preds = append(preds, fmt.Sprintf("t.price_tick < %d.%02d::tick", 50+g.intn(50), g.intn(100)))
	preds = append(preds, fmt.Sprintf("%d + %d * %d = %d", g.intn(9), g.intn(9), g.intn(9), g.intn(9)))
	preds = append(preds, fmt.Sprintf("upper('%s') = UPPER('%s')", g.pick([]string{"a", "bb"}), g.pick([]string{"a", "bb"})))
	if g.chance(0.5) {
		preds = append(preds, fmt.Sprintf("abs(%d - %d) >= 0", g.intn(100), g.intn(100)))
	}
	proj := []string{
		"t.trade_id",
		fmt.Sprintf("(t.stems * %d + %d) AS calc", 1+g.intn(5), g.intn(50)),
		fmt.Sprintf("round_to(t.discount * (1.0 + %d.%02d), 2) AS disc", g.intn(3), g.intn(100)),
	}
	return "SELECT " + strings.Join(proj, ", ") + " FROM trades t WHERE " + strings.Join(preds, " AND ")
}

func (g *Gen) genOuterToInner() string {
	kind := g.pick([]string{"LEFT", "RIGHT", "FULL"})
	sc2 := g.baseScope("trades", "t")
	joinPred := "t.lot_id = l.lot_id"
	reject := ""
	switch kind {
	case "LEFT":
		reject = g.pick([]string{"t.price_tick IS NOT NULL", "t.price_tick > 10.00::tick", "t.buyer = 'B'", "t.stems >= 0", "NOT (t.note IS NULL)"})
	case "RIGHT":
		reject = g.pick([]string{"l.stems IS NOT NULL", "l.grower = 'A'", "l.base_tick < 99.00::tick"})
	case "FULL":
		if g.chance(0.5) {
			reject = g.pick([]string{"t.price_tick IS NOT NULL", "t.stems > 0"}) + " AND " + g.pick([]string{"l.stems IS NOT NULL", "l.grower = 'A'"})
		} else {
			reject = g.pick([]string{"t.price_tick IS NOT NULL", "l.stock IS NOT NULL"})
		}
	}
	extra := ""
	if g.chance(0.4) {
		extra = " AND " + g.pred(sc2, 1, "")
	}
	proj := "l.lot_id AS ll, l.stems AS ls, t.trade_id AS tt, t.price_tick AS tp"
	return "SELECT " + proj + " FROM lots l " + kind + " JOIN trades t ON " + joinPred + " WHERE " + reject + extra
}

func (g *Gen) genFalsePredicate() string {
	t := g.pick([]string{"lots", "trades"})
	alias := "x"
	sc := g.baseScope(t, alias)
	p := g.pred(sc, 2, "false")
	extra := ""
	if g.chance(0.5) {
		extra = " AND " + g.atomPred(sc, "")
	}
	n := 1 + g.intn(30)
	proj := "x.stems AS s"
	if t == "trades" {
		proj = "x.trade_id AS s"
	}
	switch g.intn(4) {
	case 0:
		return "SELECT " + proj + " FROM " + t + " x WHERE (" + p + ") AND " + fmt.Sprintf("x.stems > %d AND x.stems < %d", n+1, n) + extra
	case 2:
		// A constant-false conjunct, possibly inside a larger OR-free chain.
		return "SELECT " + proj + " FROM " + t + " x WHERE " + fmt.Sprintf("%d = %d", n, n+1) + extra
	case 3:
		// False inner join: an always-false ON condition.
		return "SELECT l.lot_id AS a, t.trade_id AS b FROM lots l JOIN trades t ON " + fmt.Sprintf("l.stems > %d AND l.stems < %d", n+1, n)
	default:
		return "SELECT " + proj + " FROM " + t + " x WHERE " + p + extra
	}
}

func (g *Gen) genSingleCTE() string {
	t := g.pick([]string{"lots", "trades"})
	alias := "b"
	sc := g.baseScope(t, alias)
	pred := g.pred(sc, 2, "")
	secondCol := "stems"
	cols := "lot_id, stems"
	if t == "trades" {
		cols = "lot_id, price_tick"
		secondCol = "price_tick"
	}
	inner := fmt.Sprintf("SELECT %s FROM %s %s WHERE %s", cols, t, alias, pred)
	// Reference the CTE exactly once in one of several ways.
	switch g.intn(3) {
	case 0:
		return fmt.Sprintf("WITH c AS (%s) SELECT c.lot_id, c.%s AS v FROM c WHERE %s", inner, secondCol, g.ctePred(secondCol))
	case 1:
		second := "c.stems"
		if t == "trades" {
			second = "c.price_tick"
		}
		return fmt.Sprintf("WITH c AS (%s) SELECT c.lot_id, %s AS v FROM c JOIN bidders b ON c.lot_id = b.credit", inner, second)
	default:
		return fmt.Sprintf("WITH c(x, y) AS (%s) SELECT c.x, c.y FROM c WHERE c.x > %d", inner, g.intn(50))
	}
}

func (g *Gen) ctePred(secondCol string) string {
	intCmp := fmt.Sprintf("c.lot_id %s %d", g.pick([]string{">", "<", ">=", "="}), g.intn(100))
	if g.chance(0.5) {
		if secondCol == "price_tick" {
			return intCmp + fmt.Sprintf(" AND c.price_tick %s %d.%02d::tick", g.pick([]string{">", "<"}), g.intn(100), g.intn(100))
		}
		return intCmp + " AND c.stems " + g.pick([]string{">", "<"}) + " " + fmt.Sprint(g.intn(300))
	}
	return intCmp
}

func (g *Gen) genCNF() string {
	t := g.pick([]string{"lots", "trades"})
	sc := g.baseScope(t, "x")
	p := g.pred(sc, 3, "cnf")
	// Force OR-of-ANDs structure.
	leaf := func() string { return g.atomPred(sc, "") }
	p = "(" + leaf() + " OR (" + leaf() + " AND " + leaf() + "))"
	if g.chance(0.3) {
		p = p + " OR (" + leaf() + " AND " + leaf() + " AND " + leaf() + ")"
	}
	_ = sc
	if g.chance(0.5) {
		p = p + " AND " + g.atomPred(sc, "")
	}
	proj := "x.stems AS s"
	if t == "trades" {
		proj = "x.trade_id AS s"
	}
	return "SELECT " + proj + " FROM " + t + " x WHERE " + p
}

func (g *Gen) genAggregate() string {
	sc := g.baseScope("lots", "l")
	group := g.pick([]string{"l.grower", "l.grade", "l.variety"})
	var aggs []string
	aggs = append(aggs, "count(*) AS n")
	if g.chance(0.8) {
		aggs = append(aggs, "sum(l.stems) AS total")
	}
	if g.chance(0.6) {
		aggs = append(aggs, "min(l.base_tick) AS mn, max(l.base_tick) AS mx")
	}
	if g.chance(0.5) {
		aggs = append(aggs, "avg(l.stems * 1.0) AS av")
	}
	if g.chance(0.6) {
		aggs = append(aggs, "count(DISTINCT l.grade) AS ng")
	}
	where := ""
	if g.chance(0.8) {
		where = " WHERE " + g.pred(sc, 2, "")
	}
	having := ""
	if g.chance(0.5) {
		having = " HAVING count(*) > " + fmt.Sprint(g.intn(3))
	}
	order := ""
	if g.chance(0.5) {
		order = " ORDER BY 1"
	}
	return "SELECT " + group + ", " + strings.Join(aggs, ", ") + " FROM lots l" + where + " GROUP BY " + group + having + order
}

func (g *Gen) genCorrelated() string {
	// A correlated EXISTS/NOT EXISTS or scalar subquery.
	switch g.intn(5) {
	case 0:
		return "SELECT l.lot_id AS a, l.stems AS b FROM lots l WHERE EXISTS (SELECT 1 FROM trades t WHERE t.lot_id = l.lot_id AND " + g.corrPred("t") + ") AND " + g.pred(g.baseScope("lots", "l"), 1, "")
	case 1:
		return "SELECT l.lot_id AS a, (SELECT max(t.price_tick) FROM trades t WHERE t.lot_id = l.lot_id) AS top FROM lots l WHERE l.stems > " + fmt.Sprint(g.intn(200))
	case 2:
		// NOT EXISTS over a correlated aggregate-free subquery.
		return "SELECT l.lot_id AS a FROM lots l WHERE NOT EXISTS (SELECT 1 FROM trades t WHERE t.lot_id = l.lot_id AND t.buyer = '" + g.pick([]string{"B1", "B2", "B3"}) + "')"
	case 3:
		// Correlated count compared in the WHERE clause.
		return "SELECT l.lot_id AS a, l.stems AS b FROM lots l WHERE (SELECT count(*) FROM trades t WHERE t.lot_id = l.lot_id) > " + fmt.Sprint(g.intn(3))
	default:
		// Lateral derived table, with a join predicate on its output.
		if g.chance(0.5) {
			return "SELECT l.lot_id AS a, q.n AS b FROM lots l JOIN LATERAL (SELECT count(*) AS n FROM trades t WHERE t.lot_id = l.lot_id AND " + g.corrPred("t") + ") q ON q.n > " + fmt.Sprint(g.intn(2))
		}
		return "SELECT l.lot_id AS a, q.mx AS b FROM lots l LEFT JOIN LATERAL (SELECT max(t.price_tick) AS mx FROM trades t WHERE t.lot_id = l.lot_id AND t.stems < " + fmt.Sprint(g.intn(400)) + ") q ON true"
	}
}

func (g *Gen) corrPred(alias string) string {
	return fmt.Sprintf("%s.stems %s %d", alias, g.pick([]string{">", "<", ">="}), g.intn(300))
}

// genCNFOverflow builds a predicate whose CNF expansion is far larger than
// the term budget, so the rule must refuse and leave the predicate as written.
func (g *Gen) genCNFOverflow() string {
	sc := g.baseScope("lots", "x")
	leaf := func() string { return g.atomPred(sc, "") }
	// 7 groups of (a AND b) OR'd together give 2^7 = 128 conjuncts, twice the
	// MaxCNFTerms budget of 64.
	var groups []string
	for i := 0; i < 7; i++ {
		groups = append(groups, "("+leaf()+" AND "+leaf()+")")
	}
	p := strings.Join(groups, " OR ")
	return "SELECT x.lot_id AS a FROM lots x WHERE " + p
}

func (g *Gen) genMixed() string {
	// CTE referenced once + outer join + predicates + constants.
	kind := g.pick([]string{"LEFT", "INNER"})
	where := ""
	if kind == "LEFT" {
		where = " WHERE " + g.pick([]string{"t.price_tick IS NOT NULL", "t.stems > 0", "t.buyer = 'B'"})
	} else if g.chance(0.5) {
		where = " WHERE " + g.pred(g.baseScope("trades", "t"), 1, "")
	}
	if kind == "LEFT" {
		return "WITH c AS (SELECT lot_id, stems FROM lots WHERE " + fmt.Sprintf("stems > %d", g.intn(100)) + ") SELECT c.lot_id AS a, t.trade_id AS b, " + fmt.Sprintf("%d * c.stems + %d AS e", 1+g.intn(3), g.intn(9)) + " FROM c LEFT JOIN trades t ON t.lot_id = c.lot_id " + where
	}
	return "WITH c AS (SELECT lot_id, stems FROM lots WHERE " + fmt.Sprintf("stems > %d", g.intn(100)) + ") SELECT c.lot_id AS a, t.trade_id AS b, " + fmt.Sprintf("%d * c.stems + %d AS e", 1+g.intn(3), g.intn(9)) + " FROM c JOIN trades t ON t.lot_id = c.lot_id " + where
}

// ---------------------------------------------------------------------------
// Invalid query generation
// ---------------------------------------------------------------------------

// BadCase is an invalid query and the errors a correct front end must report.
type BadCase struct {
	// Faulty is the query with mistakes.
	Faulty string
	// Original is the correct query; it must bind cleanly.
	Original string
	// Expected lists the positions a correct front end must report. A
	// position is reported when some diagnostic falls on that line at or
	// before the given column (the diagnostic may point at the token start
	// or at the following token).
	Expected []ExpectedErr
	// Fault describes the injection.
	Fault string
}

// ExpectedErr is a required diagnostic location.
type ExpectedErr struct {
	Line int
	Col  int
	Code string
	// Fault names the injection that produced this position (for reporting).
	Fault string
}

type edit struct {
	pos   int // rune offset into faulty
	fault string
	line  int
	col   int
	code  string
}

// Invalid creates one invalid query with a complete expected-error list.
func (g *Gen) Invalid(i int) BadCase {
	switch i % 3 {
	case 0:
		return g.invalidFromValid()
	default:
		return g.invalidHandwritten()
	}
}

// injectAt finds a token boundary in src and injects the given fault token.
// It returns the edited source and the rune position of the fault.
func injectAt(g *Gen, src string, fault string, line, col int) (string, int) {
	// Convert line:col into a rune offset.
	runes := []rune(src)
	off := 0
	l, c := 1, 1
	for off < len(runes) && !(l == line && c == col) {
		if runes[off] == '\n' {
			l++
			c = 1
		} else {
			c++
		}
		off++
	}
	toks, _ := engine.Lex(src)
	for _, t := range toks {
		if t.Kind == engine.TString {
			continue
		}
	}
	// If the fault is an operator without an operand, insert before the
	// token at (line,col) using the token's rune offset.
	faultOff := -1
	if line > 0 && col > 0 {
		for _, t := range toks {
			if t.Pos.Line == line && t.Pos.Col >= col {
				faultOff = runeOff(src, t.Pos)
				break
			}
		}
	}
	if faultOff < 0 {
		faultOff = off
	}
	out := string(runes[:faultOff]) + fault + string(runes[faultOff:])
	return out, faultOff
}

func runeOff(src string, p engine.Pos) int {
	runes := []rune(src)
	l, c := 1, 1
	for i := 0; i < len(runes); i++ {
		if l == p.Line && c == p.Col {
			return i
		}
		if runes[i] == '\n' {
			l++
			c = 1
		} else {
			c++
		}
	}
	return len(runes)
}

func posOf(src string, off int) (int, int) {
	runes := []rune(src)
	l, c := 1, 1
	for i := 0; i < off && i < len(runes); i++ {
		if runes[i] == '\n' {
			l++
			c = 1
		} else {
			c++
		}
	}
	return l, c
}

// countSQLTokens counts non-punctuation tokens for choosing injection points.
func countSQLTokens(src string) int {
	toks, _ := engine.Lex(src)
	return len(toks)
}

func (g *Gen) invalidFromValid() BadCase {
	v := g.Valid(g.intn(1000))
	src := v.SQL
	// Choose 1-2 injection sites inside expression regions. A site is a token
	// that follows a dot, comma, opening paren, or operator: injecting there
	// damages an expression, not the FROM clause shape, so later independent
	// injections remain reachable.
	toks, _ := engine.Lex(src)
	var sites []engine.Pos
	// Skip CTE declaration lists (`WITH c(x, y) AS ...`): damage there removes
	// a declared column, which makes later references legitimately unknown
	// names rather than an error-recovery failure.
	skipDepth := 0 // >0 while inside a CTE declaration list
	for i := 1; i < len(toks); i++ {
		t := toks[i]
		if t.Kind == engine.TKeyword && t.Text == "with" {
			continue
		}
		// Enter a declaration list: WITH [RECURSIVE] name '('
		withIdx := i - 1
		if toks[withIdx].Kind == engine.TKeyword && toks[withIdx].Text == "recursive" {
			withIdx--
		}
		if withIdx >= 0 && toks[withIdx].Kind == engine.TKeyword && toks[withIdx].Text == "with" &&
			t.Kind == engine.TIdent && i+1 < len(toks) && toks[i+1].Kind == engine.TPunct && toks[i+1].Text == "(" {
			skipDepth = 1
			continue
		}
		if skipDepth > 0 {
			if t.Kind == engine.TPunct && t.Text == "(" {
				skipDepth++
			}
			if t.Kind == engine.TPunct && t.Text == ")" {
				skipDepth--
			}
			continue
		}
		if t.Kind != engine.TIdent && t.Kind != engine.TNumber && t.Kind != engine.TString {
			continue
		}
		prev := toks[i-1]
		okPrev := false
		switch {
		case prev.Kind == engine.TOp:
			okPrev = true
		case prev.Kind == engine.TPunct && (prev.Text == "," || prev.Text == "("):
			okPrev = true
		case prev.Kind == engine.TKeyword && prev.Text == "when":
			okPrev = true
		}
		if okPrev {
			sites = append(sites, t.Pos)
		}
	}
	if len(sites) == 0 {
		return g.invalidHandwritten()
	}
	g.r.Shuffle(len(sites), func(i, j int) { sites[i], sites[j] = sites[j], sites[i] })
	nfault := 1
	if g.chance(0.5) && len(sites) > 1 {
		nfault = 2
	}
	// We build the faulty string by inserting faults right-to-left so
	// earlier offsets stay valid, then compute their positions afterwards.
	type ins struct {
		off  int
		text string
	}
	var inses []ins
	var expected []ExpectedErr
	runeSrc := []rune(src)
	for k := 0; k < nfault; k++ {
		pos := sites[k]
		off := runeOff(src, pos)
		// Fault kind: comma injection, operator injection, or keyword
		// malformation.
		var text string
		switch g.intn(2) {
		case 0:
			// Stray operator sequence: cannot rebalance parentheses, so the
			// damage stays local and independent faults remain independent.
			text = " ="
		default:
			// Stray comma.
			text = ","
		}
		inses = append(inses, ins{off: off, text: text})
		_ = runeSrc
	}
	// Sort descending by offset.
	for i := 1; i < len(inses); i++ {
		for j := i; j > 0 && inses[j].off > inses[j-1].off; j-- {
			inses[j], inses[j-1] = inses[j-1], inses[j]
		}
	}
	out := src
	for _, in := range inses {
		o := runeOff(out, func() engine.Pos { l, c := posOf(src, in.off); return engine.Pos{Line: l, Col: c} }())
		_ = o
	}
	// Simpler: apply edits descending by rune offset on the original.
	var sb []rune
	sb = append(sb, runeSrc...)
	for _, in := range inses {
		insRunes := []rune(in.text)
		nr := make([]rune, 0, len(sb)+len(insRunes))
		nr = append(nr, sb[:in.off]...)
		nr = append(nr, insRunes...)
		nr = append(nr, sb[in.off:]...)
		sb = nr
	}
	out = string(sb)
	// Expected position for each insertion: the position of the original
	// token, shifted by insertions before it.
	sortedAsc := append([]ins{}, inses...)
	for i := 1; i < len(sortedAsc); i++ {
		for j := i; j > 0 && sortedAsc[j].off < sortedAsc[j-1].off; j-- {
			sortedAsc[j], sortedAsc[j-1] = sortedAsc[j-1], sortedAsc[j]
		}
	}
	for _, in := range sortedAsc {
		l, c := posOf(out, in.off)
		if in.text != "" && in.text[0] == ' ' {
			c++
		}
		expected = append(expected, ExpectedErr{Line: l, Col: c, Code: "syntax", Fault: "token injection"})
	}
	return BadCase{Faulty: out, Original: src, Expected: expected, Fault: "token injection"}
}

// invalidHandwritten returns curated invalid queries with exactly known
// fault sites.
func (g *Gen) invalidHandwritten() BadCase {
	type h struct {
		bad, good string
		faults    []ExpectedErr
		desc      string
	}
	cases := []h{
		{
			bad:  "SELECT lot_id AS FROM lots",
			good: "SELECT lot_id AS x FROM lots",
			faults: []ExpectedErr{
				{Line: 1, Col: 18, Code: "syntax"},
			},
			desc: "missing alias after AS",
		},
		{
			bad:  "SELECT lot_id, stems FROM WHERE stems > 1",
			good: "SELECT lot_id, stems FROM lots WHERE stems > 1",
			faults: []ExpectedErr{
				{Line: 1, Col: 27, Code: "syntax"},
			},
			desc: "missing table after FROM",
		},
		{
			bad:  "SELECT lot_id FROM lots WHERE stems > AND grade = 'A'",
			good: "SELECT lot_id FROM lots WHERE stems > 1 AND grade = 'A'",
			faults: []ExpectedErr{
				{Line: 1, Col: 39, Code: "syntax"},
			},
			desc: "missing operand before AND",
		},
		{
			bad:  "SELECT grower, sum(stems FROM lots GROUP BY grower",
			good: "SELECT grower, sum(stems) FROM lots GROUP BY grower",
			faults: []ExpectedErr{
				{Line: 1, Col: 25, Code: "syntax"},
			},
			desc: "unclosed function call",
		},
		{
			bad:  "SELECT l.lot_id FROM lots l WHERE l.stems > 'abc'",
			good: "SELECT l.lot_id FROM lots l WHERE l.stems > 5",
			faults: []ExpectedErr{
				{Line: 1, Col: 46, Code: "implicit-conversion"},
			},
			desc: "int compared to string",
		},
		{
			bad:  "SELECT to_clock(sold_date) FROM lots",
			good: "SELECT to_clock(sold_date, DATE '2024-01-01') FROM lots",
			faults: []ExpectedErr{
				{Line: 1, Col: 8, Code: "arity"},
			},
			desc: "clock conversion missing base",
		},
		{
			bad:  "SELECT lot_id FROM nowhere",
			good: "SELECT lot_id FROM lots",
			faults: []ExpectedErr{
				{Line: 1, Col: 21, Code: "name"},
			},
			desc: "unknown table",
		},
		{
			bad:  "SELECT lot_id FROM lots WHERE sold_date = 1",
			good: "SELECT lot_id FROM lots WHERE sold_date = DATE '2024-01-01'",
			faults: []ExpectedErr{
				{Line: 1, Col: 41, Code: "implicit-conversion"},
			},
			desc: "date compared to int",
		},
		{
			bad:  "SELECT sum(grower) FROM lots",
			good: "SELECT min(grower) FROM lots",
			faults: []ExpectedErr{
				{Line: 1, Col: 8, Code: "type"},
			},
			desc: "sum over a string",
		},
		{
			bad:  "SELECT stems FROM lots l JOIN trades t ON t.lot_id = l.lot_id",
			good: "SELECT l.stems FROM lots l JOIN trades t ON t.lot_id = l.lot_id",
			faults: []ExpectedErr{
				{Line: 1, Col: 8, Code: "ambiguous"},
			},
			desc: "ambiguous column stems",
		},
		{
			bad:  "SELECT lot_id, stems FROM lots l JOIN trades t ON t.lot_id = l.lot_id",
			good: "SELECT l.lot_id, l.stems FROM lots l JOIN trades t ON t.lot_id = l.lot_id",
			faults: []ExpectedErr{
				{Line: 1, Col: 8, Code: "ambiguous"},
				{Line: 1, Col: 15, Code: "ambiguous", Fault: "bare stems is ambiguous"},
			},
			desc: "ambiguous column from self-join",
		},
		{
			bad:  "SELECT lot_id FROM lots WHERE stems + 1",
			good: "SELECT lot_id FROM lots WHERE stems + 1 > 0",
			faults: []ExpectedErr{
				{Line: 1, Col: 31, Code: "type"},
			},
			desc: "non-boolean WHERE",
		},
		{
			bad:  "SELECT lot_id FROM lots WHERE base_tick = 95.00",
			good: "SELECT lot_id FROM lots WHERE base_tick = 95.00::tick",
			faults: []ExpectedErr{
				{Line: 1, Col: 41, Code: "implicit-conversion"},
			},
			desc: "tick compared to decimal",
		},
		{
			bad:  "SELECT lot_id FROM lots GROUP BY grade",
			good: "SELECT lot_id FROM lots GROUP BY lot_id, grade",
			faults: []ExpectedErr{
				{Line: 1, Col: 8, Code: "type"},
			},
			desc: "bare column beside GROUP BY",
		},
		{
			bad:  "SELECT count(sum(stems)) FROM lots",
			good: "SELECT sum(stems) FROM lots",
			faults: []ExpectedErr{
				{Line: 1, Col: 19, Code: "type"},
			},
			desc: "nested aggregate",
		},
		{
			bad:  "SELECT CASE WHEN stems THEN 1 ELSE 2 END FROM lots",
			good: "SELECT CASE WHEN stems > 0 THEN 1 ELSE 2 END FROM lots",
			faults: []ExpectedErr{
				{Line: 1, Col: 18, Code: "type"},
			},
			desc: "CASE WHEN non-boolean",
		},
		{
			bad:  "SELECT 'unterminated FROM lots",
			good: "SELECT 'ok' FROM lots",
			faults: []ExpectedErr{
				{Line: 1, Col: 8, Code: "lex"},
			},
			desc: "unterminated string",
		},
		{
			bad:  "SELECT stems FROM lots WHERE 1 = 2 AND",
			good: "SELECT stems FROM lots WHERE 1 = 2 AND 1 = 1",
			faults: []ExpectedErr{
				{Line: 1, Col: 38, Code: "syntax"},
			},
			desc: "trailing AND",
		},
		{
			bad:  "SELECT l.lot_id FROM lots l LEFT JOIN trades t ON t.lot_id = l.lot_id WHERE t.price_tick = 95.5",
			good: "SELECT l.lot_id FROM lots l LEFT JOIN trades t ON t.lot_id = l.lot_id WHERE t.price_tick = 95.5::tick",
			faults: []ExpectedErr{
				{Line: 1, Col: 90, Code: "implicit-conversion"},
			},
			desc: "tick comparison in outer join",
		},
		{
			bad:  "SELECT lot_id, FROM lots WHERE stems > 0",
			good: "SELECT lot_id FROM lots WHERE stems > 0",
			faults: []ExpectedErr{
				{Line: 1, Col: 17, Code: "syntax", Fault: "trailing comma"},
			},
			desc: "trailing comma in select list",
		},
		{
			bad:  "SELECT tick_round(stems) FROM lots",
			good: "SELECT tick_round(base_tick) FROM lots",
			faults: []ExpectedErr{
				{Line: 1, Col: 18, Code: "type"},
			},
			desc: "auction function type error",
		},
	}
	// Pick a case but also sometimes append a second independent fault from
	// another case in a separate clause.
	h0 := cases[g.intn(len(cases))]
	good := h0.good
	a := g.analyzeClean(good)
	_ = a
	return BadCase{Faulty: h0.bad, Original: good, Expected: h0.faults, Fault: h0.desc}
}

// analyzeClean is a sanity hook: the generator relies on the curated "good"
// query being valid; tests assert it.
func (g *Gen) analyzeClean(sql string) bool {
	e := engine.NewEngine()
	a := e.Analyze(sql)
	return len(a.Errors) == 0
}
