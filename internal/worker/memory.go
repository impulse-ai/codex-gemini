package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/impulse-ai/codex-gemini/internal/memory"
	"google.golang.org/genai"
	"os"
	"path/filepath"
	"time"
)

type MemoryInput struct {
	Workspace string `json:"workspace" jsonschema:"Absolute source repository to search; does not grant file access"`
	Query     string `json:"query" jsonschema:"Targeted question or symbols, at most 2000 bytes"`
	Semantic  bool   `json:"semantic,omitempty" jsonschema:"Use cached Gemini embeddings; false defaults to free local keyword search"`
	Limit     int    `json:"limit,omitempty" jsonschema:"1–8 excerpts, default 4"`
	MaxBytes  int    `json:"max_bytes,omitempty" jsonschema:"Combined excerpt text budget 256–12000 bytes, default 6000"`
}

func (m *Manager) initMemory() error {
	var e memory.Embedder
	if m.cfg.EmbeddingModel != "off" {
		e, _ = m.gen.(memory.Embedder)
	}
	var err error
	m.memory, err = memory.Open(filepath.Join(m.state, "memory.sqlite"), m.cfg.EmbeddingModel, e)
	if err != nil {
		return err
	}
	paths, err := filepath.Glob(filepath.Join(m.state, "contexts", "*.json"))
	if err != nil {
		return err
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var c SavedContext
		if err = json.Unmarshal(b, &c); err != nil {
			return err
		}
		if err = m.indexContext(c); err != nil {
			return err
		}
	}
	for _, j := range m.jobs {
		if err = m.indexJob(j); err != nil {
			return err
		}
	}
	return nil
}
func (m *Manager) indexContext(c SavedContext) error {
	packet := c.Packet
	packet.Workspace = "" // Provenance is returned separately, not embedded repeatedly.
	b, err := json.Marshal(packet)
	if err != nil {
		return err
	}
	return m.memory.Upsert(c.Packet.Workspace, "context:"+c.ID, c.Packet.Title, string(b))
}
func (m *Manager) SearchMemory(ctx context.Context, in MemoryInput) (memory.Result, error) {
	m.mu.Lock()
	root, err := canonicalWorkspace(in.Workspace)
	m.mu.Unlock()
	if err != nil {
		return memory.Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return m.memory.Search(ctx, root, in.Query, in.Semantic, in.Limit, in.MaxBytes)
}
func (m *Manager) workerCallContext(ctx context.Context, id, name string, args map[string]any, scopes []string) (any, error) {
	if name != "search_memory" {
		return m.workerCall(id, name, args, scopes)
	}
	m.mu.Lock()
	j, ok := m.jobs[id]
	workspace := ""
	if ok {
		workspace = j.Task.Workspace
	}
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown job")
	}
	b, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	var in MemoryInput
	if err = json.Unmarshal(b, &in); err != nil {
		return nil, err
	}
	in.Workspace = workspace
	return m.SearchMemory(ctx, in)
}
func (g *Gemini) Embed(ctx context.Context, texts []string, task string) ([][]float32, error) {
	if err := g.Limiter.Wait(ctx); err != nil {
		return nil, err
	}
	h := make([]*genai.Content, len(texts))
	for i, t := range texts {
		h[i] = genai.NewContentFromText(t, genai.RoleUser)
	}
	model := g.EmbeddingModel
	if model == "" {
		model = "gemini-embedding-001"
	}
	r, err := g.Client.Models.EmbedContent(ctx, model, h, &genai.EmbedContentConfig{TaskType: task, OutputDimensionality: genai.Ptr(int32(768)), HTTPOptions: &genai.HTTPOptions{RetryOptions: &genai.HTTPRetryOptions{Attempts: genai.Ptr(int32(1))}}})
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, fmt.Errorf("empty embedding response")
	}
	v := make([][]float32, len(r.Embeddings))
	for i, e := range r.Embeddings {
		if e == nil {
			return nil, fmt.Errorf("empty embedding")
		}
		v[i] = e.Values
	}
	return v, nil
}

// Archive only explicit reports and checkpoint findings, never model thought parts
// or entire file/tool transcripts. Status accompanies every remembered claim.
func (m *Manager) indexJob(j *Job) error {
	m.mu.Lock()
	workspace, id, title, status, updated := j.Task.Workspace, j.ID, j.Task.Label, j.Status, j.Updated
	report := clip(j.Result, 10000)
	cp := checkpointText(j.Checkpoint)
	m.mu.Unlock()
	if report == "" && cp == "" {
		return nil
	}
	text := fmt.Sprintf("Status: %s\nUpdated: %s\nReport:\n%s\nCheckpoint:\n%s\nHistorical model claims; verify current files. Read gemini_status for the full report.", status, updated.Format(time.RFC3339), report, cp)
	if title == "" {
		title = "Worker report"
	}
	return m.memory.Upsert(workspace, "job:"+id, title, text)
}
