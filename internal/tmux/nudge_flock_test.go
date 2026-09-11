package tmux

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// TestNudgeFlock_PaneAndSessionDeliveryShareLockPath is one of the six
// REVISION-3-required tests codex found not covered: concurrent direct
// (`gt nudge`, NudgeSessionWithOpts) and poller/sling (NudgePaneWithOpts)
// delivery to the SAME underlying session must serialize against each
// other under the SAME cross-process flock.
//
// NudgePaneWithOpts resolves its pane to an owning session (via
// sessionNameForTarget) and takes the flock at nudgeFlockPath(townRoot,
// THAT session) — the identical key NudgeSessionWithOpts uses. Before Fix
// 4 (codex, tmux.go:1934, changes-requested at 08964387/95f841e6 rework),
// NudgePane took no flock at all, so a sling/dispatch nudge via a pane and
// a concurrent direct nudge to the same session never serialized: this
// test holds the session's flock externally and proves NudgePaneWithOpts
// genuinely blocks on it rather than proceeding regardless.
func TestNudgeFlock_PaneAndSessionDeliveryShareLockPath(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-flock-" + fmt.Sprintf("%d", time.Now().UnixNano()%1000000)
	if err := tm.NewSession(sessionName, os.TempDir()); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	time.Sleep(200 * time.Millisecond)
	setFakeClaudePrompt(t, tm, sessionName)

	pane, err := tm.GetPaneID(sessionName)
	if err != nil {
		t.Fatalf("GetPaneID: %v", err)
	}

	townRoot := t.TempDir()
	sessionLockPath := nudgeFlockPath(townRoot, sessionName)

	unlock, err := acquireFlockLock(sessionLockPath, time.Second)
	if err != nil {
		t.Fatalf("acquireFlockLock: %v", err)
	}

	const holdTime = 500 * time.Millisecond
	released := make(chan struct{})
	go func() {
		time.Sleep(holdTime)
		unlock()
		close(released)
	}()

	start := time.Now()
	// NudgePaneWithOpts is given only the PANE, not the session name — it
	// must resolve the owning session itself and take the SAME lock this
	// test is holding directly.
	deliverErr := tm.NudgePaneWithOpts(pane, "queued while session flock held", NudgeOpts{TownRoot: townRoot})
	elapsed := time.Since(start)
	<-released

	if deliverErr != nil {
		t.Fatalf("NudgePaneWithOpts: %v", deliverErr)
	}
	if elapsed < holdTime-50*time.Millisecond {
		t.Errorf("NudgePaneWithOpts completed after %v while the session's cross-process flock was held for %v — it did not serialize on the same lock path as NudgeSessionWithOpts would", elapsed, holdTime)
	}
}

// TestNudgeFlock_SessionDeliveryBlocksOnHeldLock is the direct-delivery
// counterpart: NudgeSessionWithOpts itself must block on a flock already
// held at the path it computes for (townRoot, session) — the mechanism
// TestNudgeFlock_PaneAndSessionDeliveryShareLockPath relies on
// NudgePaneWithOpts sharing.
func TestNudgeFlock_SessionDeliveryBlocksOnHeldLock(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-flock-direct-" + fmt.Sprintf("%d", time.Now().UnixNano()%1000000)
	if err := tm.NewSession(sessionName, os.TempDir()); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	time.Sleep(200 * time.Millisecond)
	setFakeClaudePrompt(t, tm, sessionName)

	townRoot := t.TempDir()
	lockPath := nudgeFlockPath(townRoot, sessionName)

	unlock, err := acquireFlockLock(lockPath, time.Second)
	if err != nil {
		t.Fatalf("acquireFlockLock: %v", err)
	}

	const holdTime = 500 * time.Millisecond
	released := make(chan struct{})
	go func() {
		time.Sleep(holdTime)
		unlock()
		close(released)
	}()

	start := time.Now()
	deliverErr := tm.NudgeSessionWithOpts(sessionName, "queued while session flock held", NudgeOpts{TownRoot: townRoot})
	elapsed := time.Since(start)
	<-released

	if deliverErr != nil {
		t.Fatalf("NudgeSessionWithOpts: %v", deliverErr)
	}
	if elapsed < holdTime-50*time.Millisecond {
		t.Errorf("NudgeSessionWithOpts completed after %v while its own session flock was held for %v — it did not block on the lock", elapsed, holdTime)
	}
}
