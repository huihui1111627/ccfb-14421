// Package engine implements ClockQL: a hand-written SQL front end for a Dutch
// flower auction. It lexes, parses, binds names and types, lowers to a logical
// plan, rewrites the plan to a fixpoint, and can execute plans with a naive
// reference interpreter used to verify rewrites.
package engine

// Analysis is one statement's full front-end result, shaped for the UI.
type Analysis struct {
	Source     string       `json:"source"`
	Tokens     []TokenInfo  `json:"tokens"`
	AST        string       `json:"ast"`
	Bound      string       `json:"bound"`
	Schema     []ColumnInfo `json:"schema"`
	PlanBefore string       `json:"planBefore"`
	PlanAfter  string       `json:"planAfter"`
	Passes     []PassInfo   `json:"passes"`
	Rules      []RuleStat   `json:"rules"`
	Warnings   []Diag       `json:"warnings"`
	Errors     []Diag       `json:"errors"`
	CTEs       []CTEStat    `json:"ctes"`
}

type TokenInfo struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
}

type ColumnInfo struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Qualifier string `json:"qualifier"`
	Nullable  bool   `json:"nullable"`
}

type PassInfo struct {
	Pass  int    `json:"pass"`
	Tree  string `json:"tree"`
	Steps int    `json:"steps"`
}

type RuleStat struct {
	Name     string   `json:"name"`
	Doc      string   `json:"doc"`
	Applied  int      `json:"applied"`
	Checks   int      `json:"checks"`
	Rejected int      `json:"rejected"`
	Before   string   `json:"before"`
	After    string   `json:"after"`
	Cases    []string `json:"cases"`
}

type CTEStat struct {
	Name string   `json:"name"`
	Cols []string `json:"cols"`
	Refs int      `json:"refs"`
	Plan string   `json:"plan"`
}

// Diag is a user-facing diagnostic.
type Diag struct {
	Code   string   `json:"code"`
	Line   int      `json:"line"`
	Col    int      `json:"col"`
	EndCol int      `json:"endCol,omitempty"`
	Msg    string   `json:"msg"`
	Hints  []string `json:"hints,omitempty"`
	Phase  string   `json:"phase"`
}

func diagOf(e *Error, phase string) Diag {
	return Diag{Code: string(e.Code), Line: e.Pos.Line, Col: e.Pos.Col, Msg: e.Msg, Hints: e.Hints, Phase: phase}
}

// Engine bundles the catalog and registry.
type Engine struct {
	Cat *Catalog
	Reg *Registry
}

func NewEngine() *Engine { return &Engine{Cat: AuctionCatalog(), Reg: DefaultRegistry()} }

// ParseOnly lexes and parses without binding.
func (e *Engine) ParseOnly(src string) *ParseResult { return Parse(src) }

// Analyze runs the whole front end and prepares the explain payload. It never
// returns a Go error: diagnostics are part of the result. If parsing or
// binding produced errors, later phases still run on the best-effort tree so
// the UI can show partial progress, but Type says how far it got.
func (e *Engine) Analyze(src string) *Analysis {
	a := &Analysis{Source: src}
	pr := Parse(src)
	for _, t := range pr.Tokens() {
		a.Tokens = append(a.Tokens, TokenInfo{Kind: t.Kind.String(), Text: t.Text, Line: t.Pos.Line, Col: t.Pos.Col})
	}
	if pr.Stmt != nil {
		a.AST = pr.Stmt.String()
	}
	for _, er := range pr.Errs {
		a.Errors = append(a.Errors, diagOf(er, "parse"))
	}
	if pr.Stmt == nil {
		return a
	}
	var plan *Plan
	var bindErrs []*Error
	if len(pr.Errs) > 0 {
		plan, bindErrs = BindAfterErrors(pr.Stmt, e.Cat, e.Reg, pr.DamageLine, pr.DamageCol)
	} else {
		plan, bindErrs = BindOne(pr.Stmt, e.Cat, e.Reg)
	}
	for _, er := range bindErrs {
		a.Errors = append(a.Errors, diagOf(er, "bind"))
	}
	a.Errors = dedupeDiags(a.Errors)
	if plan == nil || plan.Root == nil {
		return a
	}
	a.Bound = plan.Root.label()
	a.Schema = schemaInfo(plan.Output)
	for _, c := range plan.Output {
		_ = c
	}
	a.PlanBefore = FormatTree(plan.Root)
	// CTE info
	for _, info := range plan.CTEInfo {
		a.CTEs = append(a.CTEs, CTEStat{
			Name: info.Name, Cols: info.Cols, Refs: info.Refs, Plan: FormatTree(info.Plan),
		})
	}
	rr := plan.RewritePlan(DefaultRules())
	a.PlanAfter = FormatTree(rr.Root)
	for _, s := range rr.Snapshots {
		a.Passes = append(a.Passes, PassInfo{Pass: s.Pass, Tree: s.Tree, Steps: s.Steps})
	}
	a.Rules = ruleStats(rr)
	for _, w := range rr.Warnings {
		a.Warnings = append(a.Warnings, diagOf(w, "rewrite"))
	}
	return a
}

// dedupeDiags collapses diagnostics that share a position. A single damaged
// token should produce one message, not a parser complaint plus its binder
// echo; the most specific phase (parse, then bind) wins.
func dedupeDiags(in []Diag) []Diag {
	out := make([]Diag, 0, len(in))
	seen := map[[2]int]bool{}
	for _, d := range in {
		k := [2]int{d.Line, d.Col}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, d)
	}
	return out
}

func schemaInfo(s []OutputCol) []ColumnInfo {
	out := make([]ColumnInfo, len(s))
	for i, c := range s {
		out[i] = ColumnInfo{Name: c.Name, Type: c.Type.String(), Qualifier: c.Qualifier, Nullable: c.Nullable}
	}
	return out
}

func ruleStats(rr *RewriteResult) []RuleStat {
	var out []RuleStat
	for _, r := range DefaultRules() {
		rs := RuleStat{
			Name: r.Name(), Doc: r.Doc(),
			Applied: rr.Counts[r.Name()], Checks: rr.Checks[r.Name()], Rejected: rr.Rejected[r.Name()],
		}
		for _, st := range rr.Steps {
			if st.Rule != r.Name() {
				continue
			}
			if rs.Before == "" && st.Changed > 0 {
				rs.Before = st.Before
				rs.After = st.After
			}
			for _, ch := range st.Changes {
				if len(rs.Cases) < 6 {
					rs.Cases = append(rs.Cases, ch.Before+" ⇒ "+ch.After+(func() string {
						if ch.Note != "" {
							return " (" + ch.Note + ")"
						}
						return ""
					})())
				}
			}
		}
		out = append(out, rs)
	}
	return out
}

// Tokens exposes the token stream alongside the parse result.
func (pr *ParseResult) Tokens() []Token { return pr.toks }

// Execute runs a plan on the given dataset with the reference interpreter.
func (e *Engine) Execute(p *PlanNode, ds *Dataset) ([]Row, *Error) {
	return NewEvaluator(ds, e.Reg).Run(p)
}
