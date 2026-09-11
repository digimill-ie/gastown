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

// Requeue writes previously drained nudges back to the queue for later
// delivery. Existing timestamps are preserved so FIFO ordering remains
// stable relative to one another; only expired nudges are skipped.
//
// It attempts EVERY entry even if an earlier one fails to enqueue — the
// previous version returned on the first error, silently never attempting
// the rest of the batch. Returns the entries that failed to enqueue (empty
// on full success): callers that Ack a claim only after its outcome is
// durable (see Claim.Ack) need this to know precisely which entries are
// NOT yet durable anywhere, so they can leave those claims un-acked instead
// of acking (and thereby losing) a claim whose requeue write itself failed
// (codex, nudge_poller.go:157, changes-requested at 08964387/95f841e6
// rework).
func Requeue(townRoot, session string, nudges []QueuedNudge) (failed []QueuedNudge, err error) {
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
func (c Claim) Ack() error {
	if err := os.Remove(c.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("acking nudge claim: %w", err)
	}
	return nil
}

// Persist rewrites the claim's underlying .claimed file with the claim's
// current in-memory Nudge state (atomic write-then-rename within the same
// directory). Call this to durably record a state change — in particular
// Attempts — BEFORE a fallible operation like a tmux injection attempt, so
// a crash during that operation leaves the PERSISTED (already-incremented)
// state behind for a future orphan-sweep restore, not the stale
// pre-attempt state. Without this, a poller killed while actually typing
// (after DrainClaims but before the failure/success handling that would
// otherwise persist Attempts) leaves a claim on disk that still reads
// Attempts=0, so the orphan sweep restores it as a fresh, unattempted entry
// and a future cycle retypes it — risking duplicate content in whatever
// the first, crashed attempt already delivered (codex, nudge_poller.go:146,
// changes-requested at 08964387/95f841e6 rework).
func (c Claim) Persist() error {
	data, err := json.MarshalIndent(c.Nudge, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling nudge: %w", err)
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("writing claim state: %w", err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return fmt.Errorf("persisting claim state: %w", err)
	}
	return nil
}

// MarkAttempt increments the claim's Attempts count and persists it (see
// Persist) BEFORE the caller attempts a live delivery. Callers that inject
// a batch of claims in one tmux call should MarkAttempt every claim in the
// batch first, then extract the (now-incremented) Nudge values for
// FormatForInjection/handleFailedInjection — see nudge_poller.go and
// watchAndDeliver.
func (c *Claim) MarkAttempt() error {
	c.Nudge.Attempts++
	return c.Persist()
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
	// normal processing completes in milliseconds. We rename it back to .json
	// so it gets picked up on this or a future Drain call, rather than deleting
	// it (which would permanently drop the nudge).
	staleThreshold := nudgeConfig(townRoot).StaleClaimThresholdD()
	now := time.Now()
	for _, entry := range entries {
		if !strings.Contains(entry.Name(), ".claimed") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > staleThreshold {
			orphanPath := filepath.Join(dir, entry.Name())
			// Strip everything from ".claimed" onward to restore original .json filename
			name := entry.Name()
			claimedIdx := strings.Index(name, ".claimed")
			restoredPath := filepath.Join(dir, name[:claimedIdx])
			if err := os.Rename(orphanPath, restoredPath); err != nil {
				// Rename failed — remove as last resort to prevent infinite accumulation
				fmt.Fprintf(os.Stderr, "Warning: failed to requeue orphaned claim %s: %v\n", entry.Name(), err)
				_ = os.Remove(orphanPath)
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

		// Rename does not update mtime — the claimed file still carries the
		// original enqueue-time mtime. Without resetting it, a nudge that
		// sat in the queue longer than staleThreshold (routine for a normal
		// 30-minute-TTL entry) reads as an orphaned claim the INSTANT it is
		// claimed, so a second, concurrent Drain/DrainClaims call's orphan
		// sweep can restore and redeliver it while this claimer is still
		// mid-delivery — the double-delivery the staleness check exists to
		// prevent, not enable (codex, queue.go:315, changes-requested at
		// 08964387/95f841e6 rework). Best-effort: a Chtimes failure just
		// means the claim relies on the original mtime as before, so it is
		// logged, not fatal.
		claimTime := time.Now()
		if err := os.Chtimes(claimPath, claimTime, claimTime); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to reset claim mtime for %s: %v\n", entry.Name(), err)
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

		claims = append(claims, Claim{Nudge: n, path: claimPath})
	}

	return claims, nil
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
