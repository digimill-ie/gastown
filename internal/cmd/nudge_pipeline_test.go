package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/tmux"
)

// claimFileCount lists the on-disk queue directory for session and counts
// entries whose name still contains ".claimed" — i.e. claims nobody has
// Acked yet.
func claimFileCount(t *testing.T, townRoot, session string) int {
	t.Helper()
	dir := filepath.Join(townRoot, ".runtime", "nudge_queue", session)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	n := 0
	for _, e := range entries {
		if strings.Contains(e.Name(), ".claimed") {
			n++
		}
	}
	return n
}

// TestPipeline_AckLostAfterTyping_DeadLettersAndAcksClaim is one of the six
// REVISION-3-required tests codex found not covered: an acknowledgement
// lost after typing (a submission that could not be confirmed, but is NOT
// specifically a dirty composer) must be dead-lettered, never retyped, and
// — the part existing handleFailedInjection-only tests
// (TestHandleFailedInjection_UncertainSubmitDeadLettersImmediately) don't
// reach — the ORIGINAL CLAIM must actually be resolved (Acked) once the
// dead-letter write durably lands, exercising the real DrainClaims ->
// handleFailedInjection -> ackClaims pipeline a live poller cycle runs
// (codex, nudge_failure.go:76, changes-requested at 08964387/95f841e6
// rework).
func TestPipeline_AckLostAfterTyping_DeadLettersAndAcksClaim(t *testing.T) {
	townRoot := t.TempDir()
	sessionName := "gt-pipeline-acklost"

	if err := nudge.Enqueue(townRoot, sessionName, nudge.QueuedNudge{
		ID: "ack-lost-1", Sender: "test", Message: "ack lost after typing",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	claims, err := nudge.DrainClaims(townRoot, sessionName)
	if err != nil || len(claims) != 1 {
		t.Fatalf("DrainClaims: claims=%d err=%v", len(claims), err)
	}
	if claimFileCount(t, townRoot, sessionName) != 1 {
		t.Fatalf("expected 1 claim file on disk before injection")
	}

	// Simulate the poller's real sequence: persist the attempt before
	// injecting, then the injection fails with an unverified (ack-lost)
	// error — mirrors submit_verify.go's post-C-j "pane gone" wrapping.
	marked, failedPersist := markAttempts(sourceNudgePoller, sessionName, claims)
	if len(failedPersist) != 0 {
		t.Fatalf("markAttempts: %d claims failed to persist, want 0", len(failedPersist))
	}
	claims = marked
	deliverErr := fmt.Errorf("%w (C-j reset failed: pane gone)", tmux.ErrSubmitNotVerified)

	drained := claimNudges(claims)
	unresolved := handleFailedInjection(testTmuxNoSession(), townRoot, sessionName, sourceNudgePoller, drained, deliverErr)
	if len(unresolved) != 0 {
		t.Fatalf("unresolved = %d entries, want 0 (the dead-letter write succeeded)", len(unresolved))
	}

	ackClaims(sourceNudgePoller, sessionName, claims, unresolved)

	// The claim is now resolved: its underlying file must be gone.
	if n := claimFileCount(t, townRoot, sessionName); n != 0 {
		t.Fatalf("claim files remaining after ack = %d, want 0 (dead-letter durably recorded the outcome)", n)
	}

	// Never retyped: nothing waiting in the live queue.
	requeued, err := nudge.Drain(townRoot, sessionName)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(requeued) != 0 {
		t.Fatalf("Drain got %d entries requeued, want 0 (ack-lost must not be retyped)", len(requeued))
	}

	entries, err := nudge.ListDeadLetters(townRoot, sessionName)
	if err != nil {
		t.Fatalf("ListDeadLetters: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ListDeadLetters got %d entries, want 1", len(entries))
	}
	if !entries[0].UncertainDelivery {
		t.Errorf("UncertainDelivery = false, want true: an ack-lost failure cannot prove non-delivery")
	}
	if entries[0].Attempts != 1 {
		t.Errorf("dead-lettered Attempts = %d, want 1 (persisted via MarkAttempt before the injection attempt)", entries[0].Attempts)
	}
}

// TestPipeline_BoundedDoubleFault_KeepsClaimForRecovery is one of the six
// REVISION-3-required tests codex found not covered: "dead-letter write
// failure keeping the queue file". A BOUNDED (verified-failed, non-
// unverified) entry that has already reached nudge.MaxInjectionAttempts
// takes the dead-letter path; if THAT write fails, handleFailedInjection
// falls back to requeuing instead of losing the message. This test forces
// the requeue fallback to ALSO fail (both writes double-fault) and proves
// the original claim file is NOT acked in that case — it stays on disk so
// a future orphan sweep can still recover it, instead of the caller acking
// (and thereby permanently losing) the only durable copy the moment
// handleFailedInjection returns (codex, nudge_poller.go:157,
// changes-requested at 08964387/95f841e6 rework — the ordering bug this
// whole rework exists to close).
func TestPipeline_BoundedDoubleFault_KeepsClaimForRecovery(t *testing.T) {
	townRoot := t.TempDir()
	sessionName := "gt-pipeline-doublefault"

	if err := nudge.Enqueue(townRoot, sessionName, nudge.QueuedNudge{
		ID: "double-fault-1", Sender: "test", Message: "must not be lost",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	claims, err := nudge.DrainClaims(townRoot, sessionName)
	if err != nil || len(claims) != 1 {
		t.Fatalf("DrainClaims: claims=%d err=%v", len(claims), err)
	}
	if claimFileCount(t, townRoot, sessionName) != 1 {
		t.Fatalf("expected 1 claim file on disk before injection")
	}

	// Force the dead-letter write to fail: pre-create a plain FILE at the
	// exact path DeadLetter needs to create as a DIRECTORY.
	deadLetterPath := filepath.Join(townRoot, ".runtime", "nudge_deadletter", sessionName)
	if err := os.MkdirAll(filepath.Dir(deadLetterPath), 0755); err != nil {
		t.Fatalf("MkdirAll parent: %v", err)
	}
	if err := os.WriteFile(deadLetterPath, []byte("block"), 0644); err != nil {
		t.Fatalf("blocking dead-letter dir: %v", err)
	}

	// Force the requeue fallback to ALSO fail: fill the queue to its
	// configured depth limit with unrelated entries, so Enqueue's depth
	// check rejects any further write — without touching the directory
	// our real claim file already lives in.
	for i := 0; i < nudge.MaxQueueDepth; i++ {
		if err := nudge.Enqueue(townRoot, sessionName, nudge.QueuedNudge{
			ID: "filler", Sender: "test", Message: "filler",
		}); err != nil {
			t.Fatalf("filling queue to depth limit (i=%d): %v", i, err)
		}
	}

	// A BOUNDED (non-unverified) error at MaxInjectionAttempts routes to
	// the dead-letter attempt, not the plain requeue-and-retry path.
	drained := []nudge.QueuedNudge{claims[0].Nudge}
	drained[0].Attempts = nudge.MaxInjectionAttempts
	deliverErr := errors.New("simulated verified injection failure")

	unresolved := handleFailedInjection(testTmuxNoSession(), townRoot, sessionName, sourceNudgePoller, drained, deliverErr)
	if len(unresolved) != 1 {
		t.Fatalf("unresolved = %d entries, want 1 (both the dead-letter AND requeue-fallback writes failed)", len(unresolved))
	}
	if unresolved[0].ID != "double-fault-1" {
		t.Errorf("unresolved[0].ID = %q, want %q", unresolved[0].ID, "double-fault-1")
	}

	// The caller must not ack an unresolved claim.
	ackClaims(sourceNudgePoller, sessionName, claims, unresolved)

	// The ORIGINAL claim file must still exist: it is the only durable
	// copy of "must not be lost" anywhere on disk right now.
	if n := claimFileCount(t, townRoot, sessionName); n != 1 {
		t.Fatalf("claim files remaining after ackClaims = %d, want 1 (the queue file must be KEPT, not acked away, when both writes fail)", n)
	}

	// No dead-letter entry exists: the write failed, deliberately, by the
	// same sabotage that made ListDeadLetters itself unable to even read
	// that directory (it's a file, not a directory) — which is itself
	// proof nothing was written there.
	if _, err := nudge.ListDeadLetters(townRoot, sessionName); err == nil {
		t.Fatalf("ListDeadLetters succeeded against a directory this test replaced with a file — sabotage didn't take")
	}
}

// TestPipeline_UnverifiedDoubleFault_RetainsClaim covers High 1
// (nudge_failure.go:121, R2 hq-g52db REVISION 3): an UNVERIFIED entry
// (ErrSubmitNotVerified — we cannot rule out partial delivery) whose
// dead-letter write ALSO fails must be RETAINED, never dropped. The
// previous version of this code acked the original claim anyway, calling
// it a "deliberate drop" — this proves the fixed behavior: the claim
// comes back in unresolved and survives ackClaims, and — because
// requeuing an unverified entry risks retyping into a possibly-already-
// typed composer — it must NOT appear back in the live queue either.
func TestPipeline_UnverifiedDoubleFault_RetainsClaim(t *testing.T) {
	townRoot := t.TempDir()
	sessionName := "gt-pipeline-unverified-doublefault"

	if err := nudge.Enqueue(townRoot, sessionName, nudge.QueuedNudge{
		ID: "unverified-double-fault-1", Sender: "test", Message: "must not be dropped",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	claims, err := nudge.DrainClaims(townRoot, sessionName)
	if err != nil || len(claims) != 1 {
		t.Fatalf("DrainClaims: claims=%d err=%v", len(claims), err)
	}
	if claimFileCount(t, townRoot, sessionName) != 1 {
		t.Fatalf("expected 1 claim file on disk before injection")
	}

	// Force the dead-letter write to fail, same sabotage as the bounded
	// double-fault test above.
	deadLetterPath := filepath.Join(townRoot, ".runtime", "nudge_deadletter", sessionName)
	if err := os.MkdirAll(filepath.Dir(deadLetterPath), 0755); err != nil {
		t.Fatalf("MkdirAll parent: %v", err)
	}
	if err := os.WriteFile(deadLetterPath, []byte("block"), 0644); err != nil {
		t.Fatalf("blocking dead-letter dir: %v", err)
	}

	drained := []nudge.QueuedNudge{claims[0].Nudge}
	deliverErr := fmt.Errorf("%w (C-j reset failed: pane gone)", tmux.ErrSubmitNotVerified)

	unresolved := handleFailedInjection(testTmuxNoSession(), townRoot, sessionName, sourceNudgePoller, drained, deliverErr)
	if len(unresolved) != 1 {
		t.Fatalf("unresolved = %d entries, want 1 (retained, not dropped)", len(unresolved))
	}
	if unresolved[0].ID != "unverified-double-fault-1" {
		t.Errorf("unresolved[0].ID = %q, want %q", unresolved[0].ID, "unverified-double-fault-1")
	}

	ackClaims(sourceNudgePoller, sessionName, claims, unresolved)

	// The ORIGINAL claim file must still exist: retained, not acked away.
	if n := claimFileCount(t, townRoot, sessionName); n != 1 {
		t.Fatalf("claim files remaining after ackClaims = %d, want 1 (an unverified double fault must RETAIN the claim, never drop it)", n)
	}

	// Must NOT be requeued into the live queue: an unverified entry must
	// never be retyped, and requeuing it here would risk exactly that on
	// the next drain.
	if pending, _ := nudge.Pending(townRoot, sessionName); pending != 0 {
		t.Fatalf("Pending = %d, want 0: an unverified double-fault entry must not reappear as a fresh requeued .json", pending)
	}

	if _, err := nudge.ListDeadLetters(townRoot, sessionName); err == nil {
		t.Fatalf("ListDeadLetters succeeded against a directory this test replaced with a file — sabotage didn't take")
	}
}

// TestPipeline_SingleDeadLetterFault_RequeuesAndAcksClaim is the pipeline
// (DrainClaims -> handleFailedInjection -> ackClaims) counterpart to
// TestHandleFailedInjection_DeadLetterWriteFailureRequeuesInstead, which
// only exercised handleFailedInjection directly against a hand-built
// QueuedNudge slice — never through a real Claim, so it never proved the
// ORIGINAL claim file actually gets acked once the requeue fallback durably
// lands (codex: "dead-letter-branch tests ... synthetic", nudge_test.go:742,
// changes-requested at REVISION 3 — Medium). A single fault (dead-letter
// write fails) on a BOUNDED (non-unverified) entry falls back to requeue,
// and the original claim — now redundant — must be acked away.
func TestPipeline_SingleDeadLetterFault_RequeuesAndAcksClaim(t *testing.T) {
	townRoot := t.TempDir()
	sessionName := "gt-pipeline-single-deadletter-fault"

	if err := nudge.Enqueue(townRoot, sessionName, nudge.QueuedNudge{
		ID: "single-fault-1", Sender: "test", Message: "must not be lost", Attempts: nudge.MaxInjectionAttempts - 1,
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	claims, err := nudge.DrainClaims(townRoot, sessionName)
	if err != nil || len(claims) != 1 {
		t.Fatalf("DrainClaims: claims=%d err=%v", len(claims), err)
	}
	if claimFileCount(t, townRoot, sessionName) != 1 {
		t.Fatalf("expected 1 claim file on disk before injection")
	}

	deadLetterPath := filepath.Join(townRoot, ".runtime", "nudge_deadletter", sessionName)
	if err := os.MkdirAll(filepath.Dir(deadLetterPath), 0755); err != nil {
		t.Fatalf("MkdirAll parent: %v", err)
	}
	if err := os.WriteFile(deadLetterPath, []byte("block"), 0644); err != nil {
		t.Fatalf("blocking dead-letter dir: %v", err)
	}

	drained := []nudge.QueuedNudge{claims[0].Nudge}
	deliverErr := errors.New("uncertain delivery")

	unresolved := handleFailedInjection(testTmuxNoSession(), townRoot, sessionName, sourceNudgePoller, drained, deliverErr)
	if len(unresolved) != 0 {
		t.Fatalf("unresolved = %d entries, want 0 (the requeue fallback succeeded)", len(unresolved))
	}

	ackClaims(sourceNudgePoller, sessionName, claims, unresolved)

	// The original claim is now redundant — its content durably landed via
	// requeue — and must be gone.
	if n := claimFileCount(t, townRoot, sessionName); n != 0 {
		t.Fatalf("claim files remaining after ackClaims = %d, want 0 (the requeue fallback landed, the original claim is redundant)", n)
	}

	requeued, err := nudge.Drain(townRoot, sessionName)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(requeued) != 1 || requeued[0].Message != "must not be lost" {
		t.Fatalf("Drain = %#v, want exactly 1 entry with the original payload", requeued)
	}
}
