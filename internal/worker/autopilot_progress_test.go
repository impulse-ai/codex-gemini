package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/genai"
)

func TestAutopilotRunsWithoutCompulsoryCompaction(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	cfg.MaxSteps = 25
	cfg.MaxTokens = 200000
	cfg.MaxOutput = 1024

	var calls atomic.Int32
	g := generateFunc(func(ctx context.Context, h []*genai.Content, c *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		if c.ResponseMIMEType == "application/json" {
			return checkpointResponse(Checkpoint{
				Summary:   "Incomplete checkpoint",
				Remaining: []string{"result.txt"},
				Complete:  false,
			}), nil
		}
		n := calls.Add(1)
		if n <= 16 {
			return listResponse(), nil
		}
		if n == 17 {
			return response(&genai.Part{
				FunctionCall: &genai.FunctionCall{
					ID:   "write",
					Name: "write_file",
					Args: map[string]any{
						"path":            "result.txt",
						"content":         "done",
						"expected_sha256": "new",
					},
				},
			}), nil
		}
		return response(&genai.Part{Text: "all finished"}), nil
	})

	m := newManager(t, cfg, g)
	jobs, err := m.Spawn([]Task{{
		Prompt:     "perform work and write result.txt",
		WritePaths: []string{"result.txt"},
	}})
	if err != nil {
		t.Fatal(err)
	}

	j := awaitJob(t, m, jobs[0].ID)
	if j.Status != "completed" {
		t.Fatalf("expected completed status, got %+v", j)
	}
	if j.Compactions != 0 {
		t.Fatalf("expected 0 compactions, got %d", j.Compactions)
	}
	if j.Steps != 18 {
		t.Fatalf("expected 18 steps, got %d", j.Steps)
	}

	content, err := os.ReadFile(filepath.Join(dir, "result.txt"))
	if err != nil {
		t.Fatalf("failed reading result.txt: %v", err)
	}
	if string(content) != "done" {
		t.Fatalf("expected content 'done', got %q", string(content))
	}
}

func TestSmallBudgetUsesObservedTokensToReachEdit(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.MaxSteps = 10
	cfg.MaxTokens = 14000
	cfg.MaxOutput = 8192
	if err := os.WriteFile(filepath.Join(cfg.Workspace, "a.txt"), []byte("before"), 0644); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	g := generateFunc(func(ctx context.Context, h []*genai.Content, c *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		n := calls.Add(1)
		if c.ResponseMIMEType == "application/json" {
			return checkpointResponse(Checkpoint{Summary: "not edited", Remaining: []string{"edit a.txt"}}), nil
		}
		var r *genai.GenerateContentResponse
		switch n {
		case 1:
			r = response(&genai.Part{FunctionCall: &genai.FunctionCall{ID: "read", Name: "read_file", Args: map[string]any{"path": "a.txt"}}})
		case 2:
			r = response(&genai.Part{FunctionCall: &genai.FunctionCall{ID: "edit", Name: "edit_file", Args: map[string]any{"path": "a.txt", "old_text": "before", "new_text": "after", "expected_sha256": digest([]byte("before"))}}})
		default:
			r = response(&genai.Part{Text: "edited"})
		}
		r.UsageMetadata.PromptTokenCount = 1800
		r.UsageMetadata.TotalTokenCount = 1900
		return r, nil
	})
	m := newManager(t, cfg, g)
	jobs, err := m.Spawn([]Task{{Prompt: "Change before to after in a.txt. " + strings.Repeat("Preserve the surrounding contract. ", 40), WritePaths: []string{"a.txt"}}})
	if err != nil {
		t.Fatal(err)
	}
	j := awaitJob(t, m, jobs[0].ID)
	b, err := os.ReadFile(filepath.Join(cfg.Workspace, "a.txt"))
	if err != nil || string(b) != "after" || j.Status != "completed" || j.Usage.Total > 14000 {
		t.Fatalf("premature stop with available budget: %+v %q %v", j, b, err)
	}
}
