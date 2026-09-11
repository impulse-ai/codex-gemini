package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/genai"
)

func TestImplementationGuardStopsNeedsAttentionWhenNoEdits(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	cfg.MaxTokens = 200000
	cfg.MaxSteps = 25
	cfg.MaxOutput = 1024

	g := generateFunc(func(ctx context.Context, h []*genai.Content, c *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		if c.ResponseMIMEType == "application/json" {
			return checkpointResponse(Checkpoint{
				Summary:   "blocked missing interface",
				Remaining: []string{"need interface"},
				Complete:  false,
			}), nil
		}
		return listResponse(), nil
	})

	m := newManager(t, cfg, g)
	jobs, err := m.Spawn([]Task{{
		Prompt:     "implement feature and write out.txt",
		Intent:     "implementation",
		WritePaths: []string{"out.txt"},
	}})
	if err != nil {
		t.Fatal(err)
	}

	j := awaitJob(t, m, jobs[0].ID)
	if j.Status != "needs_attention" {
		t.Fatalf("expected status needs_attention, got %q (job: %+v)", j.Status, j)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected out.txt to not exist, err: %v", err)
	}
	if j.Metrics == nil {
		t.Fatal("expected non-nil j.Metrics")
	}
	if j.Metrics.Nudges != 1 {
		t.Fatalf("expected Metrics.Nudges == 1, got %d", j.Metrics.Nudges)
	}
	if j.Metrics.SuccessfulEdits != 0 {
		t.Fatalf("expected Metrics.SuccessfulEdits == 0, got %d", j.Metrics.SuccessfulEdits)
	}
	if j.Steps > 17 {
		t.Fatalf("expected Steps <= 17, got %d", j.Steps)
	}
	if !strings.Contains(j.Result, "missing interface") {
		t.Fatalf("expected Result to contain 'missing interface', got %q", j.Result)
	}
}

func TestImplementationGuardRecoversAfterNudge(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	cfg.MaxTokens = 200000
	cfg.MaxSteps = 25
	cfg.MaxOutput = 1024

	var wrote atomic.Bool
	g := generateFunc(func(ctx context.Context, h []*genai.Content, c *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		if c.ResponseMIMEType == "application/json" {
			return checkpointResponse(Checkpoint{
				Summary:   "completed implementation",
				Remaining: nil,
				Complete:  true,
			}), nil
		}

		sawNudge := false
		for _, content := range h {
			if content.Role == "user" {
				for _, part := range content.Parts {
					if strings.Contains(part.Text, "Implementation progress check") {
						sawNudge = true
						break
					}
				}
			}
		}

		if sawNudge {
			if !wrote.Swap(true) {
				return response(&genai.Part{
					FunctionCall: &genai.FunctionCall{
						ID:   "write",
						Name: "write_file",
						Args: map[string]any{
							"path":            "out.txt",
							"content":         "done",
							"expected_sha256": "new",
						},
					},
				}), nil
			}
			return response(&genai.Part{Text: "done"}), nil
		}

		return listResponse(), nil
	})

	m := newManager(t, cfg, g)
	jobs, err := m.Spawn([]Task{{
		Prompt:     "implement feature and write out.txt",
		Intent:     "implementation",
		WritePaths: []string{"out.txt"},
	}})
	if err != nil {
		t.Fatal(err)
	}

	j := awaitJob(t, m, jobs[0].ID)
	if j.Status != "completed" {
		t.Fatalf("expected status completed, got %q (job: %+v)", j.Status, j)
	}

	content, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil {
		t.Fatalf("failed reading out.txt: %v", err)
	}
	if string(content) != "done" {
		t.Fatalf("expected out.txt content 'done', got %q", string(content))
	}

	if j.Metrics == nil {
		t.Fatal("expected non-nil j.Metrics")
	}
	if j.Metrics.Nudges != 1 {
		t.Fatalf("expected Metrics.Nudges == 1, got %d", j.Metrics.Nudges)
	}
	if j.Metrics.SuccessfulEdits != 1 {
		t.Fatalf("expected Metrics.SuccessfulEdits == 1, got %d", j.Metrics.SuccessfulEdits)
	}
}

func TestImplementationGuardBetweenEdits(t *testing.T) {
	for _, periodic := range []bool{false, true} {
		t.Run(fmt.Sprint(periodic), func(t *testing.T) {
			dir := t.TempDir()
			cfg := testConfig(dir)
			cfg.MaxTokens = 200000
			cfg.MaxSteps = 35
			cfg.MaxOutput = 1024
			calls := 0
			g := generateFunc(func(ctx context.Context, h []*genai.Content, c *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
				if c.ResponseMIMEType == "application/json" {
					// A false completion claim must not override a detected stall.
					return checkpointResponse(Checkpoint{Summary: "worker claims done", Complete: true}), nil
				}
				calls++
				if calls == 1 || (periodic && (calls == 10 || calls == 19)) {
					return response(&genai.Part{FunctionCall: &genai.FunctionCall{ID: fmt.Sprint(calls), Name: "write_file", Args: map[string]any{"path": fmt.Sprintf("edit%d.txt", calls), "content": "partial implementation", "expected_sha256": "new"}}}), nil
				}
				if periodic && calls == 22 {
					return response(&genai.Part{Text: "done"}), nil
				}
				return listResponse(), nil
			})
			m := newManager(t, cfg, g)
			jobs, err := m.Spawn([]Task{{Prompt: "implement assigned files", Intent: "implementation", WritePaths: []string{"."}}})
			if err != nil {
				t.Fatal(err)
			}
			j := awaitJob(t, m, jobs[0].ID)
			if periodic {
				if j.Status != "completed" || j.Metrics.SuccessfulEdits != 3 || j.Metrics.Nudges != 2 {
					t.Fatalf("periodic edits did not reset guard: %+v", j)
				}
			} else {
				if j.Status != "needs_attention" || j.Steps >= cfg.MaxSteps || j.Checkpoint == nil || j.Checkpoint.Complete || len(j.Checkpoint.Remaining) == 0 {
					t.Fatalf("stall not preserved: %+v", j)
				}
				if j.Metrics.SuccessfulEdits != 1 || j.Metrics.Nudges != 1 {
					t.Fatalf("unexpected metrics: %+v", j.Metrics)
				}
			}
			b, err := os.ReadFile(filepath.Join(dir, "edit1.txt"))
			if err != nil || string(b) != "partial implementation" {
				t.Fatalf("lost partial edit: %s %v", b, err)
			}
		})
	}
}
