package polecat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/tmux"
)

// SessionHeartbeatStaleThreshold is the age at which a polecat session heartbeat
// is considered stale, indicating the agent process is likely dead.
// Configurable via operational.polecat.heartbeat_stale_threshold in settings/config.json.
const SessionHeartbeatStaleThreshold = 3 * time.Minute

// HeartbeatState represents the agent-reported state in a heartbeat v2 (gt-3vr5).
// Agents report their own state; the witness makes exactly one inference:
// "is the heartbeat fresh?" Everything else is agent-reported.
type HeartbeatState string

const (
	// HeartbeatWorking means the agent is actively processing.
	HeartbeatWorking HeartbeatState = "working"
	// HeartbeatIdle means the agent is waiting for input.
	HeartbeatIdle HeartbeatState = "idle"
	// HeartbeatExiting means the agent is in the gt done flow.
	HeartbeatExiting HeartbeatState = "exiting"
	// HeartbeatStuck means the agent self-reports being stuck.
	HeartbeatStuck HeartbeatState = "stuck"
)

// HeartbeatWriter identifies which code path wrote a heartbeat: the
// launcher (session_manager.go, before the agent has run a single `gt`
// command) or the agent itself (every `gt` command's persistentPreRun,
// `gt done`, `gt heartbeat`). Revision 3 (gtn-qp7 / hq-ooijo) needs this
// distinction: session_manager.go writes its startup placeholder even on a
// nonfatal readiness failure, so trusting it the same as an agent-driven
// write would let a genuine startup dialog strand its own window closed
// forever.
type HeartbeatWriter string

const (
	// HeartbeatWriterLauncher marks the ONE heartbeat session_manager.go
	// itself writes at the end of session startup.
	HeartbeatWriterLauncher HeartbeatWriter = "launcher"
	// HeartbeatWriterAgent marks every heartbeat written from inside the
	// running agent process: persistentPreRun, `gt done`, `gt heartbeat`.
	HeartbeatWriterAgent HeartbeatWriter = "agent"
)

// SessionHeartbeat represents a polecat session's heartbeat file.
// v1: timestamp only. v2 (gt-3vr5): adds agent-reported state, context, and bead.
// v2.1 (gtn-m7s / hq-ooijo revision 2): adds Incarnation, the tmux
// session_created unix timestamp of the session this heartbeat was written
// for. v2.2 (gtn-qp7 / hq-ooijo revision 3): adds SessionID (tmux's own
// never-reused #{session_id}, closing the same-second name-reuse gap
// Incarnation alone leaves open) and Writer.
type SessionHeartbeat struct {
	Timestamp time.Time      `json:"timestamp"`
	State     HeartbeatState `json:"state,omitempty"`   // v2: agent-reported state
	Context   string         `json:"context,omitempty"` // v2: what the agent is doing
	Bead      string         `json:"bead,omitempty"`    // v2: current hook bead ID

	// Incarnation is the tmux session_created unix timestamp captured at
	// write time. A tmux session name can be REUSED: an old session dies and
	// a new one is created with the identical name (e.g. after recovery).
	// Without this field, a stale "working" heartbeat written by the DEAD
	// incarnation reads as fresh/working for the NEW incarnation too, and
	// permanently suppresses startup-stall recovery for it — the merge risk
	// named on hq-ooijo revision 2. Readers must compare this against the
	// CURRENT session's session_created before trusting State or Timestamp.
	// Zero means unknown (write-time lookup failed, or a pre-v2.1 file).
	Incarnation int64 `json:"incarnation,omitempty"`

	// SessionID is tmux's own #{session_id} (e.g. "$3"), captured at write
	// time. Unlike Incarnation (session_created), it is never reused for
	// the lifetime of the tmux server, closing the gap where a replacement
	// session created within the same second as the one it replaced would
	// otherwise carry an identical Incarnation value. Empty means unknown
	// (write-time lookup failed, or a pre-v2.2 file) — readers must treat
	// that the same as a non-matching value, never as a wildcard match.
	SessionID string `json:"session_id,omitempty"`

	// Writer records which code path wrote this heartbeat. Empty means
	// unknown (a pre-v2.2 file) — readers must treat that the same as
	// HeartbeatWriterLauncher for startup-window purposes: never proof the
	// agent itself has run.
	Writer HeartbeatWriter `json:"writer,omitempty"`
}

// EffectiveState returns the agent-reported state, defaulting to HeartbeatWorking
// for v1 heartbeats without a state field (backwards compatibility). See gt-3vr5.
func (h *SessionHeartbeat) EffectiveState() HeartbeatState {
	if h.State == "" {
		return HeartbeatWorking
	}
	return h.State
}

// IsV2 returns true if this heartbeat carries a state field (heartbeat v2).
// Used by the witness to decide whether to use agent-reported state or fall
// through to legacy timer-based detection.
func (h *SessionHeartbeat) IsV2() bool {
	return h.State != ""
}

// MatchesIncarnation reports whether this heartbeat was written for the
// session incarnation identified by currentSessionCreated (that session's
// current #{session_created} unix timestamp). A mismatch — including an
// unknown Incarnation (0), which predates this binding or means the
// write-time lookup failed — means the heartbeat belongs to a different or
// unverifiable incarnation and must not be trusted to suppress detection
// (gtn-m7s / hq-ooijo revision 2 merge risk: session name reuse).
func (h *SessionHeartbeat) MatchesIncarnation(currentSessionCreated int64) bool {
	return h.Incarnation != 0 && currentSessionCreated != 0 && h.Incarnation == currentSessionCreated
}

// heartbeatsDir returns the directory for polecat session heartbeat files.
// Heartbeats live under <townRoot>/.runtime/heartbeats/, parallel to .runtime/pids/.
func heartbeatsDir(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "heartbeats")
}

// heartbeatFile returns the path to a heartbeat file for a given session.
func heartbeatFile(townRoot, sessionName string) string {
	return filepath.Join(heartbeatsDir(townRoot), sessionName+".json")
}

// TouchSessionHeartbeat writes or updates the heartbeat file for a polecat session.
// Writes state="working" by default (heartbeat v2, gt-3vr5).
// This is best-effort: errors are silently ignored because heartbeat signals
// are non-critical and should not interrupt gt commands.
func TouchSessionHeartbeat(townRoot, sessionName string) {
	TouchSessionHeartbeatWithState(townRoot, sessionName, HeartbeatWorking, "", "")
}

// StartupHeartbeatContext marks the ONE heartbeat the launcher itself writes,
// at the end of session startup (session_manager.go), before the agent has
// run a single `gt` command. Every heartbeat after that — including the very
// first one a `gt` command's own persistentPreRun writes — overwrites this
// with Context="". Readers use that to tell "the agent has made at least one
// verified move since launch" apart from "nothing has happened since the
// launcher's own optimistic write": AcceptStartupDialogs and
// WaitForRuntimeReady are both non-fatal (session_manager.go:512,521), so a
// process left parked on an unhandled dialog gets this same state="working"
// heartbeat as a session that started cleanly — an untouched, stale one of
// these must not be trusted to suppress stall detection forever the way a
// genuinely agent-driven stale/working heartbeat is (codex Medium,
// handlers.go:2357).
const StartupHeartbeatContext = "session-startup"

// TouchSessionHeartbeatWithState writes a heartbeat with explicit state
// information, marked as AGENT-written (Writer=agent). Used by `gt done`
// (state="exiting"), `gt heartbeat` (state="stuck"), and every `gt`
// command's persistentPreRun via TouchSessionHeartbeat (state="working").
// See gt-3vr5. This is best-effort: errors are silently ignored.
//
// This write also closes the calling session incarnation's startup window
// (CloseStartupWindow): an agent-written heartbeat is exactly the proof
// revision 3 (gtn-qp7 / hq-ooijo) uses to close it irreversibly. The
// launcher's OWN startup placeholder write does not go through this
// function — see TouchLauncherStartupHeartbeat.
func TouchSessionHeartbeatWithState(townRoot, sessionName string, state HeartbeatState, context, bead string) {
	writeHeartbeat(townRoot, sessionName, state, context, bead, HeartbeatWriterAgent)
	CloseStartupWindow(townRoot, sessionName)
}

// TouchLauncherStartupHeartbeat writes the ONE heartbeat the launcher
// itself writes, at the end of session startup (session_manager.go), before
// the agent has run a single `gt` command. It is marked Writer=launcher so
// readers never treat it as proof the agent has started: AcceptStartupDialogs
// and WaitForRuntimeReady are both non-fatal (session_manager.go:512,521),
// so this write happens even when startup left a dialog unhandled
// (gtn-qp7 / hq-ooijo revision 3, item 3). It does NOT close the startup
// window. This is best-effort: errors are silently ignored.
func TouchLauncherStartupHeartbeat(townRoot, sessionName string) {
	writeHeartbeat(townRoot, sessionName, HeartbeatWorking, StartupHeartbeatContext, "", HeartbeatWriterLauncher)
}

// writeHeartbeat resolves the current session incarnation and writes the
// heartbeat file atomically (temp file + rename), so a concurrent reader
// never observes a partially written file (gtn-qp7 / hq-ooijo revision 3,
// item 2). This is best-effort: errors are silently ignored.
func writeHeartbeat(townRoot, sessionName string, state HeartbeatState, context, bead string, writer HeartbeatWriter) {
	dir := heartbeatsDir(townRoot)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}

	sessionID, created := currentSessionIdentity(sessionName)
	hb := SessionHeartbeat{
		Timestamp:   time.Now().UTC(),
		State:       state,
		Context:     context,
		Bead:        bead,
		Incarnation: created,
		SessionID:   sessionID,
		Writer:      writer,
	}

	data, err := json.Marshal(hb)
	if err != nil {
		return
	}

	_ = atomicWriteFile(heartbeatFile(townRoot, sessionName), data, 0644)
}

// currentSessionIdentity queries tmux for sessionName's current
// #{session_id} and #{session_created}, so a written heartbeat or startup
// latch can be bound to this specific incarnation of the session name
// (gtn-m7s / hq-ooijo revision 2; SessionID added in revision 3, gtn-qp7).
// Returns ("", 0) if either query fails — e.g. not running inside tmux, or
// the session isn't visible on the resolved socket — in which case the
// caller writes without a trustworthy incarnation and readers must not use
// it to suppress detection or to close a startup window.
func currentSessionIdentity(sessionName string) (sessionID string, created int64) {
	t := tmux.NewTmux()
	created, err := t.GetSessionCreatedUnix(sessionName)
	if err != nil {
		return "", 0
	}
	sessionID, err = t.GetSessionID(sessionName)
	if err != nil {
		return "", 0
	}
	return sessionID, created
}

// atomicWriteFile writes data to path via a temp file in the same directory
// followed by a rename, so a concurrent reader never observes a partially
// written file (gtn-qp7 / hq-ooijo revision 3, item 2: the heartbeat and
// startup-latch files must be written atomically).
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// ReadSessionHeartbeat reads the heartbeat for a polecat session.
// Returns nil if the file doesn't exist or can't be read.
func ReadSessionHeartbeat(townRoot, sessionName string) *SessionHeartbeat {
	data, err := os.ReadFile(heartbeatFile(townRoot, sessionName))
	if err != nil {
		return nil
	}

	var hb SessionHeartbeat
	if err := json.Unmarshal(data, &hb); err != nil {
		return nil
	}

	return &hb
}

// ReadSessionHeartbeatChecked is ReadSessionHeartbeat, but additionally
// reports whether a PRESENT file failed to parse, as opposed to no file
// existing at all. IsStartupWindowOpen needs that distinction: a missing
// heartbeat is the ordinary fresh-session case and must not by itself close
// the startup window, while a present-but-malformed file is ambiguous and
// must refuse to authorize a dismiss (gtn-qp7 / hq-ooijo revision 3, item 2).
// ReadSessionHeartbeat's own callers keep their existing nil-means-"nothing
// to trust" behavior unchanged.
func ReadSessionHeartbeatChecked(townRoot, sessionName string) (hb *SessionHeartbeat, malformed bool) {
	data, err := os.ReadFile(heartbeatFile(townRoot, sessionName))
	if err != nil {
		return nil, false // missing (or unreadable) — the ordinary case, not ambiguous
	}
	var parsed SessionHeartbeat
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, true // present but malformed — ambiguous, caller must fail closed
	}
	return &parsed, false
}

// StartupLatch durably records that a session incarnation's startup window
// has closed. It is written once, atomically, and — while that incarnation
// lives — never removed (gtn-qp7 / hq-ooijo revision 3, item 2). A latch
// belonging to a DIFFERENT incarnation of the same session name (a name
// reused after the old session died) carries no information about the
// current one and must be ignored, not treated as closing it.
type StartupLatch struct {
	SessionID string `json:"session_id"`
	Created   int64  `json:"created"`
}

func (l *StartupLatch) valid() bool {
	return l != nil && l.SessionID != "" && l.Created != 0
}

func (l *StartupLatch) matches(sessionID string, created int64) bool {
	return l.valid() && sessionID != "" && created != 0 && l.SessionID == sessionID && l.Created == created
}

// startupLatchesDir returns the directory for startup-window-closed latch
// files, parallel to heartbeatsDir.
func startupLatchesDir(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "startup-latches")
}

// startupLatchFile returns the path to the startup-closed latch file for a
// given session name.
func startupLatchFile(townRoot, sessionName string) string {
	return filepath.Join(startupLatchesDir(townRoot), sessionName+".json")
}

// CloseStartupWindow durably records that sessionName's CURRENT incarnation
// has closed its startup window. Called by every agent-written heartbeat
// (TouchSessionHeartbeatWithState) — never by the launcher's own placeholder
// write (TouchLauncherStartupHeartbeat). This is best-effort: a failure to
// resolve the current incarnation writes nothing, matching
// TouchSessionHeartbeatWithState's own best-effort contract.
func CloseStartupWindow(townRoot, sessionName string) {
	sessionID, created := currentSessionIdentity(sessionName)
	if sessionID == "" || created == 0 {
		return
	}

	// Idempotent: if already closed for this exact incarnation, skip the
	// write. Writing again would be safe (identical content) but pointless.
	if existing, malformed := readStartupLatchChecked(townRoot, sessionName); !malformed && existing.matches(sessionID, created) {
		return
	}

	dir := startupLatchesDir(townRoot)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}
	data, err := json.Marshal(StartupLatch{SessionID: sessionID, Created: created})
	if err != nil {
		return
	}
	_ = atomicWriteFile(startupLatchFile(townRoot, sessionName), data, 0644)
}

// readStartupLatchChecked reads the startup-closed latch for sessionName,
// reporting whether a PRESENT file failed to parse — the same missing-vs-
// malformed distinction ReadSessionHeartbeatChecked makes, for the same
// reason (see IsStartupWindowOpen).
func readStartupLatchChecked(townRoot, sessionName string) (latch *StartupLatch, malformed bool) {
	data, err := os.ReadFile(startupLatchFile(townRoot, sessionName))
	if err != nil {
		return nil, false
	}
	var l StartupLatch
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, true
	}
	return &l, false
}

// StartupWindowStatus is the result of checking whether a session
// incarnation's startup window is still open for dismiss authorization.
type StartupWindowStatus struct {
	Open   bool
	Reason string
}

// IsStartupWindowOpen reports whether sessionName's CURRENT incarnation —
// identified by sessionID (tmux #{session_id}) and created (tmux
// #{session_created}) — is still inside its startup window: the interval
// before the agent process has written its own first heartbeat. Sending
// dismiss keys into a session is authorized only while this is true
// (gtn-qp7 / hq-ooijo revision 3).
//
// Fails CLOSED (Open=false) whenever the answer is not provably "still
// open": an unresolvable current incarnation, or a malformed/unreadable
// latch or heartbeat file, refuses authorization rather than guessing —
// "unknown or corrupt identity means refuse to send" (revision 3, item 2).
// A MISSING file, in contrast, carries no evidence either way and does not
// by itself close the window: a session that has never written either file
// is exactly the fresh-startup case this window must stay open for.
func IsStartupWindowOpen(townRoot, sessionName, sessionID string, created int64) StartupWindowStatus {
	if sessionID == "" || created == 0 {
		return StartupWindowStatus{Open: false, Reason: "cannot resolve current session incarnation"}
	}

	latch, malformed := readStartupLatchChecked(townRoot, sessionName)
	if malformed {
		return StartupWindowStatus{Open: false, Reason: "startup latch file unreadable or malformed"}
	}
	if latch != nil {
		if !latch.valid() {
			return StartupWindowStatus{Open: false, Reason: "startup latch file has unknown identity"}
		}
		if latch.matches(sessionID, created) {
			return StartupWindowStatus{Open: false, Reason: "startup window closed (latch)"}
		}
		// Latch belongs to a different (older) incarnation of this session
		// name — irrelevant to the current one.
	}

	hb, malformed := ReadSessionHeartbeatChecked(townRoot, sessionName)
	if malformed {
		return StartupWindowStatus{Open: false, Reason: "heartbeat file unreadable or malformed"}
	}
	if hb != nil && hb.Writer == HeartbeatWriterAgent {
		if hb.SessionID == "" || hb.Incarnation == 0 {
			return StartupWindowStatus{Open: false, Reason: "agent heartbeat has unknown identity"}
		}
		if hb.SessionID == sessionID && hb.Incarnation == created {
			return StartupWindowStatus{Open: false, Reason: "startup window closed (agent heartbeat)"}
		}
		// Different incarnation — irrelevant.
	}
	// hb.Writer == HeartbeatWriterLauncher, or Writer is empty (a pre-v2.2
	// or legacy heartbeat we cannot attribute to the agent), never closes
	// the window by itself (item 3): AcceptStartupDialogs/WaitForRuntimeReady
	// are non-fatal, so the launcher writes this even when a dialog is left
	// unhandled.

	return StartupWindowStatus{Open: true, Reason: "no evidence the agent has started"}
}

// IsSessionHeartbeatStale returns true if the session's heartbeat is older than
// the stale threshold, or if no heartbeat file exists.
//
// When no heartbeat file exists, this returns false to avoid false positives
// during the rollout period where sessions may not yet be touching heartbeats.
// The caller should fall back to other liveness checks in that case.
func IsSessionHeartbeatStale(townRoot, sessionName string) (stale bool, exists bool) {
	hb := ReadSessionHeartbeat(townRoot, sessionName)
	if hb == nil {
		return false, false
	}
	return time.Since(hb.Timestamp) >= SessionHeartbeatStaleThreshold, true
}

// RemoveSessionHeartbeat removes the heartbeat file for a session, and the
// startup-closed latch alongside it: the incarnation this cleanup is for is
// ending, so "never removed while the incarnation lives" (CloseStartupWindow)
// no longer applies, and leaving it behind would only accumulate garbage
// under a session name that may never be reused.
// Called during session cleanup.
func RemoveSessionHeartbeat(townRoot, sessionName string) {
	_ = os.Remove(heartbeatFile(townRoot, sessionName))
	_ = os.Remove(startupLatchFile(townRoot, sessionName))
}
