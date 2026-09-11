package worker

import (
	"context"
	"google.golang.org/genai"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func awaitWorkflow(t *testing.T, m *Manager, id string) Workflow {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		w, err := m.GetWorkflow(id)
		if err != nil {
			t.Fatal(err)
		}
		if !active(w.Status) {
			return w
		}
		select {
		case <-deadline:
			t.Fatal("workflow timeout")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
func TestWorkflowPhasesAndBudget(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.MaxTokens = 100000
	var calls atomic.Int32
	g := generateFunc(func(ctx context.Context, h []*genai.Content, c *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		n := calls.Add(1)
		if n == 1 {
			return response(&genai.Part{Text: "Plan: write out.txt with done. Acceptance: file contains done."}), nil
		}
		hasWrite := false
		for _, tool := range c.Tools {
			for _, d := range tool.FunctionDeclarations {
				if d.Name == "write_file" {
					hasWrite = true
				}
			}
		}
		if n == 2 {
			if !hasWrite {
				t.Error("implementation missing write permission")
			}
			return response(&genai.Part{FunctionCall: &genai.FunctionCall{ID: "write", Name: "write_file", Args: map[string]any{"path": "out.txt", "content": "done", "expected_sha256": "new"}}}), nil
		}
		if n == 3 {
			return response(&genai.Part{Text: "Wrote out.txt"}), nil
		}
		if hasWrite {
			t.Error("review has write permission")
		}
		return response(&genai.Part{Text: "Review done, tests not run"}), nil
	})
	m := newManager(t, cfg, g)
	w, err := m.StartWorkflow(WorkflowInput{Task: Task{Prompt: "Write out.txt", WritePaths: []string{"out.txt"}}})
	if err != nil {
		t.Fatal(err)
	}
	done := awaitWorkflow(t, m, w.ID)
	if done.Status != "ready_for_validation" || len(done.Jobs) != 3 || done.UsedTokens != 60 {
		t.Fatalf("workflow %+v", done)
	}
	for i, id := range done.Jobs {
		j, _ := m.Get(id)
		if (i != 1 && len(j.Task.WritePaths) != 0) || j.WorkflowID != w.ID {
			t.Fatalf("phase ownership %+v", j)
		}
		if _, err = m.Continue(id, "again"); err == nil {
			t.Fatal("workflow phase continuation resets budget")
		}
	}
	b, err := os.ReadFile(filepath.Join(cfg.Workspace, "out.txt"))
	if err != nil || string(b) != "done" {
		t.Fatalf("missing edit %s %v", b, err)
	}
	snapshot, _ := m.GetWorkflow(w.ID)
	snapshot.Jobs[0] = "mutated"
	snapshot.Input.Task.WritePaths[0] = "mutated"
	again, _ := m.GetWorkflow(w.ID)
	if again.Jobs[0] == "mutated" || again.Input.Task.WritePaths[0] == "mutated" {
		t.Fatal("mutable snapshot")
	}
}
func TestWorkflowFailureAndCancellation(t *testing.T) {
	for _, cancelTest := range []bool{false, true} {
		t.Run(map[bool]string{false: "failure", true: "cancel"}[cancelTest], func(t *testing.T) {
			cfg := testConfig(t.TempDir())
			entered := make(chan struct{})
			g := generateFunc(func(ctx context.Context, h []*genai.Content, c *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
				close(entered)
				if cancelTest {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return nil, genai.APIError{Code: 403, Message: "blocked"}
			})
			m := newManager(t, cfg, g)
			w, err := m.StartWorkflow(WorkflowInput{Task: Task{Prompt: "change", WritePaths: []string{"x"}}})
			if err != nil {
				t.Fatal(err)
			}
			<-entered
			if cancelTest {
				_, _ = m.CancelWorkflow(w.ID)
			}
			done := awaitWorkflow(t, m, w.ID)
			if len(done.Jobs) != 1 || done.Status == "ready_for_validation" {
				t.Fatalf("advanced after failed phase %+v", done)
			}
			j, _ := m.Get(done.Jobs[0])
			if active(j.Status) {
				t.Fatal("orphan active child")
			}
		})
	}
}
func TestWorkflowRestoresInterruptedWithoutInference(t *testing.T) {
	cfg := testConfig(t.TempDir())
	var calls atomic.Int32
	g := generateFunc(func(context.Context, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		calls.Add(1)
		return response(&genai.Part{Text: "unexpected"}), nil
	})
	m, err := New(context.Background(), cfg, g)
	if err != nil {
		t.Fatal(err)
	}
	w := &Workflow{ID: newID(), Status: "running", Input: WorkflowInput{Task: Task{Prompt: "original", WritePaths: []string{"x"}}}}
	if err = m.saveWorkflow(w); err != nil {
		t.Fatal(err)
	}
	m.Close()
	m, err = New(context.Background(), cfg, g)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	restored, err := m.GetWorkflow(w.ID)
	if err != nil || restored.Status != "interrupted" || calls.Load() != 0 {
		t.Fatalf("unsafe restart %+v %v", restored, err)
	}
	if !strings.Contains(restored.Error, "inspect") {
		t.Fatal("missing recovery guidance")
	}
}

func TestWorkflowBudgetOvershootStopsBeforeNextPhase(t *testing.T) {
	cfg := testConfig(t.TempDir())
	var calls atomic.Int32
	g := generateFunc(func(context.Context, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		calls.Add(1)
		r := checkpointResponse(Checkpoint{Summary: "plan", Complete: true})
		r.UsageMetadata.TotalTokenCount = 13000
		return r, nil
	})
	m := newManager(t, cfg, g)
	w, err := m.StartWorkflow(WorkflowInput{Task: Task{Prompt: "write", WritePaths: []string{"x"}}, MaxTokens: 12000})
	if err != nil {
		t.Fatal(err)
	}
	done := awaitWorkflow(t, m, w.ID)
	if done.Status != "limit_reached" || calls.Load() != 1 || done.UsedTokens != 13000 {
		t.Fatalf("overspend continued %+v", done)
	}
}
