# Gemini

Go module: `github.com/impulse-ai/codex-gemini`. Binary: `codex-gemini`. MCP server identity: `Gemini`.

A small Go CLI and stdio MCP server that lets Codex delegate work to **Gemini 3.8 Flash** on your Google AI Studio API account. Multiple Codex sessions and repositories share up to **30 concurrent Gemini conversations**, each with its own workspace, tool loop, and optional file-editing scope.

Gemini workers do the delegated reasoning and editing. Codex supplies assignments, reviews results, and runs tests. These are local worker jobs, not additional Codex sidebar tasks.

## Setup

Requires Go 1.27 and macOS or Linux.

```sh
gh repo clone impulse-ai/codex-gemini
cd codex-gemini
go build -o bin/codex-gemini ./cmd/codex-gemini
./bin/codex-gemini auth
./bin/codex-gemini doctor
```

Get your API key from [Google AI Studio](https://aistudio.google.com/api-keys). `auth` prompts in your terminal with input hidden and saves the key with mode `0600` under your OS user configuration directory (`~/Library/Application Support/codex-gemini/api-key` on macOS). It never writes a key into this project or Codex's configuration. `GEMINI_API_KEY`, then `GOOGLE_API_KEY`, take precedence over the saved key. `doctor` verifies authentication and the exact model through Google's model lookup; it does not test generation quota.

This integration uses the Gemini Developer API and its project quotas/billing; it does not sign into the consumer Gemini website or consume a browser session. Your prompts and files read by workers are sent to Google. No automatic model fallback occurs.

## Connect to Codex

From this project directory:

```sh
codex mcp add gemini -- "$PWD/bin/codex-gemini" serve \
  -concurrency 30 -rpm 60
```

Start a new Codex session or reload MCP connections after setup. If migrating from the old `impulseai-codex-gemini` registration, remove that entry with `codex mcp remove impulseai-codex-gemini` after active work finishes, then register `gemini` using the command above. Register once: every task supplies an absolute `workspace`, so changing repositories needs no registration changes. The `serve` command connects stdio to a shared background service over a private Unix socket, starting it automatically when needed. All Codex sessions and CLI clients share file reservations, job history, and the same concurrency/rate limits. The service creates no state directories inside your repositories.

Equivalent configuration (replace the binary path):

```toml
[mcp_servers.gemini]
command = "/absolute/path/codex-gemini/bin/codex-gemini"
args = ["serve", "-concurrency", "30", "-rpm", "60"]
env_vars = ["GEMINI_API_KEY", "GOOGLE_API_KEY"]
```

The saved key works even when the desktop app does not inherit your shell environment. The MCP registration and skill name is `gemini`. Source repository: `impulse-ai/codex-gemini`.

## Codex skill

The repository includes [skill guidance](skills/gemini/SKILL.md) for economical delegation, advanced technical context transfer, peer coordination, and validation. Install it for your local Codex account from this repository:

```sh
mkdir -p "${CODEX_HOME:-$HOME/.codex}/skills"
ln -s "$PWD/skills/gemini" "${CODEX_HOME:-$HOME/.codex}/skills/gemini"
```

If the destination already exists, inspect it before replacing it. Reload skills or start a new session, then invoke `$gemini`. The skill supports automatic discovery for relevant Gemini delegation requests. Its guidance complements the MCP server's own tool instructions.

## Implementation, investigation, and independent review

Use Gemini workers for **implementation, investigation, and independent review**, including workers that edit files. Codex keeps responsibility for integration, running tests, and final review.

Useful assignments for Ordinant include:

- **Trace bugs:** follow a web action through the API, service, store, and runtime; return evidence and likely causes.
- **Implement separate pieces concurrently:** assign backend, frontend, and documentation changes with explicit interfaces and separate file ownership.
- **Audit boundaries:** inspect tenant isolation, authorization, task leases, idempotency, or bounded retries.
- **Write regression tests:** construct cases around a confirmed failure, then have Codex run and validate them.
- **Check deployment compatibility:** review local, BYOC, and hosted paths for assumptions that break another environment.
- **Compare architecture options:** evaluate competing designs against the same constraints.
- **Reconcile docs and code:** identify contract drift and update canonical handbook pages.
- **Coordinate across repositories:** investigate a producer and consumer separately, sharing concise technical findings through context packets.
- **Carry investigations between phases:** preserve findings in context packets, then hand off from investigation to implementation to review.

For example, ask Codex: “Have Gemini inspect completion certification and retry handling independently, then implement fixes for confirmed issues.”

Workers can read, search, and edit files, but **cannot run shell commands**. Editing requires explicit `write_paths`; concurrent editors need separate ownership. Run independent reviews against the resulting changes after editors finish. Each cross-repository task supplies its own absolute `workspace`.

Work uses Gemini API quota. Worker concurrency and reasoning/token budgets are bounded; use only as many workers as independent assignments warrant. Autopilot preserves progress and recovers within the original run limits, but does not guarantee completion or validate its own claims with executed tests.

## Delegate work

Ask Codex: “Use Gemini workers to handle these independent changes. Assign disjoint files, inspect their results, then run the tests.”

MCP tools:

| Tool | Purpose |
| --- | --- |
| `gemini_workflow` | Run investigation → implementation → independent review within one budget. |
| `gemini_workflow_status` | Inspect phase job IDs, accounted tokens, and terminal status. |
| `gemini_workflow_cancel` | Cancel the workflow and wait for its active child to stop. |
| `gemini_metrics` | Inspect measured edits, completion outcomes, repeated reads, and tokens to first edit. |
| `gemini_spawn` | Start one worker and immediately return its job ID. |
| `gemini_batch` | Start 1–30 workers in one call. |
| `gemini_status` | Fetch result, changed paths, and token counts. |
| `gemini_list` | List compact job statuses. |
| `gemini_wait` | Wait up to 50 seconds for a job. |
| `gemini_continue` | Give a completed worker another assignment with its existing conversation and write scope. |
| `gemini_cancel` | Cancel a running or queued worker. |
| `gemini_publish_context` | Save an immutable technical brief and return a reusable context ID. |
| `gemini_read_context` | Retrieve a brief by ID. |
| `gemini_send_message` | Send a short message and context references to a worker. |
| `gemini_inbox` | Read a worker's persisted mailbox with a cursor. |
| `gemini_handoff` | Start a fresh conversation carrying stopped jobs' reports and context references. |
| `gemini_search_memory` | Retrieve bounded excerpts from shared SQLite memory; optional cached semantic search. |
| `gemini_memory_stats` | Inspect memory size and embedding request/input-byte counters. |
| `gemini_usage` | Read cumulative token usage by model and counts by job status. |

Example `gemini_batch` arguments:

```json
{
  "tasks": [
    {"workspace": "/absolute/path/repo-a", "prompt": "Review the authentication code for bugs. Report concrete findings."},
    {"workspace": "/absolute/path/repo-a", "prompt": "Improve the README setup instructions after inspecting the code.", "write_paths": ["README.md"]},
    {"workspace": "/absolute/path/repo-b", "prompt": "Add focused tests for the parser's edge cases.", "write_paths": ["internal/parser/parser_test.go"]}
  ]
}
```

Workers are read-only unless assigned `write_paths`. A scope is an exact file or an entire directory subtree; `.` grants the workspace except protected paths. Overlapping active scopes or overlaps within a batch are rejected before that batch starts. A running editor retains its reservation until it stops. Completed jobs release reservations; continuation must acquire them again. Read-only workers can read files being edited, so run final reviews after editors finish.

`workspace` is required for MCP task creation and handoff. Relative workspaces are rejected. The service canonicalizes roots and compares absolute write scopes, including nested repositories and symlink aliases. Two repositories may each edit `README.md`; two sessions cannot reserve the same actual file simultaneously. Status returns each job's workspace, and `gemini_usage` groups job counts by workspace. Job-ID operations automatically route to the original workspace.

Workers have `list_files`, `search_files`, `read_file`, `write_file`, `edit_file`, `report_checkpoint`, `list_peers`, `send_message`, `publish_context`, and `read_context`. There is no shell execution tool: Codex performs builds/tests and any operations beyond file creation/replacement. Existing files must be read first; writes require the returned SHA-256. A stale hash is rejected. New files require `expected_sha256: "new"`. File replacement uses an atomic rename. Symlinks, paths outside the workspace, `.git`, `.codex`, `.gemini-workers`, and `.env`/`.env.*` are blocked. This is not a general secret detector; choose a workspace without unrelated credentials. Files are limited to 256 KiB, and listings to 2,000 entries. Reads return pages of 200 lines by default, at most 400 lines and 8 KiB, plus `next_line` and a hash of the whole file. Literal `search_files` returns at most 20 matching snippets with line numbers. Use `edit_file` to replace one exact text occurrence with a current hash while preserving unread content.

## Autopilot and useful partial results

Autopilot is enabled by default. Give each review a concrete code path and desired evidence; use `focus_paths` to identify relevant files. Prefer `thinking: "low"` for focused reviews. High thinking and repeated large reads can exhaust a run before it produces findings.

Workers save incremental findings, reviewed coverage, and remaining work with `report_checkpoint`. The host reserves an estimated token allowance for a tool-free, low-thinking checkpoint before limits are reached. It requests that checkpoint when context grows large, a `MAX_TOKENS` response is truncated, or the remaining budget requires a final report. Healthy work keeps its conversation and can use the full run allowance; there is no four-call compaction schedule. Truncated function calls are never executed. Known incomplete checkpoints keep the tool loop active; a progress message alone cannot mark that job complete. Incomplete work can resume automatically from the original assignment, explicit follow-ups, and checkpoint in a fresh conversation. Old tool transcripts are archived locally. Up to two recent file exchanges (12 KiB total), with their read hashes and call IDs, stay available after compaction so an editor can use recently inspected content. Hash checks still reject stale writes.

Recovery is limited to two context compactions per run and shares the original token, model-call, and timeout budgets. It stops when both the checkpoint and successful file observations show no progress, when no actionable work remains, or when recovery cannot fit within the budget. Exhausting the two optional context compactions does not by itself stop healthy working calls. It does not increase budgets, change models, or restart indefinitely. Auth failures, exhausted quota retries, and service crashes still need attention.

```json
{
  "workspace": "/absolute/path/repository",
  "label": "completion-recovery-review",
  "prompt": "Review the completion recovery path for lost or duplicate results. Return concrete file/line findings, coverage, and unresolved cases. Save findings as you go.",
  "focus_paths": ["internal/completion/recovery.go"],
  "thinking": "low",
  "max_tokens": 50000
}
```

`gemini_status` and `gemini_wait` include `checkpoint`, recent `activity`, and `compactions`. A stopped incomplete job returns saved findings and an explicit coverage gap instead of an empty result. Contradictory synthesized reports with both `complete: true` and remaining work are retained as incomplete. An empty or interrupted review is never evidence that the code is clean. A checkpoint is a saved intermediate report; consult the final status and result for completion. Codex validates findings and runs tests.

Set task `autopilot: false` or CLI `-autopilot=false` to disable automatic synthesis and compaction. Bounded file tools and incremental checkpoint reporting remain available. `max_tokens` optionally lowers a task's run budget; it cannot exceed the service maximum. Explicit handoff or continuation starts a new paid run, so do not blindly relaunch an exhausted review. Stopped jobs identify the limiting reservation, model-call count, time, or compaction cap. A token-reservation stop is not a Google quota error. Small budgets such as 14,000 total tokens may cover only a few calls once instructions and file reads are replayed; multi-file implementation, tests, and documentation need a scope and budget that fit together.

## Bounded implementation workflows

Use `gemini_workflow` for a confirmed change that benefits from investigation and independent review:

```json
{
  "task": {
    "workspace": "/absolute/path/repository",
    "prompt": "Fix duplicate completion delivery. Preserve authorization and idempotency constraints; add regression coverage.",
    "focus_paths": ["internal/completion"],
    "write_paths": ["internal/completion"],
    "thinking": "low"
  },
  "max_tokens": 160000,
  "timeout_seconds": 600
}
```

Three fresh jobs run sequentially: investigation is read-only, implementation gets the explicit write scope, and independent review is read-only. Every phase receives the original assignment, explicit earlier reports, and context packet references. Oversized transfers stop for consolidation rather than silently dropping constraints. The workflow has one soft generation-token budget and one wall-clock timeout; embedding costs remain separate. Investigation gets up to one quarter of the budget; implementation preserves one quarter for review; review can use the remaining allowance. An optional nested `task.max_tokens` further caps each phase. Each phase retains the service's model-call limit.

Inspect `gemini_workflow_status` and the individual phase jobs via `gemini_status`. Accounted workflow usage updates when each phase stops; the running phase's usage is visible on its job. An incomplete phase or implementation with no recorded writes stops the workflow with `needs_attention`. There are no automatic retries or budget increases. `ready_for_validation` means all three model phases finished, **not** that the review found no issues or tests passed. Codex must read the review, inspect actual changes, and run tests.

Workflow state persists under `state/workflows/`. Cancel requests wait for the current child to actually stop before final accounting. Restarted workflows become `interrupted`; they never silently resume edits. Phase jobs cannot be continued separately to bypass workflow accounting. Source files are not isolated snapshots; recheck current state between phases if other work is active.

## Implementation loop detection and outcome metrics

Set task `intent: "implementation"` (CLI `-intent implementation`) for assigned edits; it requires `write_paths`. Workflows set intent automatically. `investigation` and `review` intents require read-only scopes. Existing tasks without intent retain their behavior.

With autopilot enabled, implementation jobs receive a targeted progress nudge after eight model calls without a successful write. After sixteen calls without edits—or twelve calls with at least three repeated reads—the server requests a final checkpoint and stops with `needs_attention`. A model's final completion claim without edits also requires a checkpoint and attention. No uninformed write is forced; a worker can identify missing information or explain why no change is needed. Normal token/time limits may stop work earlier.

Job `metrics` records inspections, repeated identical reads, successful write operations, nudges, and provider-reported tokens/model calls before the first edit. These measure tool activity, not code correctness: rewriting identical content is still a write operation. `gemini_metrics` aggregates measured jobs, completed jobs, jobs with edits, and stopped editing jobs without writes. Legacy jobs without instrumentation are counted separately; continued legacy jobs record a baseline rather than attributing earlier costs to new instrumentation. Metrics do not infer test success, human acceptance, currency savings, or how much work Codex later completed.

## Shared memory for Codex and Gemini

Both platforms use **one local SQLite memory and one embedding space** through MCP. Codex calls `gemini_search_memory`; Gemini workers call `search_memory`. No OpenAI embedding account or second vector index is needed. This expands retrievable knowledge, not either model's native context window.

Published context packets and explicit worker reports/checkpoints are indexed locally, including existing saved archives on startup. Full tool transcripts and model thought parts are not embedded. Indexing itself makes no API calls. Memory lives in `memory.sqlite` beside the existing shared state, with owner-only file permissions and SQLite WAL persistence. It stores vectors as blobs and computes bounded cosine ranking in Go using a pure-Go SQLite driver; it does not require a native vector extension or a separate database server.

Start with a **free local keyword search**:

```json
{
  "workspace": "/absolute/path/repository",
  "query": "browser handback admission barrier",
  "limit": 3,
  "max_bytes": 4000
}
```

Pass these arguments to `gemini_search_memory`. Set `semantic: true` for hybrid keyword/vector retrieval when wording differs. Source references are `context:ID` or `job:ID`: use the ID with `gemini_read_context` or `gemini_status` for the full record. Each hit includes its source workspace and chunk number. Codex can explicitly search another repository; a worker's search is fixed to its own workspace. Carry selected findings across repositories through an explicit context packet.

For automatic retrieval at the start of a worker run, set task `memory_query` to a short, targeted question (or CLI `-memory-query`). The worker gets up to four excerpts totaling 6,000 text bytes. Retrieval runs once per run and participates in normal generation-input budgeting. Without `memory_query`, no startup semantic search occurs; workers can search as needed. Explicit `context_ids` still load complete packets, preserving their full constraints.

Cost controls:

- Default semantic model: `gemini-embedding-001`, 768 dimensions, shared by both platforms. `-embedding-model off` disables remote embedding calls; keyword retrieval remains available. This is a service-startup setting.
- Document chunks and queries are cached by model, task type, and content hash. Unchanged text is embedded once, including across restarts and concurrent clients. Repeated queries reuse their vectors; no LLM reranking or summarization is used for search.
- Each semantic search embeds at most 16 missing document chunks of up to 1,800 UTF-8 bytes, plus one query of up to 2,000 bytes. `pending_embeddings` exposes any remaining backlog. Later searches index additional chunks within the same bound.
- Default results: four excerpts / 6,000 text bytes; maximum eight / 12,000. Provenance metadata is additional. Exact duplicate excerpts are suppressed and weak semantic matches are filtered relative to the best result.
- Searches inspect at most the newest 5,000 chunks in the selected workspace. `truncated` reports scan/output limits; results are not an exhaustive nearest-neighbor index at larger scales.
- If embeddings fail, retrieval returns available keyword matches with a warning. Embedding requests share the Gemini request-rate limiter, have a 30-second retrieval timeout, and do not automatically retry. Their costs are separate from generation token budgets.

`gemini_memory_stats` reports indexed chunks, cached vectors, attempted embedding requests, and input bytes. The Developer API does not provide embedding billing-token counts through this response, so the service does not invent them or claim a measured net saving. Embedding calls use Google API quota; retrieved excerpts still consume tokens when supplied to Codex or Gemini. Savings depend on replacing larger replayed context with relevant excerpts. See [Google's current embedding pricing](https://ai.google.dev/gemini-api/docs/pricing#gemini-embedding).

Memory contains historical model claims, not automatically verified facts. Reports retain their completion status; incomplete findings remain incomplete. Fetch complete constraints and check current files/hashes before editing. Context packets and full reports remain the source records. The index/cache is not automatically pruned; job status exposes `memory_error` for retrieval warnings or if a report could not be indexed.

## Context transfer and local peer communication

Publish explicit working knowledge once, then reference its ID in multiple tasks. Packets support an objective, summary, invariants/constraints, decisions with short reasons, evidence with sources and verification flags, terminology, open questions, next steps, and file references with optional SHA-256 hashes. This supports advanced technical topics without relying on a worker's implicit knowledge of earlier conversations. Packet contents are author assertions; verification flags and hashes are not independently checked by the server. Workers must inspect current files before acting on a stale reference.

Example `gemini_publish_context` arguments:

```json
{
  "workspace": "/absolute/path/codex-gemini",
  "title": "Concurrent scheduler design",
  "objective": "Keep ownership and cancellation correct with 30 workers",
  "summary": "Reserve write paths before starting a job. Preserve independent conversation state.",
  "constraints": ["Never grant overlapping write scopes", "Cancellation must release worker slots"],
  "decisions": [{"choice": "Use a shared semaphore", "reason": "Bound concurrent API work"}],
  "evidence": [{"claim": "Thirty workers can run concurrently", "source": "internal/worker/worker_test.go:TestThirtyConcurrentWorkers", "verified": true}],
  "artifacts": [{"path": "internal/worker/manager.go", "note": "Lifecycle and queue implementation"}],
  "open_questions": ["What request rate does the Google project permit?"],
  "next_steps": ["Review cancellation under load"],
  "glossary": {"write scope": "A worker's exclusive file or directory reservation"}
}
```

Take the returned `id`, then launch a worker:

```json
{
  "workspace": "/absolute/path/codex-gemini",
  "label": "scheduler-reviewer",
  "prompt": "Review the scheduler against the supplied invariants. Publish a concise context packet with your findings.",
  "context_ids": ["CONTEXT_ID"],
  "thinking": "high"
}
```

Each worker can discover active peers by workspace, label, and write ownership, send them a short message, and attach context IDs. Sender identity comes from the executing worker, so a worker cannot identify itself as Codex. The message bus is shared across the local user's connected repositories and sessions. It is A2A-style coordination, not an implementation of the network A2A protocol or remote Agent Cards. Packets record their source workspace for artifact references; carrying a packet into another repository never grants file access to the source repository.

Messages are capped at 2,048 bytes, with up to eight context references. Each mailbox stores at most 128 messages. Up to eight new messages enter a worker's context between model turns. Messages arriving during its final request remain available in the inbox and on explicit continuation; they do not trigger extra paid inference. `gemini_inbox` uses `after`/`next` cursors and never consumes messages. Messages cannot expand a recipient's file permissions or assignment authority. Workers should continue useful independent work rather than poll for replies.

Packets are immutable JSON documents under the shared state directory's `contexts/`, capped at 32 KiB each. A worker can publish up to eight packets; a task can load up to eight. Packets load into the initial context once, while messages carry references for selective retrieval. They still consume Gemini input tokens when sent to the model; this is not provider-side context caching. When publishing artifact references from Codex, include their source `workspace`; workers set it automatically.

Publication returns a small receipt instead of echoing the whole packet. Job status responses omit the original assignment text and full conversation; these remain in the local archive. Use `gemini_read_context` only when the full brief is needed. [examples/context.json](examples/context.json) is a starter technical brief.

For a fresh conversation after a phase finishes, call `gemini_handoff`:

```json
{
  "sources": ["STOPPED_JOB_ID"],
  "task": {
    "workspace": "/absolute/path/destination-repository",
    "label": "implementation",
    "prompt": "Implement the reviewed plan. Recheck artifact freshness and preserve the stated invariants.",
    "thinking": "medium",
    "write_paths": ["internal/scheduler"]
  }
}
```

Handoff copies source reports and references to their initial and published packets into a fresh conversation. It requires stopped source jobs and explicit new write scopes. It does not transfer inboxes, implicit model state, or source transcripts. Publish unresolved findings and decisions before handing off. Oversized transfers are rejected rather than silently truncated; consolidate packets first. `gemini_continue` retains full conversation history when that detail is necessary. Both operations incur new API usage; handoffs reduce replay only when the selected brief is smaller than the old conversation.

## CLI usage

```sh
# Read-only analysis
./bin/codex-gemini run -workspace /path/to/repo \
  -prompt 'Inspect the code and identify the highest-value small fix.'

# Allow one worker to edit one file
./bin/codex-gemini run -workspace /path/to/repo \
  -write-paths README.md -prompt 'Improve the README quickstart.'

# Run a JSON array of up to 30 assignments concurrently
./bin/codex-gemini batch -workspace /path/to/repo -file examples/tasks.json
```

`run` reads its prompt from stdin when `-prompt` is omitted. `batch` reads its array from stdin when `-file` is omitted. Both wait for all jobs and print one JSON result per job in submission order. Any incomplete/failed job produces a nonzero exit code. Ctrl-C cancels workers; edits already made remain.

`run` also accepts `-label`, comma-separated `-context-ids` and `-focus-paths`, `-max-tokens`, and `-autopilot`. Batch tasks accept the same task fields as MCP; explicitly supplied CLI `-max-tokens` and `-autopilot` fill omitted task values. CLI tasks without an explicit workspace use `-workspace` (default: current directory). CLI clients connect to the same service as MCP. `gemini_usage` reports provider token counts across saved jobs, grouped by model; it does not read Google billing credits or redeem Codex reset credits.

## Limits and costs

Defaults: 30 active workers, 300 queued/running jobs total, 60 API requests/minute shared across workers, 25 model turns per run, 8,192 maximum output tokens per request, 200,000 total tokens per run, 15-minute run timeout, and `low` thinking. All are configurable with flags; use `serve -h`. The token budget is **soft**: requests begin with a serialized-size estimate, then calibrate upward or downward from observed provider input counts with headroom; output reservations shrink as the budget runs down, but provider tokenization and thinking can differ, so a request can exceed it. Automatic checkpoint calls count toward both model turns and token usage. Each continuation resets its run limits while retaining cumulative token counts.

Service-wide flags apply when the service starts; attaching another client does not change the running configuration. To change these limits or reload a rebuilt binary, finish/cancel work, run `codex-gemini stop`, and reconnect with the desired flags. `stop` cancels jobs across all connected sessions. Task `thinking`, `focus_paths`, `autopilot`, and a lower `max_tokens` budget apply without restarting. Use one shared state directory for all clients so reservations and limits remain coordinated.

Concurrency does not increase your Google account quota. Tune `-rpm` to your project; requests are paced and HTTP 429/5xx responses retry with bounded exponential backoff and jitter. Google token-per-minute limits can still throttle workers. Token usage includes repeated conversation input, output, thinking, and cached input where reported. No dollar estimate is shown because prices and account terms can change. More workers do not automatically reduce total cost; delegate bounded jobs and ask for concise summaries.

Jobs, checkpoints, and conversation history (including compacted `segments/`) are saved with owner-only permissions under your user configuration directory: `~/Library/Application Support/codex-gemini/state` on macOS, or the OS config directory equivalent on Linux. `-state-dir` overrides this location. The local socket is in a user-owned mode-0700 directory under `/tmp`; it exposes no network port. Startup errors are logged to `service.log` inside the state directory. The service state is protected from worker file tools.

MCP responses omit full conversation history. Closing a Codex session leaves jobs running in the shared service; explicit CLI interruption cancels that CLI's submitted jobs. Completed jobs can continue after service restart with the same model. Failed, interrupted, or limited jobs require a new assignment. Review partial edits first; rollback and automatic crash recovery are not provided. The archive is not automatically pruned. Existing version-0.2 `.gemini-workers/` archives remain untouched and are not automatically imported.

## Development

```sh
go test -race ./...
go vet ./...
```

The test suite exercises 30 simultaneous workers, file ownership and path boundaries, cancellation, conversation/call-ID preservation, continuation, persistence, the state process lock, HTTP retries, and MCP client/server round trips. Coordination tests verify technical context transfer, fresh-conversation handoff, bounded message delivery, sender identity, mailbox persistence, and size limits. Workspace tests cover independent edits, nested-root and symlink-alias conflicts, cross-repository context provenance, and multiple clients sharing jobs without cancellation on disconnect. Autopilot tests cover bounded recovery, truncated output, retained findings, stalled checkpoints, token reservations, paginated reads, and exact edits that preserve unread content. Tests use a local fake provider and do not spend API credits.

Protocol references: [Codex MCP setup](https://developers.openai.com/codex/mcp), [Gemini 3.8 Flash](https://ai.google.dev/gemini-api/docs/models/gemini-3.8-flash), [generateContent API](https://ai.google.dev/api/generate-content), and [Google API keys](https://ai.google.dev/gemini-api/docs/api-key). The implementation uses Google's official Go SDK and its generateContent `FunctionResponse.ID` field, matching the REST schema, and retains thought signatures unchanged.
