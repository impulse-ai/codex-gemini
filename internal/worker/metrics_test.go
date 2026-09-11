package worker

import (
	"errors"
	"testing"
)

func TestMetricTracker_Observe(t *testing.T) {
	tracker := newMetricTracker()
	metrics := &JobMetrics{}

	// 1. Successful read_file call
	args1 := map[string]any{
		"path":       "foo.go",
		"start_line": 1,
		"max_lines":  100,
	}
	output1 := map[string]any{
		"sha256":  "abc",
		"content": "hello",
	}
	tracker.Observe(metrics, "read_file", args1, output1, nil, 1, 100)

	if metrics.InspectionCalls != 1 {
		t.Fatalf("expected InspectionCalls 1, got %d", metrics.InspectionCalls)
	}
	if metrics.RepeatedReads != 0 {
		t.Fatalf("expected RepeatedReads 0, got %d", metrics.RepeatedReads)
	}

	// 2. Duplicate identical read_file call increments RepeatedReads
	tracker.Observe(metrics, "read_file", args1, output1, nil, 2, 200)
	if metrics.InspectionCalls != 2 {
		t.Fatalf("expected InspectionCalls 2, got %d", metrics.InspectionCalls)
	}
	if metrics.RepeatedReads != 1 {
		t.Fatalf("expected RepeatedReads 1, got %d", metrics.RepeatedReads)
	}

	// 3. Different sha256 output does not increment RepeatedReads
	output2 := map[string]any{
		"sha256":  "def",
		"content": "world",
	}
	tracker.Observe(metrics, "read_file", args1, output2, nil, 3, 300)
	if metrics.InspectionCalls != 3 {
		t.Fatalf("expected InspectionCalls 3, got %d", metrics.InspectionCalls)
	}
	if metrics.RepeatedReads != 1 {
		t.Fatalf("expected RepeatedReads 1, got %d", metrics.RepeatedReads)
	}

	// 4. Failed read call is not counted
	tracker.Observe(metrics, "read_file", args1, output1, errors.New("read error"), 4, 400)
	if metrics.InspectionCalls != 3 {
		t.Fatalf("expected InspectionCalls to remain 3, got %d", metrics.InspectionCalls)
	}
	if metrics.RepeatedReads != 1 {
		t.Fatalf("expected RepeatedReads to remain 1, got %d", metrics.RepeatedReads)
	}

	// 5. Failed write call is not counted
	writeArgs := map[string]any{"path": "foo.go", "content": "bar"}
	tracker.Observe(metrics, "write_file", writeArgs, nil, errors.New("write error"), 5, 500)
	if metrics.SuccessfulEdits != 0 {
		t.Fatalf("expected SuccessfulEdits 0, got %d", metrics.SuccessfulEdits)
	}
	if metrics.FirstEditStep != 0 || metrics.FirstEditTokens != 0 {
		t.Fatalf("expected FirstEditStep and FirstEditTokens 0, got step %d tokens %d", metrics.FirstEditStep, metrics.FirstEditTokens)
	}

	// 6. Two successful edit_file calls count 2 and FirstEditStep/Tokens retain first
	editArgs1 := map[string]any{"path": "foo.go", "new_text": "v1"}
	tracker.Observe(metrics, "edit_file", editArgs1, nil, nil, 6, 650)
	if metrics.SuccessfulEdits != 1 {
		t.Fatalf("expected SuccessfulEdits 1, got %d", metrics.SuccessfulEdits)
	}
	if metrics.FirstEditStep != 6 || metrics.FirstEditTokens != 650 {
		t.Fatalf("expected FirstEditStep 6 and FirstEditTokens 650, got step %d tokens %d", metrics.FirstEditStep, metrics.FirstEditTokens)
	}

	editArgs2 := map[string]any{"path": "foo.go", "new_text": "v2"}
	tracker.Observe(metrics, "edit_file", editArgs2, nil, nil, 7, 800)
	if metrics.SuccessfulEdits != 2 {
		t.Fatalf("expected SuccessfulEdits 2, got %d", metrics.SuccessfulEdits)
	}
	if metrics.FirstEditStep != 6 || metrics.FirstEditTokens != 650 {
		t.Fatalf("expected FirstEditStep 6 and FirstEditTokens 650 retained, got step %d tokens %d", metrics.FirstEditStep, metrics.FirstEditTokens)
	}
}
