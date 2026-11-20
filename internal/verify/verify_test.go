package verify

import (
	"testing"

	"clockql/internal/engine"
	"clockql/internal/generator"
)

// TestGeneratorProducesValidQueries guards the harness itself: every query the
// valid generator emits must bind cleanly.
func TestGeneratorProducesValidQueries(t *testing.T) {
	g := generator.NewGen(11)
	eng := engine.NewEngine()
	for i := 0; i < 300; i++ {
		var c generator.Case
		if i%3 == 2 {
			c = g.ValidExtended(i)
		} else {
			c = g.Valid(i)
		}
		pr := engine.Parse(c.SQL)
		if len(pr.Errs) > 0 {
			t.Fatalf("valid generator emitted a syntax error\n%s\n%v", c.SQL, pr.Errs)
		}
		if _, errs := engine.BindOne(pr.Stmt, eng.Cat, eng.Reg); len(errs) > 0 {
			t.Fatalf("valid generator emitted a bind error\n%s\n%v", c.SQL, errs)
		}
	}
}

// TestBadCaseOriginalsAreValid guards the other half: every curated "correct"
// query must cleanly bind, otherwise the expected positions are meaningless.
func TestBadCaseOriginalsAreValid(t *testing.T) {
	g := generator.NewGen(12)
	eng := engine.NewEngine()
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		bc := g.Invalid(i)
		if seen[bc.Original] {
			continue
		}
		seen[bc.Original] = true
		a := eng.Analyze(bc.Original)
		if len(a.Errors) > 0 {
			t.Fatalf("bad-case original does not bind: %s\n%v", bc.Original, a.Errors)
		}
		if len(bc.Expected) == 0 {
			t.Fatalf("bad case %q has no expected errors", bc.Faulty)
		}
	}
}

// TestSmallRunIsClean runs the full harness on a modest corpus; the CLI does
// the large sweep.
func TestSmallRunIsClean(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ValidCount = 250
	cfg.BadCount = 120
	cfg.Datasets = 2
	res := Run(cfg)
	if res.ValidOK != res.Valid {
		t.Fatalf("%d/%d valid queries matched:\n%s", res.ValidOK, res.Valid, res.Summary())
	}
	if res.BadOK != res.Bad {
		t.Fatalf("%d/%d invalid queries explained:\n%s", res.BadOK, res.Bad, res.Summary())
	}
	// Every rule must have fired at least once across the corpus, otherwise
	// the suite is not exercising it.
	for _, name := range []string{"constant-fold", "outer-to-inner", "simplify-false", "inline-cte", "cnf"} {
		if res.Rules.Applied[name] == 0 {
			t.Errorf("rule %s never fired over the generated corpus", name)
		}
	}
}

// TestResultsAreExact checks the comparison ignores nothing: NULLs, decimals
// with different written scales and duplicate rows all must line up.
func TestResultsAreExact(t *testing.T) {
	n1 := []engine.Row{{{Kind: engine.KInt, Null: true}}}
	n2 := []engine.Row{{{Kind: engine.KInt, Int: 0}}}
	if renderRows(n1) == renderRows(n2) {
		t.Error("NULL and 0 must not compare equal")
	}
	one := engine.Row{{Kind: engine.KInt, Int: 1}}
	dup := []engine.Row{one, one}
	single := []engine.Row{one}
	if renderRows(dup) == renderRows(single) {
		t.Error("duplicate rows must be counted (multiset comparison)")
	}
	// 1.50 and 1.500 are numerically equal but written at different scales.
	// A rewrite must not change output scale, so the harness distinguishes
	// them and the comparison stays strict.
	a := []engine.Row{{{Kind: engine.KDec, Dec: engine.Decimal{Unscaled: 150, Scale: 2}}}}
	b := []engine.Row{{{Kind: engine.KDec, Dec: engine.Decimal{Unscaled: 1500, Scale: 3}}}}
	if renderRows(a) == renderRows(b) {
		t.Errorf("1.50 and 1.500 must differ in output scale: %q", renderRows(a))
	}
	// 1.50 and 1.51 must not.
	c := []engine.Row{{{Kind: engine.KDec, Dec: engine.Decimal{Unscaled: 151, Scale: 2}}}}
	if renderRows(a) == renderRows(c) {
		t.Error("1.50 and 1.51 must differ")
	}
}
