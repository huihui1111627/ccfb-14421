package engine

import (
	"testing"
)

func TestJoinSemanticsAllKinds(t *testing.T) {
	e := NewEngine()
	ds := NewDataset()
	ds.Add(&Table{Name: "lots", Cols: []string{"lot_id", "grower", "variety", "stems", "grade", "base_tick", "sold_date", "stock"}, Rows: []Row{
		{{Kind: KInt, Int: 1}, {Kind: KString, Str: "A"}, {Kind: KString, Str: "rose"}, {Kind: KInt, Int: 10}, {Kind: KString, Str: "A1"}, {Kind: KClock, Clock: 9000}, {Kind: KDate, Date: 19700}, NullValue(KInt)},
		{{Kind: KInt, Int: 2}, {Kind: KString, Str: "B"}, {Kind: KString, Str: "tulip"}, {Kind: KInt, Int: 20}, {Kind: KString, Str: "A2"}, {Kind: KClock, Clock: 5000}, {Kind: KDate, Date: 19701}, {Kind: KInt, Int: 7}},
		{{Kind: KInt, Int: 3}, {Kind: KString, Str: "A"}, {Kind: KString, Str: "rose"}, {Kind: KInt, Int: 30}, {Kind: KString, Str: "B1"}, {Kind: KClock, Clock: 1000}, {Kind: KDate, Date: 19702}, {Kind: KInt, Int: 2}},
	}})
	ds.Add(&Table{Name: "trades", Cols: []string{"trade_id", "lot_id", "buyer", "price_tick", "stems", "trade_date", "discount", "note"}, Rows: []Row{
		{{Kind: KInt, Int: 100}, {Kind: KInt, Int: 1}, {Kind: KString, Str: "X"}, {Kind: KClock, Clock: 8000}, {Kind: KInt, Int: 10}, {Kind: KDate, Date: 19700}, NullValue(KDec), NullValue(KString)},
		{{Kind: KInt, Int: 101}, {Kind: KInt, Int: 1}, {Kind: KString, Str: "Y"}, {Kind: KClock, Clock: 7000}, {Kind: KInt, Int: 5}, {Kind: KDate, Date: 19700}, {Kind: KDec, Dec: Decimal{1000, 2}}, {Kind: KString, Str: "fast"}},
		{{Kind: KInt, Int: 102}, {Kind: KInt, Int: 9}, {Kind: KString, Str: "Z"}, {Kind: KClock, Clock: 6000}, {Kind: KInt, Int: 5}, {Kind: KDate, Date: 19703}, NullValue(KDec), {Kind: KString, Str: "late"}},
	}})
	snap := func(cols []string) *Table {
		// Fill the remaining catalog tables with empty relations so plans bind.
		return &Table{Name: cols[0], Cols: cols[1:], Rows: nil}
	}
	_ = snap

	run := func(sql string) []Row {
		pr := Parse(sql)
		if len(pr.Errs) > 0 {
			t.Fatalf("%q: %v", sql, pr.Errs)
		}
		plan, errs := BindOne(pr.Stmt, e.Cat, e.Reg)
		if len(errs) > 0 {
			t.Fatalf("%q: %v", sql, errs)
		}
		rows, err := NewEvaluator(ds, e.Reg).Run(plan.Root)
		if err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		return rows
	}

	cases := []struct {
		sql  string
		want int
	}{
		// trades reference lot 1 twice and lot 9 once; lots 2 and 3 have none.
		{"SELECT l.lot_id FROM lots l JOIN trades t ON t.lot_id = l.lot_id", 2},
		{"SELECT l.lot_id FROM lots l LEFT JOIN trades t ON t.lot_id = l.lot_id", 4},
		{"SELECT l.lot_id FROM lots l RIGHT JOIN trades t ON t.lot_id = l.lot_id", 3},
		{"SELECT l.lot_id FROM lots l FULL JOIN trades t ON t.lot_id = l.lot_id", 5},
		{"SELECT l.lot_id FROM lots l CROSS JOIN trades t", 9},
		{"SELECT l.lot_id FROM lots l LEFT JOIN trades t ON t.lot_id = l.lot_id WHERE t.trade_id IS NULL", 2},
		{"SELECT l.lot_id FROM lots l LEFT JOIN trades t ON t.lot_id = l.lot_id WHERE t.trade_id IS NOT NULL", 2},
		{"SELECT DISTINCT l.grower FROM lots l", 2},
		{"SELECT l.grower, count(*) AS n FROM lots l GROUP BY l.grower", 2},
		{"SELECT count(*) AS n FROM lots l WHERE l.stems > 1000", 1}, // aggregate over empty input
		{"SELECT l.grower FROM lots l GROUP BY l.grower HAVING count(*) > 1", 1},
		{"SELECT l.lot_id FROM lots l WHERE l.stock IS NULL", 1},
		{"SELECT l.lot_id FROM lots l WHERE l.stock <> 2", 1},
		{"SELECT l.lot_id FROM lots l WHERE l.stock NOT IN (2, 7)", 0},
		{"SELECT l.lot_id FROM lots l WHERE l.lot_id IN (1, 3)", 2},
		{"SELECT l.lot_id FROM lots l WHERE EXISTS (SELECT 1 FROM trades t WHERE t.lot_id = l.lot_id)", 1},
		{"SELECT l.lot_id FROM lots l WHERE l.lot_id NOT IN (SELECT t.lot_id FROM trades t WHERE t.price_tick > 75.00::tick)", 2},
		{"SELECT l.lot_id, (SELECT max(t.price_tick) FROM trades t WHERE t.lot_id = l.lot_id) AS top FROM lots l", 3},
		{"SELECT lot_id FROM lots UNION SELECT lot_id FROM trades", 4},
		{"SELECT lot_id FROM lots EXCEPT SELECT lot_id FROM trades", 2},
		{"SELECT lot_id FROM lots INTERSECT SELECT lot_id FROM trades", 1},
		{"SELECT l.lot_id FROM lots l ORDER BY l.lot_id DESC LIMIT 2", 2},
		{"SELECT l.lot_id FROM lots l ORDER BY l.lot_id LIMIT 2 OFFSET 1", 2},
	}
	for _, c := range cases {
		if rows := run(c.sql); len(rows) != c.want {
			t.Errorf("%q: %d rows, want %d", c.sql, len(rows), c.want)
		}
	}
}

func TestScalarSubqueryCardinalityError(t *testing.T) {
	e := NewEngine()
	ds := NewDataset()
	ds.Add(&Table{Name: "lots", Cols: []string{"lot_id", "grower", "variety", "stems", "grade", "base_tick", "sold_date", "stock"}, Rows: []Row{
		{{Kind: KInt, Int: 1}, {Kind: KString, Str: "A"}, {Kind: KString, Str: "r"}, {Kind: KInt, Int: 10}, {Kind: KString, Str: "A1"}, {Kind: KClock, Clock: 1}, {Kind: KDate, Date: 19700}, NullValue(KInt)},
		{{Kind: KInt, Int: 2}, {Kind: KString, Str: "B"}, {Kind: KString, Str: "r"}, {Kind: KInt, Int: 20}, {Kind: KString, Str: "A1"}, {Kind: KClock, Clock: 2}, {Kind: KDate, Date: 19700}, NullValue(KInt)},
	}})
	pr := Parse("SELECT (SELECT l.lot_id FROM lots l) AS x FROM lots")
	plan, errs := BindOne(pr.Stmt, e.Cat, e.Reg)
	if len(errs) > 0 {
		t.Fatalf("%v", errs)
	}
	if _, err := NewEvaluator(ds, e.Reg).Run(plan.Root); err == nil {
		t.Fatal("expected a cardinality error for a multi-row scalar subquery")
	}
}

func TestNullPropagation(t *testing.T) {
	e := NewEngine()
	ds := NewDataset()
	ds.Add(&Table{Name: "lots", Cols: []string{"lot_id", "grower", "variety", "stems", "grade", "base_tick", "sold_date", "stock"}, Rows: []Row{
		{{Kind: KInt, Int: 1}, {Kind: KString, Str: "A"}, {Kind: KString, Str: "r"}, {Kind: KInt, Int: 10}, {Kind: KString, Str: "A1"}, {Kind: KClock, Clock: 1}, {Kind: KDate, Date: 19700}, NullValue(KInt)},
	}})
	run := func(sql string) Row {
		pr := Parse(sql)
		plan, errs := BindOne(pr.Stmt, e.Cat, e.Reg)
		if len(errs) > 0 {
			t.Fatalf("%q: %v", sql, errs)
		}
		rows, err := NewEvaluator(ds, e.Reg).Run(plan.Root)
		if err != nil || len(rows) != 1 {
			t.Fatalf("%q: rows=%v err=%v", sql, rows, err)
		}
		return rows[0]
	}
	if v := run("SELECT stock + 1 AS x FROM lots")[0]; !v.Null {
		t.Errorf("NULL + 1 = %v, want NULL", v)
	}
	if v := run("SELECT stock IS NULL AS x FROM lots")[0]; v.Null || !v.Bool {
		t.Errorf("IS NULL = %v, want true", v)
	}
	if v := run("SELECT (stock > 1) OR (lot_id = 1) AS x FROM lots")[0]; v.Null || !v.Bool {
		t.Errorf("NULL OR true = %v, want true", v)
	}
	// NULL AND TRUE is NULL in three-valued logic (this is why the filter
	// treats it as non-matching, but the projected value stays NULL).
	if v := run("SELECT (stock > 1) AND (lot_id = 1) AS x FROM lots")[0]; !v.Null {
		t.Errorf("NULL AND true = %v, want NULL", v)
	}
	if v := run("SELECT CASE WHEN stock IS NULL THEN -1 ELSE stock END AS x FROM lots")[0]; v.Null || v.Int != -1 {
		t.Errorf("CASE fallback = %v, want -1", v)
	}
	if v := run("SELECT coalesce_like FROM (SELECT stock AS coalesce_like FROM lots) d")[0]; !v.Null {
		t.Errorf("derived NULL lost: %v", v)
	}
}

func TestDivisionByZeroIsAnError(t *testing.T) {
	e := NewEngine()
	ds := NewDataset()
	ds.Add(&Table{Name: "lots", Cols: []string{"lot_id", "grower", "variety", "stems", "grade", "base_tick", "sold_date", "stock"}, Rows: []Row{
		{{Kind: KInt, Int: 1}, {Kind: KString, Str: "A"}, {Kind: KString, Str: "r"}, {Kind: KInt, Int: 10}, {Kind: KString, Str: "A1"}, {Kind: KClock, Clock: 1}, {Kind: KDate, Date: 19700}, {Kind: KInt, Int: 0}},
	}})
	for _, sql := range []string{"SELECT stems / stock AS x FROM lots", "SELECT stems / 0 AS x FROM lots"} {
		pr := Parse(sql)
		plan, errs := BindOne(pr.Stmt, e.Cat, e.Reg)
		if len(errs) > 0 {
			t.Fatalf("%q: %v", sql, errs)
		}
		if _, err := NewEvaluator(ds, e.Reg).Run(plan.Root); err == nil {
			t.Errorf("%q: expected division-by-zero error", sql)
		}
	}
}

func TestLateralJoinIsEvaluatedPerRow(t *testing.T) {
	e := NewEngine()
	ds := NewDataset()
	ds.Add(&Table{Name: "lots", Cols: []string{"lot_id", "grower", "variety", "stems", "grade", "base_tick", "sold_date", "stock"}, Rows: []Row{
		{{Kind: KInt, Int: 1}, {Kind: KString, Str: "A"}, {Kind: KString, Str: "r"}, {Kind: KInt, Int: 1}, {Kind: KString, Str: "A1"}, {Kind: KClock, Clock: 1}, {Kind: KDate, Date: 19700}, NullValue(KInt)},
		{{Kind: KInt, Int: 2}, {Kind: KString, Str: "B"}, {Kind: KString, Str: "r"}, {Kind: KInt, Int: 2}, {Kind: KString, Str: "A1"}, {Kind: KClock, Clock: 2}, {Kind: KDate, Date: 19700}, NullValue(KInt)},
	}})
	ds.Add(&Table{Name: "trades", Cols: []string{"trade_id", "lot_id", "buyer", "price_tick", "stems", "trade_date", "discount", "note"}, Rows: []Row{
		{{Kind: KInt, Int: 10}, {Kind: KInt, Int: 1}, {Kind: KString, Str: "X"}, {Kind: KClock, Clock: 8}, {Kind: KInt, Int: 1}, {Kind: KDate, Date: 19700}, NullValue(KDec), NullValue(KString)},
		{{Kind: KInt, Int: 11}, {Kind: KInt, Int: 1}, {Kind: KString, Str: "X"}, {Kind: KClock, Clock: 8}, {Kind: KInt, Int: 1}, {Kind: KDate, Date: 19700}, NullValue(KDec), NullValue(KString)},
	}})
	sql := "SELECT l.lot_id AS id, q.n AS n FROM lots l JOIN LATERAL (SELECT count(*) AS n FROM trades t WHERE t.lot_id = l.lot_id) q ON true"
	for _, rewrote := range []bool{false, true} {
		pr := Parse(sql)
		plan, errs := BindOne(pr.Stmt, e.Cat, e.Reg)
		if len(errs) > 0 {
			t.Fatalf("%v", errs)
		}
		if rewrote {
			plan.RewritePlan(DefaultRules())
		}
		rows, err := NewEvaluator(ds, e.Reg).Run(plan.Root)
		if err != nil {
			t.Fatalf("rewrote=%v: %v", rewrote, err)
		}
		want := "i1|i2\ni2|i0"
		if got := RowSetString(rows); got != want {
			t.Errorf("rewrote=%v: got %q, want %q", rewrote, got, want)
		}
	}
}

func TestCorrelatedSubqueryPerRow(t *testing.T) {
	e := NewEngine()
	ds := NewDataset()
	ds.Add(&Table{Name: "lots", Cols: []string{"lot_id", "grower", "variety", "stems", "grade", "base_tick", "sold_date", "stock"}, Rows: []Row{
		{{Kind: KInt, Int: 1}, {Kind: KString, Str: "A"}, {Kind: KString, Str: "r"}, {Kind: KInt, Int: 1}, {Kind: KString, Str: "A1"}, {Kind: KClock, Clock: 1}, {Kind: KDate, Date: 19700}, NullValue(KInt)},
		{{Kind: KInt, Int: 2}, {Kind: KString, Str: "B"}, {Kind: KString, Str: "r"}, {Kind: KInt, Int: 2}, {Kind: KString, Str: "A1"}, {Kind: KClock, Clock: 2}, {Kind: KDate, Date: 19700}, NullValue(KInt)},
	}})
	ds.Add(&Table{Name: "trades", Cols: []string{"trade_id", "lot_id", "buyer", "price_tick", "stems", "trade_date", "discount", "note"}, Rows: []Row{
		{{Kind: KInt, Int: 10}, {Kind: KInt, Int: 2}, {Kind: KString, Str: "X"}, {Kind: KClock, Clock: 8}, {Kind: KInt, Int: 1}, {Kind: KDate, Date: 19700}, NullValue(KDec), NullValue(KString)},
		{{Kind: KInt, Int: 11}, {Kind: KInt, Int: 2}, {Kind: KString, Str: "X"}, {Kind: KClock, Clock: 8}, {Kind: KInt, Int: 1}, {Kind: KDate, Date: 19700}, NullValue(KDec), NullValue(KString)},
		{{Kind: KInt, Int: 12}, {Kind: KInt, Int: 2}, {Kind: KString, Str: "X"}, {Kind: KClock, Clock: 8}, {Kind: KInt, Int: 1}, {Kind: KDate, Date: 19700}, NullValue(KDec), NullValue(KString)},
	}})
	// The scalar subquery must differ per row: lot 1 has 0 trades, lot 2 has 3.
	sql := "SELECT l.lot_id AS id, (SELECT count(*) FROM trades t WHERE t.lot_id = l.lot_id) AS n FROM lots l"
	pr := Parse(sql)
	plan, errs := BindOne(pr.Stmt, e.Cat, e.Reg)
	if len(errs) > 0 {
		t.Fatalf("%v", errs)
	}
	rows, err := NewEvaluator(ds, e.Reg).Run(plan.Root)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if got, want := RowSetString(rows), "i1|i0\ni2|i3"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
