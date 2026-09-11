package polecat

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/tmux"
)

func TestTouchAndReadSessionHeartbeat(t *testing.T) {
	townRoot := t.TempDir()

	// No heartbeat initially
	hb := ReadSessionHeartbeat(townRoot, "gt-test-session")
	if hb != nil {
		t.Fatal("expected nil heartbeat before touch")
	}

	// Touch heartbeat
	TouchSessionHeartbeat(townRoot, "gt-test-session")

	// Read it back
	hb = ReadSessionHeartbeat(townRoot, "gt-test-session")
	if hb == nil {
		t.Fatal("expected non-nil heartbeat after touch")
	}

	if time.Since(hb.Timestamp) > 5*time.Second {
		t.Errorf("heartbeat timestamp too old: %v", hb.Timestamp)
	}

	// v2: TouchSessionHeartbeat writes state="working" by default (gt-3vr5)
	if hb.State != HeartbeatWorking {
		t.Errorf("heartbeat state = %q, want %q", hb.State, HeartbeatWorking)
	}
}

func TestTouchSessionHeartbeatWithState(t *testing.T) {
	townRoot := t.TempDir()

	TouchSessionHeartbeatWithState(townRoot, "gt-test-state", HeartbeatExiting, "gt done", "gt-abc123")

	hb := ReadSessionHeartbeat(townRoot, "gt-test-state")
	if hb == nil {
		t.Fatal("expected non-nil heartbeat after touch with state")
	}

	if hb.State != HeartbeatExiting {
		t.Errorf("state = %q, want %q", hb.State, HeartbeatExiting)
	}
	if hb.Context != "gt done" {
		t.Errorf("context = %q, want %q", hb.Context, "gt done")
	}
	if hb.Bead != "gt-abc123" {
		t.Errorf("bead = %q, want %q", hb.Bead, "gt-abc123")
	}
}

func TestSessionHeartbeat_EffectiveState(t *testing.T) {
	tests := []struct {
		name  string
		state HeartbeatState
		want  HeartbeatState
	}{
		{"empty (v1 compat)", "", HeartbeatWorking},
		{"working", HeartbeatWorking, HeartbeatWorking},
		{"idle", HeartbeatIdle, HeartbeatIdle},
		{"exiting", HeartbeatExiting, HeartbeatExiting},
		{"stuck", HeartbeatStuck, HeartbeatStuck},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hb := &SessionHeartbeat{State: tt.state}
			if got := hb.EffectiveState(); got != tt.want {
				t.Errorf("EffectiveState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSessionHeartbeat_IsV2(t *testing.T) {
	// v1 heartbeat (no state)
	v1 := &SessionHeartbeat{Timestamp: time.Now()}
	if v1.IsV2() {
		t.Error("expected IsV2()=false for v1 heartbeat")
	}

	// v2 heartbeat (has state)
	v2 := &SessionHeartbeat{Timestamp: time.Now(), State: HeartbeatWorking}
	if !v2.IsV2() {
		t.Error("expected IsV2()=true for v2 heartbeat")
	}
}

func TestIsSessionHeartbeatStale_NoFile(t *testing.T) {
	townRoot := t.TempDir()

	stale, exists := IsSessionHeartbeatStale(townRoot, "nonexistent")
	if exists {
		t.Error("expected exists=false for missing heartbeat")
	}
	if stale {
		t.Error("expected stale=false for missing heartbeat")
	}
}

func TestIsSessionHeartbeatStale_Fresh(t *testing.T) {
	townRoot := t.TempDir()

	TouchSessionHeartbeat(townRoot, "gt-test-fresh")

	stale, exists := IsSessionHeartbeatStale(townRoot, "gt-test-fresh")
	if !exists {
		t.Error("expected exists=true for fresh heartbeat")
	}
	if stale {
		t.Error("expected stale=false for fresh heartbeat")
	}
}

func TestIsSessionHeartbeatStale_Old(t *testing.T) {
	townRoot := t.TempDir()

	// Write a heartbeat with an old timestamp
	dir := filepath.Join(townRoot, ".runtime", "heartbeats")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	oldTime := time.Now().Add(-10 * time.Minute).UTC()
	data := []byte(`{"timestamp":"` + oldTime.Format(time.RFC3339Nano) + `"}`)
	if err := os.WriteFile(filepath.Join(dir, "gt-test-stale.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	stale, exists := IsSessionHeartbeatStale(townRoot, "gt-test-stale")
	if !exists {
		t.Error("expected exists=true for old heartbeat")
	}
	if !stale {
		t.Error("expected stale=true for 10-minute-old heartbeat")
	}
}

func TestRemoveSessionHeartbeat(t *testing.T) {
	townRoot := t.TempDir()

	TouchSessionHeartbeat(townRoot, "gt-test-remove")

	// Verify it exists
	hb := ReadSessionHeartbeat(townRoot, "gt-test-remove")
	if hb == nil {
		t.Fatal("expected heartbeat to exist before removal")
	}

	// Remove it
	RemoveSessionHeartbeat(townRoot, "gt-test-remove")

	// Verify it's gone
	hb = ReadSessionHeartbeat(townRoot, "gt-test-remove")
	if hb != nil {
		t.Error("expected nil heartbeat after removal")
	}
}

func TestRemoveSessionHeartbeat_NoopOnMissing(t *testing.T) {
	townRoot := t.TempDir()
	// Should not panic or error on missing file
	RemoveSessionHeartbeat(townRoot, "nonexistent")
}

func TestIsSessionProcessDead_HeartbeatFresh(t *testing.T) {
	townRoot := t.TempDir()
	sessionName := "gt-test-hb-alive"

	// Touch a fresh heartbeat — isSessionProcessDead should return false
	TouchSessionHeartbeat(townRoot, sessionName)

	dead := isSessionProcessDead(nil, sessionName, townRoot)
	if dead {
		t.Error("expected alive (dead=false) for session with fresh heartbeat")
	}
}

func writeStaleSessionHeartbeat(t *testing.T, townRoot, sessionName string) {
	t.Helper()
	dir := filepath.Join(townRoot, ".runtime", "heartbeats")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(SessionHeartbeat{
		Timestamp: time.Now().Add(-10 * time.Minute).UTC(),
		State:     HeartbeatWorking,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionName+".json"), data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestIsSessionProcessDead_HeartbeatStaleUsesAgentLiveness(t *testing.T) {
	townRoot := t.TempDir()
	oldSessionAgentAlive := sessionAgentAlive
	t.Cleanup(func() { sessionAgentAlive = oldSessionAgentAlive })

	tests := []struct {
		name     string
		alive    bool
		aliveErr error
		wantDead bool
	}{
		{name: "live_agent", alive: true, wantDead: false},
		{name: "not_live_agent", alive: false, wantDead: true},
		{name: "query_error", alive: false, aliveErr: errors.New("tmux query failed"), wantDead: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessionName := "gt-test-hb-stale-" + tt.name
			writeStaleSessionHeartbeat(t, townRoot, sessionName)

			called := false
			sessionAgentAlive = func(_ *tmux.Tmux, gotSession string) (bool, error) {
				called = true
				if gotSession != sessionName {
					t.Fatalf("liveness checked session %q, want %q", gotSession, sessionName)
				}
				return tt.alive, tt.aliveErr
			}

			dead := isSessionProcessDead(tmux.NewTmuxWithSocket("gt-unused-test-socket"), sessionName, townRoot)
			if dead != tt.wantDead {
				t.Fatalf("isSessionProcessDead() = %v, want %v", dead, tt.wantDead)
			}
			if !called {
				t.Fatal("expected stale heartbeat to check agent liveness")
			}
		})
	}
}

func TestIsSessionProcessDead_HeartbeatStaleWithoutTmuxFailsClosed(t *testing.T) {
	townRoot := t.TempDir()
	sessionName := "gt-test-hb-stale-no-tmux"
	writeStaleSessionHeartbeat(t, townRoot, sessionName)

	dead := isSessionProcessDead(nil, sessionName, townRoot)
	if dead {
		t.Error("expected dead=false for stale heartbeat without tmux liveness evidence")
	}
}

func TestIsSessionProcessDead_EmptyTownRoot(t *testing.T) {
	// With empty townRoot, heartbeat check is skipped entirely.
	// This tests backward compatibility when townRoot isn't available.
	// We can't test the full PID fallback without a real tmux session,
	// but we verify no panic with empty townRoot.
	sessionName := "gt-test-no-townroot"

	// Empty townRoot skips heartbeat, falls through to PID check.
	// Can't test PID path without tmux, but verify heartbeat path is skipped.
	stale, exists := IsSessionHeartbeatStale("", sessionName)
	if exists {
		t.Error("expected exists=false with empty townRoot")
	}
	if stale {
		t.Error("expected stale=false with empty townRoot")
	}
}

func TestReadSessionHeartbeat_V1BackwardsCompat(t *testing.T) {
	townRoot := t.TempDir()

	// Write a v1 heartbeat (timestamp only, no state field)
	dir := filepath.Join(townRoot, ".runtime", "heartbeats")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	ts := time.Now().UTC()
	data := []byte(`{"timestamp":"` + ts.Format(time.RFC3339Nano) + `"}`)
	if err := os.WriteFile(filepath.Join(dir, "gt-test-v1.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	hb := ReadSessionHeartbeat(townRoot, "gt-test-v1")
	if hb == nil {
		t.Fatal("expected non-nil heartbeat for v1 format")
	}

	// State should be empty (v1)
	if hb.State != "" {
		t.Errorf("v1 heartbeat state = %q, want empty", hb.State)
	}

	// IsV2 should return false
	if hb.IsV2() {
		t.Error("expected IsV2()=false for v1 heartbeat")
	}

	// EffectiveState should default to working
	if hb.EffectiveState() != HeartbeatWorking {
		t.Errorf("v1 EffectiveState() = %q, want %q", hb.EffectiveState(), HeartbeatWorking)
	}
}

func TestReadSessionHeartbeat_V2AllStates(t *testing.T) {
	townRoot := t.TempDir()

	dir := filepath.Join(townRoot, ".runtime", "heartbeats")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	states := []HeartbeatState{HeartbeatWorking, HeartbeatIdle, HeartbeatExiting, HeartbeatStuck}
	for _, state := range states {
		t.Run(string(state), func(t *testing.T) {
			session := "gt-test-v2-" + string(state)
			hb := SessionHeartbeat{
				Timestamp: time.Now().UTC(),
				State:     state,
				Context:   "test context",
				Bead:      "gt-test-bead",
			}
			data, err := json.Marshal(hb)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, session+".json"), data, 0644); err != nil {
				t.Fatal(err)
			}

			read := ReadSessionHeartbeat(townRoot, session)
			if read == nil {
				t.Fatal("expected non-nil heartbeat")
			}
			if read.State != state {
				t.Errorf("state = %q, want %q", read.State, state)
			}
			if !read.IsV2() {
				t.Error("expected IsV2()=true")
			}
			if read.EffectiveState() != state {
				t.Errorf("EffectiveState() = %q, want %q", read.EffectiveState(), state)
			}
			if read.Context != "test context" {
				t.Errorf("context = %q, want %q", read.Context, "test context")
			}
			if read.Bead != "gt-test-bead" {
				t.Errorf("bead = %q, want %q", read.Bead, "gt-test-bead")
			}
		})
	}
}

// The tests below cover the revision-3 startup-window gate (gtn-qp7 /
// hq-ooijo): IsStartupWindowOpen, the startup-closed latch, and the
// launcher-vs-agent Writer distinction. Pure-logic cases construct
// heartbeat/latch files directly and never touch tmux; live-tmux cases are
// suffixed accordingly and use their own isolated tmux socket.

func TestReadSessionHeartbeatChecked_MissingVsMalformed(t *testing.T) {
	townRoot := t.TempDir()

	hb, malformed := ReadSessionHeartbeatChecked(townRoot, "nonexistent")
	if hb != nil || malformed {
		t.Errorf("missing file: got (%v, %v), want (nil, false)", hb, malformed)
	}

	dir := filepath.Join(townRoot, ".runtime", "heartbeats")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "corrupt.json"), []byte("{not valid json"), 0644); err != nil {
		t.Fatal(err)
	}
	hb, malformed = ReadSessionHeartbeatChecked(townRoot, "corrupt")
	if hb != nil || !malformed {
		t.Errorf("malformed file: got (%v, %v), want (nil, true)", hb, malformed)
	}
}

func TestIsStartupWindowOpen_NoFiles_Open(t *testing.T) {
	townRoot := t.TempDir()
	status := IsStartupWindowOpen(townRoot, "gt-test-fresh", "$1", 1000)
	if !status.Open {
		t.Errorf("Open = false (%s), want true — a session with no heartbeat or latch is the ordinary fresh-startup case", status.Reason)
	}
}

func TestIsStartupWindowOpen_UnresolvableCurrentIdentity_Closed(t *testing.T) {
	townRoot := t.TempDir()
	tests := []struct {
		name      string
		sessionID string
		created   int64
	}{
		{"empty session id", "", 1000},
		{"zero created", "$1", 0},
		{"both unresolvable", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := IsStartupWindowOpen(townRoot, "gt-test-unresolvable", tt.sessionID, tt.created)
			if status.Open {
				t.Error("Open = true, want false — an unresolvable current incarnation must refuse to authorize")
			}
		})
	}
}

func writeTestLatch(t *testing.T, townRoot, sessionName, sessionID string, created int64) {
	t.Helper()
	dir := filepath.Join(townRoot, ".runtime", "startup-latches")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(StartupLatch{SessionID: sessionID, Created: created})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionName+".json"), data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestIsStartupWindowOpen_LatchMatchesCurrent_Closed(t *testing.T) {
	townRoot := t.TempDir()
	writeTestLatch(t, townRoot, "gt-test-latched", "$3", 5000)

	status := IsStartupWindowOpen(townRoot, "gt-test-latched", "$3", 5000)
	if status.Open {
		t.Error("Open = true, want false — a latch matching the current incarnation must close the window")
	}
}

func TestIsStartupWindowOpen_LatchDifferentIncarnation_Open(t *testing.T) {
	townRoot := t.TempDir()
	// Latch belongs to an OLDER incarnation of this session name (name
	// reuse): different session id, same created second is the exact
	// merge risk named on hq-ooijo revision 3 ("a name reused within one
	// second inherits the previous incarnation's closed window").
	writeTestLatch(t, townRoot, "gt-test-reused", "$3", 5000)

	status := IsStartupWindowOpen(townRoot, "gt-test-reused", "$4", 5000)
	if !status.Open {
		t.Errorf("Open = false (%s), want true — a latch from a different session id must not close the NEW incarnation's window", status.Reason)
	}
}

func TestIsStartupWindowOpen_LatchMalformed_ClosedFailSafe(t *testing.T) {
	townRoot := t.TempDir()
	dir := filepath.Join(townRoot, ".runtime", "startup-latches")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gt-test-badlatch.json"), []byte("{not valid json"), 0644); err != nil {
		t.Fatal(err)
	}

	status := IsStartupWindowOpen(townRoot, "gt-test-badlatch", "$3", 5000)
	if status.Open {
		t.Error("Open = true, want false — a malformed latch file must refuse to authorize, not default to open")
	}
}

func writeTestHeartbeatForWindow(t *testing.T, townRoot, sessionName, sessionID string, created int64, writer HeartbeatWriter) {
	t.Helper()
	dir := filepath.Join(townRoot, ".runtime", "heartbeats")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	hb := SessionHeartbeat{
		Timestamp:   time.Now().UTC(),
		State:       HeartbeatWorking,
		Incarnation: created,
		SessionID:   sessionID,
		Writer:      writer,
	}
	data, err := json.Marshal(hb)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionName+".json"), data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestIsStartupWindowOpen_AgentHeartbeatMatchesCurrent_Closed(t *testing.T) {
	townRoot := t.TempDir()
	writeTestHeartbeatForWindow(t, townRoot, "gt-test-agent-hb", "$3", 5000, HeartbeatWriterAgent)

	status := IsStartupWindowOpen(townRoot, "gt-test-agent-hb", "$3", 5000)
	if status.Open {
		t.Error("Open = true, want false — an agent-written heartbeat for this incarnation must close the window")
	}
}

// TestIsStartupWindowOpen_LauncherHeartbeatMatchesCurrent_Open covers item 3
// (gtn-qp7 / hq-ooijo revision 3): a launcher-written heartbeat — even one
// matching the current incarnation exactly — must NOT close the window.
// AcceptStartupDialogs and WaitForRuntimeReady are both non-fatal, so this
// is exactly what a session parked on a genuine, still-unhandled startup
// dialog looks like; trusting it would strand that dialog forever.
func TestIsStartupWindowOpen_LauncherHeartbeatMatchesCurrent_Open(t *testing.T) {
	townRoot := t.TempDir()
	writeTestHeartbeatForWindow(t, townRoot, "gt-test-launcher-hb", "$3", 5000, HeartbeatWriterLauncher)

	status := IsStartupWindowOpen(townRoot, "gt-test-launcher-hb", "$3", 5000)
	if !status.Open {
		t.Errorf("Open = false (%s), want true — a launcher heartbeat must never close the startup window", status.Reason)
	}
}

// TestIsStartupWindowOpen_LegacyHeartbeatNoWriter_Closed covers a pre-v2.2
// heartbeat file with no Writer field at all (Writer == ""): we cannot tell
// launcher from agent, so this must fail CLOSED — never send keys into a
// session we cannot positively attribute to our own fresh launcher
// placeholder (regression 1, gtn-s8i / codex review 5640759300).
func TestIsStartupWindowOpen_LegacyHeartbeatNoWriter_Closed(t *testing.T) {
	townRoot := t.TempDir()
	writeTestHeartbeatForWindow(t, townRoot, "gt-test-legacy-hb", "$3", 5000, "")

	status := IsStartupWindowOpen(townRoot, "gt-test-legacy-hb", "$3", 5000)
	if status.Open {
		t.Error("Open = true, want false — a legacy heartbeat with no Writer field must fail closed")
	}
}

// TestIsStartupWindowOpen_AgentHeartbeatDifferentIncarnation_Open is the
// same-second session-replacement case (gtn-qp7 / hq-ooijo revision 3, item
// 1 and its dedicated test): an agent heartbeat closed the OLD incarnation's
// window (same created second, different session id). The new incarnation's
// window must still read open.
func TestIsStartupWindowOpen_AgentHeartbeatDifferentIncarnation_Open(t *testing.T) {
	townRoot := t.TempDir()
	writeTestHeartbeatForWindow(t, townRoot, "gt-test-replaced", "$3", 5000, HeartbeatWriterAgent)

	status := IsStartupWindowOpen(townRoot, "gt-test-replaced", "$4", 5000)
	if !status.Open {
		t.Errorf("Open = false (%s), want true — an agent heartbeat from a DIFFERENT session id (same-second replacement) must not close the new incarnation's window", status.Reason)
	}
}

func TestIsStartupWindowOpen_HeartbeatMalformed_ClosedFailSafe(t *testing.T) {
	townRoot := t.TempDir()
	dir := filepath.Join(townRoot, ".runtime", "heartbeats")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gt-test-badhb.json"), []byte("{not valid json"), 0644); err != nil {
		t.Fatal(err)
	}

	status := IsStartupWindowOpen(townRoot, "gt-test-badhb", "$3", 5000)
	if status.Open {
		t.Error("Open = true, want false — a malformed heartbeat file must refuse to authorize, not default to open")
	}
}

// TestIsStartupWindowOpen_AgentHeartbeatUnknownIdentity_ClosedFailSafe
// covers a well-formed but identity-incomplete agent heartbeat (Writer=agent
// but SessionID empty or Incarnation zero — e.g. a write-time tmux query
// partially failed): "unknown or corrupt identity means refuse to send"
// (revision 3, item 2).
func TestIsStartupWindowOpen_AgentHeartbeatUnknownIdentity_ClosedFailSafe(t *testing.T) {
	townRoot := t.TempDir()
	writeTestHeartbeatForWindow(t, townRoot, "gt-test-unknown-identity", "", 5000, HeartbeatWriterAgent)

	status := IsStartupWindowOpen(townRoot, "gt-test-unknown-identity", "$3", 5000)
	if status.Open {
		t.Error("Open = true, want false — an agent heartbeat with unknown identity must refuse to authorize")
	}
}

// requireLiveTmuxForHeartbeat returns an isolated Tmux + socket for a live
// startup-window test. The socket name is short and counter-based, not
// derived from t.Name(): the full test name plus the town's /private/tmp
// prefix overruns sockaddr_un's ~104-byte path limit ("File name too long"
// from tmux itself, measured directly), which is unrelated to anything this
// PR changes and must not be worked around by naming tests less clearly.
func requireLiveTmuxForHeartbeat(t *testing.T) (tm *tmux.Tmux, socket string) {
	t.Helper()
	requireTmux(t)
	socket = fmt.Sprintf("gt-test-hb-%d", testSessionCounter.Add(1))
	tm = tmux.NewTmuxWithSocket(socket)
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })
	return tm, socket
}

// TestCloseStartupWindow_ClosesForLiveIncarnation is the live-tmux
// end-to-end check that CloseStartupWindow's write is readable by
// IsStartupWindowOpen using the SAME identity a real caller would resolve
// (GetSessionID + GetSessionCreatedUnix), not a hand-picked one.
func TestCloseStartupWindow_ClosesForLiveIncarnation(t *testing.T) {
	tm, socket := requireLiveTmuxForHeartbeat(t)
	townRoot := t.TempDir()
	sessionName := "gt-test-close-window"

	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// currentSessionIdentity resolves via tmux.NewTmux() (the process-wide
	// default socket), so point it at this test's isolated socket for the
	// duration of the call.
	old := tmux.GetDefaultSocket()
	tmux.SetDefaultSocket(socket)
	defer tmux.SetDefaultSocket(old)

	sessionID, err := tm.GetSessionID(sessionName)
	if err != nil {
		t.Fatalf("GetSessionID: %v", err)
	}
	created, err := tm.GetSessionCreatedUnix(sessionName)
	if err != nil {
		t.Fatalf("GetSessionCreatedUnix: %v", err)
	}

	if status := IsStartupWindowOpen(townRoot, sessionName, sessionID, created); !status.Open {
		t.Fatalf("window unexpectedly closed before CloseStartupWindow: %s", status.Reason)
	}

	CloseStartupWindow(townRoot, sessionName)

	status := IsStartupWindowOpen(townRoot, sessionName, sessionID, created)
	if status.Open {
		t.Error("Open = true, want false after CloseStartupWindow for this exact incarnation")
	}
}

// TestTouchSessionHeartbeatWithState_ClosesStartupWindow_Live covers the
// real call path: every agent-written heartbeat (persistentPreRun, `gt
// done`, `gt heartbeat`) closes its own incarnation's startup window as a
// side effect.
func TestTouchSessionHeartbeatWithState_ClosesStartupWindow_Live(t *testing.T) {
	tm, socket := requireLiveTmuxForHeartbeat(t)
	townRoot := t.TempDir()
	sessionName := "gt-test-touch-closes"

	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	old := tmux.GetDefaultSocket()
	tmux.SetDefaultSocket(socket)
	defer tmux.SetDefaultSocket(old)

	TouchSessionHeartbeatWithState(townRoot, sessionName, HeartbeatWorking, "", "")

	hb := ReadSessionHeartbeat(townRoot, sessionName)
	if hb == nil {
		t.Fatal("expected non-nil heartbeat")
	}
	if hb.Writer != HeartbeatWriterAgent {
		t.Errorf("Writer = %q, want %q", hb.Writer, HeartbeatWriterAgent)
	}
	if hb.SessionID == "" {
		t.Error("expected non-empty SessionID on a live-tmux write")
	}

	sessionID, err := tm.GetSessionID(sessionName)
	if err != nil {
		t.Fatalf("GetSessionID: %v", err)
	}
	created, err := tm.GetSessionCreatedUnix(sessionName)
	if err != nil {
		t.Fatalf("GetSessionCreatedUnix: %v", err)
	}
	if status := IsStartupWindowOpen(townRoot, sessionName, sessionID, created); status.Open {
		t.Error("Open = true, want false — TouchSessionHeartbeatWithState must close the startup window")
	}
}

// TestTouchLauncherStartupHeartbeat_DoesNotCloseStartupWindow_Live is the
// sibling case: the launcher's own placeholder write leaves the window
// open (item 3).
func TestTouchLauncherStartupHeartbeat_DoesNotCloseStartupWindow_Live(t *testing.T) {
	tm, socket := requireLiveTmuxForHeartbeat(t)
	townRoot := t.TempDir()
	sessionName := "gt-test-touch-launcher"

	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	old := tmux.GetDefaultSocket()
	tmux.SetDefaultSocket(socket)
	defer tmux.SetDefaultSocket(old)

	TouchLauncherStartupHeartbeat(townRoot, sessionName)

	hb := ReadSessionHeartbeat(townRoot, sessionName)
	if hb == nil {
		t.Fatal("expected non-nil heartbeat")
	}
	if hb.Writer != HeartbeatWriterLauncher {
		t.Errorf("Writer = %q, want %q", hb.Writer, HeartbeatWriterLauncher)
	}

	sessionID, err := tm.GetSessionID(sessionName)
	if err != nil {
		t.Fatalf("GetSessionID: %v", err)
	}
	created, err := tm.GetSessionCreatedUnix(sessionName)
	if err != nil {
		t.Fatalf("GetSessionCreatedUnix: %v", err)
	}
	if status := IsStartupWindowOpen(townRoot, sessionName, sessionID, created); !status.Open {
		t.Errorf("Open = false (%s), want true — TouchLauncherStartupHeartbeat must never close the startup window", status.Reason)
	}
}

func TestRemoveSessionHeartbeat_AlsoRemovesStartupLatch(t *testing.T) {
	townRoot := t.TempDir()
	sessionName := "gt-test-remove-latch"
	writeTestLatch(t, townRoot, sessionName, "$3", 5000)

	if latch, malformed := readStartupLatchChecked(townRoot, sessionName); latch == nil || malformed {
		t.Fatal("expected latch to exist before removal")
	}

	RemoveSessionHeartbeat(townRoot, sessionName)

	if latch, malformed := readStartupLatchChecked(townRoot, sessionName); latch != nil || malformed {
		t.Errorf("expected latch to be gone after RemoveSessionHeartbeat, got (%v, %v)", latch, malformed)
	}
}
