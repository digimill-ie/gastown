package cmd

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/tmux"
)

func TestShouldSkipDrainUntilIdle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		hasPromptDetection bool
		waitErr            error
		want               bool
	}{
		{"prompt aware idle", true, nil, false},
		{"prompt aware busy", true, errors.New("timeout"), true},
		{"no prompt detection busy", false, errors.New("timeout"), false},
		{"no prompt detection idle", false, nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldSkipDrainUntilIdle(tt.hasPromptDetection, tt.waitErr); got != tt.want {
				t.Errorf("shouldSkipDrainUntilIdle(%v, %v) = %v, want %v", tt.hasPromptDetection, tt.waitErr, got, tt.want)
			}
		})
	}
}

// TestDeadLetterDrainedNudges_OneFilePerEntryNothingRequeued covers hq-g52db
// FINAL CUT: a failed injection is dead-lettered, never requeued. RED if
// deadLetterDrainedNudges goes back to calling requeueDrainedNudges (or any
// other path that puts the entry back in the active queue) instead of
// nudge.DeadLetter.
func TestDeadLetterDrainedNudges_OneFilePerEntryNothingRequeued(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-crew-test-deadletter"
	drained := []nudge.QueuedNudge{
		{Sender: "test", Message: "first"},
		{Sender: "test", Message: "second"},
	}

	deadLetterDrainedNudges(tmux.NewTmux(), townRoot, session, drained, errors.New("injection failed"))

	entries, err := nudge.ListDeadLetters(townRoot, session)
	if err != nil {
		t.Fatalf("ListDeadLetters: %v", err)
	}
	if len(entries) != len(drained) {
		t.Fatalf("got %d dead-letter entries, want %d (one file per drained entry)", len(entries), len(drained))
	}

	// Nothing goes back into the active queue.
	pending, err := nudge.Pending(townRoot, session)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending != 0 {
		t.Errorf("active queue has %d pending entries, want 0 (no requeue on injection failure)", pending)
	}
}
