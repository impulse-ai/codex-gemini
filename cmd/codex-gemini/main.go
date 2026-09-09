package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/impulseai/codex-gemini/internal/service"
	"github.com/impulseai/codex-gemini/internal/worker"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/term"
	"golang.org/x/time/rate"
	"google.golang.org/genai"
)

func keyPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "codex-gemini", "api-key"), nil
}
func loadKey() (string, error) {
	for _, name := range []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"} {
		if k := strings.TrimSpace(os.Getenv(name)); k != "" {
			return k, nil
		}
	}
	p, err := keyPath()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("no API key; run codex-gemini auth or set GEMINI_API_KEY")
	}
	k := strings.TrimSpace(string(b))
	if k == "" {
		return "", fmt.Errorf("stored API key is empty")
	}
	return k, nil
}
func auth() error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("run auth in an interactive terminal (or use GEMINI_API_KEY)")
	}
	fmt.Fprint(os.Stderr, "Google AI Studio API key (hidden): ")
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return fmt.Errorf("empty key")
	}
	p, err := keyPath()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".key-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.WriteString(key); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), p); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "API key saved with owner-only permissions. Run codex-gemini doctor to verify access.")
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "codex-gemini:", err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: codex-gemini <serve|run|batch|doctor|auth|daemon|stop> [flags]; use <command> -h")
	}
	command := os.Args[1]
	if command == "auth" {
		return auth()
	}
	if command != "serve" && command != "run" && command != "batch" && command != "doctor" && command != "daemon" && command != "stop" {
		return fmt.Errorf("unknown command %q", command)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	workspace := flags.String("workspace", ".", "Workspace root")
	defaultState, err := defaultStateDir()
	if err != nil {
		return err
	}
	stateDir := flags.String("state-dir", defaultState, "Shared service state directory (all clients must use the same value)")
	model := flags.String("model", "gemini-3.8-flash", "Exact Google model ID (no fallback)")
	parallel := flags.Int("concurrency", 30, "Maximum simultaneous agents, 1–30")
	rpm := flags.Int("rpm", 60, "Global requests per minute; tune to your Google project quota")
	steps := flags.Int("max-steps", 25, "Maximum model turns per run")
	tokens := flags.Int64("max-tokens", 200000, "Soft total token limit per run, checked between requests")
	output := flags.Int("max-output", 8192, "Maximum output tokens per request")
	timeout := flags.Duration("timeout", 15*time.Minute, "Maximum run duration after leaving queue")
	thinking := flags.String("thinking", "low", "Gemini thinking level: low, medium, high")
	prompt := flags.String("prompt", "", "Assignment for run; if omitted read stdin")
	contextIDs := flags.String("context-ids", "", "Comma-separated context packet IDs for run")
	label := flags.String("label", "", "Short peer-discovery role label for run")
	write := flags.String("write-paths", "", "Comma-separated exclusive writable files/directories; empty means read-only")
	input := flags.String("file", "", "JSON task array for batch; if omitted read stdin")
	if err := flags.Parse(os.Args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments; put task text after -prompt")
	}
	if *rpm < 1 || *output < 1 || *output > 65536 {
		return fmt.Errorf("rpm must be positive; max-output must be 1–65536")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	state, err := filepath.Abs(*stateDir)
	if err != nil {
		return err
	}
	if command == "stop" {
		return service.Stop(ctx, state)
	}
	if command == "daemon" || command == "doctor" {
		key, err := loadKey()
		if err != nil {
			return err
		}
		client, err := genai.NewClient(ctx, &genai.ClientConfig{APIKey: key, Backend: genai.BackendGeminiAPI})
		if err != nil {
			return err
		}
		if command == "daemon" {
			cfg := worker.Config{StateDir: state, Model: *model, Concurrency: *parallel, MaxSteps: *steps, MaxTokens: *tokens, MaxOutput: int32(*output), Timeout: *timeout, Thinking: *thinking}
			generator := &worker.Gemini{Client: client, Model: *model, Limiter: rate.NewLimiter(rate.Every(time.Minute/time.Duration(*rpm)), 1)}
			manager, err := worker.New(ctx, cfg, generator)
			if err != nil {
				return err
			}
			defer manager.Close()
			return service.Serve(ctx, state, manager)
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		info, err := client.Models.Get(ctx, *model, nil)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": true, "model": info.Name, "message": "API authentication and model lookup succeeded; generation quota still applies."})
	}
	serviceArgs := []string{"-state-dir", state, "-model", *model, "-concurrency", fmt.Sprint(*parallel), "-rpm", fmt.Sprint(*rpm), "-max-steps", fmt.Sprint(*steps), "-max-tokens", fmt.Sprint(*tokens), "-max-output", fmt.Sprint(*output), "-timeout", timeout.String(), "-thinking", *thinking}
	if command == "serve" {
		conn, err := connectService(ctx, state, serviceArgs)
		if err != nil {
			return err
		}
		return proxyStdio(ctx, conn)
	}
	var tasks []worker.Task
	if command == "run" {
		if *prompt == "" {
			b, err := io.ReadAll(io.LimitReader(os.Stdin, 65537))
			if err != nil {
				return err
			}
			*prompt = string(b)
		}
		t := worker.Task{Prompt: *prompt, Label: *label, Thinking: *thinking}
		if *contextIDs != "" {
			for _, id := range strings.Split(*contextIDs, ",") {
				t.ContextIDs = append(t.ContextIDs, strings.TrimSpace(id))
			}
		}
		if *write != "" {
			for _, p := range strings.Split(*write, ",") {
				t.WritePaths = append(t.WritePaths, strings.TrimSpace(p))
			}
		}
		tasks = []worker.Task{t}
	} else {
		var reader io.Reader = os.Stdin
		if *input != "" {
			f, err := os.Open(*input)
			if err != nil {
				return err
			}
			defer f.Close()
			reader = f
		}
		decoder := json.NewDecoder(io.LimitReader(reader, 2*1024*1024))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&tasks); err != nil {
			return err
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return fmt.Errorf("expected exactly one JSON task array")
		}
	}
	abs, err := filepath.Abs(*workspace)
	if err != nil {
		return err
	}
	for i := range tasks {
		if tasks[i].Workspace == "" {
			tasks[i].Workspace = abs
		}
	}
	conn, err := connectService(ctx, state, serviceArgs)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "codex-gemini-cli", Version: "0.3.0"}, nil)
	session, err := client.Connect(ctx, &mcp.IOTransport{Reader: conn, Writer: conn}, nil)
	if err != nil {
		return err
	}
	defer session.Close()
	call := func(name string, args, out any) error {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			return err
		}
		if res.IsError {
			return fmt.Errorf("%s: %v", name, res.Content)
		}
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			return err
		}
		return json.Unmarshal(b, out)
	}
	var batch worker.JobsOutput
	if err = call("gemini_batch", worker.BatchInput{Tasks: tasks}, &batch); err != nil {
		return err
	}
	jobs := batch.Jobs
	// Explicit CLI interruption cancels its jobs. Merely closing an MCP connection does not.
	defer func() {
		if ctx.Err() == nil {
			return
		}
		cancelCLIJobs(state, jobs)
	}()
	failed := false
	for _, j := range jobs {
		for {
			var result worker.Job
			if err := call("gemini_wait", worker.WaitInput{ID: j.ID, Seconds: 50}, &result); err != nil {
				return err
			}
			if result.Status == "queued" || result.Status == "running" {
				continue
			}
			if result.Status != "completed" {
				failed = true
			}
			if err = json.NewEncoder(os.Stdout).Encode(result); err != nil {
				return err
			}
			break
		}
	}
	if failed {
		return fmt.Errorf("one or more workers did not complete; see JSON results")
	}
	return nil
}
