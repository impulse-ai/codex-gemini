package worker

import (
	"context"
	"github.com/impulse-ai/codex-gemini/internal/memory"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type IDInput struct {
	ID string `json:"id"`
}
type BatchInput struct {
	Tasks []Task `json:"tasks"`
}
type JobsOutput struct {
	Jobs []Job `json:"jobs"`
}
type ContinueInput struct {
	ID     string `json:"id"`
	Prompt string `json:"prompt"`
}
type WaitInput struct {
	ID      string `json:"id"`
	Seconds int    `json:"seconds,omitempty" jsonschema:"Wait up to 1–50 seconds; defaults to 30"`
}

func Server(m *Manager) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "Gemini", Version: "0.7.0"}, &mcp.ServerOptions{Instructions: "This shared service works across repositories and Codex sessions. Always pass the current task's absolute repository root as workspace when spawning, batching, or handing off. Never use the server binary's installation directory as the workspace. Status and usage report workspace identities. File access stays within each job's workspace; write ownership and the worker limit are shared across all sessions. Use gemini_search_memory for small historical excerpts before replaying large reports; keyword retrieval is free/local, semantic retrieval uses cached embeddings. Optional task memory_query retrieves once at startup. Memory does not replace full constraints or current file verification. Publish reusable technical context once with its source workspace and pass context_ids to tasks, including across repositories. Use labeled tasks, narrow focus_paths, disjoint write_paths, and low thinking for focused reviews. Autopilot defaults on: paginated reads, saved findings, reserved low-thinking checkpoint generation, and at most two context compactions within the original budget. max_tokens optionally lowers the per-run budget. Check checkpoint.remaining and status: incomplete work is never a clean review. Recovery stops on no progress or exhausted limits; do not blindly relaunch failed reviews. Workers exchange brief messages and context IDs directly. Handoff starts fresh from reports and published context; specify the destination workspace and write_paths. Continue keeps the original workspace and history. Workers cannot execute shell commands; Codex validates changes. Closing a client connection does not stop jobs. Do not send credentials in prompts."})
	mcp.AddTool(s, &mcp.Tool{Name: "gemini_plan", Description: "Decompose a larger implementation assignment into up to 12 bounded steps. Runs a read-only planner using task.write_paths as the maximum proposed scope; does not execute edits. Inspect gemini_plan_status before dispatching."}, func(ctx context.Context, r *mcp.CallToolRequest, in PlanInput) (*mcp.CallToolResult, Job, error) {
		out, err := m.StartPlan(in)
		return nil, out, err
	})
	mcp.AddTool(s, &mcp.Tool{Name: "gemini_plan_status", Description: "Validate completed planner JSON, file scopes, dependencies, and acceptance checks; return parallel execution waves. plan_ready is structural validation, not approval or test success. Execution uses separate job budgets."}, func(ctx context.Context, r *mcp.CallToolRequest, in IDInput) (*mcp.CallToolResult, PlanOutput, error) {
		out, err := m.GetPlan(in.ID)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{Name: "gemini_workflow", Description: "Start bounded investigation → implementation → independent review. Only implementation has write permissions. One total generation-token/time budget, no retries. Final ready_for_validation requires Codex tests and review."}, func(ctx context.Context, r *mcp.CallToolRequest, in WorkflowInput) (*mcp.CallToolResult, Workflow, error) {
		out, err := m.StartWorkflow(in)
		return nil, out, err
	})
	mcp.AddTool(s, &mcp.Tool{Name: "gemini_workflow_status", Description: "Inspect workflow phase job IDs, status, and accounted generation tokens. Read each job for findings. No automatic test validation."}, func(ctx context.Context, r *mcp.CallToolRequest, in IDInput) (*mcp.CallToolResult, Workflow, error) {
		out, err := m.GetWorkflow(in.ID)
		return nil, out, err
	})
	mcp.AddTool(s, &mcp.Tool{Name: "gemini_workflow_cancel", Description: "Cancel workflow and active phase; status settles after actual worker shutdown. Existing edits remain."}, func(ctx context.Context, r *mcp.CallToolRequest, in IDInput) (*mcp.CallToolResult, Workflow, error) {
		out, err := m.CancelWorkflow(in.ID)
		return nil, out, err
	})
	mcp.AddTool(s, &mcp.Tool{Name: "gemini_metrics", Description: "Measured job outcomes, actual edits, inspection/repeated-read counts, and tokens before first edit. Legacy jobs without metrics are identified separately. No inferred test success or dollar savings."}, func(ctx context.Context, r *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, MetricsReport, error) {
		return nil, m.OutcomeMetrics(), nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "gemini_search_memory", Description: "Search shared SQLite context memory with bounded excerpts and provenance. Keyword search is free/local by default; semantic=true uses cached embeddings. Explicit workspace scopes retrieval. Excerpts do not replace complete constraints or current file verification."}, func(ctx context.Context, r *mcp.CallToolRequest, in MemoryInput) (*mcp.CallToolResult, memory.Result, error) {
		out, err := m.SearchMemory(ctx, in)
		return nil, out, err
	})
	mcp.AddTool(s, &mcp.Tool{Name: "gemini_memory_stats", Description: "Read local memory chunk/vector counts and attempted embedding requests/input bytes. Embedding billing tokens and net savings are not measured."}, func(ctx context.Context, r *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, memory.Stats, error) {
		out, err := m.memory.Stats()
		return nil, out, err
	})
	mcp.AddTool(s, &mcp.Tool{Name: "gemini_usage", Description: "Read compact cumulative token totals grouped by model and job status counts. Includes saved jobs; does not query Google billing or estimate currency."}, func(ctx context.Context, r *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, UsageReport, error) {
		return nil, m.UsageReport(), nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "gemini_publish_context", Description: "Persist an immutable technical brief with constraints, decisions, evidence, artifact references, and open questions. Reuse the returned ID across tasks."}, func(ctx context.Context, r *mcp.CallToolRequest, in ContextPacket) (*mcp.CallToolResult, ContextReceipt, error) {
		out, err := m.Publish("codex", in)
		return nil, out.Receipt(), err
	})
	mcp.AddTool(s, &mcp.Tool{Name: "gemini_read_context", Description: "Read a shared context packet. Claims and supplied artifact hashes must still be verified against current files."}, func(ctx context.Context, r *mcp.CallToolRequest, in IDInput) (*mcp.CallToolResult, SavedContext, error) {
		out, err := m.ReadContext(in.ID)
		return nil, out, err
	})
	mcp.AddTool(s, &mcp.Tool{Name: "gemini_send_message", Description: "Send a concise message and optional context IDs to a worker. Delivered between model turns; stopped workers are not restarted."}, func(ctx context.Context, r *mcp.CallToolRequest, in SendInput) (*mcp.CallToolResult, Message, error) {
		out, err := m.Send("codex", in)
		return nil, out, err
	})
	mcp.AddTool(s, &mcp.Tool{Name: "gemini_inbox", Description: "Read up to eight persisted messages after a cursor. This read does not consume or acknowledge messages."}, func(ctx context.Context, r *mcp.CallToolRequest, in InboxInput) (*mcp.CallToolResult, InboxOutput, error) {
		out, err := m.Inbox(in)
		return nil, out, err
	})
	mcp.AddTool(s, &mcp.Tool{Name: "gemini_handoff", Description: "Start a fresh Gemini conversation from 1–8 stopped jobs, carrying explicit reports and published context. Rejects oversized transfers rather than silently truncating them. Assign write_paths explicitly."}, func(ctx context.Context, r *mcp.CallToolRequest, in HandoffInput) (*mcp.CallToolResult, Job, error) {
		out, err := m.Handoff(in)
		return nil, out, err
	})
	mcp.AddTool(s, &mcp.Tool{Name: "gemini_spawn", Description: "Start one asynchronous Gemini agent with its own conversation and optional exclusive write paths."}, func(ctx context.Context, r *mcp.CallToolRequest, in Task) (*mcp.CallToolResult, Job, error) {
		jobs, err := m.Spawn([]Task{in})
		if err != nil {
			return nil, Job{}, err
		}
		return nil, jobs[0], nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "gemini_batch", Description: "Start 1–30 independent Gemini agents in one call. The whole batch is rejected on overlapping write paths."}, func(ctx context.Context, r *mcp.CallToolRequest, in BatchInput) (*mcp.CallToolResult, JobsOutput, error) {
		jobs, err := m.Spawn(in.Tasks)
		return nil, JobsOutput{Jobs: jobs}, err
	})
	mcp.AddTool(s, &mcp.Tool{Name: "gemini_status", Description: "Read result, status, checkpoint findings and coverage, recent tool activity, compactions, changed paths, and cumulative tokens without the full conversation."}, func(ctx context.Context, r *mcp.CallToolRequest, in IDInput) (*mcp.CallToolResult, Job, error) {
		j, err := m.Get(in.ID)
		return nil, j, err
	})
	mcp.AddTool(s, &mcp.Tool{Name: "gemini_list", Description: "List compact job statuses. Use gemini_status for final results."}, func(ctx context.Context, r *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, JobsOutput, error) {
		return nil, JobsOutput{Jobs: m.List()}, nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "gemini_wait", Description: "Wait briefly for one job to finish; returns current status on timeout."}, func(ctx context.Context, r *mcp.CallToolRequest, in WaitInput) (*mcp.CallToolResult, Job, error) {
		if in.Seconds <= 0 {
			in.Seconds = 30
		}
		if in.Seconds > 50 {
			in.Seconds = 50
		}
		j, err := m.Wait(ctx, in.ID, time.Duration(in.Seconds)*time.Second)
		return nil, j, err
	})
	mcp.AddTool(s, &mcp.Tool{Name: "gemini_continue", Description: "Send a follow-up to a completed worker, keeping its conversation and write paths. Run limits reset; token totals remain cumulative."}, func(ctx context.Context, r *mcp.CallToolRequest, in ContinueInput) (*mcp.CallToolResult, Job, error) {
		j, err := m.Continue(in.ID, in.Prompt)
		return nil, j, err
	})
	mcp.AddTool(s, &mcp.Tool{Name: "gemini_cancel", Description: "Request cancellation of a queued or running job. Poll status to confirm it stopped. Existing edits remain."}, func(ctx context.Context, r *mcp.CallToolRequest, in IDInput) (*mcp.CallToolResult, Job, error) {
		j, err := m.Cancel(in.ID)
		return nil, j, err
	})
	return s
}
