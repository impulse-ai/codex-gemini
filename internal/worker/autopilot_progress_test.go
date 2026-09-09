package worker

import (
	"context"
	"os"
	"path/filepath"
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
