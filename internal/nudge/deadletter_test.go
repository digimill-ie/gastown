package nudge

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// TestReplayDeadLetterConcurrentDoesNotDoubleQueue covers the race codex
// found (deadletter.go:160, changes-requested at 08964387): replay neither
// claimed nor locked the entry, so two concurrent replays of the same ID
// could both re-enqueue it. ReplayDeadLetter now atomically claims the
// entry (rename to .replaying) before re-enqueuing, so only one of two
// concurrent replays can win; the loser must fail rather than double-queue
// the payload.
func TestReplayDeadLetterConcurrentDoesNotDoubleQueue(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-replay-race"

	n := QueuedNudge{ID: "race-me", Sender: "test", Message: "only once", Timestamp: time.Now()}
	if _, err := DeadLetter(townRoot, session, n, "nudge-poller", "boom", "", true); err != nil {
		t.Fatalf("DeadLetter: %v", err)
	}

	const attempts = 8
	results := make(chan error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- ReplayDeadLetter(townRoot, session, "race-me")
		}()
	}
	wg.Wait()
	close(results)

	var succeeded, failed int
	for err := range results {
		if err == nil {
			succeeded++
		} else {
			failed++
		}
	}
	if succeeded != 1 {
		t.Errorf("succeeded replays = %d, want exactly 1 (a race must not double-queue the payload)", succeeded)
	}
	if failed != attempts-1 {
		t.Errorf("failed replays = %d, want %d", failed, attempts-1)
	}

	drained, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(drained) != 1 {
		t.Fatalf("queue got %d entries after concurrent replay, want exactly 1 (no duplication)", len(drained))
	}
}

// TestOrphanedReplayIsSweptBackToVisible covers the codex finding at
// deadletter.go:164: a replay that crashes AFTER the claim rename but
// BEFORE the Enqueue/remove that follows it leaves a ".replaying" file
// that both ListDeadLetters and ReplayDeadLetter's own scan filter
// strictly to a ".json" suffix — invisible to both, forever, without a
// sweep (changes-requested at 08964387/95f841e6 rework).
func TestOrphanedReplayIsSweptBackToVisible(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-orphaned-replay"

	n := QueuedNudge{ID: "crashed-replay", Sender: "test", Message: "must not vanish", Timestamp: time.Now()}
	path, err := DeadLetter(townRoot, session, n, "nudge-poller", "boom", "", true)
	if err != nil {
		t.Fatalf("DeadLetter: %v", err)
	}

	// Simulate ReplayDeadLetter's crash window: claim the entry (the exact
	// rename ReplayDeadLetter performs) and stop there — no Enqueue, no
	// remove.
	claimPath := path + ".replaying"
	if err := os.Rename(path, claimPath); err != nil {
		t.Fatalf("simulating claimed-but-crashed replay: %v", err)
	}
	old := time.Now().Add(-deadLetterReplayStaleThreshold - time.Minute)
	if err := os.Chtimes(claimPath, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// Both ListDeadLetters and ReplayDeadLetter run the sweep before their
	// own scan, so either one recovers it. Exercise List first.
	entries, err := ListDeadLetters(townRoot, session)
	if err != nil {
		t.Fatalf("ListDeadLetters: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != "crashed-replay" {
		t.Fatalf("ListDeadLetters after orphan sweep = %#v, want the crashed replay visible again", entries)
	}

	// And it must be reachable by id for an actual retry, not just listed.
	if err := ReplayDeadLetter(townRoot, session, "crashed-replay"); err != nil {
		t.Fatalf("ReplayDeadLetter after orphan sweep: %v", err)
	}
	drained, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(drained) != 1 || drained[0].Message != "must not vanish" {
		t.Fatalf("drained = %#v, want the recovered payload intact", drained)
	}
}

// TestOrphanedReplayAfterSuccessfulEnqueueIsNotResurrected covers High 9
// (deadletter.go:211): a crash between ReplayDeadLetter's Enqueue
// succeeding and its claim-file removal leaves a stale ".replaying" claim
// whose payload is ALREADY live in the active queue. The prior sweep
// treated every stale ".replaying" file the same way — restore it to a
// plain, replayable dead-letter entry — which would let a SECOND replay
// enqueue a duplicate. The sweep must instead recognize that this
// specific claim's ID is already pending in the active queue and remove
// it outright instead of resurrecting it.
func TestOrphanedReplayAfterSuccessfulEnqueueIsNotResurrected(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-replay-post-enqueue-crash"

	n := QueuedNudge{ID: "already-enqueued", Sender: "test", Message: "landed before the crash", Timestamp: time.Now()}
	path, err := DeadLetter(townRoot, session, n, "nudge-poller", "boom", "", true)
	if err != nil {
		t.Fatalf("DeadLetter: %v", err)
	}

	// Simulate ReplayDeadLetter's crash window AFTER Enqueue succeeded but
	// BEFORE the claim file was removed: claim the entry, Enqueue its
	// payload for real, and stop — no os.Remove(claimPath).
	claimPath := path + ".replaying"
	if err := os.Rename(path, claimPath); err != nil {
		t.Fatalf("simulating claim: %v", err)
	}
	replay := n
	replay.Attempts = 0
	replay.LastError = ""
	if err := Enqueue(townRoot, session, replay); err != nil {
		t.Fatalf("simulating the replay's own Enqueue: %v", err)
	}
	old := time.Now().Add(-deadLetterReplayStaleThreshold - time.Minute)
	if err := os.Chtimes(claimPath, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// The sweep (via ListDeadLetters) must recognize the payload is
	// already live and remove the stale claim, NOT restore it as a fresh,
	// replayable dead-letter entry.
	entries, err := ListDeadLetters(townRoot, session)
	if err != nil {
		t.Fatalf("ListDeadLetters: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("ListDeadLetters = %#v, want none: the already-enqueued claim must not be resurrected as replayable", entries)
	}
	if _, err := os.Stat(claimPath); !os.IsNotExist(err) {
		t.Fatalf("stale claim file still exists after the sweep should have removed it (already enqueued): err=%v", err)
	}

	// Exactly one copy in the active queue — never a duplicate.
	drained, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(drained) != 1 {
		t.Fatalf("Drain got %d entries, want exactly 1 (no duplicate from the resurrected claim)", len(drained))
	}
}

// TestFreshReplayClaimIsNotSweptPrematurely is the false-case companion:
// a ".replaying" file younger than deadLetterReplayStaleThreshold (a
// genuinely in-flight replay, not a crashed one) must NOT be restored —
// doing so would let a second reader see and re-replay an entry the first
// replay is still in the middle of processing.
func TestFreshReplayClaimIsNotSweptPrematurely(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-fresh-replay-claim"

	n := QueuedNudge{ID: "in-flight", Sender: "test", Message: "still replaying", Timestamp: time.Now()}
	path, err := DeadLetter(townRoot, session, n, "nudge-poller", "boom", "", true)
	if err != nil {
		t.Fatalf("DeadLetter: %v", err)
	}
	claimPath := path + ".replaying"
	if err := os.Rename(path, claimPath); err != nil {
		t.Fatalf("claiming: %v", err)
	}
	// Freshly claimed — no Chtimes backdating.

	entries, err := ListDeadLetters(townRoot, session)
	if err != nil {
		t.Fatalf("ListDeadLetters: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("ListDeadLetters got %d entries, want 0 (a fresh in-flight replay claim must stay hidden)", len(entries))
	}

	dir := filepath.Dir(claimPath)
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	found := false
	for _, e := range dirEntries {
		if strings.HasSuffix(e.Name(), ".replaying") {
			found = true
		}
	}
	if !found {
		t.Fatalf("fresh .replaying claim file was removed/renamed by the sweep, want it left alone")
	}
}
