package engine

import (
	"strings"
	"testing"
)

// TestFormatTreeBranchGlyphs pins the tree rendering: every node line carries
// its output schema, which is what makes the rewrite trace reviewable.
func TestFormatTreeBranchGlyphs(t *testing.T) {
	e := NewEngine()
	pr := Parse("SELECT lot_id FROM lots WHERE stems > 1")
	plan, errs := BindOne(pr.Stmt, e.Cat, e.Reg)
	if len(errs) > 0 {
		t.Fatalf("%v", errs)
	}
	tree := FormatTree(plan.Root)
	if !strings.Contains(tree, "└─") {
		t.Errorf("tree is missing branch glyphs:\n%s", tree)
	}
	if !strings.Contains(tree, "⇒") {
		t.Errorf("tree is missing schema arrows:\n%s", tree)
	}
	// Every node line carries its output schema.
	for _, line := range strings.Split(strings.TrimSpace(tree), "\n") {
		if !strings.Contains(line, "⇒ (") {
			t.Errorf("node line without a schema: %q", line)
		}
	}
}

// TestSchemaMarkerGuardRejectsUnsafeRule verifies the driver's safety net: a
// rule that changes output names is discarded and reported.
func TestSchemaMarkerGuardRejectsUnsafeRule(t *testing.T) {
	e := NewEngine()
	pr := Parse("SELECT lot_id FROM lots")
	plan, errs := BindOne(pr.Stmt, e.Cat, e.Reg)
	if len(errs) > 0 {
		t.Fatalf("%v", errs)
	}
	before := append([]OutputCol(nil), plan.Output...)
	rr := plan.RewritePlan([]Rule{renamingRule{}})
	if !schemaEqual(before, rr.Root.Schema) {
		t.Fatalf("unsafe rule's change survived: %v", rr.Root.Schema)
	}
	found := false
	for _, w := range rr.Warnings {
		if strings.Contains(w.Msg, "changed the output schema") {
			found = true
		}
	}
	if !found {
		t.Errorf("no warning for the discarded unsafe rewrite: %v", rr.Warnings)
	}
}

// renamingRule deliberately violates the output contract by renaming a
// column of the plan's root projection.
type renamingRule struct{}

func (renamingRule) Name() string { return "unsafe" }
func (renamingRule) Doc() string  { return "test rule that renames output columns" }
func (renamingRule) Apply(n *PlanNode, rc *RuleCtx) *PlanNode {
	if n.Op != OpProject {
		return nil
	}
	cp := *n
	cp.Schema = append([]OutputCol(nil), n.Schema...)
	cp.Schema[0].Name = "renamed"
	return &cp
}
