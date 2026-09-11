# Gemini

A Go CLI and stdio MCP server that lets Codex delegate tasks to **Gemini 3.8 Flash** on your Google AI Studio API account. Codex coordinates jobs, reviews diffs, and runs builds or test suites. Gemini workers handle bounded reasoning, codebase investigations, and file edits across up to 30 concurrent conversations.

For tool usage, configuration, and advanced topics, see the [reference](docs/reference.md).

## Quickstart

Requires Go 1.27 and macOS or Linux.

Clone, build, configure your API key, and check health:

```sh
gh repo clone impulse-ai/codex-gemini
cd codex-gemini
go build -o bin/codex-gemini ./cmd/codex-gemini
./bin/codex-gemini auth
./bin/codex-gemini doctor
```

Get an API key from [Google AI Studio](https://aistudio.google.com/api-keys). `auth` saves the key under your OS user configuration directory with mode `0600`. Environment variables `GEMINI_API_KEY` or `GOOGLE_API_KEY` take precedence if set. `doctor` checks authentication and model lookup; it does not test generation quota. Worker prompts and files read by workers are sent to Google.

## Connect to Codex

Register the MCP server from this repository directory:

```sh
codex mcp add gemini -- "$PWD/bin/codex-gemini" serve \
  -concurrency 30 -rpm 60
```

Install the optional [Codex skill](skills/gemini/SKILL.md) for delegation guidance you can invoke with `$gemini`:

```sh
mkdir -p "${CODEX_HOME:-$HOME/.codex}/skills"
ln -s "$PWD/skills/gemini" "${CODEX_HOME:-$HOME/.codex}/skills/gemini"
```

If the skill destination already exists, inspect it before replacing it. Start a new Codex session or reload MCP connections and skills. You register the server once: every task passes an absolute `workspace`, so working in different repositories needs no re-registration.

## How Delegation Works

- **Permissions & write ownership:** Workers are read-only by default. To allow modifications, specify explicit `write_paths`. Concurrent workers cannot hold overlapping write scopes.
- **No shell execution:** Workers can search, read, write, and edit files, but **cannot run shell commands**. Codex inspects the results, executes builds, and runs the test suite.
- **Autopilot & checkpoints:** Workers save intermediate findings with `report_checkpoint`. As limits approach, autopilot saves a summary and can compact context within the original run budget. Autopilot does not guarantee completion or validate code with executed tests.
- **Cost & Google API quotas:** Workers consume tokens on your Google AI Studio API quota. Worker concurrency and request rates (`-rpm`) are bounded locally to prevent bursts, but high concurrency does not increase your Google account quota.
- **Shared memory & context:** Workers and Codex share a local SQLite vector memory (`search_memory` / `gemini_search_memory`) and immutable context packets (`publish_context` / `read_context`) to hand off findings across phases or repositories.

## Everyday Delegation Examples

Delegate natural-language tasks to Gemini through Codex:

- **Investigation:**
  > "Use Gemini to investigate why user session refresh fails under concurrent load. Return file and line evidence, probable causes, and test cases, but do not edit any files."
- **Implementation:**
  > "Use Gemini to implement the token refresh retry logic in `internal/auth/refresh.go`. Scope writes exclusively to that file and document edge cases."
- **Independent Review:**
  > "Assign a read-only Gemini worker to independently review the diffs in `internal/auth` against our concurrency invariants before I run integration tests."

For structured multi-stage tasks, the `gemini_workflow` tool coordinates: **investigation (read-only) → implementation (scoped writes) → independent review (read-only)** under a single token and time budget. A `ready_for_validation` result still requires Codex to inspect the review and run tests. See [Bounded implementation workflows](docs/reference.md#bounded-implementation-workflows).

## CLI Example

You can also run workers directly from your terminal:

```sh
# Read-only code inspection
./bin/codex-gemini run -workspace /path/to/repo \
  -prompt "Trace how request timeouts are handled in the HTTP client."

# Single-file implementation
./bin/codex-gemini run -workspace /path/to/repo \
  -intent implementation -write-paths README.md \
  -prompt "Clarify the README setup instructions."
```

## More Details

Consult the [reference](docs/reference.md) for details:

- [MCP tools overview and usage](docs/reference.md#delegate-work)
- [Autopilot, context compaction, and partial results](docs/reference.md#autopilot-and-useful-partial-results)
- [Multi-phase workflows](docs/reference.md#bounded-implementation-workflows)
- [Implementation loop detection and metrics](docs/reference.md#implementation-loop-detection-and-outcome-metrics)
- [Shared SQLite vector memory](docs/reference.md#shared-memory-for-codex-and-gemini)
- [Context transfer, peer messaging, and handoff](docs/reference.md#context-transfer-and-local-peer-communication)
- [Service limits, persistence, and costs](docs/reference.md#limits-and-costs)
