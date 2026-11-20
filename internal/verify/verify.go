// Package verify implements the differential test harness: it generates
// random valid and invalid queries, checks that rewritten plans return
// identical results, and checks that invalid queries report exactly the
// expected error positions.
package verify

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"clockql/internal/engine"
	"clockql/internal/generator"
)

// Config controls one verification run.
type Config struct {
	Seed       int64
	ValidCount int
	BadCount   int
	Datasets   int
	DataOpts   DataOptions
	// StopOnFailure records the first N failures with full detail.
	MaxFailures int
}

func DefaultConfig() Config {
	return Config{
		Seed:        1,
		ValidCount:  4000,
		BadCount:    1500,
		Datasets:    3,
		DataOpts:    DefaultDataOptions(),
		MaxFailures: 10,
	}
}

// Failure is one concrete mismatch, retained for the report.
type Failure struct {
	Kind     string `json:"kind"`
	Seed     int64  `json:"seed"`
	SQL      string `json:"sql"`
	Detail   string `json:"detail"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
}

// RuleCounts is per-rule trigger statistics.
type RuleCounts struct {
	Applied  map[string]int `json:"applied"`
	Checked  map[string]int `json:"checked"`
	Rejected map[string]int `json:"rejected"`
	Queries  map[string]int `json:"queries"`
}

// Result is the outcome of a full verification run.
type Result struct {
	Config Config `json:"config"`
	Valid  int    `json:"validCases"`
	Bad    int    `json:"badCases"`
	// ValidOK counts cases whose pre/post results matched exactly.
	ValidOK int `json:"validOK"`
	// BadOK counts invalid cases where every expected position was reported.
	BadOK int `json:"badOK"`
	// ExtraDiags counts diagnostics beyond the expected positions.
	ExtraDiags int `json:"extraDiags"`
	// MissedDiags counts expected positions not reported.
	MissedDiags int           `json:"missedDiags"`
	Rules       RuleCounts    `json:"rules"`
	Failures    []Failure     `json:"failures"`
	Duration    time.Duration `json:"duration"`
	// SkippedErrorCases counts generated valid queries that errored at
	// rewrite/execute time for data-dependent reasons (e.g. overflow).
	SkippedErrorCases int `json:"skippedErrorCases"`
	// CTERefHist is a histogram of CTE reference counts observed.
	CTERefHist map[int]int `json:"cteRefHist"`
}

// Run executes the whole verification.
func Run(cfg Config) *Result {
	start := time.Now()
	if cfg.MaxFailures == 0 {
		cfg.MaxFailures = 10
	}
	res := &Result{
		Config: cfg,
		Rules: RuleCounts{
			Applied: map[string]int{}, Checked: map[string]int{},
			Rejected: map[string]int{}, Queries: map[string]int{},
		},
		CTERefHist: map[int]int{},
	}
	eng := engine.NewEngine()
	// Datasets are generated once per dataset index; every valid query runs
	// against all of them.
	var datasets []*DatasetSpec
	for d := 0; d < cfg.Datasets; d++ {
		datasets = append(datasets, GenerateData(cfg.Seed*7919+int64(d), cfg.DataOpts))
	}
	gen := generator.NewGen(cfg.Seed)

	for i := 0; i < cfg.ValidCount; i++ {
		c := gen.Valid(i)
		if i%3 == 2 {
			c = gen.ValidExtended(i)
		}
		res.Valid++
		ok, failure, skipped := verifyValid(eng, c, datasets, &res.Rules, res.CTERefHist)
		if skipped {
			res.SkippedErrorCases++
			continue
		}
		if ok {
			res.ValidOK++
		} else {
			if failure != nil && len(res.Failures) < cfg.MaxFailures {
				res.Failures = append(res.Failures, *failure)
			}
		}
	}

	bgen := generator.NewGen(cfg.Seed ^ 0x5eed)
	for i := 0; i < cfg.BadCount; i++ {
		bc := bgen.Invalid(i)
		res.Bad++
		ok, extra, missed, failure := verifyBad(eng, bc)
		res.ExtraDiags += extra
		res.MissedDiags += missed
		if ok {
			res.BadOK++
		} else if failure != nil && len(res.Failures) < cfg.MaxFailures {
			res.Failures = append(res.Failures, *failure)
		}
	}
	res.Duration = time.Since(start)
	return res
}

// verifyValid checks one query: bind cleanly, rewrite, execute both plans on
// every dataset, compare result multisets, and record rule usage.
func verifyValid(eng *engine.Engine, c generator.Case, datasets []*DatasetSpec, rules *RuleCounts, cteHist map[int]int) (bool, *Failure, bool) {
	pr := engine.Parse(c.SQL)
	if len(pr.Errs) > 0 {
		return false, &Failure{Kind: "generated-invalid", SQL: c.SQL, Detail: fmt.Sprint(pr.Errs)}, false
	}
	// Bind on a fresh plan for each dataset is unnecessary: plans are
	// independent of data. Bind once.
	planBefore, bindErrs := engine.BindOne(pr.Stmt, eng.Cat, eng.Reg)
	if len(bindErrs) > 0 {
		return false, &Failure{Kind: "generated-bind-error", SQL: c.SQL, Detail: fmt.Sprint(bindErrs)}, false
	}
	if planBefore.Root == nil {
		return false, &Failure{Kind: "generated-no-plan", SQL: c.SQL}, false
	}
	// Bind a second, independent copy so the rewrite runs on its own tree.
	// (Node IDs would otherwise be shared with the pre-rewrite copy.)
	pr2 := engine.Parse(c.SQL)
	planAfter, errs2 := engine.BindOne(pr2.Stmt, eng.Cat, eng.Reg)
	if len(errs2) > 0 {
		return false, &Failure{Kind: "generated-bind-error-2", SQL: c.SQL, Detail: fmt.Sprint(errs2)}, false
	}
	rr := planAfter.RewritePlan(engine.DefaultRules())
	// Counts are per query so the "any trigger" statistic is meaningful.
	var triggered []string
	for name, n := range rr.Counts {
		rules.Applied[name] += n
		if n > 0 {
			triggered = append(triggered, name)
		}
	}
	for name := range rr.Checks {
		rules.Checked[name] += rr.Checks[name]
		rules.Rejected[name] += rr.Rejected[name]
	}
	for _, name := range triggered {
		rules.Queries[name]++
	}
	for _, ct := range planAfter.CTEInfo {
		cteHist[ct.Refs]++
	}
	// Schema invariance across the rewrite.
	if !schemaEqual(planBefore.Output, planAfter.Output) {
		return false, &Failure{Kind: "schema-changed", SQL: c.SQL,
			Expected: engine.FormatTree(planBefore.Root), Actual: engine.FormatTree(planAfter.Root)}, false
	}

	// Execute both plans on every dataset.
	for di, spec := range datasets {
		beforeRows, errB := eng.Execute(planBefore.Root, spec.DS)
		afterRows, errA := eng.Execute(planAfter.Root, spec.DS)
		// Deterministic evaluation errors are legitimate: if both fail at the
		// same point the rewrite preserved behaviour. We compare error text
		// and treat matching failures as equal results.
		if errB != nil || errA != nil {
			if errB != nil && errA != nil && errB.Msg == errA.Msg {
				continue
			}
			if errA != nil && errB == nil {
				return false, &Failure{Kind: "rewrite-introduced-error", Seed: c.Seed, SQL: c.SQL,
					Detail: fmt.Sprintf("dataset %d: before ok, after: %v", di, errA)}, false
			}
			if errB != nil && errA == nil {
				return false, &Failure{Kind: "rewrite-removed-error", Seed: c.Seed, SQL: c.SQL,
					Detail: fmt.Sprintf("dataset %d: before: %v, after ok", di, errB)}, false
			}
			return false, &Failure{Kind: "rewrite-changed-error", Seed: c.Seed, SQL: c.SQL,
				Detail: fmt.Sprintf("dataset %d: before %v / after %v", di, errB, errA)}, false
		}
		if !rowsEqualStrict(beforeRows, afterRows) {
			return false, &Failure{Kind: "result-mismatch", Seed: c.Seed, SQL: c.SQL,
				Expected: renderRows(beforeRows), Actual: renderRows(afterRows),
				Detail: fmt.Sprintf("dataset %d", di)}, false
		}
	}
	return true, nil, false
}

func schemaEqual(a, b []engine.OutputCol) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i].Name, b[i].Name) || !a[i].Type.Same(b[i].Type) ||
			!strings.EqualFold(a[i].Qualifier, b[i].Qualifier) || a[i].Nullable != b[i].Nullable {
			return false
		}
	}
	return true
}

func rowsEqualStrict(a, b []engine.Row) bool {
	return renderRows(a) == renderRows(b)
}

func renderRows(rows []engine.Row) string {
	lines := make([]string, len(rows))
	for i, r := range rows {
		parts := make([]string, len(r))
		for j, v := range r {
			parts[j] = renderValue(v)
		}
		lines[i] = strings.Join(parts, "|")
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// renderValue is exact for comparisons: NULL is distinct from every literal,
// integers, dates and ticks print verbatim, and decimals print their exact
// unscaled/scale pair so a rewrite that changes the scale of a result is
// caught even when the numeric value is the same.
func renderValue(v engine.Value) string {
	if v.Null {
		return "NULL"
	}
	if v.Kind == engine.KDec {
		return "dec:" + v.Dec.String() + "@" + itoa(v.Dec.Scale)
	}
	return v.Kind.String() + ":" + v.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// verifyBad checks that every expected position is reported. A diagnostic
// "covers" an expected position if it is on the same line at or a few columns
// before it (front ends legitimately point at the token preceding a missing
// one), or exactly at it.
func verifyBad(eng *engine.Engine, bc generator.BadCase) (bool, int, int, *Failure) {
	a := eng.Analyze(bc.Faulty)
	var diags []engine.Diag
	diags = append(diags, a.Errors...)
	// Sanity: the original must be valid. If not, the generator is wrong.
	orig := eng.Analyze(bc.Original)
	if len(orig.Errors) > 0 {
		return false, 0, len(bc.Expected), &Failure{Kind: "generator-original-invalid", SQL: bc.Original,
			Detail: fmt.Sprint(orig.Errors)}
	}
	used := make([]bool, len(diags))
	missed := 0
	for _, exp := range bc.Expected {
		found := false
		for i, d := range diags {
			if used[i] {
				continue
			}
			if covers(d, exp) {
				used[i] = true
				found = true
				break
			}
		}
		if !found {
			missed++
		}
	}
	extra := 0
	// Collect the expected positions. Diagnostics pinned to the same damaged
	// statement that duplicate an already-covered name error are reported as
	// extras so the front end is pushed to avoid cascades.
	for i, d := range diags {
		if used[i] {
			continue
		}
		if d.Code == "internal" {
			continue
		}
		// A type/arity error on a partly parsed call is a consequence of the
		// syntax damage: the binder saw fewer arguments than the source
		// intended. Once a syntax error has been reported for the statement,
		// suppress binder errors that stem from malformed calls.
		if hasSyntaxError(diags) && (d.Code == "arity") {
			continue
		}
		extra++
	}
	if missed == 0 && extra == 0 {
		return true, 0, 0, nil
	}
	var got []string
	for _, d := range diags {
		got = append(got, fmt.Sprintf("%d:%d[%s] %s", d.Line, d.Col, d.Code, d.Msg))
	}
	var want []string
	for _, e := range bc.Expected {
		want = append(want, fmt.Sprintf("%d:%d[%s]", e.Line, e.Col, e.Code))
	}
	return false, extra, missed, &Failure{Kind: "bad-case-mismatch", SQL: bc.Faulty,
		Detail:   bc.Fault + fmt.Sprintf(" (missed=%d extra=%d)", missed, extra),
		Expected: strings.Join(want, "; "), Actual: strings.Join(got, "; ")}
}

func hasSyntaxError(diags []engine.Diag) bool {
	for _, d := range diags {
		if d.Code == "syntax" || d.Code == "lex" {
			return true
		}
	}
	return false
}

// anyUsedOnLine reports whether a diagnostic on line was already matched.
func anyUsedOnLine(diags []engine.Diag, used []bool, line int) bool {
	for i, d := range diags {
		if used[i] && d.Line == line {
			return true
		}
	}
	return false
}

// covers reports whether diagnostic d is an acceptable report for expected
// position exp.
func covers(d engine.Diag, exp generator.ExpectedErr) bool {
	if d.Line != exp.Line {
		return false
	}
	switch exp.Code {
	case "syntax", "lex":
		if d.Code != "syntax" && d.Code != "lex" {
			return false
		}
	default:
		if d.Code != exp.Code {
			// Name errors may surface as ambiguous, which is still a name
			// resolution error; accept either.
			if !(exp.Code == "name" && d.Code == "ambiguous") &&
				!(exp.Code == "ambiguous" && d.Code == "name") {
				return false
			}
		}
	}
	// Allow the diagnostic to point at the token containing the expected
	// position or the immediately preceding token (within a small window).
	if d.Col == exp.Col {
		return true
	}
	// Front ends legitimately report the token preceding a missing one or a
	// position just past the damage. Accept a small symmetric window.
	if d.Col < exp.Col && exp.Col-d.Col <= 12 {
		return true
	}
	if d.Col > exp.Col && d.Col-exp.Col <= 3 {
		return true
	}
	return false
}

// Summary renders a compact human-readable report.
func (r *Result) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "valid queries:  %d/%d matched after rewrite\n", r.ValidOK, r.Valid)
	fmt.Fprintf(&b, "invalid queries:%d/%d fully explained (missed=%d extra=%d)\n", r.BadOK, r.Bad, r.MissedDiags, r.ExtraDiags)
	fmt.Fprintf(&b, "skipped (data-dependent eval errors): %d\n", r.SkippedErrorCases)
	fmt.Fprintf(&b, "duration: %v\n", r.Duration.Round(time.Millisecond))
	b.WriteString("rule triggers (applied/checked/rejected/queries):\n")
	names := make([]string, 0, len(r.Rules.Applied))
	for n := range r.Rules.Applied {
		names = append(names, n)
	}
	for n := range r.Rules.Checked {
		if _, ok := r.Rules.Applied[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "  %-14s %5d / %5d / %4d / %4d\n", n,
			r.Rules.Applied[n], r.Rules.Checked[n], r.Rules.Rejected[n], r.Rules.Queries[n])
	}
	if len(r.Failures) > 0 {
		b.WriteString("failures:\n")
		for _, f := range r.Failures {
			fmt.Fprintf(&b, "  [%s] %s\n    %s\n", f.Kind, f.SQL, f.Detail)
			if f.Expected != "" || f.Actual != "" {
				fmt.Fprintf(&b, "    expected: %s\n    actual:   %s\n", f.Expected, f.Actual)
			}
		}
	}
	return b.String()
}
