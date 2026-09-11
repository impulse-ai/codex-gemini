package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type WorkflowInput struct {
	Task           Task  `json:"task"`
	MaxTokens      int64 `json:"max_tokens,omitempty"`
	TimeoutSeconds int   `json:"timeout_seconds,omitempty"`
}
type Workflow struct {
	ID         string        `json:"id"`
	Status     string        `json:"status"`
	Phase      string        `json:"phase"`
	Error      string        `json:"error,omitempty"`
	Input      WorkflowInput `json:"input"`
	Jobs       []string      `json:"jobs"`
	UsedTokens int64         `json:"used_tokens"`
	Created    time.Time     `json:"created"`
	Updated    time.Time     `json:"updated"`
	cancel     context.CancelFunc
}

func workflowSnapshot(w *Workflow) Workflow {
	r := *w
	r.cancel = nil
	r.Jobs = append([]string(nil), w.Jobs...)
	b, _ := json.Marshal(w.Input)
	r.Input = WorkflowInput{}
	_ = json.Unmarshal(b, &r.Input)
	r.Input.Task.Prompt = ""
	return r
}
func (m *Manager) saveWorkflow(w *Workflow) error {
	w.Updated = time.Now().UTC()
	dir := filepath.Join(m.state, "workflows")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	b, err := json.Marshal(w)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".workflow-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, w.ID+".json"))
}
func (m *Manager) loadWorkflows() error {
	paths, err := filepath.Glob(filepath.Join(m.state, "workflows", "*.json"))
	if err != nil {
		return err
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var w Workflow
		if err = json.Unmarshal(b, &w); err != nil {
			return err
		}
		if !validID(w.ID) {
			return fmt.Errorf("invalid saved workflow ID")
		}
		if active(w.Status) {
			w.Status = "interrupted"
			w.Error = "Service stopped; inspect phase jobs before starting new work."
			w.UsedTokens = 0
			for _, id := range w.Jobs {
				if j := m.jobs[id]; j != nil {
					w.UsedTokens += j.Usage.Total
				}
			}
			if err = m.saveWorkflow(&w); err != nil {
				return err
			}
		}
		m.workflows[w.ID] = &w
	}
	return nil
}
func (m *Manager) StartWorkflow(in WorkflowInput) (Workflow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return Workflow{}, m.ctx.Err()
	}
	if len(in.Task.WritePaths) == 0 {
		return Workflow{}, fmt.Errorf("workflow implementation requires explicit write_paths")
	}
	if in.Task.Intent != "" && in.Task.Intent != "implementation" {
		return Workflow{}, fmt.Errorf("workflow requires implementation intent; use ordinary tasks for read-only investigation or review")
	}
	if in.Task.MaxTokens > 0 && in.Task.MaxTokens < 1000 {
		return Workflow{}, fmt.Errorf("per-phase token cap must be at least 1000")
	}
	in.Task.Intent = "implementation"
	task, err := m.validateTask(in.Task)
	if err != nil {
		return Workflow{}, err
	}
	in.Task = task
	if in.MaxTokens == 0 {
		in.MaxTokens = m.cfg.MaxTokens
	}
	if in.MaxTokens < 12000 || in.MaxTokens > m.cfg.MaxTokens {
		return Workflow{}, fmt.Errorf("workflow max_tokens must be 12000 through the service limit")
	}
	if in.TimeoutSeconds == 0 {
		in.TimeoutSeconds = int(m.cfg.Timeout.Seconds())
	}
	if in.TimeoutSeconds < 1 || time.Duration(in.TimeoutSeconds)*time.Second > m.cfg.Timeout {
		return Workflow{}, fmt.Errorf("workflow timeout must fit the service timeout")
	}
	running := 0
	for _, w := range m.workflows {
		if active(w.Status) {
			running++
		}
	}
	if running >= 30 {
		return Workflow{}, fmt.Errorf("at most 30 active workflows")
	}
	w := &Workflow{ID: newID(), Status: "queued", Input: in, Created: time.Now().UTC()}
	ctx, cancel := context.WithTimeout(m.ctx, time.Duration(in.TimeoutSeconds)*time.Second)
	w.cancel = cancel
	if err = m.saveWorkflow(w); err != nil {
		cancel()
		return Workflow{}, err
	}
	m.workflows[w.ID] = w
	m.wg.Add(1)
	go m.runWorkflow(ctx, w)
	return workflowSnapshot(w), nil
}
func (m *Manager) GetWorkflow(id string) (Workflow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.workflows[id]
	if w == nil {
		return Workflow{}, fmt.Errorf("unknown workflow")
	}
	return workflowSnapshot(w), nil
}
func (m *Manager) CancelWorkflow(id string) (Workflow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.workflows[id]
	if w == nil {
		return Workflow{}, fmt.Errorf("unknown workflow")
	}
	if w.cancel != nil && active(w.Status) {
		w.cancel()
	}
	return workflowSnapshot(w), nil
}
func (m *Manager) runWorkflow(ctx context.Context, w *Workflow) {
	defer m.wg.Done()
	defer w.cancel()
	finish := func(status string, err error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		w.Status = status
		if err != nil {
			w.Error = err.Error()
		}
		if e := m.saveWorkflow(w); e != nil {
			w.Error += "; save failed: " + e.Error()
		}
	}
	sources := []string{}
	for i, phase := range []string{"investigation", "implementation", "review"} {
		if err := ctx.Err(); err != nil {
			finish("cancelled", err)
			return
		}
		m.mu.Lock()
		remaining := w.Input.MaxTokens - w.UsedTokens
		task := w.Input.Task
		budget := remaining
		if i == 0 {
			budget = w.Input.MaxTokens / 4
		}
		if i == 1 {
			budget = remaining - w.Input.MaxTokens/4
		}
		if task.MaxTokens > 0 {
			budget = min(budget, task.MaxTokens)
		}
		w.Status = "running"
		w.Phase = phase
		err := m.saveWorkflow(w)
		m.mu.Unlock()
		if err != nil {
			finish("failed", err)
			return
		}
		if budget < 1000 {
			finish("limit_reached", fmt.Errorf("workflow budget cannot fund %s", phase))
			return
		}
		task.workflowID = w.ID
		task.MaxTokens = budget
		task.Intent = phase
		task.Label = phase + ":" + w.ID
		if phase != "implementation" {
			task.WritePaths = nil
		}
		guidance := map[string]string{"investigation": "Investigate only the narrow assigned change. Return file references, a concrete implementation brief and acceptance criteria; preserve uncertainties. Do not edit or expand scope.", "implementation": "Implement the assigned change from the investigation. Inspect only necessary current files, then make actual edits. If blocked, report exact missing information. Do not stop at a plan.", "review": "Independently inspect the actual implementation against the original task and acceptance criteria. Report concrete findings and unverified behavior. Do not edit. You cannot run tests; Codex will validate."}
		task.Prompt = w.Input.Task.Prompt + "\n\nWorkflow phase: " + phase + "\n" + guidance[phase]
		var job Job
		if len(sources) == 0 {
			var jobs []Job
			jobs, err = m.Spawn([]Task{task})
			if err == nil {
				job = jobs[0]
			}
		} else {
			job, err = m.Handoff(HandoffInput{Sources: sources, Task: task})
		}
		if err != nil {
			finish("failed", err)
			return
		}
		m.mu.Lock()
		w.Jobs = append(w.Jobs, job.ID)
		err = m.saveWorkflow(w)
		child := m.jobs[job.ID]
		m.mu.Unlock()
		if err != nil || ctx.Err() != nil {
			_, _ = m.Cancel(job.ID)
		}
		// Wait for actual child shutdown before accounting or releasing the workflow.
		select {
		case <-child.done:
		case <-ctx.Done():
			_, _ = m.Cancel(job.ID)
			<-child.done
		}
		job, getErr := m.Get(job.ID)
		m.mu.Lock()
		w.UsedTokens += job.Usage.Total
		saveErr := m.saveWorkflow(w)
		m.mu.Unlock()
		if err != nil {
			finish("failed", err)
			return
		}
		if getErr != nil {
			finish("failed", getErr)
			return
		}
		if saveErr != nil {
			finish("failed", saveErr)
			return
		}
		if ctx.Err() != nil {
			finish("cancelled", ctx.Err())
			return
		}
		if job.Status != "completed" {
			finish("needs_attention", fmt.Errorf("%s stopped with %s: %s", phase, job.Status, job.Error))
			return
		}
		if phase == "implementation" && len(job.Changed) == 0 {
			finish("needs_attention", fmt.Errorf("implementation completed without edits; inspect findings before proceeding"))
			return
		}
		if w.UsedTokens >= w.Input.MaxTokens {
			finish("limit_reached", fmt.Errorf("workflow token budget reached"))
			return
		}
		sources = append(sources, job.ID)
	}
	finish("ready_for_validation", nil)
}
