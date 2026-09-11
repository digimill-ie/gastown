// Package eventstream provides a durable, per-agent event stream for
// reactive delivery via the Monitor tool.
//
// Unlike internal/nudge (which deletes-before-delivery, has no stable
// event ids, and offers no replay) and internal/events (a town-wide audit
// log with no per-consumer ack), this stream is append-only, assigns each
// event a stable monotonically increasing id, and tracks an explicit
// consumer lifecycle per agent: claim -> apply effect -> complete (dedup
// key) -> ack (advances the replay position). A crashed consumer resumes
// from AckedID+1 and never re-applies an effect it already completed
// (see state.go).
//
// Scope for the smallest slice (gtn-2ml / hq-6u258): one event type
// (queued_nudge), one consumer (an opted-in Claude agent, e.g. the
// gastown witness), delivered via `gt events tail` under the Monitor
// tool. Nothing here changes any default delivery path — it is an
// additive, opt-in channel.
package eventstream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/constants"
)

// TypeQueuedNudge is the one event type carried by the smallest slice.
const TypeQueuedNudge = "queued_nudge"

// Event is one durable, immutable record in an agent's stream.
// IDs are assigned sequentially per agent, starting at 1, and never reused.
type Event struct {
	ID        int64                  `json:"id"`
	Timestamp time.Time              `json:"ts"`
	Type      string                 `json:"type"`
	Agent     string                 `json:"agent"`
	Payload   map[string]interface{} `json:"payload,omitempty"`
}

// dir returns the per-agent stream directory, keyed by the same session
// identifier internal/nudge uses (e.g. "gt-gastown-witness"), so callers
// that already resolve a Gas Town session name need no new mapping.
func dir(townRoot, agent string) string {
	safe := strings.ReplaceAll(agent, "/", "_")
	return filepath.Join(townRoot, constants.DirRuntime, "event_stream", safe)
}

func streamPath(townRoot, agent string) string {
	return filepath.Join(dir(townRoot, agent), "events.jsonl")
}

func seqLockPath(townRoot, agent string) string {
	return filepath.Join(dir(townRoot, agent), "seq.lock")
}

// Append durably adds an event to the agent's stream and returns it with
// its assigned id. The id is assigned under an flock so concurrent
// appenders (e.g. multiple senders) never collide or reorder.
func Append(townRoot, agent, eventType string, payload map[string]interface{}) (Event, error) {
	d := dir(townRoot, agent)
	if err := os.MkdirAll(d, 0755); err != nil {
		return Event{}, fmt.Errorf("creating event stream dir: %w", err)
	}

	fl := flock.New(seqLockPath(townRoot, agent))
	if err := fl.Lock(); err != nil {
		return Event{}, fmt.Errorf("locking event stream: %w", err)
	}
	defer fl.Unlock() //nolint:errcheck // best-effort unlock

	last, err := lastIDLocked(townRoot, agent)
	if err != nil {
		return Event{}, err
	}

	ev := Event{
		ID:        last + 1,
		Timestamp: time.Now().UTC(),
		Type:      eventType,
		Agent:     agent,
		Payload:   payload,
	}

	data, err := json.Marshal(ev)
	if err != nil {
		return Event{}, fmt.Errorf("marshaling event: %w", err)
	}
	data = append(data, '\n')

	f, err := os.OpenFile(streamPath(townRoot, agent), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return Event{}, fmt.Errorf("opening event stream: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return Event{}, fmt.Errorf("writing event: %w", err)
	}
	if err := f.Close(); err != nil {
		return Event{}, fmt.Errorf("closing event stream: %w", err)
	}

	return ev, nil
}

// lastIDLocked reads the last event id in the stream. Caller must hold the
// seq lock. Scans the file rather than trusting a separate counter file,
// so a torn write to a counter can never desynchronize ids from content.
func lastIDLocked(townRoot, agent string) (int64, error) {
	f, err := os.Open(streamPath(townRoot, agent))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading event stream: %w", err)
	}
	defer f.Close()

	var last int64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			// A partially-written trailing line from a crashed writer is
			// possible; ignore it rather than fail the whole read. Append
			// always writes a complete line in one Write call, so a
			// truncated line can only be the last one.
			continue
		}
		if ev.ID > last {
			last = ev.ID
		}
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("scanning event stream: %w", err)
	}
	return last, nil
}

// ReadAll returns every durable event for the agent, in id order.
func ReadAll(townRoot, agent string) ([]Event, error) {
	f, err := os.Open(streamPath(townRoot, agent))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading event stream: %w", err)
	}
	defer f.Close()

	var events []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scanning event stream: %w", err)
	}
	return events, nil
}

// ReadAfter returns durable events with id > afterID, in ascending id
// order — the ordered-replay-from-a-position primitive.
func ReadAfter(townRoot, agent string, afterID int64) ([]Event, error) {
	all, err := ReadAll(townRoot, agent)
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, ev := range all {
		if ev.ID > afterID {
			out = append(out, ev)
		}
	}
	return out, nil
}

// FormatNotification renders an event as a single line for the Monitor
// tool to surface as a notification. Kept deliberately plain (no
// <system-reminder> wrapper) — Monitor lines already arrive as
// notifications per the harness contract; wrapping them again would be
// the "printing the invocation proves nothing" trap the design warns
// against dressing up as delivery.
func FormatNotification(ev Event) string {
	sender, _ := ev.Payload["sender"].(string)
	message, _ := ev.Payload["message"].(string)
	if ev.Type == TypeQueuedNudge && (sender != "" || message != "") {
		return fmt.Sprintf("[event %d] queued nudge from %s: %s", ev.ID, sender, message)
	}
	return fmt.Sprintf("[event %d] %s", ev.ID, ev.Type)
}

// ParseAgentArg normalizes a CLI-provided id string into an int64,
// returning a descriptive error for the (never expected in normal use)
// malformed case rather than a raw strconv error.
func ParseAgentArg(s string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid event id %q: %w", s, err)
	}
	return id, nil
}
