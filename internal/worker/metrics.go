package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

type JobMetrics struct {
	BaselineTokens  int64 `json:"baseline_tokens"`
	BaselineSteps   int   `json:"baseline_steps"`
	InspectionCalls int   `json:"inspection_calls"`
	RepeatedReads   int   `json:"repeated_reads"`
	SuccessfulEdits int   `json:"successful_edits"`
	FirstEditStep   int   `json:"first_edit_step"`
	FirstEditTokens int64 `json:"first_edit_tokens"`
	Nudges          int   `json:"nudges"`
}

type metricTracker struct {
	seen map[string]bool
}

func (t *metricTracker) Observe(metrics *JobMetrics, name string, args map[string]any, output any, err error, step int, tokens int64) {
	if metrics == nil {
		return
	}
	if t.seen == nil {
		t.seen = make(map[string]bool)
	}
	if err != nil {
		return
	}

	switch name {
	case "read_file", "search_files", "list_files", "search_memory":
		metrics.InspectionCalls++
		if name == "read_file" {
			readCall := struct {
				Path   any `json:"path"`
				Start  any `json:"start_line,omitempty"`
				Max    any `json:"max_lines,omitempty"`
				Output any `json:"output"`
			}{
				Path:   args["path"],
				Start:  args["start_line"],
				Max:    args["max_lines"],
				Output: output,
			}
			b, jsonErr := json.Marshal(readCall)
			if jsonErr == nil {
				h := sha256.Sum256(b)
				key := hex.EncodeToString(h[:])
				if t.seen[key] {
					metrics.RepeatedReads++
				} else {
					t.seen[key] = true
				}
			}
		}
	case "edit_file", "write_file":
		metrics.SuccessfulEdits++
		if metrics.FirstEditStep == 0 {
			metrics.FirstEditStep = step
			metrics.FirstEditTokens = tokens
		}
	}
}

type MetricsReport struct {
	MeasuredJobs        int   `json:"measured_jobs"`
	LegacyJobs          int   `json:"legacy_jobs"`
	CompletedJobs       int   `json:"completed_jobs"`
	JobsWithEdits       int   `json:"jobs_with_edits"`
	StoppedWithoutEdits int   `json:"stopped_without_edits"`
	Tokens              int64 `json:"tokens"`
	FirstEditTokens     int64 `json:"first_edit_tokens"`
	Inspections         int   `json:"inspections"`
	RepeatedReads       int   `json:"repeated_reads"`
}

func (m *Manager) OutcomeMetrics() MetricsReport {
	m.mu.Lock()
	defer m.mu.Unlock()

	var report MetricsReport
	for _, j := range m.jobs {
		jm := j.Metrics
		if jm == nil {
			report.LegacyJobs++
			continue
		}

		report.MeasuredJobs++
		if j.Status == "completed" {
			report.CompletedJobs++
		}
		if jm.SuccessfulEdits > 0 {
			report.JobsWithEdits++
		}
		if len(j.Task.WritePaths) > 0 && !active(j.Status) && jm.SuccessfulEdits == 0 {
			report.StoppedWithoutEdits++
		}
		report.Tokens += j.Usage.Total - jm.BaselineTokens
		report.FirstEditTokens += jm.FirstEditTokens
		report.Inspections += jm.InspectionCalls
		report.RepeatedReads += jm.RepeatedReads
	}
	return report
}

func newMetricTracker() *metricTracker { return &metricTracker{} }
