package worker

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"

	"google.golang.org/genai"
)

func TestAutopilotStepLimitForcesSummaryNormalizingConflictingCompletion(t *testing.T) {
	var calls atomic.Int32
	cfg := testConfig(t.TempDir())
	cfg.MaxSteps = 2
	g := generateFunc(func(ctx context.Context, h []*genai.Content, c *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		switch n := calls.Add(1); n {
		case 1:
			return listResponse(), nil
		case 2:
			return checkpointResponse(Checkpoint{
				Summary:   "useful report",
				Findings:  []string{"a.go:4 bug"},
				Remaining: []string{"fix a.go"},
				Complete:  true,
			}), nil
		default:
			t.Errorf("unexpected call %d", n)
			return response(&genai.Part{Text: "unexpected"}), nil
		}
	})
	m := newManager(t, cfg, g)
	jobs, err := m.Spawn([]Task{{Prompt: "review"}})
	if err != nil {
		t.Fatal(err)
	}
	j := awaitJob(t, m, jobs[0].ID)
	if j.Status != "limit_reached" {
		t.Fatalf("expected status limit_reached, got %q", j.Status)
	}
	if j.Checkpoint == nil {
		t.Fatal("expected non-nil Checkpoint")
	}
	if j.Checkpoint.Complete {
		t.Errorf("expected Checkpoint.Complete to be false, got true")
	}
	expectedFindings := []string{"a.go:4 bug"}
	if !reflect.DeepEqual(j.Checkpoint.Findings, expectedFindings) {
		t.Errorf("expected Checkpoint.Findings %v, got %v", expectedFindings, j.Checkpoint.Findings)
	}
	if j.Steps != 2 {
		t.Errorf("expected Steps to be 2, got %d", j.Steps)
	}
	if totalCalls := calls.Load(); totalCalls != 2 {
		t.Errorf("expected 2 calls, got %d", totalCalls)
	}
}
