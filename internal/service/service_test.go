package service

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/impulseai/codex-gemini/internal/worker"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/genai"
)

type blockedGenerator struct {
	entered chan struct{}
	release chan struct{}
}

func (g *blockedGenerator) Generate(ctx context.Context, h []*genai.Content, c *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	g.entered <- struct{}{}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-g.release:
	}
	return &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{FinishReason: genai.FinishReasonStop, Content: genai.NewContentFromText("shared service result", genai.RoleModel)}}}, nil
}

func TestMultipleClientsShareWorkAndDisconnectSafely(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	state := t.TempDir()
	g := &blockedGenerator{entered: make(chan struct{}, 2), release: make(chan struct{})}
	manager, e := worker.New(ctx, worker.Config{StateDir: state, Concurrency: 2, MaxSteps: 2, MaxTokens: 1000, MaxOutput: 100, Timeout: time.Minute, Thinking: "low"}, g)
	if e != nil {
		t.Fatal(e)
	}
	defer manager.Close()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, state, manager) }()
	path, e := SocketPath(state)
	if e != nil {
		t.Fatal(e)
	}
	connect := func() *mcp.ClientSession {
		t.Helper()
		for {
			conn, e := net.DialTimeout("unix", path, 50*time.Millisecond)
			if e == nil {
				c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
				session, e := c.Connect(ctx, &mcp.IOTransport{Reader: conn, Writer: conn}, nil)
				if e != nil {
					t.Fatal(e)
				}
				return session
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	a, b := connect(), connect()
	defer a.Close()
	defer b.Close()
	spawn := func(client *mcp.ClientSession, root string) worker.Job {
		t.Helper()
		res, e := client.CallTool(ctx, &mcp.CallToolParams{Name: "gemini_spawn", Arguments: worker.Task{Workspace: root, Prompt: "test", WritePaths: []string{"x"}}})
		if e != nil || res.IsError {
			t.Fatalf("spawn %v %v", res, e)
		}
		data, e := json.Marshal(res.StructuredContent)
		if e != nil {
			t.Fatal(e)
		}
		var j worker.Job
		if e = json.Unmarshal(data, &j); e != nil {
			t.Fatal(e)
		}
		return j
	}
	root := t.TempDir()
	first := spawn(a, root)
	second := spawn(b, t.TempDir())
	for range 2 {
		select {
		case <-g.entered:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	// A second session cannot reserve an already-owned absolute file.
	conflict, e := b.CallTool(ctx, &mcp.CallToolParams{Name: "gemini_spawn", Arguments: worker.Task{Workspace: root, Prompt: "conflict", WritePaths: []string{"x"}}})
	if e != nil || !conflict.IsError {
		t.Fatalf("cross-session conflict accepted: %v %v", conflict, e)
	}
	a.Close()
	close(g.release)
	for _, id := range []string{first.ID, second.ID} {
		res, e := b.CallTool(ctx, &mcp.CallToolParams{Name: "gemini_wait", Arguments: worker.WaitInput{ID: id, Seconds: 2}})
		if e != nil || res.IsError {
			t.Fatalf("wait %v %v", res, e)
		}
		data, _ := json.Marshal(res.StructuredContent)
		var j worker.Job
		if e = json.Unmarshal(data, &j); e != nil || j.Status != "completed" {
			t.Fatalf("disconnect cancelled job: %+v %v", j, e)
		}
	}
	if e = Stop(ctx, state); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-ctx.Done():
		t.Fatal("service failed to stop")
	}
}
