package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/vishal0589/gstreco-tally-agent/internal/syncrun"
)

func TestErrorFromWalkResultReturnsNilForCleanWalk(t *testing.T) {
	if err := errorFromWalkResult(syncrun.WalkResult{
		MappingsRun:      1,
		TotalRows:        10,
		TotalBatchesSent: 7,
	}); err != nil {
		t.Fatalf("errorFromWalkResult clean walk = %v, want nil", err)
	}
}

func TestErrorFromWalkResultReportsLocalAndServerFailures(t *testing.T) {
	err := errorFromWalkResult(syncrun.WalkResult{
		MappingsRun:        0,
		TotalRows:          0,
		TotalBatchesSent:   0,
		TotalBatchesFailed: 1,
		TotalServerErrors:  2,
		FatalErrors:        7,
	})
	if err == nil {
		t.Fatal("errorFromWalkResult returned nil for failed walk")
	}
	msg := err.Error()
	for _, want := range []string{
		"fatal_errors=7",
		"batches_failed=1",
		"server_errors=2",
		"mappings_run=0",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error message %q missing %q", msg, want)
		}
	}
}

func TestTickStatusForErrorKeepsDetailForHeartbeat(t *testing.T) {
	status := tickStatusForError(errorFromWalkResult(syncrun.WalkResult{
		FatalErrors: 1,
	}))
	if !strings.HasPrefix(status, "error: ") {
		t.Fatalf("status = %q, want error prefix", status)
	}
	if !strings.Contains(status, "fatal_errors=1") {
		t.Fatalf("status = %q, want fatal error detail", status)
	}
}

func TestTickStatusForErrorCapsLongStatus(t *testing.T) {
	status := tickStatusForError(errors.New(strings.Repeat("x", 1000)))
	if len(status) != 500 {
		t.Fatalf("status length = %d, want 500", len(status))
	}
}
