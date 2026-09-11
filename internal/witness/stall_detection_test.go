package witness

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// These tests cover the both-polarities requirements from gtn-k43 / hq-ooijo:
// a working (even stale-heartbeat) session, a busy/background session, and a
// detached-but-active session must receive ZERO keys; a session showing a
// real, specific dialog gets dismissed; a malformed heartbeat file must not
// crash or misclassify; and --dry-run must never send a key.

func requireTmuxForStallTests(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
}

// stallTestFixture sets up a temp town with one rig, one polecat directory,
// and a live tmux session whose pane process is recognized as "the agent"
// (via GT_PROCESS_NAMES) so DetectStalledPolecats reaches the code under
// test instead of short-circuiting on session-dead / agent-dead.
type stallTestFixture struct {
	townRoot    string
	rigName     string
	polecatName string
	sessionName string
	tm          *tmux.Tmux
}

func newStallTestFixture(t *testing.T, stallThreshold, activityGrace string) *stallTestFixture {
	t.Helper()
	requireTmuxForStallTests(t)

	townRoot := t.TempDir()
	rigName := "testrig"
	polecatName := "alpha"

	polecatsDir := filepath.Join(townRoot, rigName, "polecats")
	if err := os.MkdirAll(filepath.Join(polecatsDir, polecatName), 0o755); err != nil {
		t.Fatal(err)
	}

	// Fast thresholds so tests don't wait on the 90s/60s production defaults.
	writeTestOperationalConfig(t, townRoot, stallThreshold, activityGrace)

	// DetectStalledPolecats calls session.InitRegistry(townRoot) internally,
	// which derives a PER-TOWN tmux socket from townRoot's path and sets it
	// as the process-wide default (tmux.SetDefaultSocket). Doing the same
	// here, before creating the session, ensures this fixture's session and
	// DetectStalledPolecats' own tmux.NewTmux() resolve to the SAME socket —
	// otherwise the session is created on whatever socket a PRIOR test left
	// active and becomes invisible once this call flips the global socket.
	if err := session.InitRegistry(townRoot); err != nil {
		t.Logf("session.InitRegistry (non-fatal, expected for a bare temp town): %v", err)
	}

	sessionName := session.PolecatSessionName(session.PrefixFor(rigName), polecatName)

	tm := tmux.NewTmux()
	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillSession(sessionName) })

	// The pane runs a plain shell, not a real agent. Declare it as the
	// recognized "agent" process so IsAgentAlive reports true and detection
	// reaches the stall-classification logic under test.
	if err := tm.SetEnvironment(sessionName, "GT_PROCESS_NAMES", "zsh,bash,sh"); err != nil {
		t.Fatalf("SetEnvironment GT_PROCESS_NAMES: %v", err)
	}

	return &stallTestFixture{
		townRoot:    townRoot,
		rigName:     rigName,
		polecatName: polecatName,
		sessionName: sessionName,
		tm:          tm,
	}
}

func writeTestOperationalConfig(t *testing.T, townRoot, stallThreshold, activityGrace string) {
	t.Helper()
	settingsDir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"operational": map[string]any{
			"witness": map[string]any{
				"startup_stall_threshold": stallThreshold,
				"startup_activity_grace":  activityGrace,
			},
		},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeTestHeartbeat writes a heartbeat file directly (bypassing
// TouchSessionHeartbeat, which always stamps time.Now()) so tests can
// construct a STALE heartbeat reporting a specific state.
func writeTestHeartbeat(t *testing.T, townRoot, sessionName string, ts time.Time, state polecat.HeartbeatState) {
	t.Helper()
	dir := filepath.Join(townRoot, ".runtime", "heartbeats")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	hb := polecat.SessionHeartbeat{Timestamp: ts, State: state}
	data, err := json.Marshal(hb)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionName+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeMalformedHeartbeat(t *testing.T, townRoot, sessionName string) {
	t.Helper()
	dir := filepath.Join(townRoot, ".runtime", "heartbeats")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionName+".json"), []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDetectStalledPolecats_StaleWorkingHeartbeat_ZeroKeys is the "stale-working"
// polarity: a heartbeat that is far past SessionHeartbeatStaleThreshold but
// still reports state=working must never be remediated. The heartbeat only
// advances on a `gt` command invocation, so a long tool-only turn goes stale
// while the agent is genuinely busy (gtn-k43 revision 2).
func TestDetectStalledPolecats_StaleWorkingHeartbeat_ZeroKeys(t *testing.T) {
	f := newStallTestFixture(t, "0s", "0s")
	writeTestHeartbeat(t, f.townRoot, f.sessionName, time.Now().Add(-1*time.Hour), polecat.HeartbeatWorking)

	// Give the fast thresholds room to be satisfied were the heartbeat
	// check absent, so a failure here would be a false negative, not a race.
	time.Sleep(50 * time.Millisecond)

	result := DetectStalledPolecats(f.townRoot, f.rigName, false)
	if result.Checked != 1 {
		t.Fatalf("Checked = %d, want 1", result.Checked)
	}
	if len(result.Stalled) != 0 {
		t.Errorf("Stalled = %+v, want empty (stale-working heartbeat must never be remediated)", result.Stalled)
	}
}

// TestDetectStalledPolecats_BackgroundTaskHint_ZeroKeys is the
// "busy-or-background-task" polarity: a pane showing tmux's own background
// task hint must never be remediated, even with no heartbeat at all.
func TestDetectStalledPolecats_BackgroundTaskHint_ZeroKeys(t *testing.T) {
	f := newStallTestFixture(t, "0s", "0s")

	// `read` keeps the hint as the pane's last content indefinitely.
	if err := f.tm.SendKeys(f.sessionName, "clear; printf '%s\\n' 'Running in the background (down-arrow to manage)'; read -r _bg"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	result := DetectStalledPolecats(f.townRoot, f.rigName, false)
	if len(result.Stalled) != 0 {
		t.Errorf("Stalled = %+v, want empty (background-task hint must never be remediated)", result.Stalled)
	}
}

// TestDetectStalledPolecats_RecentWindowActivity_ZeroKeys is the
// "detached-healthy" polarity: a session whose window_activity is recent
// must never be remediated, regardless of session_activity (which the
// legacy check used, and which freezes on detached sessions — hq-wisp-y46vn).
func TestDetectStalledPolecats_RecentWindowActivity_ZeroKeys(t *testing.T) {
	f := newStallTestFixture(t, "0s", "10m") // activity grace far in the future

	result := DetectStalledPolecats(f.townRoot, f.rigName, false)
	if len(result.Stalled) != 0 {
		t.Errorf("Stalled = %+v, want empty (recent window_activity must never be remediated)", result.Stalled)
	}
}

// TestDetectStalledPolecats_MalformedHeartbeat_FallsThroughSafely covers the
// malformed-heartbeat-file polarity: ReadSessionHeartbeat returns nil on
// unmarshal error, so a corrupt file must behave exactly like no file at
// all — falling through to content-based checks, never crashing, and never
// being treated as evidence of "working".
func TestDetectStalledPolecats_MalformedHeartbeat_FallsThroughSafely(t *testing.T) {
	f := newStallTestFixture(t, "0s", "0s")
	writeMalformedHeartbeat(t, f.townRoot, f.sessionName)
	time.Sleep(50 * time.Millisecond)

	result := DetectStalledPolecats(f.townRoot, f.rigName, false)
	if result.Checked != 1 {
		t.Fatalf("Checked = %d, want 1", result.Checked)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("Errors = %v, want none (malformed heartbeat must not error/crash)", result.Errors)
	}
	// No dialog is visible (plain idle shell), so it must be reported as
	// "no-known-dialog" — observed, zero keys sent — not silently dropped
	// and not treated as a working heartbeat.
	if len(result.Stalled) != 1 || result.Stalled[0].Action != "no-known-dialog" {
		t.Errorf("Stalled = %+v, want one entry with Action=no-known-dialog", result.Stalled)
	}
}

// TestDetectStalledPolecats_NoKnownDialog_ReportsWithoutActing verifies that
// window silence alone never authorises input: an old, silent session with
// no recognizable dialog text is reported for visibility but receives no keys.
func TestDetectStalledPolecats_NoKnownDialog_ReportsWithoutActing(t *testing.T) {
	f := newStallTestFixture(t, "0s", "0s")
	time.Sleep(50 * time.Millisecond)

	before, err := f.tm.CapturePane(f.sessionName, 30)
	if err != nil {
		t.Fatalf("CapturePane (before): %v", err)
	}

	result := DetectStalledPolecats(f.townRoot, f.rigName, false)

	after, err := f.tm.CapturePane(f.sessionName, 30)
	if err != nil {
		t.Fatalf("CapturePane (after): %v", err)
	}
	if before != after {
		t.Errorf("pane content changed with no known dialog present — a key was sent\nbefore: %q\nafter:  %q", before, after)
	}
	if len(result.Stalled) != 1 || result.Stalled[0].Action != "no-known-dialog" {
		t.Errorf("Stalled = %+v, want one entry with Action=no-known-dialog", result.Stalled)
	}
}

// TestDetectStalledPolecats_KnownDialog_DismissedAndReported is the positive
// case: a real, specific dialog is detected and dismissed with its own keys.
func TestDetectStalledPolecats_KnownDialog_DismissedAndReported(t *testing.T) {
	f := newStallTestFixture(t, "0s", "0s")

	if err := f.tm.SendKeys(f.sessionName, "clear; printf '%s\\n' 'Quick safety check - do you trust this folder?'; read -r _dlg; clear; echo dialog-dismissed"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	result := DetectStalledPolecats(f.townRoot, f.rigName, false)
	if len(result.Stalled) != 1 {
		t.Fatalf("Stalled = %+v, want exactly one entry", result.Stalled)
	}
	got := result.Stalled[0]
	if got.Action != "auto-dismissed:workspace-trust" {
		t.Errorf("Action = %q, want %q", got.Action, "auto-dismissed:workspace-trust")
	}
	if got.Error != nil {
		t.Errorf("Error = %v, want nil", got.Error)
	}
}

// TestDetectStalledPolecats_DryRun_NeverSendsKeys verifies dryRun classifies
// and reports a real dialog without ever sending its dismiss keys.
func TestDetectStalledPolecats_DryRun_NeverSendsKeys(t *testing.T) {
	f := newStallTestFixture(t, "0s", "0s")

	if err := f.tm.SendKeys(f.sessionName, "clear; printf '%s\\n' 'Quick safety check - do you trust this folder?'; read -r _dlg; clear; echo dialog-dismissed"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	time.Sleep(600 * time.Millisecond)

	result := DetectStalledPolecats(f.townRoot, f.rigName, true)
	if len(result.Stalled) != 1 {
		t.Fatalf("Stalled = %+v, want exactly one entry", result.Stalled)
	}
	if result.Stalled[0].Action != "would-dismiss:workspace-trust" {
		t.Errorf("Action = %q, want %q", result.Stalled[0].Action, "would-dismiss:workspace-trust")
	}

	// The dialog must still be showing — dry-run sent no keys.
	kind, err := f.tm.ClassifyVisibleDialog(f.sessionName)
	if err != nil {
		t.Fatalf("ClassifyVisibleDialog: %v", err)
	}
	if kind != tmux.DialogWorkspaceTrust {
		t.Errorf("dialog no longer visible after dry-run — a key was sent (kind=%q)", kind)
	}
}
