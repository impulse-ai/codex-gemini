package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/genai"
)

func sharedConfig(t *testing.T) Config {
	t.Helper()
	c := testConfig("")
	c.StateDir = t.TempDir()
	return c
}

func TestIndependentWorkspaceEdits(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	g := generateFunc(func(ctx context.Context, h []*genai.Content, c *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		if len(h) == 1 {
			return response(&genai.Part{FunctionCall: &genai.FunctionCall{ID: "write", Name: "write_file", Args: map[string]any{"path": "result.txt", "content": h[0].Parts[0].Text, "expected_sha256": "new"}}}), nil
		}
		return response(&genai.Part{Text: "done"}), nil
	})
	m := newManager(t, sharedConfig(t), g)
	jobs, err := m.Spawn([]Task{{Workspace: a, Prompt: "repo A", WritePaths: []string{"result.txt"}}, {Workspace: b, Prompt: "repo B", WritePaths: []string{"result.txt"}}})
	if err != nil {
		t.Fatal(err)
	}
	for i, j := range jobs {
		result := awaitJob(t, m, j.ID)
		if result.Status != "completed" {
			t.Fatalf("job %+v", result)
		}
		p := []string{a, b}[i]
		data, e := os.ReadFile(filepath.Join(p, "result.txt"))
		if e != nil || string(data) != []string{"repo A", "repo B"}[i] {
			t.Fatalf("cross-workspace write %q %v", data, e)
		}
	}
	if len(m.UsageReport().Workspaces) != 2 {
		t.Fatal("missing workspace usage")
	}
	if _, err = m.Spawn([]Task{{Prompt: "no workspace"}}); err == nil {
		t.Fatal("inferred wrong workspace")
	}
	if _, err = m.Spawn([]Task{{Workspace: "relative", Prompt: "bad"}}); err == nil {
		t.Fatal("accepted relative workspace")
	}
}

func TestOverlappingWorkspaceAliases(t *testing.T) {
	parent := t.TempDir()
	nested := filepath.Join(parent, "nested")
	if e := os.Mkdir(nested, 0755); e != nil {
		t.Fatal(e)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if e := os.Symlink(nested, alias); e != nil {
		t.Fatal(e)
	}
	g := generateFunc(func(ctx context.Context, h []*genai.Content, c *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	m := newManager(t, sharedConfig(t), g)
	for _, root := range []string{nested, alias} {
		if _, err := m.Spawn([]Task{{Workspace: parent, Prompt: "parent", WritePaths: []string{"nested/file.go"}}, {Workspace: root, Prompt: "child", WritePaths: []string{"file.go"}}}); err == nil {
			t.Fatalf("overlap allowed via %s", root)
		}
	}
	jobs, e := m.Spawn([]Task{{Workspace: parent, Prompt: "hold", WritePaths: []string{"nested"}}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = m.Spawn([]Task{{Workspace: alias, Prompt: "conflict", WritePaths: []string{"a"}}}); e == nil {
		t.Fatal("active alias overlap allowed")
	}
	m.Cancel(jobs[0].ID)
	awaitJob(t, m, jobs[0].ID)
}

func TestCrossWorkspaceContextHandoff(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	canonicalA, e := canonicalWorkspace(a)
	if e != nil {
		t.Fatal(e)
	}
	g := generateFunc(func(ctx context.Context, h []*genai.Content, c *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		return response(&genai.Part{Text: "findings"}), nil
	})
	m := newManager(t, sharedConfig(t), g)
	p, e := m.Publish("codex", ContextPacket{Workspace: a, Title: "contract", Objective: "share", Summary: "protocol v2", Artifacts: []Artifact{{Path: "contract.go"}}})
	if e != nil {
		t.Fatal(e)
	}
	jobs, e := m.Spawn([]Task{{Workspace: a, Prompt: "analyze", ContextIDs: []string{p.ID}}})
	if e != nil {
		t.Fatal(e)
	}
	awaitJob(t, m, jobs[0].ID)
	next, e := m.Handoff(HandoffInput{Sources: []string{jobs[0].ID}, Task: Task{Workspace: b, Prompt: "use contract"}})
	if e != nil {
		t.Fatal(e)
	}
	result := awaitJob(t, m, next.ID)
	canonicalB, e := canonicalWorkspace(b)
	if e != nil {
		t.Fatal(e)
	}
	if result.Task.Workspace != canonicalB {
		t.Fatal("lost destination workspace")
	}
	saved, e := m.ReadContext(p.ID)
	if e != nil || saved.Packet.Workspace != canonicalA {
		t.Fatal("lost artifact provenance")
	}
	m.mu.Lock()
	h := m.jobs[next.ID].history[0].Parts[0].Text
	m.mu.Unlock()
	if !strings.Contains(h, canonicalA) {
		t.Fatal("handoff lost source identity")
	}
	if _, e = m.Handoff(HandoffInput{Sources: []string{next.ID}, Task: Task{Prompt: "missing destination"}}); e == nil {
		t.Fatal("handoff inferred destination")
	}
	if _, e = m.Publish("codex", ContextPacket{Title: "invalid", Summary: "missing source", Artifacts: []Artifact{{Path: "x"}}}); e == nil {
		t.Fatal("artifact has no source workspace")
	}
}
