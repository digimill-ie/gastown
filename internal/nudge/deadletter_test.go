package nudge

import (
	"testing"
)

func TestDeadLetterListAndReplay(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-deadletter"

	n := QueuedNudge{Sender: "test", Message: "hello", Priority: PriorityNormal}
	id, err := DeadLetter(townRoot, session, n, "nudge-poller", "injection failed")
	if err != nil {
		t.Fatalf("DeadLetter: %v", err)
	}
	if id == "" {
		t.Fatal("DeadLetter returned empty id")
	}

	entries, err := ListDeadLetters(townRoot, session)
	if err != nil {
		t.Fatalf("ListDeadLetters: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	got := entries[0]
	if got.ID != id {
		t.Errorf("entry ID = %q, want %q", got.ID, id)
	}
	if got.Sender != n.Sender || got.Message != n.Message {
		t.Errorf("entry payload = %+v, want sender/message %q/%q", got, n.Sender, n.Message)
	}
	if got.Error != "injection failed" {
		t.Errorf("entry Error = %q, want %q", got.Error, "injection failed")
	}
	if got.Session != session {
		t.Errorf("entry Session = %q, want %q", got.Session, session)
	}

	// Not visible in the active queue until replayed.
	if pending, _ := Pending(townRoot, session); pending != 0 {
		t.Fatalf("queue has %d pending entries before replay, want 0", pending)
	}

	if err := ReplayDeadLetter(townRoot, session, id); err != nil {
		t.Fatalf("ReplayDeadLetter: %v", err)
	}

	// Replayed entry is now in the active queue...
	drained, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(drained) != 1 || drained[0].Message != n.Message {
		t.Fatalf("Drain after replay = %+v, want one entry with message %q", drained, n.Message)
	}

	// ...and gone from the dead-letter store.
	entries, err = ListDeadLetters(townRoot, session)
	if err != nil {
		t.Fatalf("ListDeadLetters after replay: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d dead-letter entries after replay, want 0", len(entries))
	}
}

func TestReplayDeadLetterUnknownID(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-deadletter-missing"

	if err := ReplayDeadLetter(townRoot, session, "does-not-exist"); err == nil {
		t.Fatal("ReplayDeadLetter with unknown id: got nil error, want an error")
	}
}

func TestListDeadLettersEmpty(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-deadletter-empty"

	entries, err := ListDeadLetters(townRoot, session)
	if err != nil {
		t.Fatalf("ListDeadLetters: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries for a session with none, want 0", len(entries))
	}
}
