package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"

	"reimburse/internal/audit"
	"reimburse/internal/llm"
	"reimburse/internal/schema"
	"reimburse/internal/task"
	"reimburse/plugins/reimburse-audit/policy"
)

// The contest uses only bundled synthetic fixtures and its own in-memory task store.
type contestDemo struct {
	base, municipal        *Server
	comparison, supplement schema.Case
}

func newContestDemo(host *Server) *contestDemo {
	c01, ok01 := host.Find("C01")
	c44, ok44 := host.Find("C44")
	if !ok01 || !ok44 || !c01.HasTruth || !c44.HasTruth {
		return nil
	}
	base, err := policy.Load()
	if err != nil {
		return nil
	}
	municipal, err := policy.LoadMunicipalDemo()
	if err != nil {
		return nil
	}
	tasks, err := task.Open("")
	if err != nil {
		return nil
	}
	demo := &contestDemo{
		base:       &Server{RF: base},
		municipal:  &Server{RF: municipal, Tasks: tasks},
		comparison: *c01, supplement: *c44,
	}
	demo.comparison.ID = "D01"
	demo.comparison.Claim.ClaimNo = "DEMO-001"
	demo.comparison.Claim.SubmitDate = "2026-10-28"
	demo.supplement.ID = "D02"
	demo.supplement.Claim.ClaimNo = "DEMO-002"
	return demo
}

func contestAudit(ctx context.Context, rf *policy.File, c *schema.Case) (*audit.Report, error) {
	ex, closeTools, err := connect(ctx, "", "", c, "", nil)
	if err != nil {
		return nil, err
	}
	defer closeTools()
	extractor := &llm.SyntheticLLM{Case: c, Synth: &llm.Synthetic{}}
	return audit.Run(audit.New(rf), ctx, &c.Claim, c.InvoiceText, c.SupportText, ex,
		&audit.Reviewer{LLM: extractor}, audit.Options{}), nil
}

func contestResult(rf *policy.File, rep *audit.Report) map[string]any {
	return map[string]any{
		"version": rf.Policy.Version, "deadline_days": rf.Params.ReimburseDeadlineDays,
		"result": rep.Result, "opinion": rep.Opinion,
	}
}

func (d *contestDemo) compare(w http.ResponseWriter, r *http.Request) {
	base, err := contestAudit(r.Context(), d.base.RF, &d.comparison)
	if err != nil {
		jsonOut(w, map[string]any{"error": "默认制度审核失败，请重试"}, 500)
		return
	}
	municipal, err := contestAudit(r.Context(), d.municipal.RF, &d.comparison)
	if err != nil {
		jsonOut(w, map[string]any{"error": "市政国企制度审核失败，请重试"}, 500)
		return
	}
	jsonOut(w, map[string]any{
		"default": contestResult(d.base.RF, base), "municipal": contestResult(d.municipal.RF, municipal),
	}, 200)
}

func (d *contestDemo) start(w http.ResponseWriter, r *http.Request) {
	rep, err := contestAudit(r.Context(), d.municipal.RF, &d.supplement)
	if err != nil {
		jsonOut(w, map[string]any{"error": "首轮审核失败，请重试"}, 500)
		return
	}
	t, err := d.municipal.Tasks.Create(rand.Text(), d.supplement.Claim.ClaimNo, d.supplement.Claim.Applicant.EmployeeID)
	if err != nil {
		jsonOut(w, map[string]any{"error": "无法创建演示任务"}, 500)
		return
	}
	d.municipal.applyReportToTask(t.ID, rep, &d.supplement)
	t, _ = d.municipal.Tasks.Get(t.ID)
	jsonOut(w, map[string]any{"task": t, "audit": contestResult(d.municipal.RF, rep)}, 200)
}

func (d *contestDemo) resume(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TaskID  string `json:"task_id"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil || strings.TrimSpace(req.Content) == "" {
		jsonOut(w, map[string]any{"error": "请填写行程单正文"}, 400)
		return
	}
	t, _, err := d.municipal.Tasks.ResubmitIdem(req.TaskID, "support", "铁路行程单", "演示报销人", "", req.Content)
	if err != nil {
		jsonOut(w, map[string]any{"error": err.Error()}, 400)
		return
	}
	t, err = d.municipal.Tasks.BeginAudit(t.ID)
	if err != nil {
		jsonOut(w, map[string]any{"error": err.Error()}, 409)
		return
	}
	effective, _ := applySupplements(&d.supplement, t)
	rep, err := contestAudit(r.Context(), d.municipal.RF, effective)
	if err != nil {
		d.municipal.releaseTask(t.ID)
		jsonOut(w, map[string]any{"error": "续审失败，请重试"}, 500)
		return
	}
	d.municipal.applyReportToTask(t.ID, rep, effective)
	t, _ = d.municipal.Tasks.Get(t.ID)
	jsonOut(w, map[string]any{"task": t, "audit": contestResult(d.municipal.RF, rep)}, 200)
}
