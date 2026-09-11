package nudge

import (
	"testing"
	"time"
)

func TestDeadLetterAndList(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-deadletter"

	n := QueuedNudge{ID: "id-1", Sender: "test", Message: "stuck payload", Attempts: 1, Timestamp: time.Now()}

	path, err := DeadLetter(townRoot, session, n, "nudge-poller", "composer dirty", "pane capture text", false)
	if err != nil {
		t.Fatalf("DeadLetter: %v", err)
	}
	if path == "" {
		t.Fatal("DeadLetter returned empty path")
	}

	entries, err := ListDeadLetters(townRoot, session)
	if err != nil {
		t.Fatalf("ListDeadLetters: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}

	got := entries[0]
	if got.ID != "id-1" || got.Message != "stuck payload" || got.Sender != "test" {
		t.Errorf("entry = %#v, want ID=id-1 Message=%q Sender=test", got, "stuck payload")
	}
	if got.Source != "nudge-poller" {
		t.Errorf("Source = %q, want nudge-poller", got.Source)
	}
	if got.PaneCapture != "pane capture text" {
		t.Errorf("PaneCapture = %q, want %q", got.PaneCapture, "pane capture text")
	}
	if got.LastError != "composer dirty" {
		t.Errorf("LastError = %q, want %q", got.LastError, "composer dirty")
	}
	if got.UncertainDelivery {
		t.Error("UncertainDelivery = true, want false")
	}
	if got.DeadLetteredAt.IsZero() {
		t.Error("DeadLetteredAt should be set")
	}
}

// TestDeadLetterAssignsIDWhenEntryHasNone covers a code-review finding: a
// caller that builds a QueuedNudge inline rather than through Enqueue (e.g.
// cmd.deliverNudge's wait-idle composer-dirty path) passes an entry with no
// ID. DeadLetter must assign one and persist it in the record, not merely
// use a locally generated id for the filename and discard it — otherwise
// the entry can never be named via `gt nudge dead-letter replay`.
func TestDeadLetterAssignsIDWhenEntryHasNone(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-deadletter-no-id"

	n := QueuedNudge{Sender: "test", Message: "no id yet", Timestamp: time.Now()}
	if _, err := DeadLetter(townRoot, session, n, "wait-idle", "composer dirty", "", false); err != nil {
		t.Fatalf("DeadLetter: %v", err)
	}

	entries, err := ListDeadLetters(townRoot, session)
	if err != nil {
		t.Fatalf("ListDeadLetters: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].ID == "" {
		t.Error("persisted entry has empty ID; gt nudge dead-letter replay can never name it")
	}

	// The assigned ID must also be replayable — a locally generated
	// filename-only id would pass ListDeadLetters but fail here.
	if err := ReplayDeadLetter(townRoot, session, entries[0].ID); err != nil {
		t.Errorf("ReplayDeadLetter(%q) failed using the persisted ID: %v", entries[0].ID, err)
	}
}

func TestListDeadLettersEmptyIsNotError(t *testing.T) {
	townRoot := t.TempDir()
	entries, err := ListDeadLetters(townRoot, "gt-no-such-session")
	if err != nil {
		t.Fatalf("ListDeadLetters: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries, want 0", len(entries))
	}
}

func TestDeadLetterOrdering(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-deadletter-order"

	for i, id := range []string{"first", "second", "third"} {
		n := QueuedNudge{ID: id, Sender: "test", Message: id, Timestamp: time.Now()}
		if _, err := DeadLetter(townRoot, session, n, "nudge-poller", "err", "", false); err != nil {
			t.Fatalf("DeadLetter %d: %v", i, err)
		}
		time.Sleep(time.Millisecond)
	}

	entries, err := ListDeadLetters(townRoot, session)
	if err != nil {
		t.Fatalf("ListDeadLetters: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	want := []string{"first", "second", "third"}
	for i, w := range want {
		if entries[i].ID != w {
			t.Errorf("entries[%d].ID = %q, want %q (oldest first)", i, entries[i].ID, w)
		}
	}
}

// TestReplayDeadLetter covers the explicit-replay half of the dead-letter
// contract (`gt nudge dead-letter replay <session> <id>`): the entry moves
// back into the active queue with attempts reset, and the dead-letter record
// is removed so the same entry cannot be replayed twice.
func TestReplayDeadLetter(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-replay"

	n := QueuedNudge{ID: "replay-me", Sender: "test", Message: "please retry", Attempts: 2, LastError: "boom", Timestamp: time.Now()}
	if _, err := DeadLetter(townRoot, session, n, "nudge-poller", "boom", "", true); err != nil {
		t.Fatalf("DeadLetter: %v", err)
	}

	if err := ReplayDeadLetter(townRoot, session, "replay-me"); err != nil {
		t.Fatalf("ReplayDeadLetter: %v", err)
	}

	// The dead-letter entry is gone.
	entries, err := ListDeadLetters(townRoot, session)
	if err != nil {
		t.Fatalf("ListDeadLetters: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("got %d dead-letter entries after replay, want 0", len(entries))
	}

	// The entry is back in the active queue, reset for a fresh attempt.
	drained, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(drained) != 1 {
		t.Fatalf("got %d drained entries, want 1", len(drained))
	}
	got := drained[0]
	if got.ID != "replay-me" || got.Message != "please retry" {
		t.Errorf("replayed entry = %#v, want ID=replay-me Message=%q", got, "please retry")
	}
	if got.Attempts != 0 {
		t.Errorf("Attempts = %d, want 0 (replay resets the retry bound)", got.Attempts)
	}
}

func TestReplayDeadLetterNotFound(t *testing.T) {
	townRoot := t.TempDir()
	if err := ReplayDeadLetter(townRoot, "gt-test-no-entries", "does-not-exist"); err == nil {
		t.Fatal("expected an error replaying a nonexistent dead-letter entry, got nil")
	}
}
