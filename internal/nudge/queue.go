// Package nudge provides non-destructive nudge delivery for Gas Town agents.
//
// The nudge queue allows messages to be delivered cooperatively: instead of
// sending text directly to a tmux session (which cancels in-flight tool calls),
// nudges are written to a queue directory and picked up by the agent's
// UserPromptSubmit hook at the next natural turn boundary.
//
// Queue location: <townRoot>/.runtime/nudge_queue/<session>/
// Each nudge is a JSON file named by timestamp for FIFO ordering.
package nudge

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
)

// Priority levels for nudge delivery.
const (
	// PriorityNormal is the default — delivered at next turn boundary.
	PriorityNormal = "normal"
	// PriorityUrgent means the agent should handle this promptly.
	PriorityUrgent = "urgent"
)

// Operational limits and defaults.
// These are compiled-in fallbacks. Configurable via operational.nudge
// in settings/config.json (ZFC pattern).
const (
	// DefaultNormalTTL is the time-to-live for normal-priority nudges.
	DefaultNormalTTL = 30 * time.Minute

	// DefaultUrgentTTL is the time-to-live for urgent-priority nudges.
	DefaultUrgentTTL = 2 * time.Hour

	// MaxQueueDepth is the maximum number of pending nudges per session.
	MaxQueueDepth = 50

	// staleClaimThreshold is how long a .claimed file must be untouched
	// before Drain considers it orphaned (from a crashed drainer) and removes it.
	staleClaimThreshold = 5 * time.Minute

	// MaxInjectionAttempts bounds how many times a failed injection is
	// requeued for automatic retry before the entry is dead-lettered instead
	// (see cmd.handleFailedInjection). A composer-dirty failure skips
	// straight to dead-letter regardless of this bound — retyping into a
	// known-dirty composer duplicates content rather than fixing anything.
	MaxInjectionAttempts = 2

	// attemptsSidecarSuffix names the append-only sidecar Claim.MarkAttempt
	// writes next to a claim to durably record an attempt (see
	// attemptsSidecarPath). idSidecarSuffix names the write-once sidecar
	// DrainClaims uses to durably record an ID backfilled onto a legacy
	// entry (see idSidecarPath). Both share their claim's ".claimed.<suffix>"
	// name as a prefix, so the orphan sweep must recognize and skip them
	// explicitly (isClaimSidecarOrTemp) rather than treating each as an
	// independent orphaned claim of the same entry. legacyTmpSuffix is kept
	// in that same exclusion list defensively: it named the old write-then-
	// rename Claim.Persist's temp file, the exact shape whose misclassification
	// by the orphan sweep caused a truncated sibling to overwrite an intact
	// claim (codex gate verdict at a752f4a7, gtn-81j) — that code path is gone,
	// but a stray file of that name must still never be mistaken for a claim.
	attemptsSidecarSuffix = ".attempts"
	idSidecarSuffix       = ".id"
	legacyTmpSuffix       = ".tmp"
)

// nudgeConfig loads nudge-specific thresholds from town settings.
func nudgeConfig(townRoot string) *config.NudgeThresholds {
	return config.LoadOperationalConfig(townRoot).GetNudgeConfig()
}

// QueuedNudge represents a nudge message stored in the queue.
type QueuedNudge struct {
	// ID is a stable identity for this logical nudge, assigned once at first
	// Enqueue and preserved across Requeue (even though each requeue writes a
	// new file with a new timestamp-based name). Used to correlate attempts
	// across the poller, the idle watcher, and a poller restart, and to
	// reference a specific dead-lettered entry for inspection/replay.
	ID        string    `json:"id,omitempty"`
	Sender    string    `json:"sender"`
	Message   string    `json:"message"`
	Priority  string    `json:"priority"`
	Kind      string    `json:"kind,omitempty"`
	ThreadID  string    `json:"thread_id,omitempty"`
	Severity  string    `json:"severity,omitempty"`
	Timestamp time.Time `json:"timestamp"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// DeliverAfter, if non-zero, defers delivery until this time has passed.
	// Drain skips (but does not discard) the nudge until the deadline is met.
	DeliverAfter time.Time `json:"deliver_after,omitempty"`
	// Attempts counts failed injection attempts for this entry. It persists
	// across Requeue (on-disk, so it survives a poller restart) and bounds
	// how many times a failed delivery is retried before dead-lettering.
	Attempts int `json:"attempts,omitempty"`
	// InFlight is true from the moment Claim.MarkAttempt persists it (just
	// before a live injection attempt) until the attempt's outcome is
	// recorded (dead-letter, requeue via Enqueue, or the claim is acked on
	// success — Enqueue always clears it). Attempts alone cannot tell an
	// unfinished delivery from a confirmed non-delivery: a poller killed
	// after typing but before recording an outcome leaves a claim whose
	// Attempts count looks like an ordinary, safe-to-retry failure, when in
	// fact some or all of the message may already have reached the
	// composer. A claim restored by the orphan sweep with InFlight still
	// true is exactly that case, and must never be retyped (codex,
	// nudge_failure.go:152, changes-requested at REVISION 3 — High 2).
	InFlight bool `json:"in_flight,omitempty"`
	// LastError records the most recent injection failure, for dead-letter
	// inspection.
	LastError string `json:"last_error,omitempty"`
}

// queueDir returns the nudge queue directory for a given session.
// Path: <townRoot>/.runtime/nudge_queue/<session>/
func queueDir(townRoot, session string) string {
	// Sanitize session name for filesystem safety
	safe := strings.ReplaceAll(session, "/", "_")
	return filepath.Join(townRoot, constants.DirRuntime, "nudge_queue", safe)
}

// randomSuffix returns a short random hex string to disambiguate filenames
// when multiple processes enqueue within the same nanosecond.
func randomSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// NewID generates a fresh stable identity for a QueuedNudge, the same
// generator Enqueue uses when a caller doesn't supply one. Exported for
// callers that build a QueuedNudge to dead-letter directly, bypassing
// Enqueue (e.g. cmd.deliverNudge's wait-idle path), and need the ID to be
// known and stable BEFORE the entry is persisted: DeadLetter also assigns
// one when missing, but only on its own internal copy of the struct, so a
// caller that reports the ID afterward (e.g. an alert mail) would still
// report a blank one (codex, nudge.go:253/282, changes-requested at
// 08964387/95f841e6 rework).
func NewID() string {
	return randomSuffix() + randomSuffix()
}

// Enqueue writes a nudge to the queue for the given session.
// The nudge will be picked up by the agent's hook at the next turn boundary.
// Returns an error if the queue is full (MaxQueueDepth reached).
func Enqueue(townRoot, session string, nudge QueuedNudge) error {
	dir := queueDir(townRoot, session)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating nudge queue dir: %w", err)
	}

	// Check queue depth before writing to prevent runaway senders.
	maxDepth := nudgeConfig(townRoot).MaxQueueDepthV()
	pending, _ := Pending(townRoot, session)
	if pending >= maxDepth {
		return fmt.Errorf("nudge queue for %s is full (%d/%d pending)", session, pending, maxDepth)
	}

	if nudge.Timestamp.IsZero() {
		nudge.Timestamp = time.Now()
	}
	if nudge.Priority == "" {
		nudge.Priority = PriorityNormal
	}
	if nudge.ID == "" {
		nudge.ID = randomSuffix() + randomSuffix()
	}
	// Every write into the pending queue represents a fresh, clean start:
	// whatever the caller learned (or didn't) about a prior attempt is
	// resolved by the time it calls Enqueue (directly, or via Requeue after
	// a determined delivery failure) — this is the ONLY place InFlight is
	// cleared, so a requeued entry never carries a stale in-flight marker
	// into the pending queue (see QueuedNudge.InFlight).
	nudge.InFlight = false

	// Set expiry if not already specified by the caller.
	if nudge.ExpiresAt.IsZero() {
		switch nudge.Priority {
		case PriorityUrgent:
			nudge.ExpiresAt = nudge.Timestamp.Add(DefaultUrgentTTL)
		default:
			nudge.ExpiresAt = nudge.Timestamp.Add(DefaultNormalTTL)
		}
	}

	data, err := json.MarshalIndent(nudge, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling nudge: %w", err)
	}

	// Use nanosecond timestamp + random suffix for unique, ordered filenames.
	// The random suffix prevents collisions when multiple agents enqueue
	// nudges for the same session within the same nanosecond.
	filename := fmt.Sprintf("%d-%s.json", nudge.Timestamp.UnixNano(), randomSuffix())
	path := filepath.Join(dir, filename)

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing nudge to queue: %w", err)
	}

	return nil
}

// Requeue writes previously drained nudges back to the queue for later delivery.
// Existing timestamps are preserved so FIFO ordering remains stable relative to
// one another; only expired nudges are skipped.
func Requeue(townRoot, session string, nudges []QueuedNudge) error {
	for _, n := range nudges {
		if !n.ExpiresAt.IsZero() && time.Now().After(n.ExpiresAt) {
			continue
		}
		if err := Enqueue(townRoot, session, n); err != nil {
			return err
		}
	}
	return nil
}

// RequeueTracked is like Requeue but attempts EVERY entry even if an earlier
// one fails to enqueue, and returns the entries that failed (empty on full
// success). Used only by the nudge-poller's failure handling
// (cmd.handleFailedInjection), which acks a claim only after its outcome is
// durable (see Claim.Ack) and needs to know precisely which entries are NOT
// yet durable anywhere, so it can leave those claims un-acked instead of
// acking (and thereby losing) a claim whose requeue write itself failed.
// Requeue itself keeps its original stop-on-first-error contract for its
// other callers.
func RequeueTracked(townRoot, session string, nudges []QueuedNudge) (failed []QueuedNudge, err error) {
	var firstErr error
	for _, n := range nudges {
		if !n.ExpiresAt.IsZero() && time.Now().After(n.ExpiresAt) {
			continue // expired: nothing left to deliver, not a failure
		}
		if enqErr := Enqueue(townRoot, session, n); enqErr != nil {
			failed = append(failed, n)
			if firstErr == nil {
				firstErr = enqErr
			}
		}
	}
	return failed, firstErr
}

// Claim is a durably-claimed queue entry returned by DrainClaims: the
// underlying file has been atomically renamed out of the pending queue (so
// no other Drain/DrainClaims call can pick it up) but NOT yet deleted. The
// caller resolves it by calling Ack once the entry's outcome — successful
// delivery, a durable dead-letter write, or a durable requeue write — is
// itself durable. An unresolved Claim is not lost: it sits on disk under its
// .claimed name, and a future Drain/DrainClaims call's orphan sweep restores
// it to the pending queue once staleClaimThreshold has passed.
type Claim struct {
	Nudge QueuedNudge
	path  string
}

// Ack removes the claim's underlying file. Call this only AFTER the entry's
// outcome is itself durable (written to the dead-letter store, rewritten to
// the queue via Enqueue, or successfully delivered) — acking first and
// crashing before that write is exactly the queue.go:305 ordering bug this
// type exists to close (hq-g52db rework, codex changes-requested at
// 08964387). Removing an already-removed file is not an error.
//
// Also removes this claim's sidecars (attemptsSidecarPath, idSidecarPath),
// if any: they exist only for this claim's lifetime, and leaving them
// behind on every resolved claim would leak one file per delivered nudge
// forever. Best-effort — a leftover sidecar is inert clutter, never a
// correctness risk (isClaimSidecarOrTemp keeps the orphan sweep from ever
// treating a bare sidecar as an independent claim), so a removal failure
// here is not reported as an Ack error.
func (c Claim) Ack() error {
	if err := os.Remove(c.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("acking nudge claim: %w", err)
	}
	_ = os.Remove(attemptsSidecarPath(c.path))
	_ = os.Remove(idSidecarPath(c.path))
	return nil
}

// attemptsSidecarPath returns the append-only attempts-count sidecar for a
// claim file. MarkAttempt appends exactly one byte per attempt and never
// rewrites or renames it; the sidecar's SIZE is the durable attempt count.
// This exists so an attempt is recorded WITHOUT ever rewriting the claim's
// own file: the previous design (Claim.Persist) wrote a full-record
// temp-then-rename onto the claim's own ".claimed.<suffix>" name, and a
// partial write there left a truncated ".tmp" sibling that the orphan
// sweep's naive ".claimed"-substring match mistook for a second, independent
// orphaned claim of the SAME entry — since directory entries sort lexically,
// the truncated file restored second and overwrote the just-restored intact
// one, and the next drain deleted the result as malformed: total loss of
// the only copy of the message (codex gate verdict at a752f4a7, gtn-81j). An
// append of a single byte cannot leave a claim's payload in that state: a
// crash mid-append leaves the sidecar at its previous size or one byte
// longer, never with any existing byte altered.
func attemptsSidecarPath(claimPath string) string {
	return claimPath + attemptsSidecarSuffix
}

// attemptsSidecarCount returns the durable attempt count recorded in
// claimPath's attempts sidecar (its file size), or 0 if no attempt has been
// recorded yet.
func attemptsSidecarCount(claimPath string) int {
	info, err := os.Stat(attemptsSidecarPath(claimPath))
	if err != nil {
		return 0
	}
	return int(info.Size())
}

// idSidecarPath returns the write-once sidecar that durably records an ID
// backfilled onto a legacy claim that predates the ID field (see NewID and
// DrainClaims). Like the attempts sidecar, this exists so an assignment is
// recorded without ever rewriting the claim's own file.
func idSidecarPath(claimPath string) string {
	return claimPath + idSidecarSuffix
}

// writeIDSidecar durably records id next to claimPath without touching
// claimPath's own content. O_EXCL makes the write single-shot: a sidecar
// that already exists is left alone rather than rewritten.
func writeIDSidecar(claimPath, id string) error {
	f, err := os.OpenFile(idSidecarPath(claimPath), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return fmt.Errorf("creating id sidecar: %w", err)
	}
	defer f.Close()
	_, err = f.WriteString(id)
	return err
}

// readIDSidecar returns the durably-assigned ID for claimPath, if any, and
// whether the sidecar was present and non-empty.
func readIDSidecar(claimPath string) (string, bool) {
	data, err := os.ReadFile(idSidecarPath(claimPath))
	if err != nil {
		return "", false
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", false
	}
	return id, true
}

// isClaimSidecarOrTemp reports whether name is a sidecar or leftover temp
// file derived from a claim's own name (attemptsSidecarSuffix,
// idSidecarSuffix, legacyTmpSuffix) rather than an independent claim. Every
// such name shares its claim's ".claimed.<suffix>" prefix, so the orphan
// sweep's substring match on ".claimed" would otherwise treat each one as a
// second, independent orphaned claim of the same entry — see
// attemptsSidecarPath's doc comment for what that misclassification cost.
func isClaimSidecarOrTemp(name string) bool {
	return strings.HasSuffix(name, attemptsSidecarSuffix) ||
		strings.HasSuffix(name, idSidecarSuffix) ||
		strings.HasSuffix(name, legacyTmpSuffix)
}

// MarkAttempt durably records one more delivery attempt for c BEFORE the
// caller attempts a live injection (see attemptsSidecarPath), then updates
// the in-memory Nudge to match: Attempts incremented, and InFlight set
// (cleared only by a subsequent Enqueue — dead-letter and successful ack
// both remove the file instead). A claim whose sidecar still reads a
// nonzero count after a restart never went through either of those — the
// attempt that recorded it never reported an outcome — see
// QueuedNudge.InFlight. Callers that inject a batch of claims in one tmux
// call should MarkAttempt every claim in the batch first, then extract the
// (now-incremented) Nudge values for FormatForInjection/
// handleFailedInjection — see nudge_poller.go and watchAndDeliver.
func (c *Claim) MarkAttempt() error {
	f, err := os.OpenFile(attemptsSidecarPath(c.path), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("opening attempts sidecar: %w", err)
	}
	if _, err := f.Write([]byte{1}); err != nil {
		f.Close()
		return fmt.Errorf("appending attempt marker: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing attempts sidecar: %w", err)
	}
	c.Nudge.Attempts++
	c.Nudge.InFlight = true
	return nil
}

// AckClaims resolves every claim in claims whose Nudge.ID is NOT present in
// unresolved, by removing its underlying file — call only after each acked
// entry's outcome (successful delivery, or a durable dead-letter/requeue
// write) is itself durable. A claim whose ID IS in unresolved is
// deliberately left un-acked: it stays on disk under its .claimed name, and
// a future Drain/DrainClaims orphan sweep restores it to the pending queue
// once staleClaimThreshold has passed. This is what keeps a Requeue write
// that itself fails (see Requeue's returned failed slice) from silently
// losing the entry — acking every original claim unconditionally, as
// before this existed, discarded the only durable copy the moment the
// caller decided (successfully or not) what to do with it (codex,
// nudge_poller.go:157 / internal/acp/propulsion.go:198,235,
// changes-requested at 08964387/95f841e6 rework). onAckErr, if non-nil, is
// called with any per-claim removal error; logging is the caller's
// business.
func AckClaims(claims []Claim, unresolved []QueuedNudge, onAckErr func(Claim, error)) {
	skip := make(map[string]bool, len(unresolved))
	for _, n := range unresolved {
		if n.ID != "" {
			skip[n.ID] = true
		}
	}
	for _, c := range claims {
		if skip[c.Nudge.ID] {
			continue
		}
		if err := c.Ack(); err != nil && onAckErr != nil {
			onAckErr(c, err)
		}
	}
}

// Drain reads and removes all queued nudges for a session, returning them
// in FIFO order. This is called by the hook to pick up pending nudges: the
// hook always "delivers" by returning formatted content to the caller, which
// cannot itself fail the way a tmux injection can, so immediate deletion is
// safe here. Callers that attempt a tmux injection (which CAN fail, and
// whose failure must be durably dead-lettered or requeued before the
// original entry disappears) should use DrainClaims instead — see its doc
// and Claim.Ack.
//
// Uses rename-then-process to prevent concurrent Drain calls from delivering
// the same nudge twice: each file is atomically renamed to a .claimed suffix
// before reading, so only one caller can claim each nudge.
//
// Expired nudges (past ExpiresAt) are silently discarded during drain.
// Orphaned .claimed files from crashed drainers are swept if older than 5 minutes.
func Drain(townRoot, session string) ([]QueuedNudge, error) {
	claims, err := DrainClaims(townRoot, session)
	if err != nil {
		return nil, err
	}
	nudges := make([]QueuedNudge, 0, len(claims))
	for _, c := range claims {
		nudges = append(nudges, c.Nudge)
		if ackErr := c.Ack(); ackErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to remove processed claim: %v\n", ackErr)
		}
	}
	return nudges, nil
}

// restoreOrphanedClaim moves an abandoned claim back into the pending
// queue, merging in any amendments recorded in its sidecars — an attempts
// count from Claim.MarkAttempt, an ID backfilled onto a legacy entry — since
// the claim's own file was last written (see attemptsSidecarPath,
// idSidecarPath).
//
// When neither sidecar exists, the claim's content is exactly what it was
// at claim time, so a bare rename is enough — cheap, and it never reads or
// rewrites the payload.
//
// When a sidecar exists, the merged content is written to a fresh temp file
// and landed with ONE atomic rename to restoredPath, which cannot already
// exist — its only creator renamed it away to become claimPath. Only after
// that lands is the original claim (and its sidecars) removed. Nothing here
// ever rewrites claimPath or restoredPath in place: the failure this
// replaces was exactly a write-then-rename onto a name the orphan sweep
// could also match (see attemptsSidecarPath's doc comment).
func restoreOrphanedClaim(claimPath, restoredPath string) error {
	attemptsDelta := attemptsSidecarCount(claimPath)
	sidecarID, hasID := readIDSidecar(claimPath)

	if attemptsDelta == 0 && !hasID {
		return os.Rename(claimPath, restoredPath)
	}

	data, err := os.ReadFile(claimPath)
	if err != nil {
		return fmt.Errorf("reading orphaned claim: %w", err)
	}
	var n QueuedNudge
	if err := json.Unmarshal(data, &n); err != nil {
		// Malformed payload — nothing sane to restore. Drop it and its
		// sidecars rather than resurrecting garbage into the live queue.
		_ = os.Remove(claimPath)
		_ = os.Remove(attemptsSidecarPath(claimPath))
		_ = os.Remove(idSidecarPath(claimPath))
		return nil
	}
	if attemptsDelta > 0 {
		n.Attempts += attemptsDelta
		n.InFlight = true
	}
	if hasID && n.ID == "" {
		n.ID = sidecarID
	}
	merged, err := json.MarshalIndent(n, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling restored claim: %w", err)
	}

	tmp := restoredPath + ".restore-" + randomSuffix()
	if err := os.WriteFile(tmp, merged, 0644); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("writing restored claim: %w", err)
	}
	if err := os.Rename(tmp, restoredPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("landing restored claim: %w", err)
	}

	if err := os.Remove(claimPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Warning: restored claim landed at %s but failed to remove original %s: %v\n", restoredPath, claimPath, err)
	}
	_ = os.Remove(attemptsSidecarPath(claimPath))
	_ = os.Remove(idSidecarPath(claimPath))
	return nil
}

// DrainClaims is like Drain but does not delete each entry's claim file —
// it returns Claims that the caller must explicitly Ack once the entry's
// outcome is durable. This closes the crash window Drain's immediate
// deletion left open for callers that attempt tmux delivery: previously the
// claimed file was removed as soon as it was unmarshaled, before the caller
// even attempted delivery, so a crash between that removal and the caller's
// dead-letter or requeue write lost the entry with no durable trace
// (queue.go:305 vs cmd/nudge_failure.go:57, codex changes-requested at
// 08964387). With DrainClaims, a crash before Ack leaves the original
// .claimed file in place: a future Drain/DrainClaims call's orphan sweep
// restores it to the pending queue once staleClaimThreshold has passed, so
// the entry survives in whichever of the two places (a fresh dead-letter/
// queue write, or the still-present original claim) the crash landed before.
//
// Expired nudges (past ExpiresAt) are discarded immediately (no ack needed —
// there is nothing more to deliver). Deferred nudges are unclaimed and left
// in the queue. Orphaned .claimed files from crashed drainers are swept if
// older than staleClaimThreshold.
func DrainClaims(townRoot, session string) ([]Claim, error) {
	dir := queueDir(townRoot, session)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading nudge queue: %w", err)
	}

	// Requeue orphaned .claimed files from crashed drainers.
	// A .claimed file older than staleClaimThreshold is certainly orphaned —
	// normal processing completes in milliseconds. We restore it to .json so
	// it gets picked up on this or a future Drain call, rather than deleting
	// it (which would permanently drop the nudge). isClaimSidecarOrTemp
	// excludes a claim's OWN sidecars/temp siblings — sharing the claim's
	// ".claimed.<suffix>" name as a prefix, they would otherwise match
	// ".claimed" too and be treated as a second, independent orphan of the
	// SAME entry (see attemptsSidecarPath's doc comment).
	staleThreshold := nudgeConfig(townRoot).StaleClaimThresholdD()
	now := time.Now()
	for _, entry := range entries {
		name := entry.Name()
		if !strings.Contains(name, ".claimed") || isClaimSidecarOrTemp(name) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > staleThreshold {
			claimPath := filepath.Join(dir, name)
			// Strip everything from ".claimed" onward to restore original .json filename
			claimedIdx := strings.Index(name, ".claimed")
			restoredPath := filepath.Join(dir, name[:claimedIdx])
			if err := restoreOrphanedClaim(claimPath, restoredPath); err != nil {
				// Restore failed — leave the orphaned claim file in place and
				// retry on a future sweep. Removing it here would permanently
				// drop the nudge, exactly what the "never drop" policy
				// (see nudge.MaxInjectionAttempts callers) prohibits.
				fmt.Fprintf(os.Stderr, "Warning: failed to requeue orphaned claim %s, will retry on a future sweep: %v\n", name, err)
			}
		}
	}

	// Sort by name (timestamp-based) for FIFO ordering
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	var claims []Claim
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		path := filepath.Join(dir, entry.Name())

		// Reset the mtime BEFORE the rename, not after. Rename does not
		// update mtime — a file freshly claimed would otherwise carry its
		// original enqueue-time mtime for the whole gap between the rename
		// and a later Chtimes call, and a nudge that sat in the queue
		// longer than staleThreshold (routine for a normal 30-minute-TTL
		// entry) would read as an orphaned claim the INSTANT it becomes
		// visible under its .claimed name — a second, concurrent
		// Drain/DrainClaims call's orphan sweep could restore and
		// redeliver it while this claimer is still mid-delivery, exactly
		// the double-delivery the staleness check exists to prevent, not
		// enable. Touching the mtime on the ORIGINAL path first closes that
		// window: by the time the file is visible under any .claimed name,
		// its mtime is already current (codex, queue.go:430,
		// changes-requested at REVISION 3 — High 3; the post-rename order
		// was the prior, still-racy fix at queue.go:315,
		// 08964387/95f841e6 rework). Best-effort: a Chtimes failure just
		// means the claim relies on the original mtime as before, so it is
		// logged, not fatal.
		claimTime := time.Now()
		if err := os.Chtimes(path, claimTime, claimTime); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to reset claim mtime for %s: %v\n", entry.Name(), err)
		}

		// Atomically claim the file by renaming it. If another Drain call
		// is racing us, only one rename will succeed — the loser gets
		// ENOENT and moves on. This prevents double-delivery.
		//
		// Each drainer uses a unique claim suffix to avoid destination
		// collisions. On Windows, os.Rename to a shared destination is
		// not atomic — two goroutines can both "succeed" via
		// MOVEFILE_REPLACE_EXISTING, causing data loss. Unique suffixes
		// ensure each rename has a distinct target.
		claimPath := path + ".claimed." + randomSuffix()
		if err := os.Rename(path, claimPath); err != nil {
			// Another Drain got it first, or file was already removed
			continue
		}

		data, err := os.ReadFile(claimPath)
		if err != nil {
			if os.IsNotExist(err) {
				// File vanished between rename and read — treat as lost race
				continue
			}
			// Transient read error (e.g., Windows AV/indexer holding a share
			// lock) — unclaim so the nudge can be retried on a future Drain
			// call rather than permanently lost.
			_ = os.Rename(claimPath, path) // best-effort unclaim; orphan sweep catches failures
			continue
		}

		var n QueuedNudge
		if err := json.Unmarshal(data, &n); err != nil {
			// Malformed — clean up
			if rmErr := os.Remove(claimPath); rmErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to remove malformed claim %s: %v\n", entry.Name(), rmErr)
			}
			continue
		}

		// Skip expired nudges — stale messages create noise, not value.
		if !n.ExpiresAt.IsZero() && now.After(n.ExpiresAt) {
			if rmErr := os.Remove(claimPath); rmErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to remove expired nudge %s: %v\n", entry.Name(), rmErr)
			}
			continue
		}

		// Deferred nudge: not ready yet — unclaim and leave in queue.
		if !n.DeliverAfter.IsZero() && now.Before(n.DeliverAfter) {
			if renameErr := os.Rename(claimPath, path); renameErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to unclaim deferred nudge %s: %v\n", entry.Name(), renameErr)
			}
			continue
		}

		claim := Claim{Nudge: n, path: claimPath}

		// A file queued by a version of this code before the ID field
		// existed deserializes with n.ID == "". AckClaims (see its skip
		// map below) and handleFailedInjection's unresolved-entry
		// matching both key exclusively on ID, and both deliberately skip
		// entries whose ID is empty — so a claim that still carries an
		// empty ID here would never be recognized as unresolved and
		// would be acked (deleted) even on a failed requeue/dead-letter
		// write. Assign a fresh ID now, at claim time, so every claim
		// leaving DrainClaims has a non-empty, comparable identity
		// regardless of what shape the file on disk started in, and
		// persist it immediately so a future orphan-sweep restore carries
		// the same ID rather than reverting to blank (codex, queue.go:288,
		// changes-requested at REVISION 3 — High 4). Recorded in a write-once
		// sidecar (see idSidecarPath), never by rewriting the claim's own
		// file — that rewrite is exactly the mechanism removed from
		// MarkAttempt (see attemptsSidecarPath's doc comment).
		if claim.Nudge.ID == "" {
			claim.Nudge.ID = NewID()
			if perr := writeIDSidecar(claim.path, claim.Nudge.ID); perr != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to persist assigned ID for %s: %v\n", entry.Name(), perr)
			}
		}

		claims = append(claims, claim)
	}

	return claims, nil
}

// queueHasPendingID reports whether the active queue for session holds a
// still-pending (.json, not yet claimed) entry with the given ID. Used by
// the dead-letter replay sweep (deadletter.go) to tell a genuinely orphaned
// ".replaying" claim from one whose Enqueue already succeeded before a
// crash — see sweepStaleDeadLetterReplays. Malformed entries are skipped
// rather than failing the scan; a read/parse error just means this entry
// doesn't count as a match.
func queueHasPendingID(townRoot, session, id string) bool {
	if id == "" {
		return false
	}
	dir := queueDir(townRoot, session)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var n QueuedNudge
		if err := json.Unmarshal(data, &n); err != nil {
			continue
		}
		if n.ID == id {
			return true
		}
	}
	return false
}

// Pending returns the count of queued nudges for a session without draining.
// This is an approximate count — it does not check expiry or read file contents.
func Pending(townRoot, session string) (int, error) {
	dir := queueDir(townRoot, session)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading nudge queue: %w", err)
	}

	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
	}

	return count, nil
}

// PendingOrClaimed reports whether the queue directory holds ANYTHING —
// either a fresh .json entry, or a leftover .claimed file from a prior
// drain that has not yet been Acked. A poller/watcher that gates its drain
// call on Pending()==0 alone (Pending counts .json files only) never calls
// DrainClaims when the queue holds ONLY .claimed files — and DrainClaims is
// what runs the orphan sweep that restores a claim left behind by a
// crashed drainer. Without this, a claimed-but-crashed entry that hasn't
// yet crossed staleClaimThreshold sits invisible forever: nothing ever
// calls DrainClaims again to notice it has now gone stale (codex,
// nudge_poller.go:111 / nudge.go:353, changes-requested at
// 08964387/95f841e6 rework). Cheaper than a full DrainClaims (no rename,
// no read) so it's safe to call on every poll tick.
func PendingOrClaimed(townRoot, session string) (bool, error) {
	dir := queueDir(townRoot, session)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading nudge queue: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".json") || strings.Contains(entry.Name(), ".claimed") {
			return true, nil
		}
	}
	return false, nil
}

// QueueLen returns the number of pending nudges for a session without draining.
// Returns 0 on error — callers use this for quick checks. Missing queue
// directories are expected (no nudges yet) and silenced; other filesystem
// errors are logged to stderr so they don't go unnoticed.
func QueueLen(townRoot, session string) int {
	n, err := Pending(townRoot, session)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: nudge queue check failed for %s: %v\n", session, err)
	}
	return n
}

// RemoveKindByThread deletes queued nudges for a session that match both the
// provided kind and thread ID. It only removes queued .json files, leaving any
// in-flight claimed files alone so concurrent drainers can finish safely.
func RemoveKindByThread(townRoot, session, kind, threadID string) (int, error) {
	if kind == "" || threadID == "" {
		return 0, nil
	}

	dir := queueDir(townRoot, session)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading nudge queue: %w", err)
	}

	removed := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, fmt.Errorf("reading queued nudge %s: %w", entry.Name(), err)
		}

		var n QueuedNudge
		if err := json.Unmarshal(data, &n); err != nil {
			continue
		}
		if n.Kind != kind || n.ThreadID != threadID {
			continue
		}

		if err := os.Remove(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, fmt.Errorf("removing queued nudge %s: %w", entry.Name(), err)
		}
		removed++
	}

	return removed, nil
}

// FormatForInjection formats queued nudges as a system-reminder block
// suitable for Claude Code hook output.
func FormatForInjection(nudges []QueuedNudge) string {
	if len(nudges) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("<system-reminder>\n")

	// Separate urgent from normal
	var urgent, normal []QueuedNudge
	for _, n := range nudges {
		if n.Priority == PriorityUrgent {
			urgent = append(urgent, n)
		} else {
			normal = append(normal, n)
		}
	}

	if len(urgent) > 0 {
		b.WriteString(fmt.Sprintf("QUEUED NUDGE (%d urgent):\n\n", len(urgent)))
		for _, n := range urgent {
			b.WriteString(fmt.Sprintf("  [URGENT from %s] %s\n", n.Sender, n.Message))
		}
		if len(normal) > 0 {
			b.WriteString(fmt.Sprintf("\nPlus %d non-urgent nudge(s):\n", len(normal)))
			for _, n := range normal {
				b.WriteString(fmt.Sprintf("  [from %s] %s\n", n.Sender, n.Message))
			}
		}
		b.WriteString("\nHandle urgent nudges before continuing current work.\n")
	} else {
		b.WriteString(fmt.Sprintf("QUEUED NUDGE (%d message(s)):\n\n", len(normal)))
		for _, n := range normal {
			b.WriteString(fmt.Sprintf("  [from %s] %s\n", n.Sender, n.Message))
		}
		b.WriteString("\nThis is a background notification. Continue current work unless the nudge is higher priority.\n")
	}

	b.WriteString("</system-reminder>\n")
	return b.String()
}
