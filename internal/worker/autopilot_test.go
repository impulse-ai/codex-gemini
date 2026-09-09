package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/genai"
)

func checkpointResponse(c Checkpoint) *genai.GenerateContentResponse {
	b, _ := json.Marshal(c)
	return response(&genai.Part{Text: string(b)})
}
func listResponse() *genai.GenerateContentResponse {
	return response(&genai.Part{FunctionCall: &genai.FunctionCall{ID: "list", Name: "list_files", Args: map[string]any{"path": "."}}})
}

func TestAutopilotCompactsAndFinishesWithinSameJob(t *testing.T) {
	var work, summaries atomic.Int32
	cfg := testConfig(t.TempDir())
	cfg.MaxSteps = 12
	g := generateFunc(func(ctx context.Context, h []*genai.Content, c *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		if c.ResponseMIMEType == "application/json" {
			summaries.Add(1)
			if work.Load() > 4 {
				return checkpointResponse(Checkpoint{Summary: "Both paths checked", Findings: []string{"a.go:4 missing guard"}, Covered: []string{"path A", "path B"}, Complete: true}), nil
			}
			if len(c.Tools) != 0 || c.ThinkingConfig.ThinkingLevel != genai.ThinkingLevelLow {
				t.Error("checkpoint can execute tools or burn high thinking")
			}
			return checkpointResponse(Checkpoint{Summary: "Path A reviewed", Findings: []string{"a.go:4 missing guard"}, Covered: []string{"path A"}, Remaining: []string{"path B"}}), nil
		}
		if n := work.Add(1); n <= 4 {
			r := listResponse()
			if n == 4 {
				r.Candidates[0].FinishReason = genai.FinishReasonMaxTokens
			}
			return r, nil
		}
		if len(h) != 2 || !strings.Contains(h[0].Parts[0].Text, "preserve invariant") || !strings.Contains(h[1].Parts[0].Text, "a.go:4") {
			t.Error("fresh conversation lost constraints or findings")
		}
		return response(&genai.Part{Text: "a.go:4 missing guard; path B checked"}), nil
	})
	m := newManager(t, cfg, g)
	jobs, e := m.Spawn([]Task{{Prompt: "Review paths A and B; preserve invariant"}})
	if e != nil {
		t.Fatal(e)
	}
	j := awaitJob(t, m, jobs[0].ID)
	if j.Status != "completed" || j.Compactions != 1 || j.Steps != 7 || j.Usage.Total != 105 || summaries.Load() != 2 {
		t.Fatalf("unexpected recovery %+v", j)
	}
	files, e := os.ReadDir(filepath.Join(m.state, "segments", j.ID))
	if e != nil || len(files) != 1 {
		t.Fatalf("missing archived segment %v %v", files, e)
	}
}

func TestAutopilotRecoversTruncatedOutputWithoutExecutingCalls(t *testing.T) {
	var calls atomic.Int32
	cfg := testConfig(t.TempDir())
	g := generateFunc(func(ctx context.Context, h []*genai.Content, c *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		if calls.Add(1) == 1 {
			r := response(&genai.Part{Text: "Potential bug in a.go:2", FunctionCall: &genai.FunctionCall{ID: "incomplete", Name: "write_file", Args: map[string]any{"path": "unsafe.txt", "content": "must not execute", "expected_sha256": "new"}}})
			r.Candidates[0].FinishReason = genai.FinishReasonMaxTokens
			return r, nil
		}
		if c.ResponseMIMEType != "application/json" {
			t.Error("missing synthesis recovery")
		}
		for _, turn := range h {
			for _, p := range turn.Parts {
				if p.FunctionCall != nil {
					t.Error("replayed truncated function call")
				}
			}
		}
		return checkpointResponse(Checkpoint{Summary: "Review finished", Findings: []string{"a.go:2 verified missing guard"}, Covered: []string{"a.go"}, Complete: true}), nil
	})
	m := newManager(t, cfg, g)
	jobs, e := m.Spawn([]Task{{Prompt: "review one path", WritePaths: []string{"unsafe.txt"}, Thinking: "high"}})
	if e != nil {
		t.Fatal(e)
	}
	j := awaitJob(t, m, jobs[0].ID)
	if j.Status != "completed" || !strings.Contains(j.Result, "verified missing guard") || calls.Load() != 2 {
		t.Fatalf("job %+v", j)
	}
	if _, e = os.Stat(filepath.Join(cfg.Workspace, "unsafe.txt")); !os.IsNotExist(e) {
		t.Fatal("executed a truncated write")
	}
}

func TestAutopilotReturnsPartialFindingsAndStopsOnNoProgress(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.MaxSteps = 25
	var summaries, work atomic.Int32
	g := generateFunc(func(ctx context.Context, h []*genai.Content, c *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		if c.ResponseMIMEType == "application/json" {
			summaries.Add(1)
			return checkpointResponse(Checkpoint{Summary: "Blocked on missing schema", Findings: []string{"a.go:3 unchecked input"}, Covered: []string{"a.go"}, Remaining: []string{"missing generated schema"}}), nil
		}
		r := listResponse()
		if work.Add(1)%4 == 0 {
			r.Candidates[0].FinishReason = genai.FinishReasonMaxTokens
		}
		return r, nil
	})
	m := newManager(t, cfg, g)
	jobs, e := m.Spawn([]Task{{Prompt: "review"}})
	if e != nil {
		t.Fatal(e)
	}
	j := awaitJob(t, m, jobs[0].ID)
	if j.Status != "limit_reached" || !strings.Contains(j.Result, "a.go:3") || !strings.Contains(j.Error, "no progress") || summaries.Load() != 2 || j.Compactions != 1 {
		t.Fatalf("unbounded or empty recovery %+v", j)
	}
}

func TestCheckpointSurvivesProviderFailure(t *testing.T) {
	cfg := testConfig(t.TempDir())
	var calls atomic.Int32
	g := generateFunc(func(ctx context.Context, h []*genai.Content, c *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		if calls.Add(1) == 1 {
			return response(&genai.Part{FunctionCall: &genai.FunctionCall{ID: "checkpoint", Name: "report_checkpoint", Args: map[string]any{"summary": "one path checked", "findings": []string{"a.go:9 unchecked error"}, "covered": []string{"a.go"}, "remaining": []string{"b.go"}, "complete": false}}}), nil
		}
		return nil, genai.APIError{Code: 403, Message: "denied"}
	})
	m := newManager(t, cfg, g)
	jobs, e := m.Spawn([]Task{{Prompt: "review"}})
	if e != nil {
		t.Fatal(e)
	}
	j := awaitJob(t, m, jobs[0].ID)
	if j.Status != "failed" || j.Checkpoint == nil || !strings.Contains(j.Result, "a.go:9") {
		t.Fatalf("lost findings %+v", j)
	}
}

func TestTinyBudgetDoesNotStartUnfundedRecovery(t *testing.T) {
	var calls atomic.Int32
	g := generateFunc(func(context.Context, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		calls.Add(1)
		return response(&genai.Part{Text: "wrong"}), nil
	})
	m := newManager(t, testConfig(t.TempDir()), g)
	jobs, e := m.Spawn([]Task{{Prompt: "review", MaxTokens: 1}})
	if e != nil {
		t.Fatal(e)
	}
	j := awaitJob(t, m, jobs[0].ID)
	if calls.Load() != 0 || j.Status != "limit_reached" || !strings.Contains(j.Result, "not a clean review") {
		t.Fatalf("unfunded inference %+v", j)
	}
}

func TestPagedReadAndScopedEditPreserveUnseenContent(t *testing.T) {
	dir := t.TempDir()
	root, e := os.OpenRoot(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer root.Close()
	f := &Files{root: root}
	data := strings.Repeat("unchanged line\n", 450) + "target text\n" + strings.Repeat("tail\n", 100)
	if e = os.WriteFile(filepath.Join(dir, "large.go"), []byte(data), 0644); e != nil {
		t.Fatal(e)
	}
	raw, e := f.call("read_file", map[string]any{"path": "large.go"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	page := raw.(map[string]any)
	if page["next_line"] != 201 || page["complete"] != false || strings.Contains(page["content"].(string), "target text") {
		t.Fatalf("unbounded page %v", page)
	}
	_, e = f.call("edit_file", map[string]any{"path": "large.go", "old_text": "target text", "new_text": "fixed text", "expected_sha256": page["sha256"]}, []string{"large.go"})
	if e != nil {
		t.Fatal(e)
	}
	actual, e := os.ReadFile(filepath.Join(dir, "large.go"))
	if e != nil || string(actual) != strings.Replace(data, "target text", "fixed text", 1) {
		t.Fatal("damaged unseen content")
	}
	if _, e = f.call("edit_file", map[string]any{"path": "large.go", "old_text": "tail", "new_text": "bad", "expected_sha256": digest(actual)}, []string{"large.go"}); e == nil {
		t.Fatal("ambiguous edit allowed")
	}
	if e = os.WriteFile(filepath.Join(dir, ".env"), []byte("fixed text secret"), 0600); e != nil {
		t.Fatal(e)
	}
	result, e := f.call("search_files", map[string]any{"path": ".", "query": "fixed text"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(result)
	if strings.Contains(string(b), "secret") || !strings.Contains(string(b), `"line":451`) {
		t.Fatalf("bad bounded search %s", b)
	}
}

func TestPrematureFinalCannotHideRemainingWork(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.MaxSteps = 12
	var calls atomic.Int32
	g := generateFunc(func(ctx context.Context, h []*genai.Content, c *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		if c.ResponseMIMEType == "application/json" {
			return checkpointResponse(Checkpoint{Summary: "Still incomplete", Covered: []string{"a.go"}, Remaining: []string{"b.go"}}), nil
		}
		if calls.Add(1) == 1 {
			return response(&genai.Part{FunctionCall: &genai.FunctionCall{ID: "checkpoint", Name: "report_checkpoint", Args: map[string]any{"summary": "one path checked", "covered": []string{"a.go"}, "remaining": []string{"b.go"}, "complete": false}}}), nil
		}
		return response(&genai.Part{Text: "Checked a.go. Next is b.go."}), nil
	})
	m := newManager(t, cfg, g)
	jobs, err := m.Spawn([]Task{{Prompt: "Review a.go and b.go"}})
	if err != nil {
		t.Fatal(err)
	}
	j := awaitJob(t, m, jobs[0].ID)
	if j.Status != "limit_reached" || j.Compactions != 1 || !strings.Contains(j.Error, "no progress") || !strings.Contains(j.Result, "b.go") {
		t.Fatalf("premature success or unbounded retries: %+v", j)
	}
}

func TestBudgetForecastReservesCheckpointInsteadOfAnotherRead(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.MaxTokens = 20000
	cfg.MaxOutput = 2048
	var calls atomic.Int32
	g := generateFunc(func(ctx context.Context, h []*genai.Content, c *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		if calls.Add(1) == 1 {
			r := listResponse()
			r.UsageMetadata.TotalTokenCount = 15000
			return r, nil
		}
		if c.ResponseMIMEType != "application/json" || len(c.Tools) != 0 {
			t.Error("spent reserved summary tokens on more exploration")
		}
		return checkpointResponse(Checkpoint{Summary: "Budget nearly exhausted", Findings: []string{"one tentative finding"}, Remaining: []string{"verify a.go:4"}}), nil
	})
	m := newManager(t, cfg, g)
	jobs, err := m.Spawn([]Task{{Prompt: "Review a.go"}})
	if err != nil {
		t.Fatal(err)
	}
	j := awaitJob(t, m, jobs[0].ID)
	if calls.Load() != 2 || j.Status != "limit_reached" || j.Checkpoint == nil || j.Usage.Total > cfg.MaxTokens || j.Compactions != 0 {
		t.Fatalf("lost reservation or enlarged budget: %+v", j)
	}
}

func TestRecoveryRetainsReadForHashCheckedEdit(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.MaxSteps = 10
	if err := os.WriteFile(filepath.Join(cfg.Workspace, "a.txt"), []byte("before"), 0644); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	g := generateFunc(func(ctx context.Context, h []*genai.Content, c *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		n := calls.Add(1)
		if c.ResponseMIMEType == "application/json" {
			return checkpointResponse(Checkpoint{Summary: "Edit a.txt", Remaining: func() []string {
				if n < 5 {
					return []string{"edit a.txt"}
				}
				return nil
			}(), Complete: n >= 5}), nil
		}
		switch n {
		case 1:
			return response(&genai.Part{FunctionCall: &genai.FunctionCall{ID: "read-id", Name: "read_file", Args: map[string]any{"path": "a.txt"}}, ThoughtSignature: []byte("signature")}), nil
		case 2:
			r := response(&genai.Part{Text: "prepare edit"})
			r.Candidates[0].FinishReason = genai.FinishReasonMaxTokens
			return r, nil
		case 4:
			var hash string
			for _, turn := range h {
				for _, p := range turn.Parts {
					if p.FunctionResponse != nil && p.FunctionResponse.ID == "read-id" {
						out := p.FunctionResponse.Response["output"].(map[string]any)
						hash, _ = out["sha256"].(string)
					}
				}
			}
			if hash == "" {
				t.Error("compaction lost the inspected file and its hash")
			}
			return response(&genai.Part{FunctionCall: &genai.FunctionCall{ID: "edit-id", Name: "edit_file", Args: map[string]any{"path": "a.txt", "old_text": "before", "new_text": "after", "expected_sha256": hash}}}), nil
		default:
			return response(&genai.Part{Text: "edited"}), nil
		}
	})
	m := newManager(t, cfg, g)
	jobs, err := m.Spawn([]Task{{Prompt: "Edit a.txt", WritePaths: []string{"a.txt"}}})
	if err != nil {
		t.Fatal(err)
	}
	j := awaitJob(t, m, jobs[0].ID)
	b, err := os.ReadFile(filepath.Join(cfg.Workspace, "a.txt"))
	if err != nil || string(b) != "after" || j.Status != "completed" || j.Compactions != 1 {
		t.Fatalf("lost usable read state: %+v %s %v", j, b, err)
	}
}

func TestNewFileEvidenceIsProgressWithUnchangedCheckpoint(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.MaxSteps = 12
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(cfg.Workspace, name), []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	g := generateFunc(func(ctx context.Context, h []*genai.Content, c *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		n := calls.Add(1)
		if c.ResponseMIMEType == "application/json" {
			if n >= 8 {
				return checkpointResponse(Checkpoint{Summary: "review finished", Complete: true}), nil
			}
			return checkpointResponse(Checkpoint{Summary: "checking", Remaining: []string{"verify behavior"}}), nil
		}
		if n == 1 || n == 4 {
			name := "a.txt"
			if n == 4 {
				name = "b.txt"
			}
			return response(&genai.Part{FunctionCall: &genai.FunctionCall{ID: name, Name: "read_file", Args: map[string]any{"path": name}}}), nil
		}
		if n == 2 || n == 5 {
			r := response(&genai.Part{Text: "partial"})
			r.Candidates[0].FinishReason = genai.FinishReasonMaxTokens
			return r, nil
		}
		return response(&genai.Part{Text: "review finished"}), nil
	})
	m := newManager(t, cfg, g)
	jobs, err := m.Spawn([]Task{{Prompt: "Review both files"}})
	if err != nil {
		t.Fatal(err)
	}
	j := awaitJob(t, m, jobs[0].ID)
	if j.Status != "completed" || j.Compactions != 2 {
		t.Fatalf("new evidence misclassified as stalled: %+v", j)
	}
}
