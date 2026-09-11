// deadletter.go provides durable storage for nudges that failed delivery and
// were removed from the active queue rather than retried forever.
//
// A dead-lettered entry is never retyped automatically: it sits outside the
// drained queue directory until an operator inspects it (`gt nudge
// dead-letter list <session>`) and explicitly replays it (`gt nudge
// dead-letter replay <session> <id>`).
package nudge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// DeadLetterEntry is a nudge that failed delivery, persisted for inspection
// and explicit replay.
type DeadLetterEntry struct {
	QueuedNudge

	// Source identifies which caller dead-lettered the entry: "nudge-poller",
	// "idle-watcher", or "wait-idle".
	Source string `json:"source"`
	// PaneCapture holds the last lines of the target pane at the moment of
	// failure, for diagnosis.
	PaneCapture string `json:"pane_capture,omitempty"`
	// UncertainDelivery is true when the failure could not rule out that the
	// message actually reached the agent (e.g., composer-dirty proves nothing
	// was duplicated by a retype; a generic injection error does not prove
	// the opposite). False means delivery is known to have not happened.
	UncertainDelivery bool `json:"uncertain_delivery"`
	// DeadLetteredAt is when this entry was moved out of the active queue.
	DeadLetteredAt time.Time `json:"dead_lettered_at"`
}

// deadLetterDir returns the dead-letter directory for a given session.
// Path: <townRoot>/.runtime/nudge_deadletter/<session>/
func deadLetterDir(townRoot, session string) string {
	safe := strings.ReplaceAll(session, "/", "_")
	return filepath.Join(townRoot, constants.DirRuntime, "nudge_deadletter", safe)
}

// deadLetterReplayStaleThreshold bounds how long a ".replaying" claim file
// can sit unresolved before it is treated as orphaned by a crashed replay.
const deadLetterReplayStaleThreshold = 5 * time.Minute

// sweepStaleDeadLetterReplays restores any ".replaying" claim file older
// than deadLetterReplayStaleThreshold back to its original dead-letter
// filename, so it becomes visible to List/Replay again.
//
// ReplayDeadLetter claims an entry (atomic rename to "<name>.json.replaying")
// before re-enqueuing it and removing the original — but both ListDeadLetters
// and ReplayDeadLetter's own scan filter strictly to a ".json" suffix, so a
// process that crashes between the claim rename and the Enqueue/remove that
// follows leaves that file invisible to both: not listed, not reachable by
// id, forever (codex, deadletter.go:164, changes-requested at
// 08964387/95f841e6 rework). This mirrors the nudge queue's orphaned-.claimed
// sweep in DrainClaims. Best-effort: a rename race with a genuinely in-flight
// replay just means this loses the race (ENOENT) and no-ops, which is safe —
// the same race pattern the queue sweep already relies on.
func sweepStaleDeadLetterReplays(townRoot, session, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	now := time.Now()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".replaying") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) <= deadLetterReplayStaleThreshold {
			continue
		}
		stalePath := filepath.Join(dir, entry.Name())
		restoredPath := strings.TrimSuffix(stalePath, ".replaying")

		// A crash between ReplayDeadLetter's Enqueue succeeding (:221) and
		// its subsequent claim removal leaves exactly this shape: a stale
		// .replaying claim whose payload is ALREADY live in the active
		// queue. Restoring it here, as before, would silently re-enqueue a
		// second copy the next time it is replayed — this claim isn't
		// actually orphaned, it's finished (codex, deadletter.go:211,
		// changes-requested at REVISION 3 — High 9). Read the claim's ID
		// and check the active queue for a still-pending entry with the
		// same ID first; if found, the crash landed after Enqueue and this
		// claim is done — remove it rather than resurrect it.
		if data, rerr := os.ReadFile(stalePath); rerr == nil {
			var e DeadLetterEntry
			if jerr := json.Unmarshal(data, &e); jerr == nil && e.ID != "" && queueHasPendingID(townRoot, session, e.ID) {
				if rmErr := os.Remove(stalePath); rmErr != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to remove already-replayed dead-letter claim %s: %v\n", entry.Name(), rmErr)
				}
				continue
			}
		}

		if err := os.Rename(stalePath, restoredPath); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to restore orphaned dead-letter replay %s: %v\n", entry.Name(), err)
		}
	}
}

// DeadLetter durably persists a failed nudge outside the active queue and
// returns the path it was written to. It does not touch the active queue —
// callers are responsible for not requeuing an entry they dead-letter.
func DeadLetter(townRoot, session string, n QueuedNudge, source, lastError, paneCapture string, uncertain bool) (string, error) {
	dir := deadLetterDir(townRoot, session)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("creating dead-letter dir: %w", err)
	}

	// Assign an ID before marshaling, not just for the filename: a caller
	// that builds a QueuedNudge inline (e.g. the wait-idle composer-dirty
	// path in cmd.deliverNudge, which never goes through Enqueue) would
	// otherwise persist a record with an empty `id` field, making it
	// impossible to name via `gt nudge dead-letter replay`.
	if n.ID == "" {
		n.ID = randomSuffix() + randomSuffix()
	}
	n.LastError = lastError
	entry := DeadLetterEntry{
		QueuedNudge:       n,
		Source:            source,
		PaneCapture:       paneCapture,
		UncertainDelivery: uncertain,
		DeadLetteredAt:    time.Now(),
	}

	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshaling dead-letter entry: %w", err)
	}

	filename := fmt.Sprintf("%d-%s.json", time.Now().UnixNano(), entry.ID)
	path := filepath.Join(dir, filename)

	// Land the entry with one atomic rename rather than writing directly to
	// its final name: a partial write (e.g. a full disk) can otherwise leave
	// a truncated, malformed file sitting AT the final dead-letter path,
	// where ListDeadLetters silently skips it as unparseable — a
	// dead-letter record that looks like it never existed. Writing to a
	// fresh, uniquely-named temp file first means a partial write only ever
	// leaves harmless scratch bytes under a name nothing reads; the final
	// path is either fully present or entirely absent, never corrupted.
	tmp := path + ".incoming-" + randomSuffix()
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("writing dead-letter entry: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("landing dead-letter entry: %w", err)
	}

	return path, nil
}

// ListDeadLetters returns all dead-lettered entries for a session, oldest
// first. Malformed entries are skipped rather than failing the whole list.
func ListDeadLetters(townRoot, session string) ([]DeadLetterEntry, error) {
	dir := deadLetterDir(townRoot, session)

	sweepStaleDeadLetterReplays(townRoot, session, dir)

	files, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading dead-letter dir: %w", err)
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })

	var entries []DeadLetterEntry
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, f.Name()))
		if err != nil {
			continue
		}
		var e DeadLetterEntry
		if err := json.Unmarshal(data, &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// ReplayDeadLetter finds a dead-lettered entry by ID, re-enqueues it as a
// fresh nudge (new expiry, attempts reset to zero, original ID preserved so
// it can still be traced back to this dead-letter record), and removes the
// dead-letter file. Returns an error if no entry with that ID exists.
//
// Claims the entry file (atomic rename) before acting on it, so two
// concurrent replays of the same ID cannot both re-enqueue it: only one
// rename succeeds, the loser sees ENOENT/a missing match and returns "not
// found" rather than double-queuing the payload (codex, deadletter.go:160,
// changes-requested at 08964387).
func ReplayDeadLetter(townRoot, session, id string) error {
	dir := deadLetterDir(townRoot, session)

	sweepStaleDeadLetterReplays(townRoot, session, dir)

	files, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no dead-letter entries for session %q", session)
		}
		return fmt.Errorf("reading dead-letter dir: %w", err)
	}

	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, f.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var e DeadLetterEntry
		if err := json.Unmarshal(data, &e); err != nil {
			continue
		}
		if e.ID != id {
			continue
		}

		// Reset the mtime on the ORIGINAL path before the rename, not
		// after: otherwise the file carries its old dead-letter mtime for
		// the gap between becoming visible under ".replaying" and a later
		// Chtimes call, and an entry that had already sat in the
		// dead-letter store past deadLetterReplayStaleThreshold (routine —
		// operators don't always replay promptly) would read as an
		// orphaned replay the INSTANT it is claimed, letting a concurrent
		// ReplayDeadLetter/ListDeadLetters call's sweep restore it while
		// this replay is still in flight and enqueue it a second time
		// (codex, deadletter.go:211, changes-requested at REVISION 3 —
		// High 9; same fix shape as queue.go:430).
		claimTime := time.Now()
		if err := os.Chtimes(path, claimTime, claimTime); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to reset dead-letter claim mtime for %s: %v\n", f.Name(), err)
		}

		// Atomically claim this entry before acting on it. If a concurrent
		// replay already renamed it away, this fails and we fall through to
		// "not found" instead of racing to re-enqueue the same payload twice.
		claimPath := path + ".replaying"
		if err := os.Rename(path, claimPath); err != nil {
			return fmt.Errorf("dead-letter entry %q for session %q is already being replayed", id, session)
		}

		replay := e.QueuedNudge
		replay.Attempts = 0
		replay.LastError = ""
		replay.Timestamp = time.Now()
		replay.ExpiresAt = time.Time{} // Enqueue recomputes from Priority + Timestamp

		if err := Enqueue(townRoot, session, replay); err != nil {
			// Restore the claim so the entry isn't stranded under a
			// ".replaying" name with no way to list or retry it.
			_ = os.Rename(claimPath, path)
			return fmt.Errorf("re-enqueuing dead-letter entry %s: %w", id, err)
		}
		if err := os.Remove(claimPath); err != nil {
			return fmt.Errorf("removing dead-letter entry %s after replay: %w", id, err)
		}
		return nil
	}

	return fmt.Errorf("dead-letter entry %q not found for session %q", id, session)
}
