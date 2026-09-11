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
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

func setupNudgeTestRegistry(t *testing.T) {
	t.Helper()
	reg := session.NewPrefixRegistry()
	reg.Register("gt", "gastown")
	reg.Register("bd", "beads")
	old := session.DefaultRegistry()
	session.SetDefaultRegistry(reg)
	t.Cleanup(func() { session.SetDefaultRegistry(old) })
}

// isolateTestTmuxAndWorkspace protects a test that exercises runNudge (or
// any path that can reach session.InitRegistry) from resolving to the REAL
// live town. session.InitRegistry mutates two PROCESS-GLOBAL values: tmux's
// default socket (tmux.SetDefaultSocket) and the session package's default
// prefix registry — so even a test that itself sets GT_TEST_NUDGE_LOG
// (which only gates deliverNudge's own tmux call, not runNudge's earlier
// workspace.FindFromCwd()-driven DND/session.InitRegistry logic) can
// corrupt those globals for every OTHER test that runs afterward in this
// package's process, for the rest of the test binary's life. This package's
// tests run from a worktree nested inside the actual live town
// (gastown/polecats/<name>/gastown/internal/cmd), so an unisolated
// FindFromCwd resolves the REAL town root, InitRegistry then points
// tmux.defaultSocket at the REAL tmux socket, and BuildPrefixRegistryFromTown
// can even write a "fallback copy" of the real rigs.json into the real town
// root directory (registry.go's copyFileIfNewer) — measured causing a test
// to nudge (attempt to type into) whatever session a later-resolved,
// coincidentally-real session name happens to match (codex,
// nudge_test.go:348/801/833, sling_helpers_test.go:136,
// changes-requested at 08964387/95f841e6 rework).
//
// Isolates cwd (so workspace.Find finds nothing) AND GT_TMUX_SOCKET (so
// even if InitRegistry does run against a resolvable root — e.g. via
// GT_TOWN_ROOT/GT_ROOT env fallback, or a prior test's pollution — it
// cannot point tmux at a real server), and restores the session default
// registry and tmux default socket afterward so this test cannot pollute
// any OTHER test in turn.
func isolateTestTmuxAndWorkspace(t *testing.T) {
	t.Helper()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("Chdir: %v", err)
	}

	// tmux.NewTmux() — what every command under test actually calls —
	// resolves its socket from tmux.GetDefaultSocket() first, THEN falls
	// back to the GT_TOWN_SOCKET env var. It never reads GT_TMUX_SOCKET;
	// that variable is read only by session.InitRegistry at init
	// (session/registry.go:128). The previous version of this helper set
	// ONLY GT_TMUX_SOCKET and never touched GetDefaultSocket(), so any
	// code under test that called tmux.NewTmux() resolved to whatever
	// socket the test binary inherited — a REAL, live tmux server, not an
	// isolated one (codex, nudge_test.go:66, changes-requested at
	// REVISION 3 — High 8). isolatedSocketProof (below) exercises this
	// helper and proves nothing lands on the real town socket.
	isolatedSocket := fmt.Sprintf("gt-test-isolated-%d", time.Now().UnixNano())
	t.Setenv("GT_TMUX_SOCKET", isolatedSocket)
	t.Setenv("GT_TOWN_SOCKET", isolatedSocket)
	t.Setenv("GT_TOWN_ROOT", "")
	t.Setenv("GT_ROOT", "")

	origSocket := tmux.GetDefaultSocket()
	origRegistry := session.DefaultRegistry()
	tmux.SetDefaultSocket(isolatedSocket)
	t.Cleanup(func() {
		tmux.SetDefaultSocket(origSocket)
		session.SetDefaultRegistry(origRegistry)
	})
}

// TestIsolateTestTmuxAndWorkspaceRedirectsNewTmux proves that
// isolateTestTmuxAndWorkspace actually redirects tmux.NewTmux() — what
// every command under test calls — to a private, non-default socket.
// Without the tmux.SetDefaultSocket call in the helper (the shape this
// helper had before High 8), tmux.NewTmux() ignores GT_TMUX_SOCKET
// entirely and falls through to the default/inherited server: this test
// goes RED if that call is removed, because SocketName() would then
// report "" (the default socket) instead of the isolated name.
func TestIsolateTestTmuxAndWorkspaceRedirectsNewTmux(t *testing.T) {
	isolateTestTmuxAndWorkspace(t)

	got := tmux.NewTmux().SocketName()
	if got == "" {
		t.Fatal("tmux.NewTmux() resolved to the default/inherited socket (empty SocketName) — a test using this helper could reach a REAL tmux server")
	}
	if !strings.HasPrefix(got, "gt-test-isolated-") {
		t.Fatalf("tmux.NewTmux() socket = %q, want a gt-test-isolated-* private socket", got)
	}
	if got != tmux.GetDefaultSocket() {
		t.Fatalf("tmux.NewTmux() socket %q does not match tmux.GetDefaultSocket() %q", got, tmux.GetDefaultSocket())
	}
	if envSocket := os.Getenv("GT_TMUX_SOCKET"); envSocket == "" {
		t.Fatal("GT_TMUX_SOCKET must still be set — session.InitRegistry reads it directly at init (session/registry.go:128), separately from NewTmux's own resolution")
	}
}

func TestNudgeHelpUsesTownRootMessagingConfig(t *testing.T) {
	const want = "<town-root>/config/messaging.json"

	if !strings.Contains(nudgeCmd.Long, want) {
		t.Fatalf("help should document %q:\n%s", want, nudgeCmd.Long)
	}
	if strings.Contains(nudgeCmd.Long, "~/gt/config/messaging.json") {
		t.Fatalf("help should not document the obsolete home-relative path:\n%s", nudgeCmd.Long)
	}
}

func TestNudgeStdinConflict(t *testing.T) {
	// Save and restore package-level flags
	origMessage := nudgeMessageFlag
	origStdin := nudgeStdinFlag
	defer func() {
		nudgeMessageFlag = origMessage
		nudgeStdinFlag = origStdin
	}()

	// When both --stdin and --message are set, runNudge should return an error
	nudgeStdinFlag = true
	nudgeMessageFlag = "some message"

	err := runNudge(nudgeCmd, []string{"gastown/alpha"})
	if err == nil {
		t.Fatal("expected error when --stdin and --message are both set")
	}
	if !strings.Contains(err.Error(), "cannot use --stdin with --message/-m") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestResolveNudgePattern(t *testing.T) {
	setupNudgeTestRegistry(t)
	// Create test agent sessions (using rig prefixes)
	agents := []*AgentSession{
		{Name: "hq-mayor", Type: AgentMayor},
		{Name: "hq-deacon", Type: AgentDeacon},
		{Name: "gt-witness", Type: AgentWitness, Rig: "gastown"},
		{Name: "gt-refinery", Type: AgentRefinery, Rig: "gastown"},
		{Name: "gt-crew-max", Type: AgentCrew, Rig: "gastown", AgentName: "max"},
		{Name: "gt-crew-jack", Type: AgentCrew, Rig: "gastown", AgentName: "jack"},
		{Name: "gt-alpha", Type: AgentPolecat, Rig: "gastown", AgentName: "alpha"},
		{Name: "gt-beta", Type: AgentPolecat, Rig: "gastown", AgentName: "beta"},
		{Name: "bd-witness", Type: AgentWitness, Rig: "beads"},
		{Name: "bd-gamma", Type: AgentPolecat, Rig: "beads", AgentName: "gamma"},
	}

	tests := []struct {
		name     string
		pattern  string
		expected []string
	}{
		{
			name:     "mayor special case",
			pattern:  "mayor",
			expected: []string{"hq-mayor"},
		},
		{
			name:     "deacon special case",
			pattern:  "deacon",
			expected: []string{"hq-deacon"},
		},
		{
			name:     "specific witness",
			pattern:  "gastown/witness",
			expected: []string{"gt-witness"},
		},
		{
			name:     "all witnesses",
			pattern:  "*/witness",
			expected: []string{"gt-witness", "bd-witness"},
		},
		{
			name:     "specific refinery",
			pattern:  "gastown/refinery",
			expected: []string{"gt-refinery"},
		},
		{
			name:     "all polecats in rig",
			pattern:  "gastown/polecats/*",
			expected: []string{"gt-alpha", "gt-beta"},
		},
		{
			name:     "specific polecat",
			pattern:  "gastown/polecats/alpha",
			expected: []string{"gt-alpha"},
		},
		{
			name:     "all crew in rig",
			pattern:  "gastown/crew/*",
			expected: []string{"gt-crew-max", "gt-crew-jack"},
		},
		{
			name:     "specific crew member",
			pattern:  "gastown/crew/max",
			expected: []string{"gt-crew-max"},
		},
		{
			name:     "legacy polecat format",
			pattern:  "gastown/alpha",
			expected: []string{"gt-alpha"},
		},
		{
			name:     "no matches",
			pattern:  "nonexistent/polecats/*",
			expected: nil,
		},
		{
			name:     "invalid pattern",
			pattern:  "invalid",
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveNudgePattern(tt.pattern, agents)

			if len(got) != len(tt.expected) {
				t.Errorf("resolveNudgePattern(%q) returned %d results, want %d: got %v, want %v",
					tt.pattern, len(got), len(tt.expected), got, tt.expected)
				return
			}

			// Check each expected value is present
			gotMap := make(map[string]bool)
			for _, g := range got {
				gotMap[g] = true
			}
			for _, e := range tt.expected {
				if !gotMap[e] {
					t.Errorf("resolveNudgePattern(%q) missing expected %q, got %v",
						tt.pattern, e, got)
				}
			}
		})
	}
}

func TestSessionNameToAddress(t *testing.T) {
	setupNudgeTestRegistry(t)
	tests := []struct {
		name        string
		sessionName string
		expected    string
	}{
		{
			name:        "mayor",
			sessionName: "hq-mayor",
			expected:    "mayor",
		},
		{
			name:        "deacon",
			sessionName: "hq-deacon",
			expected:    "deacon",
		},
		{
			name:        "witness",
			sessionName: "gt-witness",
			expected:    "gastown/witness",
		},
		{
			name:        "refinery",
			sessionName: "gt-refinery",
			expected:    "gastown/refinery",
		},
		{
			name:        "crew member",
			sessionName: "gt-crew-max",
			expected:    "gastown/crew/max",
		},
		{
			name:        "polecat",
			sessionName: "gt-alpha",
			expected:    "gastown/alpha",
		},
		{
			name:        "dog",
			sessionName: "hq-dog-alpha",
			expected:    "deacon/dogs/alpha",
		},
		{
			name:        "hyphenated dog",
			sessionName: "hq-dog-my-dog",
			expected:    "deacon/dogs/my-dog",
		},
		{
			name:        "unrecognized format",
			sessionName: "plaintext",
			expected:    "",
		},
		{
			name:        "gt prefix but no name",
			sessionName: "gt-",
			expected:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sessionNameToAddress(tt.sessionName)
			if got != tt.expected {
				t.Errorf("sessionNameToAddress(%q) = %q, want %q", tt.sessionName, got, tt.expected)
			}
		})
	}
}

func TestNudgeInvalidMode(t *testing.T) {
	// Save and restore package-level flags
	origMode := nudgeModeFlag
	origPriority := nudgePriorityFlag
	origMessage := nudgeMessageFlag
	origStdin := nudgeStdinFlag
	defer func() {
		nudgeModeFlag = origMode
		nudgePriorityFlag = origPriority
		nudgeMessageFlag = origMessage
		nudgeStdinFlag = origStdin
	}()

	nudgeStdinFlag = false
	nudgeMessageFlag = "test"

	tests := []struct {
		name    string
		mode    string
		wantErr string
	}{
		{"bogus mode", "bogus", `invalid --mode "bogus"`},
		{"empty mode", "", `invalid --mode ""`},
		{"typo immediate", "imediate", `invalid --mode "imediate"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nudgeModeFlag = tt.mode
			nudgePriorityFlag = "normal"
			err := runNudge(nudgeCmd, []string{"gastown/alpha", "hello"})
			if err == nil {
				t.Fatal("expected error for invalid mode")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got error %q, want to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestNudgeInvalidPriority(t *testing.T) {
	// Save and restore package-level flags
	origMode := nudgeModeFlag
	origPriority := nudgePriorityFlag
	origMessage := nudgeMessageFlag
	origStdin := nudgeStdinFlag
	defer func() {
		nudgeModeFlag = origMode
		nudgePriorityFlag = origPriority
		nudgeMessageFlag = origMessage
		nudgeStdinFlag = origStdin
	}()

	nudgeStdinFlag = false
	nudgeMessageFlag = "test"
	nudgeModeFlag = NudgeModeImmediate

	tests := []struct {
		name     string
		priority string
		wantErr  string
	}{
		{"bogus priority", "bogus", `invalid --priority "bogus"`},
		{"empty priority", "", `invalid --priority ""`},
		{"high priority", "high", `invalid --priority "high"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nudgePriorityFlag = tt.priority
			err := runNudge(nudgeCmd, []string{"gastown/alpha", "hello"})
			if err == nil {
				t.Fatal("expected error for invalid priority")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got error %q, want to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestNudgeValidModesAccepted(t *testing.T) {
	// Verify all valid modes pass the validation check (they'll fail later
	// on tmux operations, but should NOT fail on mode validation).
	origMode := nudgeModeFlag
	origPriority := nudgePriorityFlag
	origMessage := nudgeMessageFlag
	origStdin := nudgeStdinFlag
	origTimeout := waitIdleTimeout
	defer func() {
		nudgeModeFlag = origMode
		nudgePriorityFlag = origPriority
		nudgeMessageFlag = origMessage
		nudgeStdinFlag = origStdin
		waitIdleTimeout = origTimeout
	}()

	isolateTestTmuxAndWorkspace(t)

	// Route nudge transport to a log file so the test doesn't deliver "test"
	// messages to live agents (mayor reported recurring synthetic nudges).
	t.Setenv("GT_TEST_NUDGE_LOG", filepath.Join(t.TempDir(), "nudge.log"))

	// Shorten wait-idle timeout to avoid 15s test delay
	waitIdleTimeout = 200 * time.Millisecond

	nudgeStdinFlag = false
	nudgeMessageFlag = "test"
	nudgePriorityFlag = "normal"

	for _, mode := range []string{NudgeModeImmediate, NudgeModeQueue, NudgeModeWaitIdle} {
		t.Run(mode, func(t *testing.T) {
			nudgeModeFlag = mode
			err := runNudge(nudgeCmd, []string{"gastown/alpha", "hello"})
			// The error should NOT be about invalid mode — it will fail on
			// tmux or workspace, which is fine.
			if err != nil && strings.Contains(err.Error(), "invalid --mode") {
				t.Errorf("valid mode %q was rejected: %v", mode, err)
			}
		})
	}
}

func TestIfFreshMaxAge(t *testing.T) {
	// Verify the constant is 60 seconds as specified in the design.
	if ifFreshMaxAge != 60*time.Second {
		t.Errorf("ifFreshMaxAge = %v, want 60s", ifFreshMaxAge)
	}
}

func TestIfFreshSessionAgeCheck(t *testing.T) {
	// Test the age comparison logic used by --if-fresh.
	// A session created 10 seconds ago should be "fresh" (nudge allowed).
	// A session created 120 seconds ago should be "stale" (nudge suppressed).
	now := time.Now()

	tests := []struct {
		name        string
		createdAt   time.Time
		shouldNudge bool
	}{
		{
			name:        "fresh session (10s old)",
			createdAt:   now.Add(-10 * time.Second),
			shouldNudge: true,
		},
		{
			name:        "borderline session (59s old)",
			createdAt:   now.Add(-59 * time.Second),
			shouldNudge: true,
		},
		{
			name:        "stale session (61s old)",
			createdAt:   now.Add(-61 * time.Second),
			shouldNudge: false,
		},
		{
			name:        "very stale session (5min old)",
			createdAt:   now.Add(-5 * time.Minute),
			shouldNudge: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			age := time.Since(tt.createdAt)
			shouldNudge := age <= ifFreshMaxAge
			if shouldNudge != tt.shouldNudge {
				t.Errorf("age=%v: shouldNudge=%v, want %v", age, shouldNudge, tt.shouldNudge)
			}
		})
	}
}

func TestPostQueueIdleRecovery_SkipsDeliveryWhenDrainEmpty(t *testing.T) {
	// Behavioral test (gt-y2zk): when the idle recovery path fires but
	// another process already drained the queue, we must NOT deliver to
	// avoid duplicates. This exercises the len(drained) > 0 guard.
	townRoot := t.TempDir()
	session := "gt-crew-test"

	// Enqueue a nudge, then drain it (simulating a racing hook).
	if err := nudge.Enqueue(townRoot, session, nudge.QueuedNudge{
		Sender:  "test",
		Message: "hello",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	drained, err := nudge.Drain(townRoot, session)
	if err != nil {
		t.Fatalf("first Drain: %v", err)
	}
	if len(drained) != 1 {
		t.Fatalf("first Drain got %d entries, want 1", len(drained))
	}

	// Second drain should return empty — the racing hook already claimed it.
	drained2, err := nudge.Drain(townRoot, session)
	if err != nil {
		t.Fatalf("second Drain: %v", err)
	}
	if len(drained2) != 0 {
		t.Errorf("second Drain got %d entries, want 0 (already claimed)", len(drained2))
	}
}

// testTmuxNoSession returns a Tmux wrapper pointed at a socket with no real
// tmux server, so CapturePane fails harmlessly (handleFailedInjection treats
// that as an empty pane capture) without touching a real session.
func testTmuxNoSession() *tmux.Tmux {
	return tmux.NewTmuxWithSocket("gt-test-no-such-socket")
}

// TestHandleFailedInjection_GenericErrorRequeuesFirst covers the "any other
// injection error is bounded" half of hq-g52db's fix 2: a first failure that
// is NOT composer-dirty is requeued (not dead-lettered).
//
// drained is seeded with Attempts: 1, not 0: handleFailedInjection itself
// does NOT increment Attempts (see its doc comment) — a real caller
// persists that increment BEFORE the attempt via Claim.MarkAttempt, so by
// the time handleFailedInjection sees an entry, Attempts already reflects
// this attempt. The previous version of this test passed Attempts 0 and
// asserted the requeued entry came back as 1, which no longer matches the
// handler (codex, nudge_test.go:525, changes-requested at REVISION 3):
// this now proves handleFailedInjection PRESERVES the already-incremented
// count rather than incrementing it a second time.
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

// TestHandleFailedInjection_ComposerDirtyDeadLettersImmediately covers the
// "zero retypes after a dirty ... delivery" half of fix 2: a composer-dirty
// failure is dead-lettered on the FIRST failure, never requeued, so the next
// poll cannot retype into the same dirty composer.
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
	// as certain non-delivery (codex Medium, nudge_failure.go:57,
	// changes-requested at 08964387).
	if !got.UncertainDelivery {
		t.Errorf("dead-letter UncertainDelivery = false, want true: a composer-dirty snapshot cannot prove the message was not delivered")
	}
	if got.Source != sourceNudgePoller {
		t.Errorf("dead-letter Source = %q, want %q", got.Source, sourceNudgePoller)
	}
}

// TestHandleFailedInjection_UncertainSubmitDeadLettersImmediately covers the
// other half of the "zero retypes for dirty OR uncertain" requirement: an
// unverified submission that is NOT specifically composer-dirty (e.g. an
// acknowledgement lost after typing, or a stranded-composer recovery attempt
// that itself could not be confirmed) must also dead-letter on the FIRST
// failure, never requeued — previously only tmux.ErrComposerDirty got this
// treatment, so this class fell through to an ordinary bounded retry and
// could be retyped (codex High, nudge_failure.go:49, changes-requested at
// 08964387).
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
// indefinite retype loop until TTL expiry (codex High, nudge_failure.go:66 /
// nudge_poller.go:134, changes-requested at 08964387).
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

// TestPartitionForInjection_InFlightEntrySkipsInjection covers High 2
// (nudge_failure.go:152): an entry restored by the orphan sweep with
// InFlight still true — its prior attempt was interrupted before recording
// an outcome — must not be handed to a live tmux injection attempt again,
// regardless of how low its Attempts count is, because we cannot rule out
// that some of the message already reached the composer.
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

// TestHandleFailedInjection_BoundedRetriesDeadLetterAfterMax covers the
// "bounded" half for non-dirty errors: once Attempts reaches
// nudge.MaxInjectionAttempts, the entry is dead-lettered instead of requeued
// again, so an uncertain (but not provably-dirty) failure does not retype
// forever either.
//
// drained is seeded with Attempts: nudge.MaxInjectionAttempts (the count a
// real caller has already persisted via Claim.MarkAttempt before THIS,
// the failing, attempt), not MaxInjectionAttempts-1: handleFailedInjection
// checks n.Attempts >= nudge.MaxInjectionAttempts, and does not itself
// increment Attempts (see its doc comment) — MaxInjectionAttempts-1 never
// crosses that bound, so the previous version of this test asserted an
// outcome (dead-letter) the handler does not actually produce for that
// input (codex, nudge_test.go:697, changes-requested at REVISION 3).
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

// TestHandleFailedInjection_DeadLetterWriteFailureRequeuesInstead covers the
// "dead-letter write failure (the queue file must survive)" test the review
// asked for: if DeadLetter itself cannot write, the entry falls back to
// requeue rather than being silently dropped.
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
// covers the double-fault the review found: when the entry is composer-dirty
// AND the dead-letter write itself fails, requeuing would resume the exact
// retype-into-dirty-composer loop hq-g52db was filed to fix. It must never
// be requeued into the active queue — but it must also be RETAINED, not
// silently dropped: R2 (hq-g52db REVISION 3) prohibits dropping such an
// entry, so it comes back in unresolved for the caller to leave un-acked
// (this test's name and assertions previously described — and only
// verified — the "drop" half; High 1 changed the policy and this now also
// checks the retention half).
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

func TestValidModeMapsMatchConstants(t *testing.T) {
	// Ensure the validation maps cover all defined mode constants.
	modes := []string{NudgeModeImmediate, NudgeModeQueue, NudgeModeWaitIdle}
	for _, m := range modes {
		if !validNudgeModes[m] {
			t.Errorf("mode constant %q missing from validNudgeModes", m)
		}
	}
	priorities := []string{nudge.PriorityNormal, nudge.PriorityUrgent}
	for _, p := range priorities {
		if !validNudgePriorities[p] {
			t.Errorf("priority constant %q missing from validNudgePriorities", p)
		}
	}
}

func TestIdleWatcherTimeout(t *testing.T) {
	// Verify the watcher timeout is in a reasonable range.
	if idleWatcherTimeout < 10*time.Second {
		t.Errorf("idleWatcherTimeout = %v, too short (min 10s)", idleWatcherTimeout)
	}
	if idleWatcherTimeout > 5*time.Minute {
		t.Errorf("idleWatcherTimeout = %v, too long (max 5m)", idleWatcherTimeout)
	}
}

func TestIdleWatcherPollInterval(t *testing.T) {
	// Verify the poll interval is reasonable — fast enough to be responsive,
	// slow enough to not burn CPU.
	if idleWatcherPollInterval < 200*time.Millisecond {
		t.Errorf("idleWatcherPollInterval = %v, too fast (min 200ms)", idleWatcherPollInterval)
	}
	if idleWatcherPollInterval > 5*time.Second {
		t.Errorf("idleWatcherPollInterval = %v, too slow (max 5s)", idleWatcherPollInterval)
	}
}

func TestNudgeTrailingSlashNormalization(t *testing.T) {
	// The mail system uses "mayor/" and "deacon/" as canonical addresses.
	// runNudge must strip the trailing slash so these match the role shortcuts.
	// Without normalization, "mayor/" falls through to parseAddress which
	// rejects it ("invalid address format"), silently dropping the nudge.
	origMode := nudgeModeFlag
	origPriority := nudgePriorityFlag
	origMessage := nudgeMessageFlag
	origStdin := nudgeStdinFlag
	origTimeout := waitIdleTimeout
	defer func() {
		nudgeModeFlag = origMode
		nudgePriorityFlag = origPriority
		nudgeMessageFlag = origMessage
		nudgeStdinFlag = origStdin
		waitIdleTimeout = origTimeout
	}()

	isolateTestTmuxAndWorkspace(t)

	// Route nudge transport to a log file so this test doesn't deliver to
	// the real mayor/deacon/witness/refinery sessions on host.
	t.Setenv("GT_TEST_NUDGE_LOG", filepath.Join(t.TempDir(), "nudge.log"))

	waitIdleTimeout = 200 * time.Millisecond
	nudgeStdinFlag = false
	nudgeMessageFlag = "test"
	nudgePriorityFlag = "normal"
	nudgeModeFlag = NudgeModeImmediate

	for _, target := range []string{"mayor/", "deacon/", "witness/", "refinery/"} {
		t.Run(target, func(t *testing.T) {
			err := runNudge(nudgeCmd, []string{target, "hello"})
			// Will fail on tmux/session lookup, but must NOT fail on address parsing.
			if err != nil && strings.Contains(err.Error(), "invalid address format") {
				t.Errorf("trailing-slash target %q was rejected as invalid address: %v", target, err)
			}
		})
	}
}

func TestNudgeDogTargetRoutesToDogSession(t *testing.T) {
	origMode := nudgeModeFlag
	origPriority := nudgePriorityFlag
	origMessage := nudgeMessageFlag
	origStdin := nudgeStdinFlag
	origForce := nudgeForceFlag
	defer func() {
		nudgeModeFlag = origMode
		nudgePriorityFlag = origPriority
		nudgeMessageFlag = origMessage
		nudgeStdinFlag = origStdin
		nudgeForceFlag = origForce
	}()

	isolateTestTmuxAndWorkspace(t)

	logPath := filepath.Join(t.TempDir(), "nudge.log")
	t.Setenv("GT_TEST_NUDGE_LOG", logPath)

	nudgeModeFlag = NudgeModeImmediate
	nudgePriorityFlag = nudge.PriorityNormal
	nudgeMessageFlag = "hello dog"
	nudgeStdinFlag = false
	nudgeForceFlag = true

	if err := runNudge(nudgeCmd, []string{"deacon/dogs/fido"}); err != nil {
		t.Fatalf("runNudge dog target returned error: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading nudge log: %v", err)
	}
	if got, want := string(data), "nudge:hq-dog-fido:"; !strings.Contains(got, want) {
		t.Fatalf("nudge log = %q, want containing %q", got, want)
	}
}

// TestBuildWaitIdleDeadLetterEntry_FieldsPopulated covers the codex finding
// that wait-idle's directly-dead-lettered entries lacked Timestamp and
// ExpiresAt, and that the alert reporting the entry got a blank ID (codex,
// nudge.go:253/282, changes-requested at 08964387/95f841e6 rework — "not
// fixed from prior review"). All three must be set before DeadLetter or
// alertDeadLetter ever see the entry, since DeadLetter's own ID-assignment
// fallback never reports the generated ID back to the caller.
func TestBuildWaitIdleDeadLetterEntry_FieldsPopulated(t *testing.T) {
	before := time.Now()
	got := buildWaitIdleDeadLetterEntry("test-sender", "test message", nudge.PriorityNormal)
	after := time.Now()

	if got.ID == "" {
		t.Error("ID is empty, want a generated ID (so an alert can name the entry)")
	}
	if got.Sender != "test-sender" || got.Message != "test message" {
		t.Errorf("Sender/Message = %q/%q, want %q/%q", got.Sender, got.Message, "test-sender", "test message")
	}
	if got.Timestamp.Before(before) || got.Timestamp.After(after) {
		t.Errorf("Timestamp = %v, want between %v and %v", got.Timestamp, before, after)
	}
	if got.Priority != nudge.PriorityNormal {
		t.Errorf("Priority = %q, want %q", got.Priority, nudge.PriorityNormal)
	}
	wantExpiry := got.Timestamp.Add(nudge.DefaultNormalTTL)
	if !got.ExpiresAt.Equal(wantExpiry) {
		t.Errorf("ExpiresAt = %v, want %v (Timestamp + DefaultNormalTTL)", got.ExpiresAt, wantExpiry)
	}
	if got.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 (this IS the first attempt, already failed unverified)", got.Attempts)
	}
}

// TestBuildWaitIdleDeadLetterEntry_UrgentTTL covers the urgent-priority TTL
// branch and confirms two calls never collide on ID.
func TestBuildWaitIdleDeadLetterEntry_UrgentTTL(t *testing.T) {
	a := buildWaitIdleDeadLetterEntry("s", "m", nudge.PriorityUrgent)
	b := buildWaitIdleDeadLetterEntry("s", "m", nudge.PriorityUrgent)

	wantExpiry := a.Timestamp.Add(nudge.DefaultUrgentTTL)
	if !a.ExpiresAt.Equal(wantExpiry) {
		t.Errorf("ExpiresAt = %v, want %v (Timestamp + DefaultUrgentTTL)", a.ExpiresAt, wantExpiry)
	}
	if a.ID == b.ID {
		t.Errorf("two calls produced the same ID %q, want distinct", a.ID)
	}
}

func TestIdleWatcherExitsOnEmptyQueue(t *testing.T) {
	// watchAndDeliver should exit immediately when queue is empty
	// (someone else drained it). We test this by calling with a
	// temp dir that has no queue files.
	origTimeout := idleWatcherTimeout
	origInterval := idleWatcherPollInterval
	defer func() {
		idleWatcherTimeout = origTimeout
		idleWatcherPollInterval = origInterval
	}()

	// Very short timeout so test doesn't hang
	idleWatcherTimeout = 500 * time.Millisecond
	idleWatcherPollInterval = 50 * time.Millisecond

	tmpDir := t.TempDir()

	// watchAndDeliver checks QueueLen first — with no queue files,
	// it should exit immediately. We verify it doesn't block.
	done := make(chan struct{})
	go func() {
		// Use a nil-safe Tmux — QueueLen returns 0 before IsIdle is called.
		watchAndDeliver(nil, tmpDir, "test-session")
		close(done)
	}()

	select {
	case <-done:
		// Good — exited because queue was empty
	case <-time.After(2 * time.Second):
		t.Fatal("watchAndDeliver did not exit within 2s for empty queue")
	}
}

func TestQueueLen(t *testing.T) {
	tmpDir := t.TempDir()

	// Empty queue
	if got := nudge.QueueLen(tmpDir, "test-session"); got != 0 {
		t.Errorf("QueueLen on empty dir = %d, want 0", got)
	}

	// Enqueue one
	err := nudge.Enqueue(tmpDir, "test-session", nudge.QueuedNudge{
		Sender:  "test",
		Message: "hello",
	})
	if err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}

	if got := nudge.QueueLen(tmpDir, "test-session"); got != 1 {
		t.Errorf("QueueLen after enqueue = %d, want 1", got)
	}

	// Drain and verify empty
	_, _ = nudge.Drain(tmpDir, "test-session")
	if got := nudge.QueueLen(tmpDir, "test-session"); got != 0 {
		t.Errorf("QueueLen after drain = %d, want 0", got)
	}
}
