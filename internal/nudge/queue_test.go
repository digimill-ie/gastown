package nudge

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEnqueueAndDrain(t *testing.T) {
	townRoot := t.TempDir()

	session := "gt-gastown-crew-sean"
	n1 := QueuedNudge{
		Sender:   "mayor",
		Message:  "Check your hook",
		Priority: PriorityNormal,
	}
	n2 := QueuedNudge{
		Sender:   "gastown/witness",
		Message:  "Polecat alpha is stuck",
		Priority: PriorityUrgent,
	}

	// Enqueue two nudges
	if err := Enqueue(townRoot, session, n1); err != nil {
		t.Fatalf("Enqueue n1: %v", err)
	}
	// Small delay to ensure different timestamps
	time.Sleep(time.Millisecond)
	if err := Enqueue(townRoot, session, n2); err != nil {
		t.Fatalf("Enqueue n2: %v", err)
	}

	// Check pending count
	count, err := Pending(townRoot, session)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if count != 2 {
		t.Errorf("Pending = %d, want 2", count)
	}

	// Drain
	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 2 {
		t.Fatalf("Drain returned %d nudges, want 2", len(nudges))
	}

	// Verify FIFO order
	if nudges[0].Sender != "mayor" {
		t.Errorf("nudges[0].Sender = %q, want %q", nudges[0].Sender, "mayor")
	}
	if nudges[1].Sender != "gastown/witness" {
		t.Errorf("nudges[1].Sender = %q, want %q", nudges[1].Sender, "gastown/witness")
	}

	// After drain, pending should be 0
	count, err = Pending(townRoot, session)
	if err != nil {
		t.Fatalf("Pending after drain: %v", err)
	}
	if count != 0 {
		t.Errorf("Pending after drain = %d, want 0", count)
	}
}

func TestDrainEmptyQueue(t *testing.T) {
	townRoot := t.TempDir()

	nudges, err := Drain(townRoot, "nonexistent-session")
	if err != nil {
		t.Fatalf("Drain empty: %v", err)
	}
	if len(nudges) != 0 {
		t.Errorf("Drain empty returned %d nudges, want 0", len(nudges))
	}
}

func TestDrainSkipsMalformed(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test"

	// Create queue dir and a malformed file
	dir := filepath.Join(townRoot, ".runtime", "nudge_queue", session)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "100.json"), []byte("not json"), 0644); err != nil {
		t.Fatal(err)
	}

	// Enqueue a valid nudge (with later timestamp)
	n := QueuedNudge{
		Sender:    "test",
		Message:   "valid",
		Timestamp: time.Now().Add(time.Second),
	}
	if err := Enqueue(townRoot, session, n); err != nil {
		t.Fatal(err)
	}

	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 1 {
		t.Fatalf("got %d nudges, want 1 (malformed should be skipped)", len(nudges))
	}
	if nudges[0].Message != "valid" {
		t.Errorf("got message %q, want %q", nudges[0].Message, "valid")
	}

	// Malformed file should have been cleaned up (renamed to .claimed then removed)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("queue dir should be empty after drain, got %d entries: %v", len(entries), names)
	}
}

func TestFormatForInjection_Normal(t *testing.T) {
	nudges := []QueuedNudge{
		{Sender: "mayor", Message: "Check status", Priority: PriorityNormal},
	}
	output := FormatForInjection(nudges)

	if output == "" {
		t.Fatal("FormatForInjection returned empty string")
	}
	if !strings.Contains(output, "<system-reminder>") {
		t.Error("missing <system-reminder> tag")
	}
	if !strings.Contains(output, "background notification") {
		t.Error("normal nudges should mention background notification")
	}
	if strings.Contains(output, "URGENT") {
		t.Error("normal nudges should not contain URGENT")
	}
}

func TestFormatForInjection_Urgent(t *testing.T) {
	nudges := []QueuedNudge{
		{Sender: "witness", Message: "Polecat stuck", Priority: PriorityUrgent},
		{Sender: "mayor", Message: "FYI", Priority: PriorityNormal},
	}
	output := FormatForInjection(nudges)

	if !strings.Contains(output, "URGENT") {
		t.Error("should mention URGENT for urgent nudges")
	}
	if !strings.Contains(output, "Handle urgent") {
		t.Error("should instruct agent to handle urgent nudges")
	}
	if !strings.Contains(output, "non-urgent") {
		t.Error("should mention non-urgent nudges")
	}
}

func TestFormatForInjection_Empty(t *testing.T) {
	output := FormatForInjection(nil)
	if output != "" {
		t.Errorf("FormatForInjection(nil) = %q, want empty", output)
	}
}

func TestPendingNonexistentDir(t *testing.T) {
	count, err := Pending("/nonexistent/path", "session")
	if err != nil {
		t.Fatalf("Pending on nonexistent dir should not error: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
}

func TestEnqueueDefaults(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-defaults"

	// Enqueue with zero timestamp and empty priority — should get defaults
	n := QueuedNudge{
		Sender:  "test",
		Message: "hello",
	}
	if err := Enqueue(townRoot, session, n); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 1 {
		t.Fatalf("got %d nudges, want 1", len(nudges))
	}
	if nudges[0].Priority != PriorityNormal {
		t.Errorf("Priority = %q, want %q", nudges[0].Priority, PriorityNormal)
	}
	if nudges[0].Timestamp.IsZero() {
		t.Error("Timestamp should have been set to non-zero default")
	}
	if nudges[0].ExpiresAt.IsZero() {
		t.Error("ExpiresAt should have been set to non-zero default")
	}
	// Normal priority should get DefaultNormalTTL
	expectedExpiry := nudges[0].Timestamp.Add(DefaultNormalTTL)
	if !nudges[0].ExpiresAt.Equal(expectedExpiry) {
		t.Errorf("ExpiresAt = %v, want %v (Timestamp + DefaultNormalTTL)", nudges[0].ExpiresAt, expectedExpiry)
	}
}

func TestEnqueueUrgentTTL(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-urgent-ttl"

	n := QueuedNudge{
		Sender:   "test",
		Message:  "urgent message",
		Priority: PriorityUrgent,
	}
	if err := Enqueue(townRoot, session, n); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 1 {
		t.Fatalf("got %d nudges, want 1", len(nudges))
	}
	// Urgent priority should get DefaultUrgentTTL
	expectedExpiry := nudges[0].Timestamp.Add(DefaultUrgentTTL)
	if !nudges[0].ExpiresAt.Equal(expectedExpiry) {
		t.Errorf("ExpiresAt = %v, want %v (Timestamp + DefaultUrgentTTL)", nudges[0].ExpiresAt, expectedExpiry)
	}
}

func TestEnqueueCustomExpiry(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-custom-expiry"

	customExpiry := time.Now().Add(5 * time.Minute)
	n := QueuedNudge{
		Sender:    "test",
		Message:   "custom expiry",
		ExpiresAt: customExpiry,
	}
	if err := Enqueue(townRoot, session, n); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 1 {
		t.Fatalf("got %d nudges, want 1", len(nudges))
	}
	// Custom expiry should be preserved, not overwritten by default TTL
	if !nudges[0].ExpiresAt.Equal(customExpiry) {
		t.Errorf("ExpiresAt = %v, want %v (custom)", nudges[0].ExpiresAt, customExpiry)
	}
}

func TestDrainSkipsExpired(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-expired"

	// Enqueue an already-expired nudge
	expired := QueuedNudge{
		Sender:    "old-sender",
		Message:   "stale message",
		Timestamp: time.Now().Add(-time.Hour),
		ExpiresAt: time.Now().Add(-30 * time.Minute), // expired 30 min ago
	}
	if err := Enqueue(townRoot, session, expired); err != nil {
		t.Fatalf("Enqueue expired: %v", err)
	}

	// Enqueue a fresh nudge
	time.Sleep(time.Millisecond)
	fresh := QueuedNudge{
		Sender:  "new-sender",
		Message: "fresh message",
	}
	if err := Enqueue(townRoot, session, fresh); err != nil {
		t.Fatalf("Enqueue fresh: %v", err)
	}

	// Pending counts both (doesn't check expiry)
	pending, err := Pending(townRoot, session)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending != 2 {
		t.Errorf("Pending = %d, want 2 (counts all files)", pending)
	}

	// Drain should skip the expired nudge
	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 1 {
		t.Fatalf("Drain returned %d nudges, want 1 (expired should be skipped)", len(nudges))
	}
	if nudges[0].Sender != "new-sender" {
		t.Errorf("got sender %q, want %q", nudges[0].Sender, "new-sender")
	}

	// After drain, queue dir should be empty (both files removed)
	dir := filepath.Join(townRoot, ".runtime", "nudge_queue", session)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("queue dir should be empty after drain, got %d entries", len(entries))
	}
}

func TestEnqueueQueueDepthLimit(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-depth"

	// Fill the queue to MaxQueueDepth
	for i := 0; i < MaxQueueDepth; i++ {
		n := QueuedNudge{
			Sender:  "sender",
			Message: "msg",
		}
		if err := Enqueue(townRoot, session, n); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}

	// Next enqueue should fail
	overflow := QueuedNudge{
		Sender:  "sender",
		Message: "overflow",
	}
	err := Enqueue(townRoot, session, overflow)
	if err == nil {
		t.Fatal("expected error when queue is full")
	}
	if !strings.Contains(err.Error(), "is full") {
		t.Errorf("got error %q, want to contain 'is full'", err.Error())
	}

	// Verify pending count is at max
	pending, _ := Pending(townRoot, session)
	if pending != MaxQueueDepth {
		t.Errorf("Pending = %d, want %d", pending, MaxQueueDepth)
	}

	// After draining, enqueue should work again
	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != MaxQueueDepth {
		t.Errorf("Drain returned %d, want %d", len(nudges), MaxQueueDepth)
	}

	err = Enqueue(townRoot, session, overflow)
	if err != nil {
		t.Errorf("Enqueue after drain should succeed: %v", err)
	}
}

func TestDrainSweepsOrphanedClaims(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-orphans"

	dir := filepath.Join(townRoot, ".runtime", "nudge_queue", session)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create an orphaned .claimed file with old mod time
	// Claim files now use the format: <original>.json.claimed.<suffix>
	orphanPath := filepath.Join(dir, "100.json.claimed.deadbeef")
	if err := os.WriteFile(orphanPath, []byte(`{"sender":"ghost"}`), 0644); err != nil {
		t.Fatal(err)
	}
	// Set mod time to well past the stale threshold
	oldTime := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(orphanPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	// Create a fresh .claimed file (should NOT be swept)
	freshClaimPath := filepath.Join(dir, "200.json.claimed.cafebabe")
	if err := os.WriteFile(freshClaimPath, []byte(`{"sender":"active"}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Enqueue a valid nudge
	n := QueuedNudge{Sender: "test", Message: "valid"}
	if err := Enqueue(townRoot, session, n); err != nil {
		t.Fatal(err)
	}

	// First Drain: requeues the orphaned claim (rename .claimed → .json),
	// keeps the fresh claim, and returns the valid nudge.
	// The requeued file isn't in the current ReadDir snapshot, so it's
	// picked up on the next Drain call.
	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 1 {
		t.Fatalf("first Drain got %d nudges, want 1", len(nudges))
	}
	if nudges[0].Message != "valid" {
		t.Errorf("got message %q, want %q", nudges[0].Message, "valid")
	}

	// The orphaned .claimed file should have been requeued as .json
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Error("orphaned .claimed file should no longer exist (requeued to .json)")
	}
	// Restored path strips everything from ".claimed" onward
	restoredPath := filepath.Join(dir, "100.json")
	if _, err := os.Stat(restoredPath); os.IsNotExist(err) {
		t.Error("restored .json file should exist after requeue")
	}

	// Second Drain: picks up the requeued orphan
	nudges2, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("second Drain: %v", err)
	}
	if len(nudges2) != 1 {
		t.Fatalf("second Drain got %d nudges, want 1 (the requeued orphan)", len(nudges2))
	}
	if nudges2[0].Sender != "ghost" {
		t.Errorf("got sender %q, want %q", nudges2[0].Sender, "ghost")
	}

	// The fresh claim should still exist (not old enough to sweep)
	if _, err := os.Stat(freshClaimPath); os.IsNotExist(err) {
		t.Error("fresh .claimed file should NOT have been swept")
	}
}

// TestDrainClaims_UnackedClaimSurvivesACrash is REVISION 3's required test
// for fix 3 (claim-then-deliver-then-delete ordering): kill the caller
// between drain and its dead-letter/requeue write, and show the entry
// still exists in one of the two places. Previously (queue.go:305) Drain
// deleted the claimed file as soon as it was unmarshaled, before the caller
// even attempted delivery, so a crash right here lost the entry with no
// durable trace anywhere (codex, changes-requested at 08964387).
//
// DrainClaims closes that window: it claims the entry but does not delete
// it, so a caller that crashes before calling Ack leaves the original
// .claimed file in place — recoverable first by direct inspection, and
// second, once staleClaimThreshold has passed, by a future
// Drain/DrainClaims call's orphan sweep restoring it to the pending queue.
func TestDrainClaims_UnackedClaimSurvivesACrash(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-crash-test"

	if err := Enqueue(townRoot, session, QueuedNudge{Sender: "s", Message: "must survive a crash"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	claims, err := DrainClaims(townRoot, session)
	if err != nil {
		t.Fatalf("DrainClaims: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("DrainClaims got %d claims, want 1", len(claims))
	}
	if claims[0].Nudge.Message != "must survive a crash" {
		t.Fatalf("claimed nudge = %#v", claims[0].Nudge)
	}

	// Simulate a crash right here: the caller never Acks, never
	// dead-letters, never requeues. Place 1: the claim file must still be
	// on disk, not deleted by DrainClaims itself.
	dir := queueDir(townRoot, session)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var claimFiles int
	for _, e := range entries {
		if strings.Contains(e.Name(), ".claimed") {
			claimFiles++
			// Age it past staleClaimThreshold so the next Drain's orphan
			// sweep restores it, proving place 2: recovery via the queue.
			old := time.Now().Add(-staleClaimThreshold - time.Minute)
			if err := os.Chtimes(filepath.Join(dir, e.Name()), old, old); err != nil {
				t.Fatalf("Chtimes: %v", err)
			}
		}
	}
	if claimFiles != 1 {
		t.Fatalf("expected 1 claimed file surviving an unacked DrainClaims, got %d", claimFiles)
	}

	// The first post-crash Drain only restores the orphan (.claimed -> .json)
	// — it isn't in that call's own directory snapshot, so it can't be
	// delivered in the same call (see TestDrainSweepsOrphanedClaims). A
	// second Drain picks it up.
	firstRecovery, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("first recovery Drain: %v", err)
	}
	if len(firstRecovery) != 0 {
		t.Fatalf("first recovery Drain got %d entries, want 0 (orphan is only restored, not yet delivered)", len(firstRecovery))
	}

	recovered, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("second recovery Drain: %v", err)
	}
	if len(recovered) != 1 || recovered[0].Message != "must survive a crash" {
		t.Fatalf("orphaned claim was not recovered intact: %#v", recovered)
	}
}

// TestClaimAck_RemovesUnderlyingFile is a direct unit test of Claim.Ack:
// once a caller has durably resolved an entry's outcome, Ack must remove
// the now-redundant original claim file, and calling it again (idempotent
// double-ack, e.g. after a partial failure and retry) must not error.
func TestClaimAck_RemovesUnderlyingFile(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-ack-test"

	if err := Enqueue(townRoot, session, QueuedNudge{Sender: "s", Message: "ack me"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claims, err := DrainClaims(townRoot, session)
	if err != nil {
		t.Fatalf("DrainClaims: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("got %d claims, want 1", len(claims))
	}

	if err := claims[0].Ack(); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if err := claims[0].Ack(); err != nil {
		t.Fatalf("second Ack (idempotent) errored: %v", err)
	}

	dir := queueDir(townRoot, session)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".claimed") {
			t.Errorf("claim file %q still present after Ack", e.Name())
		}
	}
}

// TestDrain_MatchesDrainClaimsThenAck asserts Drain's public contract is
// unchanged by its DrainClaims-based reimplementation: immediate delete,
// same payload, same behavior every existing Drain caller (the
// turn-boundary hook, internal/acp/propulsion.go) already depends on.
func TestDrain_MatchesDrainClaimsThenAck(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-drain-contract-test"

	if err := Enqueue(townRoot, session, QueuedNudge{Sender: "s", Message: "m1"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := Enqueue(townRoot, session, QueuedNudge{Sender: "s", Message: "m2"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 2 {
		t.Fatalf("Drain got %d nudges, want 2", len(nudges))
	}

	dir := queueDir(townRoot, session)
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("Drain left %d files behind, want 0 (immediate delete on success)", len(entries))
	}
}

func TestConcurrentEnqueueNoDuplicateLoss(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-concurrent"

	// Fire 20 concurrent enqueues — all should succeed without collision.
	const count = 20
	var wg sync.WaitGroup
	errs := make(chan error, count)

	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n := QueuedNudge{
				Sender:  "sender",
				Message: strings.Repeat("x", i+1), // unique per goroutine
			}
			if err := Enqueue(townRoot, session, n); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent Enqueue failed: %v", err)
	}

	// All 20 should be pending
	pending, err := Pending(townRoot, session)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending != count {
		t.Errorf("Pending = %d, want %d (some nudges lost to collision?)", pending, count)
	}

	// Drain should return all 20
	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != count {
		t.Errorf("Drain returned %d, want %d", len(nudges), count)
	}
}

// --- DeliverAfter tests ---

// TestDrainSkipsDeferredNudge verifies that a nudge with a future DeliverAfter
// is not returned by Drain and remains in the queue.
func TestDrainSkipsDeferredNudge(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-deferred"

	deferred := QueuedNudge{
		Sender:       "system",
		Message:      "reply reminder",
		DeliverAfter: time.Now().Add(10 * time.Second), // far future
	}
	if err := Enqueue(townRoot, session, deferred); err != nil {
		t.Fatalf("Enqueue deferred: %v", err)
	}

	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 0 {
		t.Fatalf("Drain returned %d nudges, want 0 (deferred not ready)", len(nudges))
	}

	// File should still be in queue (not discarded)
	pending, err := Pending(townRoot, session)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending != 1 {
		t.Errorf("Pending = %d, want 1 (deferred nudge still in queue)", pending)
	}
}

// TestDrainDeliversDeferredNudgeWhenReady verifies that a nudge with a past
// DeliverAfter is delivered normally.
func TestDrainDeliversDeferredNudgeWhenReady(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-deferred-ready"

	ready := QueuedNudge{
		Sender:       "system",
		Message:      "reply reminder",
		DeliverAfter: time.Now().Add(-1 * time.Second), // already past
	}
	if err := Enqueue(townRoot, session, ready); err != nil {
		t.Fatalf("Enqueue ready: %v", err)
	}

	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 1 {
		t.Fatalf("Drain returned %d nudges, want 1 (deferred is ready)", len(nudges))
	}
	if nudges[0].Message != "reply reminder" {
		t.Errorf("got message %q, want %q", nudges[0].Message, "reply reminder")
	}
}

// TestDrainMixedDeferredAndReady verifies that only ready nudges are returned
// when a mix of deferred and immediately-deliverable nudges are queued.
func TestDrainMixedDeferredAndReady(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-mixed-deferred"

	// Enqueue: immediate, then deferred, then immediate (interleaved order).
	n1 := QueuedNudge{Sender: "mayor", Message: "immediate-1"}
	if err := Enqueue(townRoot, session, n1); err != nil {
		t.Fatalf("Enqueue n1: %v", err)
	}
	time.Sleep(time.Millisecond)

	deferred := QueuedNudge{
		Sender:       "system",
		Message:      "deferred",
		DeliverAfter: time.Now().Add(60 * time.Second),
	}
	if err := Enqueue(townRoot, session, deferred); err != nil {
		t.Fatalf("Enqueue deferred: %v", err)
	}
	time.Sleep(time.Millisecond)

	n2 := QueuedNudge{Sender: "witness", Message: "immediate-2"}
	if err := Enqueue(townRoot, session, n2); err != nil {
		t.Fatalf("Enqueue n2: %v", err)
	}

	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 2 {
		t.Fatalf("Drain returned %d nudges, want 2 (deferred stays in queue)", len(nudges))
	}
	if nudges[0].Message != "immediate-1" {
		t.Errorf("nudges[0].Message = %q, want %q", nudges[0].Message, "immediate-1")
	}
	if nudges[1].Message != "immediate-2" {
		t.Errorf("nudges[1].Message = %q, want %q", nudges[1].Message, "immediate-2")
	}

	// Deferred nudge remains in queue
	pending, err := Pending(townRoot, session)
	if err != nil {
		t.Fatalf("Pending after drain: %v", err)
	}
	if pending != 1 {
		t.Errorf("Pending = %d, want 1 (deferred nudge still in queue)", pending)
	}
}

func TestRemoveKindByThread(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-remove"

	keep := QueuedNudge{Sender: "system", Message: "keep", Kind: "mail", ThreadID: "thread-1"}
	removeA := QueuedNudge{Sender: "system", Message: "remove-a", Kind: "reply-reminder", ThreadID: "thread-1"}
	removeB := QueuedNudge{Sender: "system", Message: "remove-b", Kind: "reply-reminder", ThreadID: "thread-1"}
	otherThread := QueuedNudge{Sender: "system", Message: "other-thread", Kind: "reply-reminder", ThreadID: "thread-2"}

	for _, n := range []QueuedNudge{keep, removeA, removeB, otherThread} {
		if err := Enqueue(townRoot, session, n); err != nil {
			t.Fatalf("Enqueue(%q): %v", n.Message, err)
		}
		time.Sleep(time.Millisecond)
	}

	removed, err := RemoveKindByThread(townRoot, session, "reply-reminder", "thread-1")
	if err != nil {
		t.Fatalf("RemoveKindByThread: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}

	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 2 {
		t.Fatalf("Drain returned %d nudges, want 2", len(nudges))
	}
	if nudges[0].Message != "keep" {
		t.Fatalf("nudges[0].Message = %q, want %q", nudges[0].Message, "keep")
	}
	if nudges[1].Message != "other-thread" {
		t.Fatalf("nudges[1].Message = %q, want %q", nudges[1].Message, "other-thread")
	}
}

// TestDeferredNudgeDeliveredAfterDelay uses a very short DeliverAfter to confirm
// that the same nudge is skipped on first Drain and delivered on a second Drain
// after the deadline elapses.
func TestDeferredNudgeDeliveredAfterDelay(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-deferred-sequence"

	shortDelay := QueuedNudge{
		Sender:       "system",
		Message:      "reply via mail",
		DeliverAfter: time.Now().Add(50 * time.Millisecond),
	}
	if err := Enqueue(townRoot, session, shortDelay); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// First Drain: not ready yet.
	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("first Drain: %v", err)
	}
	if len(nudges) != 0 {
		t.Fatalf("first Drain: got %d nudges, want 0 (deferred not ready)", len(nudges))
	}

	// Wait for deadline.
	time.Sleep(60 * time.Millisecond)

	// Second Drain: ready now.
	nudges, err = Drain(townRoot, session)
	if err != nil {
		t.Fatalf("second Drain: %v", err)
	}
	if len(nudges) != 1 {
		t.Fatalf("second Drain: got %d nudges, want 1 (deferred now ready)", len(nudges))
	}
	if nudges[0].Message != "reply via mail" {
		t.Errorf("got message %q, want %q", nudges[0].Message, "reply via mail")
	}

	// Queue should now be empty.
	pending, err := Pending(townRoot, session)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending != 0 {
		t.Errorf("Pending = %d, want 0 (deferred nudge delivered)", pending)
	}
}

// TestZeroDeliverAfterIsImmediate verifies that a zero DeliverAfter (unset)
// is treated as immediately deliverable (not deferred).
func TestZeroDeliverAfterIsImmediate(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-zero-deliver-after"

	n := QueuedNudge{
		Sender:  "mayor",
		Message: "no delay",
		// DeliverAfter intentionally left zero
	}
	if err := Enqueue(townRoot, session, n); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 1 {
		t.Fatalf("got %d nudges, want 1 (zero DeliverAfter = immediate)", len(nudges))
	}
}

func TestConcurrentDrainNoDoubleDeli(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-drain-race"

	// Enqueue 10 nudges
	const count = 10
	for i := 0; i < count; i++ {
		n := QueuedNudge{
			Sender:  "sender",
			Message: strings.Repeat("m", i+1),
		}
		if err := Enqueue(townRoot, session, n); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
		time.Sleep(time.Millisecond) // ensure ordering
	}

	// Race 5 concurrent Drains — total nudges collected should equal count.
	const drainers = 5
	var wg sync.WaitGroup
	results := make(chan []QueuedNudge, drainers)

	for i := 0; i < drainers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			nudges, err := Drain(townRoot, session)
			if err != nil {
				t.Errorf("concurrent Drain: %v", err)
				return
			}
			results <- nudges
		}()
	}
	wg.Wait()
	close(results)

	total := 0
	for nudges := range results {
		total += len(nudges)
	}

	// On Windows, transient sharing violations (antivirus, search indexer)
	// can prevent all concurrent drainers from claiming a file.  The nudge
	// stays as .json and is picked up on the next Drain — mirror that here
	// with a straggler sweep so the test validates no-loss, not one-shot
	// completeness.
	for retries := 0; retries < 3 && total < count; retries++ {
		time.Sleep(50 * time.Millisecond)
		stragglers, err := Drain(townRoot, session)
		if err != nil {
			t.Fatalf("straggler Drain: %v", err)
		}
		total += len(stragglers)
	}

	if total != count {
		t.Errorf("concurrent Drains delivered %d total nudges, want exactly %d (double-delivery or loss)", total, count)
	}

	// Verify no double-delivery: total must be exactly count, not more.
	if total > count {
		t.Errorf("double delivery detected: got %d total nudges, want exactly %d", total, count)
	}
}

// TestConcurrentDrainOldEntryNoDoubleDeliOrphanRace covers High 3
// (queue.go:430): DrainClaims used to reset a newly-claimed file's mtime
// AFTER the rename, leaving a window where the file was already visible
// under its .claimed name but still carried its old, pre-claim mtime. A
// pending entry that had simply been sitting in the queue longer than
// staleClaimThreshold (routine for a normal 30-minute-TTL nudge, not a
// bug) would read as an orphaned claim the INSTANT a concurrent
// DrainClaims call observed it in that window, restoring it to pending
// and handing it to a second, concurrent claimer while the first was
// still mid-delivery. The fix resets the mtime on the ORIGINAL path
// before the rename, so the file is never visible under any .claimed name
// with a stale mtime. Backdates ONE entry well past staleClaimThreshold
// and races many concurrent DrainClaims calls against it.
func TestConcurrentDrainOldEntryNoDoubleDeliOrphanRace(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-old-entry-race"

	if err := Enqueue(townRoot, session, QueuedNudge{Sender: "test", Message: "long-waiting"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	dir := filepath.Join(townRoot, ".runtime", "nudge_queue", session)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ReadDir: entries=%d err=%v", len(entries), err)
	}
	oldTime := time.Now().Add(-10 * time.Minute) // well past staleClaimThreshold (5m)
	path := filepath.Join(dir, entries[0].Name())
	if err := os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	const drainers = 20
	var wg sync.WaitGroup
	var claimedCount int32
	start := make(chan struct{})
	for i := 0; i < drainers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			claims, err := DrainClaims(townRoot, session)
			if err != nil {
				t.Errorf("concurrent DrainClaims: %v", err)
				return
			}
			atomic.AddInt32(&claimedCount, int32(len(claims)))
			for _, c := range claims {
				if ackErr := c.Ack(); ackErr != nil {
					t.Errorf("Ack: %v", ackErr)
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	if claimedCount != 1 {
		t.Fatalf("concurrent DrainClaims claimed the entry %d times, want exactly 1 (an old-but-pending entry must not be re-surfaced as orphaned the instant it is claimed)", claimedCount)
	}
}

// TestEnqueueAssignsID covers hq-g52db: every enqueued nudge gets a stable
// identity so failed-delivery attempts can be correlated across the poller,
// the idle watcher, and a poller restart even though each requeue writes a
// new filename.
func TestEnqueueAssignsID(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-id"

	if err := Enqueue(townRoot, session, QueuedNudge{Sender: "test", Message: "hello"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 1 {
		t.Fatalf("got %d nudges, want 1", len(nudges))
	}
	if nudges[0].ID == "" {
		t.Error("Enqueue did not assign an ID to a nudge with none set")
	}
}

// TestEnqueuePreservesExplicitID covers Requeue's contract: an already-set ID
// (as Requeue passes through from a drained entry) must not be overwritten
// with a new one, or attempts recorded against the original ID (e.g. in a
// dead-letter record) would no longer match the requeued entry.
func TestEnqueuePreservesExplicitID(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-id-preserved"

	if err := Enqueue(townRoot, session, QueuedNudge{ID: "fixed-id-123", Sender: "test", Message: "hello"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 1 {
		t.Fatalf("got %d nudges, want 1", len(nudges))
	}
	if nudges[0].ID != "fixed-id-123" {
		t.Errorf("ID = %q, want %q (Enqueue must not overwrite an explicit ID)", nudges[0].ID, "fixed-id-123")
	}
}

// TestDrainClaims_AssignsIDToLegacyEntryWithoutOne covers High 4
// (queue.go:288): a file queued by a version of this code before the ID
// field existed (or otherwise missing one) deserializes with ID == "".
// AckClaims' skip map (see its doc) only ever adds a NON-empty ID, so a
// claim that still carried an empty ID by the time it reached AckClaims
// could never be recognized as unresolved — it would be acked (deleted)
// even after a failed requeue/dead-letter write. DrainClaims must assign
// (and persist) an ID at claim time so this can never happen.
func TestDrainClaims_AssignsIDToLegacyEntryWithoutOne(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-legacy-no-id"

	dir := filepath.Join(townRoot, ".runtime", "nudge_queue", session)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	// A pre-ID-field queue entry: no "id" key at all.
	legacyPath := filepath.Join(dir, "100-legacy.json")
	if err := os.WriteFile(legacyPath, []byte(`{"sender":"ghost","message":"pre-ID entry","priority":"normal"}`), 0644); err != nil {
		t.Fatal(err)
	}

	claims, err := DrainClaims(townRoot, session)
	if err != nil {
		t.Fatalf("DrainClaims: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("DrainClaims got %d claims, want 1", len(claims))
	}
	if claims[0].Nudge.ID == "" {
		t.Fatal("DrainClaims left the claim's ID empty — AckClaims can never recognize this entry as unresolved")
	}

	// The assigned ID must be PERSISTED, not just held in memory: simulate a
	// crash-and-restore by reading it straight off disk. It lives in a
	// companion sidecar (see idSidecarPath), never written into the claim's
	// own file — that in-place rewrite is exactly the tmp-then-rename
	// hazard this package no longer uses (see restoreOrphanedClaim).
	onDiskID, ok := readIDSidecar(claims[0].path)
	if !ok {
		t.Fatal("assigned ID sidecar missing — a crash before resolution would lose the assigned ID")
	}
	if onDiskID != claims[0].Nudge.ID {
		t.Errorf("sidecar ID = %q, in-memory ID = %q: the assigned ID must be persisted", onDiskID, claims[0].Nudge.ID)
	}

	// Reproduce the actual failure mode: a caller that decides this
	// entry's outcome did NOT durably land anywhere (its ID appears in
	// unresolved) must be able to leave its claim un-acked.
	AckClaims(claims, []QueuedNudge{claims[0].Nudge}, func(c Claim, ackErr error) {
		t.Errorf("unexpected ack error: %v", ackErr)
	})
	if _, err := os.Stat(claims[0].path); os.IsNotExist(err) {
		t.Fatal("AckClaims deleted a claim named in unresolved — the empty-ID matching gap let it through")
	}
}

// TestPollerRestart_RecoversAndDeliversExactlyOnce is one of the six
// REVISION-3-required tests codex found not covered: a poller restart mid-
// entry. TestDrainClaims_UnackedClaimSurvivesACrash (line ~467) checks only
// that the claim survives and is RESTORED to the queue — codex named that
// PARTIAL, because it stops short of a restarted consumer actually
// completing delivery (codex: "queue_test.go:467 checks retention, not a
// restarted consumer"). This test carries the same crash scenario all the
// way through: MarkAttempt (the durable pre-injection persist a real
// poller performs — see Claim.MarkAttempt), a simulated crash with no Ack,
// staleness-driven recovery, and a second "poller instance" that
// successfully claims, sees the CORRECT persisted Attempts count, and
// completes delivery. The payload is checked to have been claimable
// exactly once at every step — never twice, never zero.
func TestPollerRestart_RecoversAndDeliversExactlyOnce(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-restart-test"

	if err := Enqueue(townRoot, session, QueuedNudge{Sender: "s", Message: "resume"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// First "poller instance": drains, persists an incremented Attempts
	// count as it would just before a live tmux injection attempt (Claim.
	// MarkAttempt), then crashes before Ack.
	claims1, err := DrainClaims(townRoot, session)
	if err != nil || len(claims1) != 1 {
		t.Fatalf("first DrainClaims: claims=%d err=%v", len(claims1), err)
	}
	if err := claims1[0].MarkAttempt(); err != nil {
		t.Fatalf("MarkAttempt: %v", err)
	}
	// ... crash here: no Ack, no dead-letter, no requeue ...

	// Nothing is deliverable yet — the entry is claimed, not queued or
	// stale, so a second drainer must not see it (exactly-once, part 1).
	if pending, _ := Pending(townRoot, session); pending != 0 {
		t.Fatalf("Pending = %d, want 0 (entry is claimed, not queued)", pending)
	}
	tooSoon, err := DrainClaims(townRoot, session)
	if err != nil {
		t.Fatalf("DrainClaims before staleness: %v", err)
	}
	if len(tooSoon) != 0 {
		t.Fatalf("DrainClaims before staleness got %d claims, want 0 (still owned by the crashed attempt)", len(tooSoon))
	}

	// Age the crashed claim past staleClaimThreshold, simulating enough
	// wall-clock time for a restarted poller to treat it as orphaned.
	dir := queueDir(townRoot, session)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".claimed") {
			old := time.Now().Add(-staleClaimThreshold - time.Minute)
			if err := os.Chtimes(filepath.Join(dir, e.Name()), old, old); err != nil {
				t.Fatalf("Chtimes: %v", err)
			}
		}
	}

	// "Restarted poller": the orphan sweep restores the entry on this call
	// (not yet delivered in the same call — see TestDrainSweepsOrphanedClaims),
	// so nothing is claimed here either (exactly-once, part 2: the sweep
	// itself never double-delivers).
	sweepOnly, err := DrainClaims(townRoot, session)
	if err != nil {
		t.Fatalf("post-restart DrainClaims (sweep only): %v", err)
	}
	if len(sweepOnly) != 0 {
		t.Fatalf("post-restart sweep call delivered %d claims, want 0", len(sweepOnly))
	}

	// A second call after the sweep actually claims and would deliver it.
	claims2, err := DrainClaims(townRoot, session)
	if err != nil || len(claims2) != 1 {
		t.Fatalf("post-restart redelivery: claims=%d err=%v", len(claims2), err)
	}
	if claims2[0].Nudge.Message != "resume" {
		t.Fatalf("recovered nudge = %#v", claims2[0].Nudge)
	}
	// The persisted Attempts count survived the crash: the restarted
	// poller sees 1 (from the crashed attempt), not a fresh 0 — this is
	// what bounds MaxInjectionAttempts across a restart instead of
	// resetting the budget every time a poller dies mid-injection.
	if claims2[0].Nudge.Attempts != 1 {
		t.Fatalf("Attempts after restart = %d, want 1 (persisted from the crashed attempt via MarkAttempt)", claims2[0].Nudge.Attempts)
	}

	// This time delivery succeeds: ack it.
	if err := claims2[0].Ack(); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	// Exactly once, part 3: nothing left to deliver anywhere, ever again.
	if pending, _ := Pending(townRoot, session); pending != 0 {
		t.Fatalf("Pending after ack = %d, want 0", pending)
	}
	final, err := DrainClaims(townRoot, session)
	if err != nil {
		t.Fatalf("final DrainClaims: %v", err)
	}
	if len(final) != 0 {
		t.Fatalf("final DrainClaims got %d claims, want 0 (delivered exactly once)", len(final))
	}
}

// TestRequeuePreservesAttemptsAndID covers hq-g52db fix 2: the attempt count
// must persist across a requeue (which is what happens on every poller
// restart, since it is read back from the on-disk queue file) so retries are
// bounded across restarts, not reset to zero each time.
func TestRequeuePreservesAttemptsAndID(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-requeue-attempts"

	drained := []QueuedNudge{
		{ID: "abc", Sender: "test", Message: "hello", Attempts: 1, LastError: "boom", Timestamp: time.Now()},
	}
	if failed, err := RequeueTracked(townRoot, session, drained); err != nil {
		t.Fatalf("RequeueTracked: %v (failed=%v)", err, failed)
	}

	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 1 {
		t.Fatalf("got %d nudges, want 1", len(nudges))
	}
	if nudges[0].ID != "abc" {
		t.Errorf("ID = %q, want %q", nudges[0].ID, "abc")
	}
	if nudges[0].Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", nudges[0].Attempts)
	}
	if nudges[0].LastError != "boom" {
		t.Errorf("LastError = %q, want %q", nudges[0].LastError, "boom")
	}
}

// TestMarkAttempt_SidecarIsAppendOnlyAndNeverRewritesClaim covers the
// REQUIRED SHAPE for gtn-81j: Attempts must be recorded in an append-only
// sidecar file — one byte per attempt, count = size — that is never
// rewritten and never renamed, and the claim's own file must never be
// touched by MarkAttempt at all.
func TestMarkAttempt_SidecarIsAppendOnlyAndNeverRewritesClaim(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-attempts-sidecar"

	if err := Enqueue(townRoot, session, QueuedNudge{Sender: "s", Message: "m"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claims, err := DrainClaims(townRoot, session)
	if err != nil || len(claims) != 1 {
		t.Fatalf("DrainClaims: claims=%d err=%v", len(claims), err)
	}
	claim := claims[0]

	claimBefore, err := os.ReadFile(claim.path)
	if err != nil {
		t.Fatalf("ReadFile claim (before): %v", err)
	}

	for i := 1; i <= 3; i++ {
		if err := claim.MarkAttempt(); err != nil {
			t.Fatalf("MarkAttempt #%d: %v", i, err)
		}
		if claim.Nudge.Attempts != i {
			t.Fatalf("in-memory Attempts after MarkAttempt #%d = %d, want %d", i, claim.Nudge.Attempts, i)
		}
		if !claim.Nudge.InFlight {
			t.Fatalf("in-memory InFlight after MarkAttempt #%d = false, want true", i)
		}

		sidecarPath := attemptsSidecarPath(claim.path)
		info, err := os.Stat(sidecarPath)
		if err != nil {
			t.Fatalf("stat attempts sidecar after MarkAttempt #%d: %v", i, err)
		}
		if info.Size() != int64(i) {
			t.Fatalf("attempts sidecar size after MarkAttempt #%d = %d, want %d", i, info.Size(), i)
		}

		// The claim's own file must be byte-for-byte unchanged: MarkAttempt
		// records the attempt entirely in the sidecar, never by rewriting
		// the claim (the old Claim.Persist did exactly that, and a partial
		// rewrite there is the regression this replaces).
		claimNow, err := os.ReadFile(claim.path)
		if err != nil {
			t.Fatalf("ReadFile claim (after #%d): %v", i, err)
		}
		if string(claimNow) != string(claimBefore) {
			t.Fatalf("claim file content changed after MarkAttempt #%d: MarkAttempt must never rewrite the claim's own file", i)
		}
	}
}

// TestOrphanSweep_StaleTmpSiblingNeverClobbersIntactClaim reproduces the
// High regression at a752f4a7 (codex gate verdict, gtn-81j): the old
// Claim.Persist wrote "<claim>.tmp" then renamed it OVER the claim's own
// ".claimed.<suffix>" file. A partial write (e.g. a full disk) left a
// truncated ".tmp" sibling whose name ALSO contained ".claimed" — so the
// orphan sweep's substring match treated it as a second, independent
// orphaned claim of the SAME entry. Directory entries sort lexically, so
// the (shorter) intact claim name sorts before its own "<name>.tmp"
// sibling: the intact one restored first, and the truncated .tmp restored
// SECOND, overwriting it with garbage. The next drain then deleted the
// result as malformed — the only copy of the message was lost.
//
// This test manufactures exactly that disk shape — an intact, attempted
// claim plus a stray, garbage ".claimed...tmp" sibling, both aged past
// staleness — and asserts the entry survives, in exactly one place, with
// its content and attempt count intact. It must go RED if the orphan sweep
// loses its isClaimSidecarOrTemp exclusion (e.g. if the old Claim.Persist,
// and the un-excluded ".claimed" substring match it relied on, are
// restored).
func TestOrphanSweep_StaleTmpSiblingNeverClobbersIntactClaim(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-tmp-sibling-crash"

	if err := Enqueue(townRoot, session, QueuedNudge{Sender: "s", Message: "do not lose me"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	claims, err := DrainClaims(townRoot, session)
	if err != nil || len(claims) != 1 {
		t.Fatalf("DrainClaims: claims=%d err=%v", len(claims), err)
	}
	claim := claims[0]

	if err := claim.MarkAttempt(); err != nil {
		t.Fatalf("MarkAttempt: %v", err)
	}

	// Manufacture the crash artifact: a truncated sibling sharing the
	// claim's ".claimed.<suffix>" prefix, exactly the shape a partial write
	// through the old tmp-then-rename Persist left behind.
	garbage := claim.path + ".tmp"
	if err := os.WriteFile(garbage, []byte("{truncat"), 0644); err != nil {
		t.Fatalf("writing garbage sibling: %v", err)
	}

	// Age both the intact claim and the garbage sibling past staleness, so
	// this call's orphan sweep considers both.
	dir := queueDir(townRoot, session)
	old := time.Now().Add(-staleClaimThreshold - time.Minute)
	for _, p := range []string{claim.path, garbage} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatalf("Chtimes %s: %v", p, err)
		}
	}

	// The orphan sweep runs inside DrainClaims.
	if _, err := DrainClaims(townRoot, session); err != nil {
		t.Fatalf("DrainClaims (sweep): %v", err)
	}

	// The entry must exist in EXACTLY one place: a single pending .json
	// entry — never zero (lost) and never duplicated.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var jsonFiles []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			jsonFiles = append(jsonFiles, e.Name())
		}
	}
	if len(jsonFiles) != 1 {
		t.Fatalf("pending .json entries after sweep = %v, want exactly 1", jsonFiles)
	}

	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 1 {
		t.Fatalf("Drain got %d nudges, want 1", len(nudges))
	}
	if nudges[0].Message != "do not lose me" {
		t.Fatalf("recovered message = %q, want %q (content was lost or clobbered)", nudges[0].Message, "do not lose me")
	}
	if nudges[0].Attempts != 1 {
		t.Fatalf("recovered Attempts = %d, want 1 (persisted from the crashed attempt)", nudges[0].Attempts)
	}
	if !nudges[0].InFlight {
		t.Fatalf("recovered InFlight = false, want true (the crashed attempt never reported an outcome)")
	}
}
