package eventstream

import (
	"testing"
	"time"
)

func TestAck_RefusesToSkipAheadOfCompletion(t *testing.T) {
	townRoot := t.TempDir()
	agent := "gt-gastown-witness"

	if err := Ack(townRoot, agent, 5); err == nil {
		t.Fatal("Ack of an uncompleted id should fail, got nil error")
	}
	pos, err := AckedPosition(townRoot, agent)
	if err != nil {
		t.Fatalf("AckedPosition: %v", err)
	}
	if pos != 0 {
		t.Fatalf("AckedPosition after refused ack = %d, want 0 (cursor must not move)", pos)
	}
}

func TestAck_IsIdempotentAndMonotonic(t *testing.T) {
	townRoot := t.TempDir()
	agent := "gt-gastown-witness"

	if err := Complete(townRoot, agent, 1); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := Ack(townRoot, agent, 1); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	// Re-acking the same (now-past) id must be a no-op, not an error.
	if err := Ack(townRoot, agent, 1); err != nil {
		t.Fatalf("re-Ack of same id: %v", err)
	}
	pos, _ := AckedPosition(townRoot, agent)
	if pos != 1 {
		t.Fatalf("AckedPosition = %d, want 1", pos)
	}
}

func TestClaim_IsDurableAcrossReload(t *testing.T) {
	townRoot := t.TempDir()
	agent := "gt-gastown-witness"

	if err := Claim(townRoot, agent, 7); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// Simulate a fresh process (new load) reading state from disk rather
	// than from any in-memory struct.
	st, err := LoadState(townRoot, agent)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if st.ClaimedID != 7 {
		t.Fatalf("reloaded ClaimedID = %d, want 7 (claim did not survive reload)", st.ClaimedID)
	}
}

func TestComplete_IsIdempotent(t *testing.T) {
	townRoot := t.TempDir()
	agent := "gt-gastown-witness"

	if err := Complete(townRoot, agent, 3); err != nil {
		t.Fatalf("Complete 1: %v", err)
	}
	if err := Complete(townRoot, agent, 3); err != nil {
		t.Fatalf("Complete 2 (repeat): %v", err)
	}
	done, err := IsCompleted(townRoot, agent, 3)
	if err != nil {
		t.Fatalf("IsCompleted: %v", err)
	}
	if !done {
		t.Fatal("IsCompleted(3) = false, want true")
	}
}

func TestIsArmed_FreshHeartbeatIsArmed(t *testing.T) {
	townRoot := t.TempDir()
	agent := "gt-gastown-witness"

	if err := Heartbeat(townRoot, agent); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	armed, age, err := IsArmed(townRoot, agent, 30*time.Second)
	if err != nil {
		t.Fatalf("IsArmed: %v", err)
	}
	if !armed {
		t.Fatalf("IsArmed = false immediately after heartbeat (age %s), want true", age)
	}
}

func TestIsArmed_NoHeartbeatIsNotArmed(t *testing.T) {
	townRoot := t.TempDir()
	agent := "gt-gastown-witness"

	armed, _, err := IsArmed(townRoot, agent, 30*time.Second)
	if err != nil {
		t.Fatalf("IsArmed: %v", err)
	}
	if armed {
		t.Fatal("IsArmed = true with no heartbeat ever recorded, want false")
	}
}

func TestIsArmed_StaleHeartbeatIsNotArmed(t *testing.T) {
	townRoot := t.TempDir()
	agent := "gt-gastown-witness"

	// Write a heartbeat directly in the past — a crashed `gt events tail`
	// leaves exactly this shape: a state file whose HeartbeatAt stops
	// advancing while everything else in the town keeps moving.
	err := withStateLock(townRoot, agent, func(st *ConsumerState) error {
		st.HeartbeatAt = time.Now().Add(-time.Hour)
		return nil
	})
	if err != nil {
		t.Fatalf("seeding stale heartbeat: %v", err)
	}

	armed, age, err := IsArmed(townRoot, agent, 30*time.Second)
	if err != nil {
		t.Fatalf("IsArmed: %v", err)
	}
	if armed {
		t.Fatalf("IsArmed = true with a 1h-old heartbeat (age %s, max 30s), want false", age)
	}
}
