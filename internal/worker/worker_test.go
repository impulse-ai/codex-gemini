package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/time/rate"
	"google.golang.org/genai"
)

type generateFunc func(context.Context, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error)

func (f generateFunc) Generate(c context.Context, h []*genai.Content, g *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	return f(c, h, g)
}
func response(parts ...*genai.Part) *genai.GenerateContentResponse {
	return &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{Content: &genai.Content{Role: "model", Parts: parts}, FinishReason: genai.FinishReasonStop}}, UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 10, CandidatesTokenCount: 5, TotalTokenCount: 15}}
}
func testConfig(dir string) Config {
	return Config{Workspace: dir, Concurrency: 30, MaxSteps: 5, MaxTokens: 100000, MaxOutput: 1024, Timeout: time.Minute, Thinking: "low"}
}
func newManager(t *testing.T, cfg Config, g Generator) *Manager {
	t.Helper()
	m, err := New(context.Background(), cfg, g)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}
func awaitJob(t *testing.T, m *Manager, id string) Job {
	t.Helper()
	j, e := m.Wait(context.Background(), id, 5*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	if active(j.Status) {
		t.Fatalf("job did not finish: %+v", j)
	}
	return j
}

func TestThirtyConcurrentWorkers(t *testing.T) {
	var current, peak atomic.Int32
	entered := make(chan struct{}, 30)
	release := make(chan struct{})
	g := generateFunc(func(ctx context.Context, h []*genai.Content, c *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		n := current.Add(1)
		defer current.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		entered <- struct{}{}
		select {
		case <-release:
			return response(&genai.Part{Text: "done"}), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	m := newManager(t, testConfig(t.TempDir()), g)
	tasks := make([]Task, 30)
	for i := range tasks {
		tasks[i] = Task{Prompt: fmt.Sprintf("task %d", i)}
	}
	jobs, err := m.Spawn(tasks)
	if err != nil {
		t.Fatal(err)
	}
	for range 30 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("did not start 30 workers")
		}
	}
	close(release)
	for _, job := range jobs {
		j := awaitJob(t, m, job.ID)
		if j.Status != "completed" || j.Usage.Total != 15 {
			t.Fatalf("unexpected job %+v", j)
		}
	}
	if peak.Load() != 30 {
		t.Fatalf("peak %d", peak.Load())
	}
}

func TestAgentEditsPreservesHistoryAndContinues(t *testing.T) {
	dir := t.TempDir()
	var turns atomic.Int32
	g := generateFunc(func(ctx context.Context, h []*genai.Content, c *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		switch turns.Add(1) {
		case 1:
			return response(&genai.Part{ThoughtSignature: []byte("signature"), FunctionCall: &genai.FunctionCall{ID: "call-1", Name: "write_file", Args: map[string]any{"path": "result.txt", "content": "hello", "expected_sha256": "new"}}}), nil
		case 2:
			if string(h[1].Parts[0].ThoughtSignature) != "signature" || h[2].Parts[0].FunctionResponse.ID != "call-1" {
				t.Error("lost signature or call ID")
			}
			return response(&genai.Part{Text: "wrote result.txt"}), nil
		default:
			if len(h) != 5 || h[4].Parts[0].Text != "follow up" {
				t.Errorf("lost conversation: %+v", h)
			}
			return response(&genai.Part{Text: "followed up"}), nil
		}
	})
	m := newManager(t, testConfig(dir), g)
	jobs, err := m.Spawn([]Task{{Prompt: "write hello", WritePaths: []string{"result.txt"}}})
	if err != nil {
		t.Fatal(err)
	}
	j := awaitJob(t, m, jobs[0].ID)
	if j.Status != "completed" || len(j.Changed) != 1 {
		t.Fatalf("job %+v", j)
	}
	b, e := os.ReadFile(filepath.Join(dir, "result.txt"))
	if e != nil || string(b) != "hello" {
		t.Fatalf("file %q %v", b, e)
	}
	if _, err = m.Continue(j.ID, "follow up"); err != nil {
		t.Fatal(err)
	}
	j = awaitJob(t, m, j.ID)
	if j.Result != "followed up" || j.Usage.Total != 45 {
		t.Fatalf("job %+v", j)
	}
}

func TestFileBoundariesAndStaleWrites(t *testing.T) {
	dir := t.TempDir()
	root, e := os.OpenRoot(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer root.Close()
	f := &Files{root: root}
	write := func(p, content, hash string, scopes []string) error {
		_, e := f.call("write_file", map[string]any{"path": p, "content": content, "expected_sha256": hash}, scopes)
		return e
	}
	for _, p := range []string{"../escape", "/tmp/escape", ".git/config", ".GIT/config", ".codex/config.toml", ".env", ".gemini-workers/x"} {
		if e := write(p, "bad", "new", []string{"."}); e == nil {
			t.Fatalf("allowed %s", p)
		}
	}
	if e := write("a.txt", "one", "new", nil); e == nil {
		t.Fatal("read-only write allowed")
	}
	if e := write("a.txt", "one", "new", []string{"a.txt"}); e != nil {
		t.Fatal(e)
	}
	if e := write("a.txt", "two", "new", []string{"a.txt"}); e == nil {
		t.Fatal("stale write allowed")
	}
	if e := write("a.txt", "two", digest([]byte("one")), []string{"a.txt"}); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink("a.txt", filepath.Join(dir, "alias")); e != nil {
		t.Fatal(e)
	}
	if _, e = f.call("read_file", map[string]any{"path": "alias"}, nil); e == nil {
		t.Fatal("symlink read allowed")
	}
	if e = os.Symlink(t.TempDir(), filepath.Join(dir, "outside")); e != nil {
		t.Fatal(e)
	}
	if e := write("outside/x", "bad", "new", []string{"."}); e == nil {
		t.Fatal("symlink escape allowed")
	}
}

func TestOverlapCancellationAndConcurrencyLimit(t *testing.T) {
	entered := make(chan struct{}, 2)
	g := generateFunc(func(ctx context.Context, h []*genai.Content, c *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	cfg := testConfig(t.TempDir())
	cfg.Concurrency = 1
	m := newManager(t, cfg, g)
	if _, e := m.Spawn([]Task{{Prompt: "one", WritePaths: []string{"src"}}, {Prompt: "two", WritePaths: []string{"src/a.go"}}}); e == nil {
		t.Fatal("batch overlap accepted")
	}
	if len(m.List()) != 0 {
		t.Fatal("partial batch started")
	}
	jobs, e := m.Spawn([]Task{{Prompt: "one", WritePaths: []string{"src"}}})
	if e != nil {
		t.Fatal(e)
	}
	<-entered
	if _, e = m.Spawn([]Task{{Prompt: "two", WritePaths: []string{"src/a.go"}}}); e == nil {
		t.Fatal("active overlap accepted")
	}
	queued, e := m.Spawn([]Task{{Prompt: "queued"}})
	if e != nil {
		t.Fatal(e)
	}
	m.Cancel(queued[0].ID)
	if j := awaitJob(t, m, queued[0].ID); j.Status != "cancelled" {
		t.Fatalf("queued cancel: %+v", j)
	}
	m.Cancel(jobs[0].ID)
	if j := awaitJob(t, m, jobs[0].ID); j.Status != "cancelled" {
		t.Fatalf("running cancel: %+v", j)
	}
	select {
	case <-entered:
		t.Fatal("queued worker called provider")
	default:
	}
}

func TestPersistenceAndWorkspaceLock(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	g := generateFunc(func(context.Context, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		return response(&genai.Part{Text: "saved"}), nil
	})
	m, e := New(context.Background(), cfg, g)
	if e != nil {
		t.Fatal(e)
	}
	if other, e := New(context.Background(), cfg, g); e == nil {
		other.Close()
		m.Close()
		t.Fatal("double workspace lock")
	}
	jobs, e := m.Spawn([]Task{{Prompt: "persist"}})
	if e != nil {
		m.Close()
		t.Fatal(e)
	}
	awaitJob(t, m, jobs[0].ID)
	m.Close()
	m = newManager(t, cfg, g)
	j, e := m.Get(jobs[0].ID)
	if e != nil || j.Result != "saved" {
		t.Fatalf("restored %+v %v", j, e)
	}
	if _, e = m.Continue(j.ID, "continue"); e != nil {
		t.Fatal(e)
	}
	awaitJob(t, m, j.ID)
}

func TestMCPRoundTrip(t *testing.T) {
	g := generateFunc(func(context.Context, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		return response(&genai.Part{Text: "MCP works"}), nil
	})
	m := newManager(t, testConfig(t.TempDir()), g)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, ct := mcp.NewInMemoryTransports()
	ss, e := Server(m).Connect(ctx, st, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, e := client.Connect(ctx, ct, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer cs.Close()
	list, e := cs.ListTools(ctx, nil)
	if e != nil || len(list.Tools) != 13 {
		t.Fatalf("tools %v %v", list, e)
	}
	result, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "gemini_spawn", Arguments: map[string]any{"prompt": "test"}})
	if e != nil {
		t.Fatal(e)
	}
	if result.IsError {
		t.Fatalf("MCP error %+v", result)
	}
	b, e := json.Marshal(result.StructuredContent)
	if e != nil {
		t.Fatal(e)
	}
	var j Job
	if e = json.Unmarshal(b, &j); e != nil {
		t.Fatal(e)
	}
	if awaitJob(t, m, j.ID).Result != "MCP works" {
		t.Fatal("missing result")
	}
}

func TestSDKWireFormatAndRetry(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "test-key" {
			t.Error("missing API auth")
		}
		if r.URL.Path != "/v1beta/models/gemini-3.8-flash:generateContent" {
			t.Errorf("wrong endpoint %s", r.URL.Path)
		}
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":{"code":429,"message":"retry","status":"RESOURCE_EXHAUSTED"}}`)
			return
		}
		var body struct {
			Contents []*genai.Content `json:"contents"`
		}
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			t.Error(e)
		}
		if len(body.Contents) != 3 || body.Contents[2].Parts[0].FunctionResponse.ID != "call-id" {
			t.Error("lost wire call ID")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response(&genai.Part{Text: "ok"}))
	}))
	defer server.Close()
	c, e := genai.NewClient(context.Background(), &genai.ClientConfig{APIKey: "test-key", Backend: genai.BackendGeminiAPI, HTTPOptions: genai.HTTPOptions{BaseURL: server.URL}})
	if e != nil {
		t.Fatal(e)
	}
	g := &Gemini{Client: c, Model: "gemini-3.8-flash", Limiter: rate.NewLimiter(rate.Inf, 1)}
	h := []*genai.Content{genai.NewContentFromText("test", genai.RoleUser), {Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "call-id", Name: "read_file", Args: map[string]any{"path": "a"}}}}}, {Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "call-id", Name: "read_file", Response: map[string]any{"output": "a"}}}}}}
	if _, e = g.Generate(context.Background(), h, modelConfig(100, "low", nil)); e != nil {
		t.Fatal(e)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests %d", requests.Load())
	}
}
