// Command server runs the ClockQL inspection UI: parse, bind, rewrite and
// execute a query, showing every stage.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"clockql/internal/engine"
	"clockql/internal/verify"
	"clockql/web"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8765", "listen address")
	verifyJSON := flag.String("verify-json", "", "optional verification summary JSON to display")
	flag.Parse()

	srv := &server{eng: engine.NewEngine()}
	if *verifyJSON != "" {
		srv.verify = loadVerify(*verifyJSON)
	}

	http.HandleFunc("/", srv.index)
	http.HandleFunc("/api/analyze", srv.analyze)
	http.HandleFunc("/api/run", srv.run)
	http.HandleFunc("/api/schema", srv.schema)
	http.HandleFunc("/api/verify", srv.verifySummary)
	log.Printf("ClockQL inspector on http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

type server struct {
	eng    *engine.Engine
	verify *verify.Result
	mu     sync.Mutex
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(web.FS, "index.html")
	if err != nil {
		http.Error(w, "index.html: "+err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func (s *server) analyze(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SQL string `json:"sql"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	a := s.eng.Analyze(req.SQL)
	writeJSON(w, a)
}

// run executes the query before and after rewriting on generated auction data
// and reports whether the results agree.
type runResponse struct {
	OK          bool                `json:"ok"`
	Error       string              `json:"error,omitempty"`
	Before      [][]string          `json:"before"`
	After       [][]string          `json:"after"`
	Columns     []engine.ColumnInfo `json:"columns"`
	Equal       bool                `json:"equal"`
	RowCount    int                 `json:"rowCount"`
	BeforeTree  string              `json:"beforeTree"`
	AfterTree   string              `json:"afterTree"`
	ExecError   string              `json:"execError,omitempty"`
	AfterError  string              `json:"afterError,omitempty"`
	SchemaEqual bool                `json:"schemaEqual"`
}

func (s *server) run(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SQL string `json:"sql"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	resp := runResponse{}
	pr := engine.Parse(req.SQL)
	if len(pr.Errs) > 0 {
		resp.OK = false
		resp.Error = fmt.Sprint(pr.Errs)
		writeJSON(w, resp)
		return
	}
	var before *engine.Plan
	var errs []*engine.Error
	if len(pr.Errs) > 0 {
		// Show whatever partial plan exists, but do not execute a damaged
		// statement.
		before, errs = engine.BindAfterErrors(pr.Stmt, s.eng.Cat, s.eng.Reg, pr.DamageLine, pr.DamageCol)
		if before == nil || before.Root == nil {
			before, errs = engine.BindOne(pr.Stmt, s.eng.Cat, s.eng.Reg)
		}
		resp.OK = false
		resp.Error = "语句包含语法错误，未执行：" + pr.Errs[0].Error()
		if before != nil && before.Root != nil {
			resp.BeforeTree = engine.FormatTree(before.Root)
		}
		writeJSON(w, resp)
		return
	}
	before, errs = engine.BindOne(pr.Stmt, s.eng.Cat, s.eng.Reg)
	if len(errs) > 0 || before.Root == nil {
		resp.OK = false
		resp.Error = fmt.Sprint(errs)
		writeJSON(w, resp)
		return
	}
	pr2 := engine.Parse(req.SQL)
	after, _ := engine.BindOne(pr2.Stmt, s.eng.Cat, s.eng.Reg)
	rr := after.RewritePlan(engine.DefaultRules())

	spec := verify.GenerateData(424242, verify.DefaultDataOptions())
	s.mu.Lock()
	rowsBefore, eb := s.eng.Execute(before.Root, spec.DS)
	rowsAfter, ea := s.eng.Execute(after.Root, spec.DS)
	s.mu.Unlock()

	resp.OK = true
	resp.BeforeTree = engine.FormatTree(before.Root)
	resp.AfterTree = engine.FormatTree(after.Root)
	resp.Columns = columnInfo(before.Output)
	resp.SchemaEqual = sameSchema(before.Output, after.Output)
	if eb != nil {
		resp.ExecError = eb.Error()
	} else {
		resp.Before = renderTable(rowsBefore)
		resp.RowCount = len(rowsBefore)
	}
	if ea != nil {
		resp.AfterError = ea.Error()
	} else {
		resp.After = renderTable(rowsAfter)
	}
	resp.Equal = eb == nil && ea == nil && engine.RowsEqual(rowsBefore, rowsAfter)
	_ = rr
	writeJSON(w, resp)
}

func columnInfo(o []engine.OutputCol) []engine.ColumnInfo {
	out := make([]engine.ColumnInfo, len(o))
	for i, c := range o {
		out[i] = engine.ColumnInfo{Name: c.Name, Type: c.Type.String(), Qualifier: c.Qualifier, Nullable: c.Nullable}
	}
	return out
}

func sameSchema(a, b []engine.OutputCol) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i].Name, b[i].Name) || !a[i].Type.Same(b[i].Type) {
			return false
		}
	}
	return true
}

func renderTable(rows []engine.Row) [][]string {
	const limit = 200
	out := make([][]string, 0, min(len(rows), limit))
	for i, r := range rows {
		if i >= limit {
			break
		}
		row := make([]string, len(r))
		for j, v := range r {
			row[j] = v.String()
		}
		out = append(out, row)
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (s *server) schema(w http.ResponseWriter, r *http.Request) {
	type col struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Null bool   `json:"nullable"`
	}
	type tab struct {
		Name string `json:"name"`
		Note string `json:"note"`
		Cols []col  `json:"cols"`
	}
	var out struct {
		Tables []tab    `json:"tables"`
		Funcs  []string `json:"functions"`
	}
	for _, t := range s.eng.Cat.Tables() {
		tc := tab{Name: t.Name, Note: t.Note}
		for _, c := range t.Columns {
			tc.Cols = append(tc.Cols, col{Name: c.Name, Type: c.Type.String(), Null: c.Nullable})
		}
		out.Tables = append(out.Tables, tc)
	}
	out.Funcs = s.eng.Reg.Names()
	writeJSON(w, out)
}

func (s *server) verifySummary(w http.ResponseWriter, r *http.Request) {
	if s.verify == nil {
		writeJSON(w, map[string]any{"available": false})
		return
	}
	writeJSON(w, map[string]any{"available": true, "result": s.verify})
}

func loadVerify(path string) *verify.Result {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("verify json: %v", err)
		return nil
	}
	var r verify.Result
	if err := json.Unmarshal(b, &r); err != nil {
		log.Printf("verify json: %v", err)
		return nil
	}
	return &r
}
