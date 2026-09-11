package worker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var stepIDRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// PlanInput defines the request to decompose a task into structured steps.
type PlanInput struct {
	Task     Task `json:"task"`
	MaxTasks int  `json:"max_tasks,omitempty"`
}

// PlanStep represents a single step in a decomposed plan.
type PlanStep struct {
	ID         string   `json:"id"`
	Prompt     string   `json:"prompt"`
	WritePaths []string `json:"write_paths"`
	DependsOn  []string `json:"depends_on,omitempty"`
	Acceptance []string `json:"acceptance"`
	MaxTokens  int64    `json:"max_tokens"`
	Task       *Task    `json:"task,omitempty"`
}

// Decomposition contains the ordered/structured plan steps.
type Decomposition struct {
	Steps []PlanStep `json:"steps"`
}

// PlanOutput conveys the decomposition results or execution status.
type PlanOutput struct {
	JobID  string         `json:"job_id,omitempty"`
	Status string         `json:"status,omitempty"`
	Error  string         `json:"error,omitempty"`
	Plan   *Decomposition `json:"plan,omitempty"`
	Waves  [][]string     `json:"waves,omitempty"`
}

// StartPlan starts a read-only planning job to decompose an implementation task.
func (m *Manager) StartPlan(in PlanInput) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if in.Task.Workspace == "" || !filepath.IsAbs(in.Task.Workspace) {
		return Job{}, fmt.Errorf("explicit absolute workspace is required")
	}
	if len(in.Task.WritePaths) == 0 {
		return Job{}, fmt.Errorf("non-empty write_paths are required for implementation task decomposition")
	}
	if in.Task.Intent != "" && in.Task.Intent != "implementation" {
		return Job{}, fmt.Errorf("task intent must be empty or implementation")
	}

	normTask := in.Task
	normTask.Intent = "implementation"
	validatedTask, err := m.validateTask(normTask)
	if err != nil {
		return Job{}, fmt.Errorf("task validation failed: %w", err)
	}

	maxTasks := in.MaxTasks
	if maxTasks == 0 {
		maxTasks = 6
	} else if maxTasks < 1 || maxTasks > 12 {
		return Job{}, fmt.Errorf("max_tasks must be between 1 and 12")
	}

	normalizedInput := PlanInput{
		Task:     validatedTask,
		MaxTasks: maxTasks,
	}

	// Readonly planner copy: intent "investigation", write_paths nil
	plannerTask := validatedTask
	plannerTask.Intent = "investigation"
	plannerTask.WritePaths = nil

	promptBuf := &bytes.Buffer{}
	fmt.Fprintf(promptBuf, "You are decomposing a complex implementation task into smaller sequential or parallel implementation steps.\n\n")
	fmt.Fprintf(promptBuf, "ORIGINAL OBJECTIVE:\n%s\n\n", validatedTask.Prompt)
	fmt.Fprintf(promptBuf, "AUTHORIZED WRITE PATHS:\n")
	for _, wp := range validatedTask.WritePaths {
		fmt.Fprintf(promptBuf, "- %s\n", wp)
	}
	fmt.Fprintf(promptBuf, "\nCONSTRAINTS:\n")
	fmt.Fprintf(promptBuf, "- Maximum steps allowed: %d\n", maxTasks)
	fmt.Fprintf(promptBuf, "- Each step must be a focused implementation task with non-empty write_paths within authorized caller scope.\n")
	fmt.Fprintf(promptBuf, "- Each step must have 1-8 concrete acceptance criteria (each non-blank, <= 2000 chars).\n")
	fmt.Fprintf(promptBuf, "- Dependencies must reference valid step IDs. No cycles or self-references.\n")
	fmt.Fprintf(promptBuf, "- Steps that run concurrently (no transitive dependency) MUST NOT have overlapping write paths.\n")
	fmt.Fprintf(promptBuf, "- max_tokens for each step must be positive and <= %d.\n", m.cfg.MaxTokens)
	fmt.Fprintf(promptBuf, "- Do not edit any files. You are in read-only investigation mode.\n")
	fmt.Fprintf(promptBuf, "- Complete your work by calling report_checkpoint with complete=true, then output the FINAL RAW JSON plan.\n")
	fmt.Fprintf(promptBuf, "- The final message MUST BE RAW JSON ONLY (no markdown formatting, no code fences, no explanations) matching this schema:\n")
	fmt.Fprintf(promptBuf, `{"steps": [{"id": "step_1", "prompt": "...", "write_paths": ["..."], "depends_on": [], "acceptance": ["..."], "max_tokens": %d}]}`+"\n", m.cfg.MaxTokens)

	plannerTask.Prompt = promptBuf.String()

	jobs, err := m.spawnLocked([]Task{plannerTask})
	if err != nil {
		return Job{}, err
	}
	if len(jobs) != 1 {
		return Job{}, fmt.Errorf("unexpected job count spawned: %d", len(jobs))
	}

	spawned := jobs[0]
	stored, ok := m.jobs[spawned.ID]
	if !ok {
		return Job{}, fmt.Errorf("spawned job not found in manager")
	}

	stored.PlanSpec = &normalizedInput
	if err := m.save(stored); err != nil {
		if stored.cancel != nil {
			stored.cancel()
		}
		stored.Status = "cancelled"
		stored.Error = fmt.Sprintf("failed to save plan spec: %v", err)
		return Job{}, err
	}

	return snapshot(stored, false), nil
}

// GetPlan checks the status of a planning job and returns the validated plan if ready.
func (m *Manager) GetPlan(id string) (PlanOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	j, ok := m.jobs[id]
	if !ok {
		return PlanOutput{}, fmt.Errorf("job not found: %s", id)
	}
	if j.PlanSpec == nil {
		return PlanOutput{}, fmt.Errorf("job %s is not a planning job", id)
	}

	out := PlanOutput{
		JobID:  j.ID,
		Status: j.Status,
		Error:  j.Error,
	}

	if active(j.Status) {
		return out, nil
	}

	if j.Status != "completed" || j.Checkpoint == nil || !j.Checkpoint.Complete || len(j.Checkpoint.Remaining) > 0 {
		out.Status = "needs_attention"
		return out, nil
	}

	// Parse Result max 64KiB strict JSON
	resultBytes := []byte(j.Result)
	if len(resultBytes) > 65536 {
		out.Status = "needs_attention"
		out.Error = "result exceeds 64KiB limit"
		return out, nil
	}

	decomp, err := parseDecompositionResult(resultBytes)
	if err != nil {
		out.Status = "needs_attention"
		out.Error = fmt.Sprintf("failed to parse plan decomposition: %v", err)
		return out, nil
	}

	validDecomp, waves, err := m.validatePlanLocked(*j.PlanSpec, decomp)
	if err != nil {
		out.Status = "needs_attention"
		out.Error = fmt.Sprintf("plan validation failed: %v", err)
		return out, nil
	}

	out.Status = "plan_ready"
	out.Plan = &validDecomp
	out.Waves = waves
	return out, nil
}

// parseDecompositionResult trims optional ```json fences and parses strict JSON.
func parseDecompositionResult(b []byte) (Decomposition, error) {
	s := strings.TrimSpace(string(b))
	if strings.HasPrefix(s, "```json") && strings.HasSuffix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	} else if strings.HasPrefix(s, "```") && strings.HasSuffix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}

	var d Decomposition
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return Decomposition{}, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return Decomposition{}, fmt.Errorf("unexpected trailing content after JSON")
	}
	return d, nil
}

// validatePlanLocked validates a Decomposition against a PlanInput. Requires m.mu locked.
func (m *Manager) validatePlanLocked(in PlanInput, p Decomposition) (Decomposition, [][]string, error) {
	maxTasks := in.MaxTasks
	if maxTasks < 1 || maxTasks > 12 {
		maxTasks = 6
	}

	if len(p.Steps) < 1 || len(p.Steps) > maxTasks {
		return Decomposition{}, nil, fmt.Errorf("step count %d outside valid range 1..%d", len(p.Steps), maxTasks)
	}

	seenIDs := make(map[string]bool)

	for _, step := range p.Steps {
		if step.Task != nil {
			return Decomposition{}, nil, fmt.Errorf("planner cannot supply executable task overrides")
		}
		if !stepIDRegex.MatchString(step.ID) {
			return Decomposition{}, nil, fmt.Errorf("invalid step id %q: must match ^[a-zA-Z0-9_-]{1,64}$", step.ID)
		}
		if seenIDs[step.ID] {
			return Decomposition{}, nil, fmt.Errorf("duplicate step id: %s", step.ID)
		}
		seenIDs[step.ID] = true

		if strings.TrimSpace(step.Prompt) == "" {
			return Decomposition{}, nil, fmt.Errorf("step %s prompt is empty", step.ID)
		}

		if len(step.Acceptance) < 1 || len(step.Acceptance) > 8 {
			return Decomposition{}, nil, fmt.Errorf("step %s must have 1..8 acceptance criteria", step.ID)
		}
		for i, acc := range step.Acceptance {
			trimmed := strings.TrimSpace(acc)
			if trimmed == "" {
				return Decomposition{}, nil, fmt.Errorf("step %s acceptance item %d is blank", step.ID, i)
			}
			if len(trimmed) > 2000 {
				return Decomposition{}, nil, fmt.Errorf("step %s acceptance item %d exceeds 2000 bytes", step.ID, i)
			}
		}

		if step.MaxTokens <= 0 || step.MaxTokens > m.cfg.MaxTokens {
			return Decomposition{}, nil, fmt.Errorf("step %s max_tokens must be positive and <= %d (got %d)", step.ID, m.cfg.MaxTokens, step.MaxTokens)
		}

		if len(step.WritePaths) == 0 {
			return Decomposition{}, nil, fmt.Errorf("step %s write_paths cannot be empty", step.ID)
		}

	}

	// Validate dependencies: no self, no missing, no duplicate
	for _, step := range p.Steps {
		depSet := make(map[string]bool)
		for _, dep := range step.DependsOn {
			if dep == step.ID {
				return Decomposition{}, nil, fmt.Errorf("step %s depends on itself", step.ID)
			}
			if !seenIDs[dep] {
				return Decomposition{}, nil, fmt.Errorf("step %s references unknown dependency %s", step.ID, dep)
			}
			if depSet[dep] {
				return Decomposition{}, nil, fmt.Errorf("step %s has duplicate dependency %s", step.ID, dep)
			}
			depSet[dep] = true
		}
	}

	// Detect cycles & compute transitive dependencies
	// transitiveDeps[a][b] == true means 'a' depends on 'b' (b must finish before a)
	transitiveDeps := make(map[string]map[string]bool)
	for _, step := range p.Steps {
		transitiveDeps[step.ID] = make(map[string]bool)
		for _, dep := range step.DependsOn {
			transitiveDeps[step.ID][dep] = true
		}
	}

	// Floyd-Warshall / reachability for transitive closure
	for k := range seenIDs {
		for i := range seenIDs {
			for j := range seenIDs {
				if transitiveDeps[i][k] && transitiveDeps[k][j] {
					transitiveDeps[i][j] = true
				}
			}
		}
	}

	// Check cycles: if transitiveDeps[i][i] is true, cycle exists
	for id := range seenIDs {
		if transitiveDeps[id][id] {
			return Decomposition{}, nil, fmt.Errorf("dependency cycle detected involving step %s", id)
		}
	}

	// Validate and normalize child tasks
	normalizedSteps := make([]PlanStep, len(p.Steps))
	for idx, step := range p.Steps {
		childPromptBuf := &bytes.Buffer{}
		fmt.Fprintf(childPromptBuf, "ORIGINAL GOAL:\n%s\n\n", in.Task.Prompt)
		fmt.Fprintf(childPromptBuf, "STEP OBJECTIVE:\n%s\n\n", step.Prompt)
		fmt.Fprintf(childPromptBuf, "ACCEPTANCE CRITERIA:\n")
		for _, acc := range step.Acceptance {
			fmt.Fprintf(childPromptBuf, "- %s\n", strings.TrimSpace(acc))
		}

		childTask := in.Task
		childTask.Prompt = childPromptBuf.String()
		childTask.WritePaths = append([]string(nil), step.WritePaths...)
		childTask.Intent = "implementation"
		childTask.MaxTokens = step.MaxTokens

		validatedChild, err := m.validateTask(childTask)
		if err != nil {
			return Decomposition{}, nil, fmt.Errorf("child task for step %s validation failed: %w", step.ID, err)
		}

		// Ensure EACH normalized child writepath is contained in authorized scope via owns(spec.Task.WritePaths, path)
		for _, wp := range validatedChild.WritePaths {
			if !owns(in.Task.WritePaths, wp) {
				return Decomposition{}, nil, fmt.Errorf("step %s write_path %s is outside authorized scope", step.ID, wp)
			}
		}

		normalizedStep := step
		normalizedStep.WritePaths = validatedChild.WritePaths
		normalizedStep.Task = &validatedChild
		normalizedSteps[idx] = normalizedStep
	}

	// Reject overlapping writes for any unordered pair:
	// If neither i depends on j nor j depends on i transitively, they cannot have overlapping write paths
	for i := 0; i < len(normalizedSteps); i++ {
		for j := i + 1; j < len(normalizedSteps); j++ {
			s1 := normalizedSteps[i]
			s2 := normalizedSteps[j]

			s1DependsOnS2 := transitiveDeps[s1.ID][s2.ID]
			s2DependsOnS1 := transitiveDeps[s2.ID][s1.ID]

			if !s1DependsOnS2 && !s2DependsOnS1 {
				// Unordered pair: check write overlap
				if conflicts(s1.WritePaths, s2.WritePaths) {
					return Decomposition{}, nil, fmt.Errorf("concurrent steps %s and %s have conflicting write paths", s1.ID, s2.ID)
				}
			}
		}
	}

	// Compute stable topological waves where all prerequisites appear in earlier waves
	inDegree := make(map[string]int)
	dependents := make(map[string][]string)
	for _, step := range normalizedSteps {
		inDegree[step.ID] = len(step.DependsOn)
		for _, dep := range step.DependsOn {
			dependents[dep] = append(dependents[dep], step.ID)
		}
	}

	var waves [][]string
	remainingCount := len(normalizedSteps)

	for remainingCount > 0 {
		var currentWave []string
		for _, step := range normalizedSteps {
			if inDegree[step.ID] == 0 {
				currentWave = append(currentWave, step.ID)
			}
		}

		if len(currentWave) == 0 {
			return Decomposition{}, nil, fmt.Errorf("dependency resolution deadlocked")
		}

		// Keep currentWave sorted deterministically
		sort.Strings(currentWave)
		waves = append(waves, currentWave)

		for _, id := range currentWave {
			inDegree[id] = -1 // marked as resolved
			remainingCount--
			for _, depID := range dependents[id] {
				inDegree[depID]--
			}
		}
	}

	return Decomposition{Steps: normalizedSteps}, waves, nil
}
