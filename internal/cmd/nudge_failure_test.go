package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/tmux"
)

// testTmuxNoSession returns a Tmux instance pointed at a socket with no
// server, so calls that would otherwise reach a real tmux session (e.g.
// CapturePane inside handleFailedInjection) fail harmlessly.
func testTmuxNoSession() *tmux.Tmux {
	return tmux.NewTmuxWithSocket("gt-test-no-such-socket")
}

// TestHandleFailedInjection_GenericErrorRequeuesFirst covers the "any other
// injection error is bounded" half of item 2: a first failure that is NOT
// composer-dirty is requeued (not dead-lettered).
//
// drained is seeded with Attempts: 1, not 0: handleFailedInjection itself
// does NOT increment Attempts (see its doc comment) — a real caller
// persists that increment BEFORE the attempt via Claim.MarkAttempt, so by
// the time handleFailedInjection sees an entry, Attempts already reflects
// this attempt. This proves handleFailedInjection PRESERVES the
// already-incremented count rather than incrementing it a second time.
func TestHandleFailedInjection_GenericErrorRequeuesFirst(t *testing.T) {
	townRoot := t.TempDir()
	sessionName := "gt-crew-test"
	drained := []nudge.QueuedNudge{
		{ID: "abc123", Sender: "test", Message: "first", Timestamp: time.Now().Add(-time.Second), Attempts: 1},
		{ID: "def456", Sender: "test", Message: "second", Timestamp: time.Now(), Attempts: 1},
	}

	handleFailedInjection(testTmuxNoSession(), townRoot, sessionName, sourceIdleWatcher, drained, errors.New("generic injection failure"))

	got, err := nudge.Drain(townRoot, sessionName)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(got) != len(drained) {
		t.Fatalf("Drain got %d nudges, want %d (requeued, not dead-lettered)", len(got), len(drained))
	}
	for i := range drained {
		if got[i].Message != drained[i].Message || got[i].Sender != drained[i].Sender {
			t.Fatalf("requeued[%d] = %#v, want %#v", i, got[i], drained[i])
		}
		if got[i].Attempts != 1 {
			t.Errorf("requeued[%d].Attempts = %d, want 1 (preserved, not re-incremented)", i, got[i].Attempts)
		}
	}

	entries, err := nudge.ListDeadLetters(townRoot, sessionName)
	if err != nil {
		t.Fatalf("ListDeadLetters: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("ListDeadLetters got %d entries, want 0 (first failure should requeue, not dead-letter)", len(entries))
	}
}

// TestHandleFailedInjection_ComposerDirtyDeadLettersImmediately covers item
// 1: a composer-dirty failure is dead-lettered on the FIRST failure, never
// requeued, so the next poll cannot retype into the same dirty composer.
func TestHandleFailedInjection_ComposerDirtyDeadLettersImmediately(t *testing.T) {
	townRoot := t.TempDir()
	sessionName := "gt-crew-test"
	drained := []nudge.QueuedNudge{
		{ID: "dirty1", Sender: "test", Message: "stuck payload", Timestamp: time.Now()},
	}
	deliverErr := fmt.Errorf("%w: %w (composer contains other text after Enter)", tmux.ErrSubmitNotVerified, tmux.ErrComposerDirty)

	handleFailedInjection(testTmuxNoSession(), townRoot, sessionName, sourceNudgePoller, drained, deliverErr)

	requeued, err := nudge.Drain(townRoot, sessionName)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(requeued) != 0 {
		t.Fatalf("Drain got %d entries requeued, want 0 (composer-dirty must not be retyped)", len(requeued))
	}

	entries, err := nudge.ListDeadLetters(townRoot, sessionName)
	if err != nil {
		t.Fatalf("ListDeadLetters: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ListDeadLetters got %d entries, want 1", len(entries))
	}
	// Assert the COMPLETE payload was preserved in the dead-letter record,
	// not merely that the queue file disappeared.
	got := entries[0]
	if got.Message != "stuck payload" || got.Sender != "test" {
		t.Errorf("dead-letter payload = %#v, want Message=%q Sender=%q", got, "stuck payload", "test")
	}
	// A composer-dirty snapshot proves nothing about what happened to our
	// message before the other content appeared, so it must NOT be recorded
	// as certain non-delivery.
	if !got.UncertainDelivery {
		t.Errorf("dead-letter UncertainDelivery = false, want true: a composer-dirty snapshot cannot prove the message was not delivered")
	}
	if got.Source != sourceNudgePoller {
		t.Errorf("dead-letter Source = %q, want %q", got.Source, sourceNudgePoller)
	}
}

// TestHandleFailedInjection_UncertainSubmitDeadLettersImmediately covers the
// other half of item 1's "zero retypes for dirty OR uncertain" requirement:
// an unverified submission that is NOT specifically composer-dirty (e.g. an
// acknowledgement lost after typing) must also dead-letter on the FIRST
// failure, never requeued.
func TestHandleFailedInjection_UncertainSubmitDeadLettersImmediately(t *testing.T) {
	townRoot := t.TempDir()
	sessionName := "gt-crew-test"
	drained := []nudge.QueuedNudge{
		{ID: "uncertain-submit", Sender: "test", Message: "ack lost after typing", Timestamp: time.Now()},
	}
	// Mirrors submit_verify.go's C-j-reset-failed wrapping: ErrSubmitNotVerified
	// alone, with no ErrComposerDirty.
	deliverErr := fmt.Errorf("%w (C-j reset failed: pane gone)", tmux.ErrSubmitNotVerified)

	handleFailedInjection(testTmuxNoSession(), townRoot, sessionName, sourceNudgePoller, drained, deliverErr)

	requeued, err := nudge.Drain(townRoot, sessionName)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(requeued) != 0 {
		t.Fatalf("Drain got %d entries requeued, want 0 (an unverified submission must not be retyped)", len(requeued))
	}

	entries, err := nudge.ListDeadLetters(townRoot, sessionName)
	if err != nil {
		t.Fatalf("ListDeadLetters: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ListDeadLetters got %d entries, want 1 (first failure, not yet at MaxInjectionAttempts)", len(entries))
	}
	if !entries[0].UncertainDelivery {
		t.Errorf("dead-letter UncertainDelivery = false, want true")
	}
}

// TestPartitionForInjection_ExhaustedEntriesSkipInjection covers "the poller
// injects before checking attempts": an entry that already reached
// nudge.MaxInjectionAttempts on a prior cycle (via the dead-letter-write
// durability fallback, which requeues past the bound rather than losing the
// message) must not be handed to a live tmux injection attempt again —
// otherwise a persistently broken dead-letter store turns into an
// indefinite retype loop until TTL expiry.
func TestPartitionForInjection_ExhaustedEntriesSkipInjection(t *testing.T) {
	townRoot := t.TempDir()
	sessionName := "gt-crew-test"

	seed := []nudge.QueuedNudge{
		{ID: "fresh", Sender: "test", Message: "m1", Attempts: 0, Timestamp: time.Now().Add(-3 * time.Millisecond)},
		{ID: "one-attempt", Sender: "test", Message: "m2", Attempts: nudge.MaxInjectionAttempts - 1, Timestamp: time.Now().Add(-2 * time.Millisecond)},
		{ID: "exhausted", Sender: "test", Message: "m3", Attempts: nudge.MaxInjectionAttempts, Timestamp: time.Now().Add(-1 * time.Millisecond)},
		{ID: "over-exhausted", Sender: "test", Message: "m4", Attempts: nudge.MaxInjectionAttempts + 3, Timestamp: time.Now()},
	}
	for _, n := range seed {
		if err := nudge.Enqueue(townRoot, sessionName, n); err != nil {
			t.Fatalf("Enqueue(%s): %v", n.ID, err)
		}
	}

	claims, err := nudge.DrainClaims(townRoot, sessionName)
	if err != nil {
		t.Fatalf("DrainClaims: %v", err)
	}
	if len(claims) != len(seed) {
		t.Fatalf("DrainClaims got %d claims, want %d", len(claims), len(seed))
	}

	toInject, exhausted, staleInFlight := partitionForInjection(claims)

	var injectIDs, exhaustedIDs []string
	for _, c := range toInject {
		injectIDs = append(injectIDs, c.Nudge.ID)
	}
	for _, c := range exhausted {
		exhaustedIDs = append(exhaustedIDs, c.Nudge.ID)
	}

	wantInject := []string{"fresh", "one-attempt"}
	wantExhausted := []string{"exhausted", "over-exhausted"}
	if !reflect.DeepEqual(injectIDs, wantInject) {
		t.Errorf("toInject IDs = %v, want %v", injectIDs, wantInject)
	}
	if !reflect.DeepEqual(exhaustedIDs, wantExhausted) {
		t.Errorf("exhausted IDs = %v, want %v", exhaustedIDs, wantExhausted)
	}
	if len(staleInFlight) != 0 {
		t.Errorf("staleInFlight = %v, want none (no seeded entry has InFlight set)", staleInFlight)
	}
}

// TestPartitionForInjection_InFlightEntrySkipsInjection: an entry restored
// by the orphan sweep with InFlight still true — its prior attempt was
// interrupted before recording an outcome — must not be handed to a live
// tmux injection attempt again, regardless of how low its Attempts count
// is, because we cannot rule out that some of the message already reached
// the composer.
func TestPartitionForInjection_InFlightEntrySkipsInjection(t *testing.T) {
	townRoot := t.TempDir()
	sessionName := "gt-crew-test"

	if err := nudge.Enqueue(townRoot, sessionName, nudge.QueuedNudge{
		ID: "crashed-mid-attempt", Sender: "test", Message: "m1",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	claims, err := nudge.DrainClaims(townRoot, sessionName)
	if err != nil || len(claims) != 1 {
		t.Fatalf("DrainClaims: claims=%d err=%v", len(claims), err)
	}
	// Simulate MarkAttempt's pre-attempt persist, then a crash: InFlight
	// stays true because nothing ever cleared it.
	if err := claims[0].MarkAttempt(); err != nil {
		t.Fatalf("MarkAttempt: %v", err)
	}
	if !claims[0].Nudge.InFlight {
		t.Fatalf("MarkAttempt did not set InFlight")
	}

	toInject, exhausted, staleInFlight := partitionForInjection(claims)
	if len(toInject) != 0 {
		t.Errorf("toInject = %v, want none: an in-flight entry must never be retyped", toInject)
	}
	if len(exhausted) != 0 {
		t.Errorf("exhausted = %v, want none: Attempts (1) is below MaxInjectionAttempts", exhausted)
	}
	if len(staleInFlight) != 1 || staleInFlight[0].Nudge.ID != "crashed-mid-attempt" {
		t.Fatalf("staleInFlight = %v, want [crashed-mid-attempt]", staleInFlight)
	}
}

// TestHandleFailedInjection_ExhaustedRoutingPreservesRealLastError: the
// routing sentinels errAttemptsExhausted / errPriorAttemptUnresolved are
// used when NO live injection was attempted this cycle — overwriting the
// entry's LastError with the sentinel's own generic text would destroy the
// real diagnostic error recorded on a PRIOR cycle's actual attempt, which is
// exactly what an operator inspecting a dead-letter entry needs.
func TestHandleFailedInjection_ExhaustedRoutingPreservesRealLastError(t *testing.T) {
	townRoot := t.TempDir()
	sessionName := "gt-crew-test"
	const realError = "tmux: session not found (from the actual failed attempt)"
	drained := []nudge.QueuedNudge{
		{ID: "exhausted-1", Sender: "test", Message: "must keep real error", Timestamp: time.Now(), Attempts: nudge.MaxInjectionAttempts, LastError: realError},
	}

	handleFailedInjection(testTmuxNoSession(), townRoot, sessionName, sourceNudgePoller, drained, errAttemptsExhausted)

	entries, err := nudge.ListDeadLetters(townRoot, sessionName)
	if err != nil {
		t.Fatalf("ListDeadLetters: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ListDeadLetters got %d entries, want 1", len(entries))
	}
	if entries[0].LastError != realError {
		t.Errorf("dead-lettered LastError = %q, want the preserved real error %q (not the errAttemptsExhausted routing sentinel's own text)", entries[0].LastError, realError)
	}
}

// TestHandleFailedInjection_BoundedRetriesDeadLetterAfterMax covers the
// "bounded" half of item 2 for non-dirty errors: once Attempts reaches
// nudge.MaxInjectionAttempts, the entry is dead-lettered instead of requeued
// again, so an uncertain (but not provably-dirty) failure does not retype
// forever either.
//
// drained is seeded with Attempts: nudge.MaxInjectionAttempts (the count a
// real caller has already persisted via Claim.MarkAttempt before THIS,
// the failing, attempt): handleFailedInjection checks
// n.Attempts >= nudge.MaxInjectionAttempts, and does not itself increment
// Attempts (see its doc comment).
func TestHandleFailedInjection_BoundedRetriesDeadLetterAfterMax(t *testing.T) {
	townRoot := t.TempDir()
	sessionName := "gt-crew-test"
	drained := []nudge.QueuedNudge{
		{ID: "uncertain1", Sender: "test", Message: "ack lost after typing", Timestamp: time.Now(), Attempts: nudge.MaxInjectionAttempts},
	}

	handleFailedInjection(testTmuxNoSession(), townRoot, sessionName, sourceIdleWatcher, drained, errors.New("uncertain delivery"))

	requeued, err := nudge.Drain(townRoot, sessionName)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(requeued) != 0 {
		t.Fatalf("Drain got %d entries requeued, want 0 (attempts bound reached)", len(requeued))
	}

	entries, err := nudge.ListDeadLetters(townRoot, sessionName)
	if err != nil {
		t.Fatalf("ListDeadLetters: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ListDeadLetters got %d entries, want 1", len(entries))
	}
	if !entries[0].UncertainDelivery {
		t.Errorf("dead-letter UncertainDelivery = false, want true for a generic (non-composer-dirty) bounded failure")
	}
}

// blockDeadLetterDir occupies a session's dead-letter directory path with a
// regular file, so nudge.DeadLetter's MkdirAll fails deterministically.
func blockDeadLetterDir(t *testing.T, townRoot, sessionName string) {
	t.Helper()
	safeName := strings.ReplaceAll(sessionName, "/", "_")
	deadLetterParent := filepath.Join(townRoot, ".runtime", "nudge_deadletter")
	if err := os.MkdirAll(deadLetterParent, 0755); err != nil {
		t.Fatalf("setup MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(deadLetterParent, safeName), []byte("blocking file"), 0644); err != nil {
		t.Fatalf("setup WriteFile: %v", err)
	}
}

// TestHandleFailedInjection_DeadLetterWriteFailureRequeuesInstead covers the
// BOUNDED (non-dirty) failure path: if DeadLetter itself cannot write, the
// entry falls back to requeue rather than being silently dropped. Safe here
// because the next cycle attempts an ordinary retry, not a retype into a
// known-dirty composer.
func TestHandleFailedInjection_DeadLetterWriteFailureRequeuesInstead(t *testing.T) {
	townRoot := t.TempDir()
	sessionName := "gt-crew-test"
	blockDeadLetterDir(t, townRoot, sessionName)

	drained := []nudge.QueuedNudge{
		{ID: "willfail", Sender: "test", Message: "must not be lost", Attempts: nudge.MaxInjectionAttempts - 1, Timestamp: time.Now()},
	}
	deliverErr := errors.New("uncertain delivery")

	handleFailedInjection(testTmuxNoSession(), townRoot, sessionName, sourceNudgePoller, drained, deliverErr)

	requeued, err := nudge.Drain(townRoot, sessionName)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(requeued) != 1 {
		t.Fatalf("Drain got %d entries, want 1 (dead-letter write failed on a bounded failure, must fall back to requeue)", len(requeued))
	}
	if requeued[0].Message != "must not be lost" {
		t.Errorf("requeued message = %q, want %q", requeued[0].Message, "must not be lost")
	}
}

// TestHandleFailedInjection_DirtyDeadLetterWriteFailureRetainsRatherThanRequeues
// covers the double-fault: when the entry is composer-dirty AND the
// dead-letter write itself fails, requeuing would resume the exact
// retype-into-dirty-composer loop item 1 exists to fix. It must never be
// requeued into the active queue — but it must also be RETAINED, not
// silently dropped (item 5): it comes back in unresolved for the caller to
// leave un-acked.
func TestHandleFailedInjection_DirtyDeadLetterWriteFailureRetainsRatherThanRequeues(t *testing.T) {
	townRoot := t.TempDir()
	sessionName := "gt-crew-test"
	blockDeadLetterDir(t, townRoot, sessionName)

	drained := []nudge.QueuedNudge{
		{ID: "dirty-and-unwritable", Sender: "test", Message: "must not be retyped", Timestamp: time.Now()},
	}
	deliverErr := fmt.Errorf("%w: %w", tmux.ErrSubmitNotVerified, tmux.ErrComposerDirty)

	unresolved := handleFailedInjection(testTmuxNoSession(), townRoot, sessionName, sourceNudgePoller, drained, deliverErr)
	if len(unresolved) != 1 || unresolved[0].ID != "dirty-and-unwritable" {
		t.Fatalf("unresolved = %v, want [dirty-and-unwritable] (retained, not dropped)", unresolved)
	}

	requeued, err := nudge.Drain(townRoot, sessionName)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(requeued) != 0 {
		t.Fatalf("Drain got %d entries requeued, want 0 (a dirty entry must never be requeued, even when dead-lettering it failed)", len(requeued))
	}
}
