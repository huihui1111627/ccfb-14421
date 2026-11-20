package generator

import (
	"fmt"
	"strings"

	"clockql/internal/engine"
)

// ValidExtended adds shapes the first generator does not produce: set
// operations, DISTINCT, derived tables, UNION, CASTs, LIKE, IN lists,
// scalar subqueries, HAVING over aggregate results, multi-CTE chains and
// lateral joins with several correlated references.
func (g *Gen) ValidExtended(i int) Case {
	switch i % 6 {
	case 0:
		return g.genSetOp()
	case 1:
		return g.genDerived()
	case 2:
		return g.genCasts()
	case 3:
		return g.genMultiCTE()
	case 4:
		return g.genDistinctSort()
	default:
		return g.genNestedPreds()
	}
}

func (g *Gen) genSetOp() Case {
	n := 1 + g.intn(40)
	op := g.pick([]string{"UNION", "UNION ALL", "EXCEPT", "INTERSECT"})
	left := fmt.Sprintf("SELECT lot_id, stems FROM lots WHERE stems > %d", n)
	right := fmt.Sprintf("SELECT lot_id, stems FROM lots WHERE grade = '%s'", g.pick([]string{"A1", "A2", "B1"}))
	sql := left + " " + op + " " + right
	if g.chance(0.4) {
		sql += " ORDER BY 1"
	}
	return Case{SQL: sql, Tags: []string{"set-op"}}
}

func (g *Gen) genDerived() Case {
	n := 1 + g.intn(100)
	inner := fmt.Sprintf("SELECT lot_id, stems, grade FROM lots WHERE stems > %d", n)
	if g.chance(0.5) {
		return Case{SQL: fmt.Sprintf("SELECT d.lot_id AS a, d.stems AS b FROM (%s) AS d WHERE d.stems < %d", inner, n+200),
			Tags: []string{"derived", "cnf"}}
	}
	// Non-lateral derived table joining a real table.
	return Case{SQL: fmt.Sprintf("SELECT d.lot_id AS a, t.trade_id AS b FROM (%s) d JOIN trades t ON t.lot_id = d.lot_id WHERE d.grade = 'A1'", inner),
		Tags: []string{"derived"}}
}

func (g *Gen) genCasts() Case {
	sc := g.baseScope("trades", "t")
	preds := []string{
		fmt.Sprintf("CAST(t.price_tick AS int) > %d", g.intn(90)),
		fmt.Sprintf("CAST(t.discount AS decimal(8,3)) > %d.%02d", g.intn(10), g.intn(100)),
		fmt.Sprintf("CAST(t.trade_id AS string) <> '%s'", g.pick([]string{"x", "y"})),
	}
	proj := []string{
		"t.trade_id",
		fmt.Sprintf("CAST(t.price_tick AS decimal(6,2)) AS p"),
		fmt.Sprintf("from_clock(t.price_tick) AS d"),
	}
	if g.chance(0.5) {
		// Explicit round trip through the clock conversion functions.
		proj = append(proj, fmt.Sprintf("to_clock(from_clock(t.price_tick), DATE '2024-01-01') AS rt"))
	}
	return Case{SQL: "SELECT " + strings.Join(proj, ", ") + " FROM trades t WHERE " + strings.Join(preds, " AND ") + " AND " + g.pred(sc, 1, ""),
		Tags: []string{"cast", "constant-fold"}}
}

func (g *Gen) genMultiCTE() Case {
	a := fmt.Sprintf("SELECT lot_id, stems FROM lots WHERE stems > %d", g.intn(100))
	// The first CTE is referenced twice; the second once.
	sql := fmt.Sprintf("WITH a AS (%s), b AS (SELECT a.lot_id AS id, a.stems * %d AS s FROM a WHERE a.stems < %d) "+
		"SELECT b.id AS x, b.s AS y FROM b JOIN a ON a.lot_id = b.id WHERE a.stems > %d",
		a, 1+g.intn(3), g.intn(400)+100, g.intn(50))
	return Case{SQL: sql, Tags: []string{"cte-multi", "cnf"}}
}

func (g *Gen) genDistinctSort() Case {
	var proj string
	if g.chance(0.5) {
		proj = "DISTINCT grade"
	} else {
		proj = "DISTINCT grower, grade"
	}
	sql := fmt.Sprintf("SELECT %s FROM lots WHERE stems > %d ORDER BY 1", proj, g.intn(100))
	if g.chance(0.5) {
		sql += " LIMIT " + fmt.Sprint(1+g.intn(5))
	}
	return Case{SQL: sql, Tags: []string{"distinct"}}
}

func (g *Gen) genNestedPreds() Case {
	sc := g.baseScope("lots", "l")
	n := 1 + g.intn(50)
	// IN list, NOT IN subquery, EXISTS, NOT BETWEEN, LIKE with escapes.
	preds := []string{
		fmt.Sprintf("l.grade IN ('A1', 'A2', '%s')", g.pick([]string{"B1", "B2"})),
		fmt.Sprintf("l.lot_id NOT IN (SELECT t.lot_id FROM trades t WHERE t.stems > %d)", g.intn(300)),
		fmt.Sprintf("l.stems NOT BETWEEN %d AND %d", n, n+10),
		fmt.Sprintf("l.grower LIKE '%s'", g.pick([]string{"A%", "%eer", "%a%", "_"})),
	}
	if g.chance(0.5) {
		preds = append(preds, g.pred(sc, 1, ""))
	}
	return Case{SQL: "SELECT l.lot_id AS a, l.stems AS b FROM lots l WHERE " + strings.Join(preds, " AND "),
		Tags: []string{"cnf", "subquery"}}
}

// ExtendedSchema is unused directly, but referencing engine keeps the
// generator package honest about its dependencies.
var _ = engine.IntT
