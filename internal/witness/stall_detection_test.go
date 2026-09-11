package witness

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

	// Force an ISOLATED, test-private tmux socket. session.InitRegistry
	// (called both here and internally by DetectStalledPolecats) derives its
	// socket from townRoot's path ONLY when GT_TMUX_SOCKET is unset/
	// "default"/"auto" (internal/session/registry.go). A GT_TMUX_SOCKET
	// inherited from the real enclosing session — e.g. this test running
	// inside a live polecat, which is exactly how these tests are normally
	// run — is used AS-IS instead, which can silently target the REAL
	// town's tmux server: an unregistered rig name resolves to the "gt"
	// legacy prefix, so PolecatSessionName produces "gt-alpha" — a name a
	// real session can hold. Clearing the env var here forces every
	// InitRegistry call in this test (this one and DetectStalledPolecats'
	// own internal one) to fall through to the per-townRoot derived socket,
	// which is unique to this test's TempDir and touches nothing real
	// (gtn-m7s / hq-ooijo revision 2, codex finding on this file:64).
	t.Setenv("GT_TMUX_SOCKET", "")

	// DetectStalledPolecats calls session.InitRegistry(townRoot) internally,
	// which sets the resolved socket as the process-wide default
	// (tmux.SetDefaultSocket). Doing the same here, before creating the
	// session, ensures this fixture's session and DetectStalledPolecats' own
	// tmux.NewTmux() resolve to the SAME socket.
	if err := session.InitRegistry(townRoot); err != nil {
		t.Logf("session.InitRegistry (non-fatal, expected for a bare temp town): %v", err)
	}

	sessionName := session.PolecatSessionName(session.PrefixFor(rigName), polecatName)

	tm := tmux.NewTmux()
	// Defense in depth beyond the socket isolation above: never kill or
	// reuse a session this fixture did not create itself. If the resolved
	// name already exists, something about the isolation assumption above
	// is wrong, and blindly killing it (the prior behavior) is exactly the
	// hazard this fixes — it could be a real gt-alpha/gt-bravo session.
	if alive, _ := tm.HasSession(sessionName); alive {
		t.Fatalf("refusing to reuse pre-existing tmux session %q — expected a fresh isolated socket", sessionName)
	}
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
// construct a STALE heartbeat reporting a specific state. It stamps the
// CURRENT live session's real incarnation (session_created), so it tests
// the intended scenario — a stale-but-working heartbeat for THIS session,
// not a reused/dead one — matching MatchesIncarnation's binding (gtn-m7s /
// hq-ooijo revision 2). Use writeTestHeartbeatWithIncarnation directly to
// construct a mismatched-incarnation fixture.
func writeTestHeartbeat(t *testing.T, townRoot, sessionName string, ts time.Time, state polecat.HeartbeatState) {
	t.Helper()
	created, err := tmux.NewTmux().GetSessionCreatedUnix(sessionName)
	if err != nil {
		t.Fatalf("GetSessionCreatedUnix(%s): %v", sessionName, err)
	}
	writeTestHeartbeatWithIncarnation(t, townRoot, sessionName, ts, state, created)
}

// writeTestHeartbeatWithIncarnation is writeTestHeartbeat with an explicit
// Incarnation value, letting a test construct a heartbeat that belongs to a
// DIFFERENT (or unknown, 0) session incarnation than the live session it's
// written for.
func writeTestHeartbeatWithIncarnation(t *testing.T, townRoot, sessionName string, ts time.Time, state polecat.HeartbeatState, incarnation int64) {
	t.Helper()
	dir := filepath.Join(townRoot, ".runtime", "heartbeats")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	hb := polecat.SessionHeartbeat{Timestamp: ts, State: state, Incarnation: incarnation}
	data, err := json.Marshal(hb)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionName+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeTestStartupHeartbeat writes a heartbeat carrying
// polecat.StartupHeartbeatContext — the launcher's own untouched placeholder
// (session_manager.go), as opposed to one a `gt` command's persistentPreRun
// has since overwritten. Stamps the live session's real incarnation so
// MatchesIncarnation passes, isolating the Context distinction under test.
func writeTestStartupHeartbeat(t *testing.T, townRoot, sessionName string, ts time.Time, state polecat.HeartbeatState) {
	t.Helper()
	created, err := tmux.NewTmux().GetSessionCreatedUnix(sessionName)
	if err != nil {
		t.Fatalf("GetSessionCreatedUnix(%s): %v", sessionName, err)
	}
	dir := filepath.Join(townRoot, ".runtime", "heartbeats")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	hb := polecat.SessionHeartbeat{
		Timestamp:   ts,
		State:       state,
		Context:     polecat.StartupHeartbeatContext,
		Incarnation: created,
	}
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

// TestDetectStalledPolecats_StaleStartupHeartbeat_NotSuppressed is the sibling
// polarity to the test above, for the codex Medium finding on
// handlers.go:2357: a heartbeat still carrying StartupHeartbeatContext — the
// launcher's own placeholder, never overwritten by a `gt` command — must NOT
// get the same indefinite trust as a genuinely agent-driven stale-working
// heartbeat. AcceptStartupDialogs/WaitForRuntimeReady are non-fatal, so this
// is exactly what a session parked on an unhandled startup dialog looks
// like: state=working forever, with nobody ever having run a `gt` command to
// prove it. It must fall through to the dialog-content check below instead
// of being skipped forever.
func TestDetectStalledPolecats_StaleStartupHeartbeat_NotSuppressed(t *testing.T) {
	f := newStallTestFixture(t, "0s", "0s")
	writeTestStartupHeartbeat(t, f.townRoot, f.sessionName, time.Now().Add(-1*time.Hour), polecat.HeartbeatWorking)
	time.Sleep(50 * time.Millisecond)

	result := DetectStalledPolecats(f.townRoot, f.rigName, false)
	if result.Checked != 1 {
		t.Fatalf("Checked = %d, want 1", result.Checked)
	}
	// No known dialog is showing (plain idle shell), so a correctly
	// distrusted startup placeholder must fall through and be reported —
	// not silently skipped as if it were agent-verified working.
	if len(result.Stalled) != 1 || result.Stalled[0].Action != "no-known-dialog" {
		t.Errorf("Stalled = %+v, want one entry with Action=no-known-dialog "+
			"(an untouched startup heartbeat must not suppress detection forever)", result.Stalled)
	}
}

// TestDetectStalledPolecats_StaleStartupHeartbeat_ComposerQuotesDialogText_ZeroKeys
// is the end-to-end danger scenario codex named for the High finding (codex,
// tmux.go:2175/:2355, REVISION 3): "with the start-up placeholder present ...
// Enter or Down/Enter can reach a WORKING composer." A session that never ran
// a single `gt` command has its startup-placeholder heartbeat correctly
// distrusted (the sibling test above) and reaches dialog classification —
// but the pane merely holds a composer QUOTING dialog marker text (numbered,
// as a user might type it), never having shown the real dialog at all. The
// fixed classifier must read this as DialogNone; before the fix it
// misclassified the composer's own text as a still-showing dialog and this
// test would receive real dismiss keys into that composer.
func TestDetectStalledPolecats_StaleStartupHeartbeat_ComposerQuotesDialogText_ZeroKeys(t *testing.T) {
	f := newStallTestFixture(t, "0s", "0s")
	writeTestStartupHeartbeat(t, f.townRoot, f.sessionName, time.Now().Add(-1*time.Hour), polecat.HeartbeatWorking)

	// `read` keeps the composer's quoted text as the pane's last content
	// indefinitely, same technique as the classifier's own live-tmux tests.
	if err := f.tm.SendKeys(f.sessionName, "clear; printf '%s' '› 1. Explain Quick safety check'; read -r _dlg"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

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
		t.Errorf("pane content changed — a key reached the composer quoting dialog text\nbefore: %q\nafter:  %q", before, after)
	}
	if len(result.Stalled) != 1 || result.Stalled[0].Action != "no-known-dialog" {
		t.Errorf("Stalled = %+v, want one entry with Action=no-known-dialog "+
			"(numbered composer text quoting a dialog marker must never classify as that dialog)", result.Stalled)
	}
}

// TestDetectStalledPolecats_StaleWorkingHeartbeat_WrongIncarnation_NotSuppressed
// is the merge risk named on hq-ooijo revision 2: a tmux session name can be
// REUSED (the old session dies, a new one is created with the identical
// name). A stale "working" heartbeat left by the DEAD incarnation must not
// suppress recovery for the new one — unlike the same-incarnation case above,
// which must still be suppressed. Constructed with an Incarnation value that
// does not match this fixture's actual live session (an arbitrary unix time
// far from now), simulating exactly that: a heartbeat file that predates the
// current session.
func TestDetectStalledPolecats_StaleWorkingHeartbeat_WrongIncarnation_NotSuppressed(t *testing.T) {
	f := newStallTestFixture(t, "0s", "0s")
	writeTestHeartbeatWithIncarnation(t, f.townRoot, f.sessionName,
		time.Now().Add(-1*time.Hour), polecat.HeartbeatWorking, 1)
	time.Sleep(50 * time.Millisecond)

	result := DetectStalledPolecats(f.townRoot, f.rigName, false)
	if result.Checked != 1 {
		t.Fatalf("Checked = %d, want 1", result.Checked)
	}
	// The session shows no known dialog (plain idle shell), so with the
	// mismatched-incarnation heartbeat correctly distrusted it must fall
	// through to content/activity checks and be reported — not silently
	// skipped as if the stale "working" heartbeat were trustworthy.
	if len(result.Stalled) != 1 || result.Stalled[0].Action != "no-known-dialog" {
		t.Errorf("Stalled = %+v, want one entry with Action=no-known-dialog "+
			"(a heartbeat from a different session incarnation must not suppress detection)", result.Stalled)
	}
}

// TestDetectStalledPolecats_FreshHeartbeat_WrongIncarnation_NotSuppressed
// covers the fresh-but-wrong-incarnation case: even a heartbeat with a
// CURRENT timestamp must not be trusted if it was not written for this
// session's incarnation — a dead session's very last heartbeat write can be
// timestamped moments before the new session (same name) comes up.
func TestDetectStalledPolecats_FreshHeartbeat_WrongIncarnation_NotSuppressed(t *testing.T) {
	f := newStallTestFixture(t, "0s", "0s")
	writeTestHeartbeatWithIncarnation(t, f.townRoot, f.sessionName,
		time.Now(), polecat.HeartbeatWorking, 1)
	time.Sleep(50 * time.Millisecond)

	result := DetectStalledPolecats(f.townRoot, f.rigName, false)
	if len(result.Stalled) != 1 || result.Stalled[0].Action != "no-known-dialog" {
		t.Errorf("Stalled = %+v, want one entry with Action=no-known-dialog "+
			"(a fresh-looking heartbeat from a different incarnation must still not suppress detection)", result.Stalled)
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

// TestDetectStalledPolecats_BackgroundTaskHint_ResolvesAgentPane_NotActiveWindow
// is the agent-pane-resolution polarity: the busy and activity checks must
// read the resolved AGENT pane (via GT_PANE_ID), never whichever pane or
// window merely happens to be ACTIVE in the session. A second window is
// added and declared the agent pane via GT_PANE_ID; it shows the
// background-task hint. Window 0 — the fixture's original, genuinely idle
// pane — is left selected as the session's active window. If the checks
// read the active window instead of resolving the agent pane, they would
// see a blank idle pane and miss the hint entirely (gtn-m7s / hq-ooijo
// revision 2, codex finding on handlers.go:2348).
func TestDetectStalledPolecats_BackgroundTaskHint_ResolvesAgentPane_NotActiveWindow(t *testing.T) {
	f := newStallTestFixture(t, "0s", "0s")
	socket := tmux.GetDefaultSocket()

	if out, err := exec.Command("tmux", "-u", "-L", socket, "new-window", "-d", "-t", f.sessionName, "-n", "agent").CombinedOutput(); err != nil {
		t.Fatalf("tmux new-window: %v (%s)", err, out)
	}
	agentPaneOut, err := exec.Command("tmux", "-u", "-L", socket, "display-message", "-t", f.sessionName+":1", "-p", "#{pane_id}").Output()
	if err != nil {
		t.Fatalf("resolving new window's pane id: %v", err)
	}
	agentPane := strings.TrimSpace(string(agentPaneOut))

	// Declare window 1's pane as the agent pane (mirrors GT_PANE_ID set at
	// real session startup, gt-qmsx).
	if err := f.tm.SetEnvironment(f.sessionName, "GT_PANE_ID", agentPane); err != nil {
		t.Fatalf("SetEnvironment GT_PANE_ID: %v", err)
	}

	// Background-task hint lives in the agent's pane (window 1); `read`
	// keeps it as the pane's last content indefinitely.
	if out, err := exec.Command("tmux", "-u", "-L", socket, "send-keys", "-t", agentPane,
		"clear; printf '%s\\n' 'Running in the background (down-arrow to manage)'; read -r _bg", "Enter").CombinedOutput(); err != nil {
		t.Fatalf("send-keys to agent pane: %v (%s)", err, out)
	}

	// Window 0 — blank, genuinely idle — is left as the session's ACTIVE
	// window, mirroring an idle auxiliary window being selected while the
	// agent works elsewhere.
	if out, err := exec.Command("tmux", "-u", "-L", socket, "select-window", "-t", f.sessionName+":0").CombinedOutput(); err != nil {
		t.Fatalf("select-window 0: %v (%s)", err, out)
	}
	time.Sleep(300 * time.Millisecond)

	result := DetectStalledPolecats(f.townRoot, f.rigName, false)
	if len(result.Stalled) != 0 {
		t.Errorf("Stalled = %+v, want empty — the agent pane (window 1) shows a "+
			"background-task hint even though the idle window 0 is active", result.Stalled)
	}
}

// TestDetectStalledPolecats_WindowActivity_ResolvesAgentPane_NotActiveWindow
// is the agent-pane-resolution polarity for GetWindowActivity specifically
// (codex Medium, stall_detection_test.go:341, REVISION 3): "no test
// independently covers agent-window activity targeting. The agent-pane test
// exits on its background hint before it reads activity, so reverting only
// GetWindowActivity to the active window stays green." The background-hint
// test above short-circuits at the busy-hint check before ever reaching
// GetWindowActivity, so it cannot catch a regression there. This test
// carries NO busy hint on either window, forcing detection past the busy
// check and into the activity read itself: window 0 (the session's ACTIVE
// window) is left stale past the grace period while window 1 (the resolved
// AGENT pane, via GT_PANE_ID) gets fresh output just before the check runs.
// Only a check that resolves the AGENT pane — not whichever window merely
// happens to be active — reads the fresh timestamp and stays quiet.
func TestDetectStalledPolecats_WindowActivity_ResolvesAgentPane_NotActiveWindow(t *testing.T) {
	f := newStallTestFixture(t, "0s", "3s")
	socket := tmux.GetDefaultSocket()

	if out, err := exec.Command("tmux", "-u", "-L", socket, "new-window", "-d", "-t", f.sessionName, "-n", "agent").CombinedOutput(); err != nil {
		t.Fatalf("tmux new-window: %v (%s)", err, out)
	}
	agentPaneOut, err := exec.Command("tmux", "-u", "-L", socket, "display-message", "-t", f.sessionName+":1", "-p", "#{pane_id}").Output()
	if err != nil {
		t.Fatalf("resolving new window's pane id: %v", err)
	}
	agentPane := strings.TrimSpace(string(agentPaneOut))

	// Declare window 1's pane as the agent pane (mirrors GT_PANE_ID set at
	// real session startup, gt-qmsx).
	if err := f.tm.SetEnvironment(f.sessionName, "GT_PANE_ID", agentPane); err != nil {
		t.Fatalf("SetEnvironment GT_PANE_ID: %v", err)
	}

	// Both windows are freshly created and silent right now. Sleep past the
	// 3s grace so both start out stale — same discriminating pattern as
	// TestDetectStalledPolecats_StaleSessionActivity_RecentWindowActivity_ZeroKeys.
	time.Sleep(3500 * time.Millisecond)

	// Window 0 — blank, genuinely idle — is left as the session's ACTIVE
	// window, mirroring an idle auxiliary window being selected while the
	// agent works elsewhere.
	if out, err := exec.Command("tmux", "-u", "-L", socket, "select-window", "-t", f.sessionName+":0").CombinedOutput(); err != nil {
		t.Fatalf("select-window 0: %v (%s)", err, out)
	}

	// Only the AGENT pane (window 1) produces fresh output.
	if out, err := exec.Command("tmux", "-u", "-L", socket, "send-keys", "-t", agentPane,
		"echo fresh-output-on-agent-pane", "Enter").CombinedOutput(); err != nil {
		t.Fatalf("send-keys to agent pane: %v (%s)", err, out)
	}
	time.Sleep(1500 * time.Millisecond)

	result := DetectStalledPolecats(f.townRoot, f.rigName, false)
	if len(result.Stalled) != 0 {
		t.Errorf("Stalled = %+v, want empty — the AGENT pane (window 1) has fresh "+
			"activity even though the idle, stale window 0 is active; a check "+
			"reading the active window instead of the resolved agent pane would "+
			"wrongly report this as stalled", result.Stalled)
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

// TestDetectStalledPolecats_StaleSessionActivity_RecentWindowActivity_ZeroKeys
// is the DISCRIMINATING version of the test above. With a 10-minute grace,
// session_activity (frozen at session_created on every detached session,
// hq-wisp-y46vn) is ALSO within grace immediately after creation, so that
// test passes even if the code still keyed on session_activity — exactly
// the bug this fix replaces (codex finding on this file: a test must be
// able to fail). Here session_activity is made genuinely stale by a real
// sleep past a short grace, then the pane produces output, which advances
// window_activity but — per hq-wisp-y46vn — not session_activity on a
// detached session. Only a check actually keyed on window_activity passes.
func TestDetectStalledPolecats_StaleSessionActivity_RecentWindowActivity_ZeroKeys(t *testing.T) {
	// tmux's activity timestamps are second-granularity, so the margins here
	// are generous: 3.5s past a 3s grace makes session_activity clearly
	// stale, then a fresh 1.5s window_activity reading is still clearly
	// inside the 3s grace, with over a full second of headroom either side.
	f := newStallTestFixture(t, "0s", "3s")

	time.Sleep(3500 * time.Millisecond) // past the 3s grace: session_activity now stale

	sessionActivityBefore, err := f.tm.GetSessionActivity(f.sessionName)
	if err != nil {
		t.Fatalf("GetSessionActivity: %v", err)
	}
	if time.Since(sessionActivityBefore) < 3*time.Second {
		t.Fatalf("fixture invalid: session_activity is not yet stale (%v old)", time.Since(sessionActivityBefore))
	}

	// Real pane output advances window_activity; session_activity is
	// documented to NOT reliably move on a detached session (hq-wisp-y46vn).
	if err := f.tm.SendKeys(f.sessionName, "echo fresh-output"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)

	result := DetectStalledPolecats(f.townRoot, f.rigName, false)
	if len(result.Stalled) != 0 {
		t.Errorf("Stalled = %+v, want empty — window_activity is recent even though "+
			"session_activity is stale (a check still keyed on session_activity "+
			"would wrongly stall this)", result.Stalled)
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
