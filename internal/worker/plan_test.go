package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/genai"
)

func TestParseDecompositionResult(t *testing.T) {
	t.Parallel()

	// Valid plain JSON
	validJSON := `{"steps": [{"id": "step1", "prompt": "p", "write_paths": ["a.go"], "acceptance": ["ok"], "max_tokens": 100}]}`
	d, err := parseDecompositionResult([]byte(validJSON))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(d.Steps) != 1 || d.Steps[0].ID != "step1" {
		t.Fatalf("unexpected steps: %+v", d.Steps)
	}

	// Valid markdown code fence ```json ... ```
	fencedJSON := "```json\n" + validJSON + "\n```"
	d, err = parseDecompositionResult([]byte(fencedJSON))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(d.Steps) != 1 || d.Steps[0].ID != "step1" {
		t.Fatalf("unexpected steps: %+v", d.Steps)
	}

	// Valid markdown code fence ``` ... ```
	plainFencedJSON := "```\n" + validJSON + "\n```"
	d, err = parseDecompositionResult([]byte(plainFencedJSON))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(d.Steps) != 1 || d.Steps[0].ID != "step1" {
		t.Fatalf("unexpected steps: %+v", d.Steps)
	}

	// Reject trailing second JSON
	trailingJSON := validJSON + `{"steps": []}`
	if _, err := parseDecompositionResult([]byte(trailingJSON)); err == nil {
		t.Fatalf("expected error for trailing second JSON, got nil")
	}

	// Reject trailing garbage
	trailingGarbage := validJSON + " trailing garbage"
	if _, err := parseDecompositionResult([]byte(trailingGarbage)); err == nil {
		t.Fatalf("expected error for trailing garbage, got nil")
	}

	// Reject unknown fields
	unknownFields := `{"steps": [], "unknown_field": 123}`
	if _, err := parseDecompositionResult([]byte(unknownFields)); err == nil {
		t.Fatalf("expected error for unknown fields, got nil")
	}

	// Reject invalid JSON syntax
	if _, err := parseDecompositionResult([]byte(`{"steps": `)); err == nil {
		t.Fatalf("expected error for invalid syntax, got nil")
	}
}

func TestValidatePlanLocked(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := testConfig(dir)
	m := newManager(t, cfg, nil)

	baseTask := Task{
		Workspace:  dir,
		Prompt:     "original constraints",
		WritePaths: []string{"src"},
	}
	baseInput := PlanInput{
		Task:     baseTask,
		MaxTasks: 6,
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// 1. Valid chain: step1 -> step2 (step2 depends on step1) reusing same file "src/a.go"
	// and independent disjoint step3 modifying "src/b.go"
	// Expected waves: Wave 1: [step1, step3], Wave 2: [step2]
	validChainPlan := Decomposition{
		Steps: []PlanStep{
			{
				ID:         "step1",
				Prompt:     "first step",
				WritePaths: []string{"src/a.go"},
				Acceptance: []string{"test a passes"},
				MaxTokens:  1000,
			},
			{
				ID:         "step2",
				Prompt:     "second step",
				WritePaths: []string{"src/a.go"},
				DependsOn:  []string{"step1"},
				Acceptance: []string{"test a update passes"},
				MaxTokens:  1000,
			},
			{
				ID:         "step3",
				Prompt:     "independent step",
				WritePaths: []string{"src/b.go"},
				Acceptance: []string{"test b passes"},
				MaxTokens:  1000,
			},
		},
	}
	validated, waves, err := m.validatePlanLocked(baseInput, validChainPlan)
	if err != nil {
		t.Fatalf("expected valid chain to succeed, got: %v", err)
	}
	expectedWaves := [][]string{
		{"step1", "step3"},
		{"step2"},
	}
	if !reflect.DeepEqual(waves, expectedWaves) {
		t.Fatalf("expected waves %v, got %v", expectedWaves, waves)
	}
	if len(validated.Steps) != 3 {
		t.Fatalf("expected 3 validated steps, got %d", len(validated.Steps))
	}
	for _, step := range validated.Steps {
		if step.Task == nil {
			t.Fatalf("expected step %s Task to be populated", step.ID)
		}
		if step.Task.Intent != "implementation" {
			t.Fatalf("expected child task intent to be implementation, got %s", step.Task.Intent)
		}
	}

	// 2. Reject empty steps
	if _, _, err := m.validatePlanLocked(baseInput, Decomposition{Steps: []PlanStep{}}); err == nil {
		t.Fatalf("expected error for empty steps")
	}

	// 3. Reject exceeding MaxTasks
	oversizedPlan := Decomposition{
		Steps: []PlanStep{
			{ID: "s1", Prompt: "p", WritePaths: []string{"src/1.go"}, Acceptance: []string{"ok"}, MaxTokens: 100},
			{ID: "s2", Prompt: "p", WritePaths: []string{"src/2.go"}, Acceptance: []string{"ok"}, MaxTokens: 100},
			{ID: "s3", Prompt: "p", WritePaths: []string{"src/3.go"}, Acceptance: []string{"ok"}, MaxTokens: 100},
		},
	}
	if _, _, err := m.validatePlanLocked(PlanInput{Task: baseTask, MaxTasks: 2}, oversizedPlan); err == nil {
		t.Fatalf("expected error exceeding maxTasks")
	}

	// 4. Reject client supplying Task override
	dummyTask := baseTask
	stepWithTask := Decomposition{
		Steps: []PlanStep{
			{ID: "s1", Prompt: "p", WritePaths: []string{"src/a.go"}, Acceptance: []string{"ok"}, MaxTokens: 100, Task: &dummyTask},
		},
	}
	if _, _, err := m.validatePlanLocked(baseInput, stepWithTask); err == nil {
		t.Fatalf("expected error when step.Task is not nil")
	}

	// 5. Reject invalid step ID regex
	if _, _, err := m.validatePlanLocked(baseInput, Decomposition{
		Steps: []PlanStep{
			{ID: "bad id with space", Prompt: "p", WritePaths: []string{"src/a.go"}, Acceptance: []string{"ok"}, MaxTokens: 100},
		},
	}); err == nil {
		t.Fatalf("expected error for invalid step ID regex")
	}

	// 6. Reject duplicate step ID
	if _, _, err := m.validatePlanLocked(baseInput, Decomposition{
		Steps: []PlanStep{
			{ID: "step1", Prompt: "p1", WritePaths: []string{"src/a.go"}, Acceptance: []string{"ok"}, MaxTokens: 100},
			{ID: "step1", Prompt: "p2", WritePaths: []string{"src/b.go"}, Acceptance: []string{"ok"}, MaxTokens: 100},
		},
	}); err == nil {
		t.Fatalf("expected error for duplicate step IDs")
	}

	// 7. Reject empty prompt
	if _, _, err := m.validatePlanLocked(baseInput, Decomposition{
		Steps: []PlanStep{
			{ID: "step1", Prompt: "   ", WritePaths: []string{"src/a.go"}, Acceptance: []string{"ok"}, MaxTokens: 100},
		},
	}); err == nil {
		t.Fatalf("expected error for empty prompt")
	}

	// 8. Reject missing or invalid acceptance criteria (0 criteria, >8 criteria, blank item, >2000 chars)
	// 0 criteria
	if _, _, err := m.validatePlanLocked(baseInput, Decomposition{
		Steps: []PlanStep{
			{ID: "step1", Prompt: "p", WritePaths: []string{"src/a.go"}, Acceptance: []string{}, MaxTokens: 100},
		},
	}); err == nil {
		t.Fatalf("expected error for 0 acceptance criteria")
	}
	// >8 criteria
	nineCriteria := make([]string, 9)
	for i := range nineCriteria {
		nineCriteria[i] = "ok"
	}
	if _, _, err := m.validatePlanLocked(baseInput, Decomposition{
		Steps: []PlanStep{
			{ID: "step1", Prompt: "p", WritePaths: []string{"src/a.go"}, Acceptance: nineCriteria, MaxTokens: 100},
		},
	}); err == nil {
		t.Fatalf("expected error for >8 acceptance criteria")
	}
	// blank item
	if _, _, err := m.validatePlanLocked(baseInput, Decomposition{
		Steps: []PlanStep{
			{ID: "step1", Prompt: "p", WritePaths: []string{"src/a.go"}, Acceptance: []string{"   "}, MaxTokens: 100},
		},
	}); err == nil {
		t.Fatalf("expected error for blank acceptance item")
	}
	// >2000 chars
	if _, _, err := m.validatePlanLocked(baseInput, Decomposition{
		Steps: []PlanStep{
			{ID: "step1", Prompt: "p", WritePaths: []string{"src/a.go"}, Acceptance: []string{strings.Repeat("a", 2001)}, MaxTokens: 100},
		},
	}); err == nil {
		t.Fatalf("expected error for >2000 char acceptance item")
	}

	// 9. Reject oversized or non-positive max_tokens
	if _, _, err := m.validatePlanLocked(baseInput, Decomposition{
		Steps: []PlanStep{
			{ID: "step1", Prompt: "p", WritePaths: []string{"src/a.go"}, Acceptance: []string{"ok"}, MaxTokens: 0},
		},
	}); err == nil {
		t.Fatalf("expected error for max_tokens 0")
	}
	if _, _, err := m.validatePlanLocked(baseInput, Decomposition{
		Steps: []PlanStep{
			{ID: "step1", Prompt: "p", WritePaths: []string{"src/a.go"}, Acceptance: []string{"ok"}, MaxTokens: cfg.MaxTokens + 1},
		},
	}); err == nil {
		t.Fatalf("expected error for oversized max_tokens")
	}

	// 10. Reject empty write_paths
	if _, _, err := m.validatePlanLocked(baseInput, Decomposition{
		Steps: []PlanStep{
			{ID: "step1", Prompt: "p", WritePaths: []string{}, Acceptance: []string{"ok"}, MaxTokens: 100},
		},
	}); err == nil {
		t.Fatalf("expected error for empty write_paths")
	}

	// 11. Reject self-dependency
	if _, _, err := m.validatePlanLocked(baseInput, Decomposition{
		Steps: []PlanStep{
			{ID: "step1", Prompt: "p", WritePaths: []string{"src/a.go"}, DependsOn: []string{"step1"}, Acceptance: []string{"ok"}, MaxTokens: 100},
		},
	}); err == nil {
		t.Fatalf("expected error for self-dependency")
	}

	// 12. Reject missing dependency
	if _, _, err := m.validatePlanLocked(baseInput, Decomposition{
		Steps: []PlanStep{
			{ID: "step1", Prompt: "p", WritePaths: []string{"src/a.go"}, DependsOn: []string{"nonexistent"}, Acceptance: []string{"ok"}, MaxTokens: 100},
		},
	}); err == nil {
		t.Fatalf("expected error for nonexistent dependency")
	}

	// 13. Reject duplicate dependency
	if _, _, err := m.validatePlanLocked(baseInput, Decomposition{
		Steps: []PlanStep{
			{ID: "step1", Prompt: "p1", WritePaths: []string{"src/a.go"}, Acceptance: []string{"ok"}, MaxTokens: 100},
			{ID: "step2", Prompt: "p2", WritePaths: []string{"src/b.go"}, DependsOn: []string{"step1", "step1"}, Acceptance: []string{"ok"}, MaxTokens: 100},
		},
	}); err == nil {
		t.Fatalf("expected error for duplicate dependency")
	}

	// 14. Reject direct dependency cycle
	if _, _, err := m.validatePlanLocked(baseInput, Decomposition{
		Steps: []PlanStep{
			{ID: "step1", Prompt: "p1", WritePaths: []string{"src/a.go"}, DependsOn: []string{"step2"}, Acceptance: []string{"ok"}, MaxTokens: 100},
			{ID: "step2", Prompt: "p2", WritePaths: []string{"src/b.go"}, DependsOn: []string{"step1"}, Acceptance: []string{"ok"}, MaxTokens: 100},
		},
	}); err == nil {
		t.Fatalf("expected error for direct cycle")
	}

	// 15. Reject transitive dependency cycle
	if _, _, err := m.validatePlanLocked(baseInput, Decomposition{
		Steps: []PlanStep{
			{ID: "step1", Prompt: "p1", WritePaths: []string{"src/a.go"}, DependsOn: []string{"step3"}, Acceptance: []string{"ok"}, MaxTokens: 100},
			{ID: "step2", Prompt: "p2", WritePaths: []string{"src/b.go"}, DependsOn: []string{"step1"}, Acceptance: []string{"ok"}, MaxTokens: 100},
			{ID: "step3", Prompt: "p3", WritePaths: []string{"src/c.go"}, DependsOn: []string{"step2"}, Acceptance: []string{"ok"}, MaxTokens: 100},
		},
	}); err == nil {
		t.Fatalf("expected error for transitive cycle")
	}

	// 16. Reject out-of-scope write path
	if _, _, err := m.validatePlanLocked(baseInput, Decomposition{
		Steps: []PlanStep{
			{ID: "step1", Prompt: "p", WritePaths: []string{"other_dir/a.go"}, Acceptance: []string{"ok"}, MaxTokens: 100},
		},
	}); err == nil {
		t.Fatalf("expected error for out-of-scope write path")
	}

	// 17. Reject traversal or protected paths in step write_paths
	if _, _, err := m.validatePlanLocked(baseInput, Decomposition{
		Steps: []PlanStep{
			{ID: "step1", Prompt: "p", WritePaths: []string{"../escape.go"}, Acceptance: []string{"ok"}, MaxTokens: 100},
		},
	}); err == nil {
		t.Fatalf("expected error for path traversal")
	}
	if _, _, err := m.validatePlanLocked(baseInput, Decomposition{
		Steps: []PlanStep{
			{ID: "step1", Prompt: "p", WritePaths: []string{".git/config"}, Acceptance: []string{"ok"}, MaxTokens: 100},
		},
	}); err == nil {
		t.Fatalf("expected error for protected path")
	}

	// 18. Reject unordered overlapping directory and file writes
	if _, _, err := m.validatePlanLocked(baseInput, Decomposition{
		Steps: []PlanStep{
			{ID: "step1", Prompt: "p1", WritePaths: []string{"src"}, Acceptance: []string{"ok"}, MaxTokens: 100},
			{ID: "step2", Prompt: "p2", WritePaths: []string{"src/a.go"}, Acceptance: []string{"ok"}, MaxTokens: 100},
		},
	}); err == nil {
		t.Fatalf("expected error for unordered overlapping directory and file writes")
	}
}

func TestStartPlanValidation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := testConfig(dir)
	m := newManager(t, cfg, nil)

	// Invalid workspace: empty
	_, err := m.StartPlan(PlanInput{
		Task: Task{
			Workspace:  "",
			Prompt:     "test",
			WritePaths: []string{"src"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "absolute workspace is required") {
		t.Fatalf("expected absolute workspace error, got %v", err)
	}

	// Invalid workspace: relative
	_, err = m.StartPlan(PlanInput{
		Task: Task{
			Workspace:  "relative/path",
			Prompt:     "test",
			WritePaths: []string{"src"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "absolute workspace is required") {
		t.Fatalf("expected absolute workspace error, got %v", err)
	}

	// Invalid write paths: empty
	_, err = m.StartPlan(PlanInput{
		Task: Task{
			Workspace:  dir,
			Prompt:     "test",
			WritePaths: []string{},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "non-empty write_paths are required") {
		t.Fatalf("expected non-empty write_paths error, got %v", err)
	}

	// Invalid intent: not implementation or empty
	_, err = m.StartPlan(PlanInput{
		Task: Task{
			Workspace:  dir,
			Prompt:     "test",
			WritePaths: []string{"src"},
			Intent:     "investigation",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "task intent must be empty or implementation") {
		t.Fatalf("expected intent error, got %v", err)
	}

	// Invalid maxTasks < 1 or > 12
	_, err = m.StartPlan(PlanInput{
		Task: Task{
			Workspace:  dir,
			Prompt:     "test",
			WritePaths: []string{"src"},
		},
		MaxTasks: -1,
	})
	if err == nil || !strings.Contains(err.Error(), "max_tasks must be between 1 and 12") {
		t.Fatalf("expected max_tasks error, got %v", err)
	}

	_, err = m.StartPlan(PlanInput{
		Task: Task{
			Workspace:  dir,
			Prompt:     "test",
			WritePaths: []string{"src"},
		},
		MaxTasks: 13,
	})
	if err == nil || !strings.Contains(err.Error(), "max_tasks must be between 1 and 12") {
		t.Fatalf("expected max_tasks error, got %v", err)
	}
}

func TestStartPlanExecutionAndGetPlan(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := testConfig(dir)

	planJSON := `{
		"steps": [
			{
				"id": "step_1",
				"prompt": "implement foo",
				"write_paths": ["src/foo.go"],
				"depends_on": [],
				"acceptance": ["foo unit test passes"],
				"max_tokens": 1000
			}
		]
	}`

	stepCount := 0
	gf := func(ctx context.Context, history []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		stepCount++
		if stepCount == 1 {
			// Call report_checkpoint first
			return &genai.GenerateContentResponse{
				Candidates: []*genai.Candidate{
					{
						Content: &genai.Content{
							Parts: []*genai.Part{
								{
									FunctionCall: &genai.FunctionCall{
										Name: "report_checkpoint",
										Args: map[string]any{
											"summary":   "planning done",
											"findings":  []any{"ready to output JSON"},
											"covered":   []any{"investigated workspace"},
											"remaining": []any{},
											"complete":  true,
										},
									},
								},
							},
						},
					},
				},
			}, nil
		}
		// Return final JSON plan
		return response(&genai.Part{Text: planJSON}), nil
	}

	m := newManager(t, cfg, generateFunc(gf))

	job, err := m.StartPlan(PlanInput{
		Task: Task{
			Workspace:  dir,
			Prompt:     "build feature",
			WritePaths: []string{"src"},
		},
	})
	if err != nil {
		t.Fatalf("StartPlan failed: %v", err)
	}

	// Verify StartPlan returns job snapshot with PlanSpec hidden
	if job.PlanSpec != nil {
		t.Fatalf("StartPlan returned snapshot should have PlanSpec hidden (nil), got %+v", job.PlanSpec)
	}

	// Verify stored planner job is read-only (intent "investigation" and empty write_paths)
	m.mu.Lock()
	storedJob, exists := m.jobs[job.ID]
	if !exists {
		m.mu.Unlock()
		t.Fatalf("job not found in manager")
	}
	if storedJob.Task.Intent != "investigation" {
		m.mu.Unlock()
		t.Fatalf("planner task intent should be investigation, got %s", storedJob.Task.Intent)
	}
	if len(storedJob.Task.WritePaths) != 0 {
		m.mu.Unlock()
		t.Fatalf("planner task write paths should be empty, got %v", storedJob.Task.WritePaths)
	}
	m.mu.Unlock()

	// Await completion
	finalJob := awaitJob(t, m, job.ID)
	if finalJob.Status != "completed" {
		t.Fatalf("expected job completed, got %s (error: %s)", finalJob.Status, finalJob.Error)
	}

	// GetPlan
	out, err := m.GetPlan(job.ID)
	if err != nil {
		t.Fatalf("GetPlan failed: %v", err)
	}
	if out.Status != "plan_ready" {
		t.Fatalf("expected plan_ready status, got %s (error: %s)", out.Status, out.Error)
	}
	if out.Plan == nil || len(out.Plan.Steps) != 1 {
		t.Fatalf("expected 1 step in plan, got %+v", out.Plan)
	}
	if len(out.Waves) != 1 || len(out.Waves[0]) != 1 || out.Waves[0][0] != "step_1" {
		t.Fatalf("expected waves [[step_1]], got %+v", out.Waves)
	}
}

func TestGetPlanIncompleteAndMalformed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := testConfig(dir)

	// 1. Incomplete checkpoint: complete=false -> GetPlan returns needs_attention, not plan_ready
	gfIncomplete := func(ctx context.Context, history []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		return &genai.GenerateContentResponse{
			Candidates: []*genai.Candidate{
				{
					Content: &genai.Content{
						Parts: []*genai.Part{
							{
								FunctionCall: &genai.FunctionCall{
									Name: "report_checkpoint",
									Args: map[string]any{
										"summary":   "not finished",
										"findings":  []any{"in progress"},
										"covered":   []any{"part"},
										"remaining": []any{"more"},
										"complete":  false,
									},
								},
							},
						},
					},
				},
			},
		}, nil
	}

	mIncomplete := newManager(t, cfg, generateFunc(gfIncomplete))
	jobInc, err := mIncomplete.StartPlan(PlanInput{
		Task: Task{
			Workspace:  dir,
			Prompt:     "incomplete job",
			WritePaths: []string{"src"},
		},
	})
	if err != nil {
		t.Fatalf("StartPlan failed: %v", err)
	}
	awaitJob(t, mIncomplete, jobInc.ID)

	outInc, err := mIncomplete.GetPlan(jobInc.ID)
	if err != nil {
		t.Fatalf("GetPlan failed: %v", err)
	}
	if outInc.Status == "plan_ready" {
		t.Fatalf("expected status not plan_ready for incomplete checkpoint, got %s", outInc.Status)
	}
	if outInc.Status != "needs_attention" {
		t.Fatalf("expected needs_attention, got %s", outInc.Status)
	}

	// 2. Malformed final JSON: returns needs_attention
	stepCount := 0
	gfMalformed := func(ctx context.Context, history []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		stepCount++
		if stepCount == 1 {
			return &genai.GenerateContentResponse{
				Candidates: []*genai.Candidate{
					{
						Content: &genai.Content{
							Parts: []*genai.Part{
								{
									FunctionCall: &genai.FunctionCall{
										Name: "report_checkpoint",
										Args: map[string]any{
											"summary":   "planning done",
											"findings":  []any{"findings"},
											"covered":   []any{"all"},
											"remaining": []any{},
											"complete":  true,
										},
									},
								},
							},
						},
					},
				},
			}, nil
		}
		return response(&genai.Part{Text: "not a valid json output"}), nil
	}

	mMalformed := newManager(t, testConfig(t.TempDir()), generateFunc(gfMalformed))
	jobMal, err := mMalformed.StartPlan(PlanInput{
		Task: Task{
			Workspace:  dir,
			Prompt:     "malformed json job",
			WritePaths: []string{"src"},
		},
	})
	if err != nil {
		t.Fatalf("StartPlan failed: %v", err)
	}
	awaitJob(t, mMalformed, jobMal.ID)

	outMal, err := mMalformed.GetPlan(jobMal.ID)
	if err != nil {
		t.Fatalf("GetPlan failed: %v", err)
	}
	if outMal.Status == "plan_ready" {
		t.Fatalf("expected status not plan_ready for malformed json, got %s", outMal.Status)
	}
	if outMal.Status != "needs_attention" {
		t.Fatalf("expected needs_attention, got %s", outMal.Status)
	}
	if !strings.Contains(outMal.Error, "failed to parse plan decomposition") {
		t.Fatalf("expected parse failure error message, got: %s", outMal.Error)
	}

	// 3. Non-existent job
	if _, err := mMalformed.GetPlan("nonexistent"); err == nil {
		t.Fatalf("expected error for nonexistent job ID in GetPlan")
	}

	// 4. Non-planning job
	nonPlanJobs, err := mMalformed.Spawn([]Task{{
		Workspace: dir,
		Prompt:    "plain task",
		Intent:    "investigation",
	}})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	if _, err := mMalformed.GetPlan(nonPlanJobs[0].ID); err == nil || !strings.Contains(err.Error(), "is not a planning job") {
		t.Fatalf("expected error 'not a planning job', got: %v", err)
	}
}

func TestPlanRetainsConstraintsAndPersists(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	m, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	packet, err := m.Publish("codex", ContextPacket{Workspace: dir, Title: "invariants", Objective: "compatibility", Summary: "preserve API"})
	if err != nil {
		m.Close()
		t.Fatal(err)
	}
	off := false
	spec := PlanInput{Task: Task{Workspace: dir, Prompt: "preserve existing API", Intent: "implementation", WritePaths: []string{"src"}, ContextIDs: []string{packet.ID}, Thinking: "medium", Autopilot: &off}, MaxTasks: 2}
	raw := `{"steps":[{"id":"one","prompt":"implement change","write_paths":["src/a.go"],"acceptance":["regression passes"],"max_tokens":5000}]}`
	j := &Job{ID: newID(), Task: Task{Workspace: dir, Prompt: "planner"}, PlanSpec: &spec, Status: "completed", Result: raw, Checkpoint: &Checkpoint{Complete: true, Summary: "done"}}
	m.mu.Lock()
	m.jobs[j.ID] = j
	err = m.save(j)
	m.mu.Unlock()
	m.Close()
	if err != nil {
		t.Fatal(err)
	}
	m = newManager(t, cfg, nil)
	out, err := m.GetPlan(j.ID)
	if err != nil || out.Status != "plan_ready" {
		t.Fatalf("restore: %+v %v", out, err)
	}
	child := out.Plan.Steps[0].Task
	canonical, err := canonicalWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	if child == nil || child.Workspace != canonical || child.Intent != "implementation" || child.Thinking != "medium" || child.Autopilot == nil || *child.Autopilot || len(child.ContextIDs) != 1 || child.ContextIDs[0] != packet.ID || !strings.Contains(child.Prompt, spec.Task.Prompt) || !strings.Contains(child.Prompt, "regression passes") {
		t.Fatalf("lost constraints: %+v", child)
	}
	snap, err := m.Get(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(snap)
	if strings.Contains(string(encoded), "preserve existing API") {
		t.Fatal("snapshot leaked planner assignment")
	}
	if _, err = m.Continue(j.ID, "different goal"); err == nil {
		t.Fatal("planner continuation accepted")
	}
	if err = os.Symlink(t.TempDir(), filepath.Join(dir, "src")); err != nil {
		t.Fatal(err)
	}
	out, err = m.GetPlan(j.ID)
	if err != nil || out.Status != "needs_attention" || out.Plan != nil {
		t.Fatalf("stale path not rejected: %+v %v", out, err)
	}
}
