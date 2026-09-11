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

// DeadLetter durably persists a failed nudge outside the active queue and
// returns the path it was written to. It does not touch the active queue —
// callers are responsible for not requeuing an entry they dead-letter.
func DeadLetter(townRoot, session string, n QueuedNudge, source, lastError, paneCapture string, uncertain bool) (string, error) {
	dir := deadLetterDir(townRoot, session)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("creating dead-letter dir: %w", err)
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

	id := entry.ID
	if id == "" {
		id = randomSuffix() + randomSuffix()
	}
	filename := fmt.Sprintf("%d-%s.json", time.Now().UnixNano(), id)
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", fmt.Errorf("writing dead-letter entry: %w", err)
	}

	return path, nil
}

// ListDeadLetters returns all dead-lettered entries for a session, oldest
// first. Malformed entries are skipped rather than failing the whole list.
func ListDeadLetters(townRoot, session string) ([]DeadLetterEntry, error) {
	dir := deadLetterDir(townRoot, session)

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
func ReplayDeadLetter(townRoot, session, id string) error {
	dir := deadLetterDir(townRoot, session)

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

		replay := e.QueuedNudge
		replay.Attempts = 0
		replay.LastError = ""
		replay.Timestamp = time.Now()
		replay.ExpiresAt = time.Time{} // Enqueue recomputes from Priority + Timestamp

		if err := Enqueue(townRoot, session, replay); err != nil {
			return fmt.Errorf("re-enqueuing dead-letter entry %s: %w", id, err)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("removing dead-letter entry %s after replay: %w", id, err)
		}
		return nil
	}

	return fmt.Errorf("dead-letter entry %q not found for session %q", id, session)
}
