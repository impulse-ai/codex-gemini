package worker

import (
	"context"
	"encoding/json"
	"google.golang.org/genai"
	"strings"
	"testing"
)

func TestMemorySharedBetweenCodexAndWorkers(t *testing.T) {
	cfg := testConfig(t.TempDir())
	g := generateFunc(func(ctx context.Context, h []*genai.Content, c *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		b, _ := json.Marshal(h)
		if !strings.Contains(string(b), "Unique invariant: drain before release") {
			t.Error("memory query omitted shared knowledge")
		}
		return response(&genai.Part{Text: "Unique verified-by-model conclusion: retain admission barrier"}), nil
	})
	m := newManager(t, cfg, g)
	packet, err := m.Publish("codex", ContextPacket{Title: "Browser", Summary: "Unique invariant: drain before release"})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := m.Spawn([]Task{{Prompt: "Investigate browser", MemoryQuery: "drain release"}})
	if err != nil {
		t.Fatal(err)
	}
	j := awaitJob(t, m, jobs[0].ID)
	if j.Status != "completed" {
		t.Fatalf("job %+v", j)
	}
	r, err := m.SearchMemory(context.Background(), MemoryInput{Workspace: cfg.Workspace, Query: "admission barrier"})
	if err != nil || len(r.Hits) == 0 || r.Hits[0].Source != "job:"+j.ID {
		t.Fatalf("Codex cannot retrieve worker finding: %+v %v", r, err)
	}
	raw, err := m.workerCallContext(context.Background(), j.ID, "search_memory", map[string]any{"query": "drain release", "workspace": t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(raw)
	if !strings.Contains(string(b), packet.ID) {
		t.Fatalf("worker cannot retrieve Codex packet: %s", b)
	}
}
