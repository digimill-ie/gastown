// deadletter.go provides durable storage for nudges that failed delivery.
//
// A dead-lettered entry is never retried automatically: it sits outside the
// active queue until an operator inspects it (`gt nudge dead-letter list
// <session>`) and explicitly replays it (`gt nudge dead-letter replay
// <session> <id>`).
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
	// ID identifies this entry for list/replay. Assigned once, at
	// dead-letter time.
	ID string `json:"id"`
	// Session is the target session the delivery was attempted against.
	Session string `json:"session"`
	// Source identifies which caller dead-lettered the entry (e.g.
	// "nudge-poller").
	Source string `json:"source"`
	// Error is the injection failure that caused this entry to be
	// dead-lettered instead of retried.
	Error string `json:"error"`
	// DeadLetteredAt is when this entry was written.
	DeadLetteredAt time.Time `json:"dead_lettered_at"`
}

// deadLetterDir returns the dead-letter directory for a given session.
// Path: <townRoot>/.runtime/nudge_deadletter/<session>/
func deadLetterDir(townRoot, session string) string {
	safe := strings.ReplaceAll(session, "/", "_")
	return filepath.Join(townRoot, constants.DirRuntime, "nudge_deadletter", safe)
}

// DeadLetter durably persists a failed nudge outside the active queue, as a
// single new file (O_EXCL — never overwrites an existing entry), and
// returns the id it can be listed and replayed by.
//
// This is the ONLY thing that happens to a nudge whose injection fails: no
// requeue, no retry. A crash between the failed injection and this write
// still loses the message, exactly as at base — see the poller's injection
// error path.
func DeadLetter(townRoot, session string, n QueuedNudge, source, errMsg string) (id string, err error) {
	dir := deadLetterDir(townRoot, session)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("creating dead-letter dir: %w", err)
	}

	id = fmt.Sprintf("%d-%s", time.Now().UnixNano(), randomSuffix())
	entry := DeadLetterEntry{
		QueuedNudge:    n,
		ID:             id,
		Session:        session,
		Source:         source,
		Error:          errMsg,
		DeadLetteredAt: time.Now(),
	}

	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshaling dead-letter entry: %w", err)
	}

	path := filepath.Join(dir, id+".json")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return "", fmt.Errorf("creating dead-letter entry: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(data); err != nil {
		return "", fmt.Errorf("writing dead-letter entry: %w", err)
	}
	return id, nil
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
// fresh nudge, and removes the dead-letter file. Returns an error if no
// entry with that ID exists.
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
