package eventstream

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestConsumeOnce_DeliversInOrder(t *testing.T) {
	townRoot := t.TempDir()
	agent := "gt-gastown-witness"

	for i := 0; i < 3; i++ {
		if _, err := Append(townRoot, agent, TypeQueuedNudge, map[string]interface{}{"i": i}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	var seen []int64
	delivered, err := ConsumeOnce(townRoot, agent, func(ev Event) error {
		seen = append(seen, ev.ID)
		return nil
	})
	if err != nil {
		t.Fatalf("ConsumeOnce: %v", err)
	}
	if len(delivered) != 3 {
		t.Fatalf("delivered %d events, want 3", len(delivered))
	}
	for i, id := range seen {
		if id != int64(i+1) {
			t.Fatalf("seen[%d] = %d, want %d (out of order)", i, id, i+1)
		}
	}

	pos, err := AckedPosition(townRoot, agent)
	if err != nil {
		t.Fatalf("AckedPosition: %v", err)
	}
	if pos != 3 {
		t.Fatalf("AckedPosition = %d, want 3", pos)
	}
}

func TestConsumeOnce_NothingPendingReturnsEmpty(t *testing.T) {
	townRoot := t.TempDir()
	agent := "gt-gastown-witness"

	called := false
	delivered, err := ConsumeOnce(townRoot, agent, func(Event) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("ConsumeOnce on empty stream: %v", err)
	}
	if called {
		t.Fatal("apply was invoked with no pending events")
	}
	if len(delivered) != 0 {
		t.Fatalf("delivered = %d, want 0", len(delivered))
	}
}

// TestConsumeOnce_RestartRecovery_NoDuplicateEffect is the "restart
// recovery replays from the acked position without duplicate effects"
// demonstration required by the bead. It constructs the exact crash
// window the design calls out — Complete recorded, Ack never happened —
// by driving Claim/Complete directly (standing in for a consumer process
// that died in that gap) and then proves a fresh ConsumeOnce pass never
// re-invokes the effect for that event, only catches the cursor up.
func TestConsumeOnce_RestartRecovery_NoDuplicateEffect(t *testing.T) {
	townRoot := t.TempDir()
	agent := "gt-gastown-witness"

	for i := 0; i < 3; i++ {
		if _, err := Append(townRoot, agent, TypeQueuedNudge, nil); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	effectCount := map[int64]int{}

	// --- "First run": processes event 1, then crashes after Complete but
	// before Ack. Driven directly rather than via ConsumeOnce so the test
	// can land exactly in the gap between those two calls.
	if err := Claim(townRoot, agent, 1); err != nil {
		t.Fatalf("Claim(1): %v", err)
	}
	effectCount[1]++ // the effect (e.g. printing the notification) runs here
	if err := Complete(townRoot, agent, 1); err != nil {
		t.Fatalf("Complete(1): %v", err)
	}
	// Crash: no Ack(1). AckedPosition is still 0, but event 1's effect is
	// already durably marked complete.

	pos, err := AckedPosition(townRoot, agent)
	if err != nil {
		t.Fatalf("AckedPosition after crash: %v", err)
	}
	if pos != 0 {
		t.Fatalf("AckedPosition = %d before recovery pass, want 0", pos)
	}

	// --- "Restart": a fresh consumer pass over the same durable state.
	delivered, err := ConsumeOnce(townRoot, agent, func(ev Event) error {
		effectCount[ev.ID]++
		return nil
	})
	if err != nil {
		t.Fatalf("ConsumeOnce (recovery pass): %v", err)
	}

	if effectCount[1] != 1 {
		t.Fatalf("effect for event 1 ran %d times, want exactly 1 (duplicate effect after restart)", effectCount[1])
	}
	if effectCount[2] != 1 || effectCount[3] != 1 {
		t.Fatalf("effect counts for 2,3 = %d,%d, want 1,1", effectCount[2], effectCount[3])
	}

	// The recovery pass must not report event 1 as freshly delivered —
	// its effect already happened before the crash.
	for _, ev := range delivered {
		if ev.ID == 1 {
			t.Fatal("recovery pass reported event 1 as delivered; its effect was already applied pre-crash")
		}
	}
	if len(delivered) != 2 {
		t.Fatalf("recovery pass delivered %d events, want 2 (events 2 and 3)", len(delivered))
	}

	finalPos, err := AckedPosition(townRoot, agent)
	if err != nil {
		t.Fatalf("AckedPosition after recovery: %v", err)
	}
	if finalPos != 3 {
		t.Fatalf("AckedPosition after recovery = %d, want 3 (cursor must catch up over the pre-crashed event too)", finalPos)
	}
}

// TestConsumeOnce_StopsOnApplyErrorAndRetriesInOrder proves that a stuck
// event blocks acking anything after it (ordered replay), and that once
// it succeeds, its effect still runs exactly once (no double-apply from
// the earlier failed attempt).
func TestConsumeOnce_StopsOnApplyErrorAndRetriesInOrder(t *testing.T) {
	townRoot := t.TempDir()
	agent := "gt-gastown-witness"

	for i := 0; i < 3; i++ {
		if _, err := Append(townRoot, agent, TypeQueuedNudge, nil); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	effectCount := map[int64]int{}
	failEventTwo := true

	apply := func(ev Event) error {
		if ev.ID == 2 && failEventTwo {
			return fmt.Errorf("simulated transient failure on event 2")
		}
		effectCount[ev.ID]++
		return nil
	}

	delivered, err := ConsumeOnce(townRoot, agent, apply)
	if err == nil {
		t.Fatal("expected ConsumeOnce to return the apply error for event 2")
	}
	if len(delivered) != 1 || delivered[0].ID != 1 {
		t.Fatalf("delivered = %v, want exactly [event 1]", delivered)
	}
	pos, _ := AckedPosition(townRoot, agent)
	if pos != 1 {
		t.Fatalf("AckedPosition after failed pass = %d, want 1 (event 3 must not be acked ahead of stuck event 2)", pos)
	}

	// Retry: event 2 now succeeds.
	failEventTwo = false
	delivered, err = ConsumeOnce(townRoot, agent, apply)
	if err != nil {
		t.Fatalf("ConsumeOnce retry: %v", err)
	}
	if len(delivered) != 2 || delivered[0].ID != 2 || delivered[1].ID != 3 {
		t.Fatalf("retry delivered = %v, want [event 2, event 3]", delivered)
	}
	if effectCount[1] != 1 || effectCount[2] != 1 || effectCount[3] != 1 {
		t.Fatalf("effect counts = %v, want exactly 1 each", effectCount)
	}
	pos, _ = AckedPosition(townRoot, agent)
	if pos != 3 {
		t.Fatalf("AckedPosition after retry = %d, want 3", pos)
	}
}

// TestConsumeOnce_ConcurrentConsumersNeverDoubleApply guards against an
// accidentally double-armed "gt events tail <target>" — a real recurring
// failure mode elsewhere in this codebase (duplicate watchers on a single-
// consumer channel). Two goroutines race ConsumeOnce against the same
// agent stream; the per-event lock in processEventLocked must serialize
// them so each event's effect runs exactly once, however the race lands.
func TestConsumeOnce_ConcurrentConsumersNeverDoubleApply(t *testing.T) {
	townRoot := t.TempDir()
	agent := "gt-gastown-witness"

	const numEvents = 20
	for i := 0; i < numEvents; i++ {
		if _, err := Append(townRoot, agent, TypeQueuedNudge, nil); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	var effectCounts [numEvents + 1]int32 // index by event id (1-based)
	apply := func(ev Event) error {
		atomic.AddInt32(&effectCounts[ev.ID], 1)
		return nil
	}

	var wg sync.WaitGroup
	const numConsumers = 8
	for c := 0; c < numConsumers; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each concurrent "consumer" hammers ConsumeOnce until the
			// stream is fully acked, mirroring what a real accidental
			// double-tail would do (poll repeatedly against the same
			// target).
			for i := 0; i < numEvents*2; i++ {
				if _, err := ConsumeOnce(townRoot, agent, apply); err != nil {
					t.Errorf("ConsumeOnce: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	for id := 1; id <= numEvents; id++ {
		if got := atomic.LoadInt32(&effectCounts[id]); got != 1 {
			t.Errorf("effect for event %d ran %d times, want exactly 1 (double-delivery under concurrent consumers)", id, got)
		}
	}

	pos, err := AckedPosition(townRoot, agent)
	if err != nil {
		t.Fatalf("AckedPosition: %v", err)
	}
	if pos != numEvents {
		t.Fatalf("AckedPosition = %d, want %d", pos, numEvents)
	}
}
