# impulseai/codex-gemini

Go module: `github.com/impulseai/codex-gemini`. Binary: `codex-gemini`. MCP server identity: `impulseai/codex-gemini`.

A small Go CLI and stdio MCP server that lets Codex delegate work to **Gemini 3.8 Flash** on your Google AI Studio API account. Multiple Codex sessions and repositories share up to **30 concurrent Gemini conversations**, each with its own workspace, tool loop, and optional file-editing scope.

Gemini workers do the delegated reasoning and editing. Codex supplies assignments, reviews results, and runs tests. These are local worker jobs, not additional Codex sidebar tasks.

## Setup

Requires Go 1.27 and macOS or Linux.

```sh
gh repo clone impulseai/codex-gemini
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
codex mcp add impulseai-codex-gemini -- "$PWD/bin/codex-gemini" serve \
  -concurrency 30 -rpm 60
```

Start a new Codex session or reload MCP connections after setup. Register once: every task supplies an absolute `workspace`, so changing repositories needs no registration changes. The `serve` command connects stdio to a shared background service over a private Unix socket, starting it automatically when needed. All Codex sessions and CLI clients share file reservations, job history, and the same concurrency/rate limits. The service creates no state directories inside your repositories.

Equivalent configuration (replace the binary path):

```toml
[mcp_servers.impulseai-codex-gemini]
command = "/absolute/path/codex-gemini/bin/codex-gemini"
args = ["serve", "-concurrency", "30", "-rpm", "60"]
env_vars = ["GEMINI_API_KEY", "GOOGLE_API_KEY"]
```

The saved key works even when the desktop app does not inherit your shell environment.

## Codex skill

The repository includes [skill guidance](skills/impulseai-codex-gemini/SKILL.md) for economical delegation, advanced technical context transfer, peer coordination, and validation. Install it for your local Codex account from this repository:

```sh
mkdir -p "${CODEX_HOME:-$HOME/.codex}/skills"
ln -s "$PWD/skills/impulseai-codex-gemini" "${CODEX_HOME:-$HOME/.codex}/skills/impulseai-codex-gemini"
```

If the destination already exists, inspect it before replacing it. Reload skills or start a new session, then invoke `$impulseai-codex-gemini`. The skill supports automatic discovery for relevant Gemini delegation requests. Its guidance complements the MCP server's own tool instructions.

## Delegate work

Ask Codex: “Use Gemini workers to handle these independent changes. Assign disjoint files, inspect their results, then run the tests.”

MCP tools:

| Tool | Purpose |
| --- | --- |
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

Workers have `list_files`, `read_file`, `write_file`, `list_peers`, `send_message`, `publish_context`, and `read_context`. There is no shell execution tool: Codex performs builds/tests and any operations beyond file creation/replacement. Existing files must be read first; writes require the returned SHA-256. A stale hash is rejected. New files require `expected_sha256: "new"`. File replacement uses an atomic rename. Symlinks, paths outside the workspace, `.git`, `.codex`, `.gemini-workers`, and `.env`/`.env.*` are blocked. This is not a general secret detector; choose a workspace without unrelated credentials. Reads/writes are limited to 256 KiB per file, and listings to 2,000 entries; workers can narrow directory listings.

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

`run` also accepts `-label` and comma-separated `-context-ids`. Batch tasks accept `workspace`, `label`, `context_ids`, and per-task `thinking`. CLI tasks without an explicit workspace use `-workspace` (default: current directory). CLI clients connect to the same service as MCP. `gemini_usage` reports provider token counts across saved jobs, grouped by model; it does not read Google billing credits or redeem Codex reset credits.

## Limits and costs

Defaults: 30 active workers, 300 queued/running jobs total, 60 API requests/minute shared across workers, 25 model turns per run, 8,192 maximum output tokens per request, 200,000 total tokens per run, 15-minute run timeout, and `low` thinking. All are configurable with flags; use `serve -h`. The token budget is **soft**, checked between requests, so the final request can exceed it. Each continuation resets its run limits while retaining cumulative token counts.

Service-wide flags apply when the service starts; attaching another client does not change the running configuration. To change these limits or reload a rebuilt binary, finish/cancel work, run `codex-gemini stop`, and reconnect with the desired flags. `stop` cancels jobs across all connected sessions. Task `thinking` overrides the service default. Use one shared state directory for all clients so reservations and limits remain coordinated.

Concurrency does not increase your Google account quota. Tune `-rpm` to your project; requests are paced and HTTP 429/5xx responses retry with bounded exponential backoff and jitter. Google token-per-minute limits can still throttle workers. Token usage includes repeated conversation input, output, thinking, and cached input where reported. No dollar estimate is shown because prices and account terms can change. More workers do not automatically reduce total cost; delegate bounded jobs and ask for concise summaries.

Jobs and conversation history are saved with owner-only permissions under your user configuration directory: `~/Library/Application Support/codex-gemini/state` on macOS, or the OS config directory equivalent on Linux. `-state-dir` overrides this location. The local socket is in a user-owned mode-0700 directory under `/tmp`; it exposes no network port. Startup errors are logged to `service.log` inside the state directory. The service state is protected from worker file tools.

MCP responses omit full conversation history. Closing a Codex session leaves jobs running in the shared service; explicit CLI interruption cancels that CLI's submitted jobs. Completed jobs can continue after service restart with the same model. Failed, interrupted, or limited jobs require a new assignment. Review partial edits first; rollback and automatic crash recovery are not provided. The archive is not automatically pruned. Existing version-0.2 `.gemini-workers/` archives remain untouched and are not automatically imported.

## Development

```sh
go test -race ./...
go vet ./...
```

The test suite exercises 30 simultaneous workers, file ownership and path boundaries, cancellation, conversation/call-ID preservation, continuation, persistence, the state process lock, HTTP retries, and MCP client/server round trips. Coordination tests verify technical context transfer, fresh-conversation handoff, bounded message delivery, sender identity, mailbox persistence, and size limits. Workspace tests cover independent edits, nested-root and symlink-alias conflicts, cross-repository context provenance, and multiple clients sharing jobs without cancellation on disconnect. Tests use a local fake provider and do not spend API credits.

Protocol references: [Codex MCP setup](https://developers.openai.com/codex/mcp), [Gemini 3.8 Flash](https://ai.google.dev/gemini-api/docs/models/gemini-3.8-flash), [generateContent API](https://ai.google.dev/api/generate-content), and [Google API keys](https://ai.google.dev/gemini-api/docs/api-key). The implementation uses Google's official Go SDK and its generateContent `FunctionResponse.ID` field, matching the REST schema, and retains thought signatures unchanged.
