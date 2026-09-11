package eventstream

import (
	"testing"
)

func TestAppend_AssignsStableSequentialIDs(t *testing.T) {
	townRoot := t.TempDir()
	agent := "gt-gastown-witness"

	e1, err := Append(townRoot, agent, TypeQueuedNudge, map[string]interface{}{"message": "one"})
	if err != nil {
		t.Fatalf("Append 1: %v", err)
	}
	e2, err := Append(townRoot, agent, TypeQueuedNudge, map[string]interface{}{"message": "two"})
	if err != nil {
		t.Fatalf("Append 2: %v", err)
	}
	e3, err := Append(townRoot, agent, TypeQueuedNudge, map[string]interface{}{"message": "three"})
	if err != nil {
		t.Fatalf("Append 3: %v", err)
	}

	if e1.ID != 1 || e2.ID != 2 || e3.ID != 3 {
		t.Fatalf("ids = %d, %d, %d; want 1, 2, 3", e1.ID, e2.ID, e3.ID)
	}
}

func TestAppend_IDsArePerAgent(t *testing.T) {
	townRoot := t.TempDir()

	a1, err := Append(townRoot, "gt-gastown-witness", TypeQueuedNudge, nil)
	if err != nil {
		t.Fatalf("Append agent1: %v", err)
	}
	b1, err := Append(townRoot, "gt-metalworks-witness", TypeQueuedNudge, nil)
	if err != nil {
		t.Fatalf("Append agent2: %v", err)
	}
	if a1.ID != 1 || b1.ID != 1 {
		t.Fatalf("each agent should start its own sequence at 1, got %d and %d", a1.ID, b1.ID)
	}
}

func TestReadAfter_OrderedReplay(t *testing.T) {
	townRoot := t.TempDir()
	agent := "gt-gastown-witness"

	for i := 0; i < 5; i++ {
		if _, err := Append(townRoot, agent, TypeQueuedNudge, nil); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	events, err := ReadAfter(townRoot, agent, 2)
	if err != nil {
		t.Fatalf("ReadAfter: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("ReadAfter(2) returned %d events, want 3", len(events))
	}
	for i, ev := range events {
		wantID := int64(3 + i)
		if ev.ID != wantID {
			t.Errorf("events[%d].ID = %d, want %d (out of order)", i, ev.ID, wantID)
		}
	}
}

func TestReadAfter_EmptyStreamIsNotAnError(t *testing.T) {
	townRoot := t.TempDir()
	events, err := ReadAfter(townRoot, "gt-gastown-witness", 0)
	if err != nil {
		t.Fatalf("ReadAfter on empty stream: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected no events, got %d", len(events))
	}
}
