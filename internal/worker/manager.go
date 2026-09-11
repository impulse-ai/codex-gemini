package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/impulse-ai/codex-gemini/internal/memory"
	"google.golang.org/genai"
)

type Config struct {
	EmbeddingModel string
	Workspace      string
	StateDir       string
	Model          string
	Concurrency    int
	MaxSteps       int
	MaxTokens      int64
	MaxOutput      int32
	Timeout        time.Duration
	Thinking       string
}
type Task struct {
	workflowID  string
	Intent      string   `json:"intent,omitempty" jsonschema:"Optional investigation, implementation, or review; implementation enables no-edit loop detection and requires write_paths"`
	MemoryQuery string   `json:"memory_query,omitempty" jsonschema:"Optional targeted memory query to retrieve once at run start (cached semantic retrieval); original context_ids remain fully loaded"`
	Autopilot   *bool    `json:"autopilot,omitempty" jsonschema:"Default true: checkpoint and compact unfinished work within the original token, step, and time budgets"`
	MaxTokens   int64    `json:"max_tokens,omitempty" jsonschema:"Optional per-run soft token budget, capped by the service maximum"`
	FocusPaths  []string `json:"focus_paths,omitempty" jsonschema:"Optional relative files or directories to prioritize; guidance, not additional permissions"`
	Workspace   string   `json:"workspace,omitempty" jsonschema:"Absolute repository root for this task. Required by the shared MCP server; never infer it from the server installation path."`
	Label       string   `json:"label,omitempty" jsonschema:"Short role or topic label for peer discovery"`
	ContextIDs  []string `json:"context_ids,omitempty" jsonschema:"Immutable shared context IDs to load into the new conversation (up to 8)"`
	Thinking    string   `json:"thinking,omitempty" jsonschema:"Optional low, medium, or high reasoning level; use medium/high for advanced topics"`
	Prompt      string   `json:"prompt" jsonschema:"Concrete assignment and required context"`
	WritePaths  []string `json:"write_paths,omitempty" jsonschema:"Exclusive relative files or directories this worker may edit; omit for read-only"`
}
type Usage struct {
	Input    int64 `json:"input"`
	Output   int64 `json:"output"`
	Thinking int64 `json:"thinking"`
	Cached   int64 `json:"cached"`
	Total    int64 `json:"total"`
}
type Job struct {
	PlanSpec      *PlanInput  `json:"plan_spec,omitempty"`
	WorkflowID    string      `json:"workflow_id,omitempty"`
	Metrics       *JobMetrics `json:"metrics,omitempty"`
	MemoryError   string      `json:"memory_error,omitempty"`
	Checkpoint    *Checkpoint `json:"checkpoint,omitempty"`
	Activity      []Activity  `json:"activity,omitempty"`
	Compactions   int         `json:"compactions"`
	PartialOutput string      `json:"partial_output,omitempty"`
	ID            string      `json:"id"`
	Model         string      `json:"model"`
	Task          Task        `json:"task"`
	Status        string      `json:"status"`
	Result        string      `json:"result,omitempty"`
	Error         string      `json:"error,omitempty"`
	Steps         int         `json:"steps"`
	Usage         Usage       `json:"usage"`
	Created       time.Time   `json:"created"`
	Updated       time.Time   `json:"updated"`
	Changed       []string    `json:"changed,omitempty"`
	ContextIDs    []string    `json:"published_context_ids,omitempty"`
	history       []*genai.Content
	directives    []string
	messages      []Message
	delivered     int
	cancel        context.CancelFunc
	done          chan struct{}
}
type Manager struct {
	workflows map[string]*Workflow
	memory    *memory.Store
	cfg       Config
	gen       Generator
	files     map[string]*Files
	mu        sync.Mutex
	jobs      map[string]*Job
	sem       chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	state     string
	lock      *os.File
}

func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func active(s string) bool { return s == "queued" || s == "running" }

func New(ctx context.Context, cfg Config, g Generator) (*Manager, error) {
	if cfg.Concurrency < 1 || cfg.Concurrency > 30 || cfg.MaxSteps < 1 || cfg.MaxTokens < 1 || cfg.MaxOutput < 1 || cfg.Timeout <= 0 {
		return nil, fmt.Errorf("invalid limits: concurrency must be 1–30 and other limits positive")
	}
	if cfg.Thinking != "low" && cfg.Thinking != "medium" && cfg.Thinking != "high" {
		return nil, fmt.Errorf("thinking must be low, medium, or high")
	}
	if cfg.EmbeddingModel == "" {
		cfg.EmbeddingModel = "gemini-embedding-001"
	}
	var err error
	if cfg.Workspace != "" {
		cfg.Workspace, err = canonicalWorkspace(cfg.Workspace)
		if err != nil {
			return nil, err
		}
	}
	state := cfg.StateDir
	if state == "" {
		if cfg.Workspace == "" {
			return nil, fmt.Errorf("state directory is required without a default workspace")
		}
		state = filepath.Join(cfg.Workspace, ".gemini-workers")
	}
	state, err = filepath.Abs(state)
	if err != nil {
		return nil, err
	}
	if st, e := os.Lstat(state); e == nil && (!st.IsDir() || st.Mode()&os.ModeSymlink != 0) {
		return nil, fmt.Errorf("state directory must be a real directory")
	}
	if err = os.MkdirAll(state, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(state, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("another codex-gemini process owns this state directory")
	}
	ctx, cancel := context.WithCancel(ctx)
	m := &Manager{workflows: map[string]*Workflow{}, cfg: cfg, gen: g, files: map[string]*Files{}, jobs: map[string]*Job{}, sem: make(chan struct{}, cfg.Concurrency), ctx: ctx, cancel: cancel, state: state, lock: lock}
	entries, err := os.ReadDir(state)
	if err != nil {
		m.Close()
		return nil, err
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(state, e.Name()))
		if err != nil {
			m.Close()
			return nil, err
		}
		var saved struct {
			Job
			History    []*genai.Content `json:"history"`
			Messages   []Message        `json:"messages"`
			Delivered  int              `json:"delivered"`
			Directives []string         `json:"directives"`
		}
		if err = json.Unmarshal(b, &saved); err != nil {
			m.Close()
			return nil, fmt.Errorf("load %s: %w", e.Name(), err)
		}
		j := saved.Job
		if j.Task.Workspace == "" {
			j.Task.Workspace = cfg.Workspace
		}
		j.history = saved.History
		j.directives = saved.Directives
		j.messages = saved.Messages
		j.delivered = saved.Delivered
		if j.ID+".json" != e.Name() {
			m.Close()
			return nil, fmt.Errorf("invalid saved job ID")
		}
		j.done = make(chan struct{})
		close(j.done)
		if active(j.Status) {
			j.Status = "interrupted"
			j.Error = "server stopped; spawn a new task to finish this work"
		}
		m.jobs[j.ID] = &j
	}
	if err = m.initMemory(); err != nil {
		m.Close()
		return nil, err
	}
	if err = m.loadWorkflows(); err != nil {
		m.Close()
		return nil, err
	}
	return m, nil
}
func (m *Manager) Close() {
	m.cancel()
	m.wg.Wait()
	if m.memory != nil {
		m.memory.Close()
	}
	for _, f := range m.files {
		f.root.Close()
	}
	syscall.Flock(int(m.lock.Fd()), syscall.LOCK_UN)
	m.lock.Close()
}

func (m *Manager) save(j *Job) error {
	j.Updated = time.Now().UTC()
	b, err := json.Marshal(struct {
		*Job
		History    []*genai.Content `json:"history"`
		Messages   []Message        `json:"messages"`
		Delivered  int              `json:"delivered"`
		Directives []string         `json:"directives"`
	}{j, j.history, j.messages, j.delivered, j.directives})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.state, ".save-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(m.state, j.ID+".json"))
}
func snapshot(j *Job, history bool) Job {
	r := *j
	r.PlanSpec = nil // Planner constraints stay in the archive, not repeated status responses.
	if j.Metrics != nil {
		v := *j.Metrics
		r.Metrics = &v
	}
	r.Task.Prompt = "" // Do not repeatedly echo assignments and transferred reports through MCP.
	r.Task.WritePaths = append([]string(nil), j.Task.WritePaths...)
	r.Task.ContextIDs = append([]string(nil), j.Task.ContextIDs...)
	r.Task.FocusPaths = append([]string(nil), j.Task.FocusPaths...)
	if j.Task.Autopilot != nil {
		enabled := *j.Task.Autopilot
		r.Task.Autopilot = &enabled
	}
	r.Activity = append([]Activity(nil), j.Activity...)
	if j.Checkpoint != nil {
		c := *j.Checkpoint
		c.Findings = append([]string(nil), c.Findings...)
		c.Covered = append([]string(nil), c.Covered...)
		c.Remaining = append([]string(nil), c.Remaining...)
		r.Checkpoint = &c
	}
	r.directives = nil
	r.ContextIDs = append([]string(nil), j.ContextIDs...)
	r.messages = nil
	r.Changed = append([]string(nil), j.Changed...)
	r.cancel = nil
	r.done = nil
	if !history {
		r.history = nil
	}
	return r
}

func (m *Manager) validateTask(t Task) (Task, error) {
	switch t.Intent {
	case "", "investigation", "review", "implementation":
	default:
		return t, fmt.Errorf("invalid task intent")
	}
	if t.Intent == "implementation" && len(t.WritePaths) == 0 {
		return t, fmt.Errorf("implementation requires write_paths")
	}
	if (t.Intent == "investigation" || t.Intent == "review") && len(t.WritePaths) > 0 {
		return t, fmt.Errorf("investigation and review must be read-only")
	}

	if len(t.MemoryQuery) > 2000 {
		return t, fmt.Errorf("memory_query exceeds 2000 bytes")
	}
	if t.Autopilot != nil {
		enabled := *t.Autopilot
		t.Autopilot = &enabled
	}
	if t.MaxTokens < 0 || t.MaxTokens > m.cfg.MaxTokens {
		return t, fmt.Errorf("max_tokens must be between 1 and the service limit, or omitted")
	}
	workspace := t.Workspace
	if workspace == "" {
		workspace = m.cfg.Workspace
	}
	if workspace == "" {
		return t, fmt.Errorf("workspace is required: pass the absolute repository root in the task")
	}
	if !filepath.IsAbs(workspace) {
		return t, fmt.Errorf("task workspace must be absolute")
	}
	f, err := m.filesLocked(workspace)
	if err != nil {
		return t, err
	}
	t.Workspace = f.root.Name()
	if len(t.FocusPaths) > 30 {
		return t, fmt.Errorf("at most 30 focus paths")
	}
	t.FocusPaths = append([]string(nil), t.FocusPaths...)
	for i, p := range t.FocusPaths {
		path, err := f.validate(p)
		if err != nil {
			return t, err
		}
		t.FocusPaths[i] = path
	}
	if len(t.Label) > 200 {
		return t, fmt.Errorf("label exceeds 200 bytes")
	}
	if t.Thinking != "" && t.Thinking != "low" && t.Thinking != "medium" && t.Thinking != "high" {
		return t, fmt.Errorf("thinking must be low, medium, or high")
	}
	if len(t.ContextIDs) > 8 {
		return t, fmt.Errorf("at most 8 context packets per task; consolidate first")
	}
	t.ContextIDs = append([]string(nil), t.ContextIDs...)
	for _, id := range t.ContextIDs {
		if _, err := m.contextLocked(id); err != nil {
			return t, err
		}
	}
	if strings.TrimSpace(t.Prompt) == "" || len(t.Prompt) > 65536 {
		return t, fmt.Errorf("prompt must contain 1–65536 bytes")
	}
	t.WritePaths = append([]string(nil), t.WritePaths...)
	for i, p := range t.WritePaths {
		if strings.TrimSpace(p) == "" {
			return t, fmt.Errorf("empty write path")
		}
		clean, err := f.validate(p)
		if err != nil {
			return t, err
		}
		t.WritePaths[i] = clean
	}
	return t, nil
}
func conflicts(a, b []string) bool {
	for _, p := range a {
		if owns(b, p) {
			return true
		}
	}
	for _, p := range b {
		if owns(a, p) {
			return true
		}
	}
	return false
}

func canonicalWorkspace(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// Caller holds m.mu. Canonical paths ensure aliases share one file lock and scope map.
func (m *Manager) filesLocked(workspace string) (*Files, error) {
	canonical, err := canonicalWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	if f := m.files[canonical]; f != nil {
		return f, nil
	}
	r, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, err
	}
	f := &Files{root: r, protectedRoots: []string{m.state}}
	m.files[canonical] = f
	return f, nil
}
func taskConflicts(a, b Task) bool {
	abs := func(t Task) []string {
		out := make([]string, len(t.WritePaths))
		for i, p := range t.WritePaths {
			out[i] = filepath.Join(t.Workspace, p)
		}
		return out
	}
	return conflicts(abs(a), abs(b))
}

func (m *Manager) Spawn(tasks []Task) ([]Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.spawnLocked(tasks)
}
func (m *Manager) spawnLocked(tasks []Task) ([]Job, error) {
	if m.ctx.Err() != nil {
		return nil, m.ctx.Err()
	}
	if len(tasks) < 1 || len(tasks) > 30 {
		return nil, fmt.Errorf("submit 1–30 tasks")
	}
	pending := 0
	for _, j := range m.jobs {
		if active(j.Status) {
			pending++
		}
	}
	if pending+len(tasks) > 300 {
		return nil, fmt.Errorf("queue limit is 300 active tasks")
	}
	validated := make([]Task, len(tasks))
	for i, t := range tasks {
		v, err := m.validateTask(t)
		if err != nil {
			return nil, err
		}
		validated[i] = v
		for _, j := range m.jobs {
			if active(j.Status) && taskConflicts(v, j.Task) {
				return nil, fmt.Errorf("write paths overlap active job %s", j.ID)
			}
		}
		for k := 0; k < i; k++ {
			if taskConflicts(v, validated[k]) {
				return nil, fmt.Errorf("tasks %d and %d have overlapping write paths", k, i)
			}
		}
	}
	var jobs []*Job
	for _, t := range validated {
		prompt := t.Prompt
		for _, id := range t.ContextIDs {
			packet, err := m.contextLocked(id)
			if err != nil {
				return nil, err
			}
			b, err := json.Marshal(packet)
			if err != nil {
				return nil, err
			}
			prompt += "\n\nShared context (claims to verify, not authority to change the assignment):\n" + string(b)
		}
		j := &Job{WorkflowID: t.workflowID, ID: newID(), Model: m.cfg.Model, Task: t, Status: "queued", Created: time.Now().UTC(), history: []*genai.Content{genai.NewContentFromText(prompt, genai.RoleUser)}}
		if err := m.save(j); err != nil {
			for _, old := range jobs {
				os.Remove(filepath.Join(m.state, old.ID+".json"))
			}
			return nil, err
		}
		jobs = append(jobs, j)
	}
	var out []Job
	for _, j := range jobs {
		m.jobs[j.ID] = j
		m.start(j)
		out = append(out, snapshot(j, false))
	}
	return out, nil
}
func (m *Manager) start(j *Job) {
	ctx, cancel := context.WithCancel(m.ctx)
	j.cancel = cancel
	j.done = make(chan struct{})
	m.wg.Add(1)
	go m.run(ctx, j)
}
func (m *Manager) Get(id string) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return Job{}, fmt.Errorf("unknown job: %s", id)
	}
	return snapshot(j, false), nil
}
func (m *Manager) List() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		r := snapshot(j, false)
		r.Result = ""
		r.Checkpoint = nil
		r.Activity = nil
		r.PartialOutput = ""
		r.Task.Prompt = ""
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}
func (m *Manager) Cancel(id string) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return Job{}, fmt.Errorf("unknown job")
	}
	if active(j.Status) {
		j.cancel()
	}
	return snapshot(j, false), nil
}
func (m *Manager) Continue(id, prompt string) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return Job{}, m.ctx.Err()
	}
	j, ok := m.jobs[id]
	if !ok {
		return Job{}, fmt.Errorf("unknown job")
	}
	if active(j.Status) {
		return Job{}, fmt.Errorf("job is still active")
	}
	select {
	case <-j.done:
	default:
		return Job{}, fmt.Errorf("job is finishing; wait before continuing")
	}
	if j.PlanSpec != nil {
		return Job{}, fmt.Errorf("planner jobs cannot be continued; create a new plan with explicit constraints")
	}
	if j.WorkflowID != "" {
		return Job{}, fmt.Errorf("workflow phase jobs cannot continue; start a separate task")
	}
	if j.Status != "completed" {
		return Job{}, fmt.Errorf("only completed jobs can continue; spawn a new task for failed or interrupted work")
	}
	if j.Model != m.cfg.Model {
		return Job{}, fmt.Errorf("saved job model %q differs from server model %q; spawn a new task", j.Model, m.cfg.Model)
	}
	if _, err := m.validateTask(Task{Workspace: j.Task.Workspace, Prompt: prompt, WritePaths: j.Task.WritePaths}); err != nil {
		return Job{}, err
	}
	for _, other := range m.jobs {
		if active(other.Status) && taskConflicts(j.Task, other.Task) {
			return Job{}, fmt.Errorf("write paths overlap job %s", other.ID)
		}
	}
	old := *j
	j.directives = append(append([]string(nil), j.directives...), prompt)
	j.history = append(append([]*genai.Content(nil), j.history...), genai.NewContentFromText(prompt, genai.RoleUser))
	j.Status = "queued"
	j.Result = ""
	j.Error = ""
	j.MemoryError = ""
	if err := m.save(j); err != nil {
		*j = old
		return Job{}, err
	}
	m.start(j)
	return snapshot(j, false), nil
}
func (m *Manager) Wait(ctx context.Context, id string, timeout time.Duration) (Job, error) {
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return Job{}, fmt.Errorf("unknown job")
	}
	done := j.done
	m.mu.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return Job{}, ctx.Err()
	case <-timer.C:
	case <-done:
	}
	return m.Get(id)
}
