package eventstream

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// ConsumerState is the durable per-agent consumer cursor. It is the
// mechanism, not a description of one: every field here is written by
// Claim/Ack/Complete before the caller acts on it, so a crash between two
// writes leaves a state a fresh process can read and continue from
// correctly (see ConsumeOnce).
type ConsumerState struct {
	// ClaimedID is the id of the event currently (or most recently) being
	// processed. It is a durable claim: set before the effect is
	// attempted, so recovery knows what was in flight.
	ClaimedID int64 `json:"claimed_id"`

	// AckedID is the ordered replay position: the highest event id whose
	// effect is known-complete. ReadAfter(AckedID) is always safe to
	// replay from — nothing at or below AckedID needs redelivery.
	AckedID int64 `json:"acked_id"`

	// CompletedIDs is the effect-dedup set: ids whose effect has been
	// durably applied at least once. Checked before ever re-applying an
	// effect, independent of whether AckedID has caught up yet — this is
	// what makes replay safe even when a crash lands between Complete and
	// Ack (see ConsumeOnce).
	CompletedIDs map[int64]bool `json:"completed_ids,omitempty"`

	// HeartbeatAt is touched by an active consumer on every poll cycle,
	// whether or not there were new events. It is the only field in this
	// struct that observes liveness rather than progress — a consumer
	// that is alive but has nothing to do still advances it, while a dead
	// consumer leaves it stale. This is what makes monitor failure
	// independently observable: the reading is about the WATCHER, not
	// about whether events are flowing.
	HeartbeatAt time.Time `json:"heartbeat_at,omitempty"`
}

func statePath(townRoot, agent string) string {
	return filepath.Join(dir(townRoot, agent), "state.json")
}

func stateLockPath(townRoot, agent string) string {
	return filepath.Join(dir(townRoot, agent), "state.lock")
}

// loadStateLocked reads the current state. Caller must hold the state lock.
// A missing file is a fresh consumer (all zero values), not an error.
func loadStateLocked(townRoot, agent string) (ConsumerState, error) {
	data, err := os.ReadFile(statePath(townRoot, agent))
	if err != nil {
		if os.IsNotExist(err) {
			return ConsumerState{CompletedIDs: map[int64]bool{}}, nil
		}
		return ConsumerState{}, fmt.Errorf("reading consumer state: %w", err)
	}
	var st ConsumerState
	if err := json.Unmarshal(data, &st); err != nil {
		return ConsumerState{}, fmt.Errorf("parsing consumer state: %w", err)
	}
	if st.CompletedIDs == nil {
		st.CompletedIDs = map[int64]bool{}
	}
	return st, nil
}

// saveStateLocked writes state atomically (temp file + rename) so a crash
// mid-write never leaves a torn/partial state file behind. Caller must
// hold the state lock.
func saveStateLocked(townRoot, agent string, st ConsumerState) error {
	d := dir(townRoot, agent)
	if err := os.MkdirAll(d, 0755); err != nil {
		return fmt.Errorf("creating event stream dir: %w", err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling consumer state: %w", err)
	}
	tmp, err := os.CreateTemp(d, "state-*.json.tmp")
	if err != nil {
		return fmt.Errorf("creating temp state file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("writing temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("closing temp state file: %w", err)
	}
	if err := os.Rename(tmpPath, statePath(townRoot, agent)); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("renaming temp state file: %w", err)
	}
	return nil
}

// withStateLock runs fn with the per-agent state lock held, so
// Claim/Ack/Complete never race each other even under concurrent callers.
func withStateLock(townRoot, agent string, fn func(*ConsumerState) error) error {
	d := dir(townRoot, agent)
	if err := os.MkdirAll(d, 0755); err != nil {
		return fmt.Errorf("creating event stream dir: %w", err)
	}
	fl := flock.New(stateLockPath(townRoot, agent))
	if err := fl.Lock(); err != nil {
		return fmt.Errorf("locking consumer state: %w", err)
	}
	defer fl.Unlock() //nolint:errcheck // best-effort unlock

	st, err := loadStateLocked(townRoot, agent)
	if err != nil {
		return err
	}
	if err := fn(&st); err != nil {
		return err
	}
	return saveStateLocked(townRoot, agent, st)
}

// LoadState returns a snapshot of the current consumer state.
func LoadState(townRoot, agent string) (ConsumerState, error) {
	var out ConsumerState
	err := withStateLock(townRoot, agent, func(st *ConsumerState) error {
		out = *st
		return nil
	})
	return out, err
}

// Claim durably records that id is now being processed. It does not
// advance AckedID — a claim without a following Complete/Ack is exactly
// the "in flight when the consumer died" case that ConsumeOnce recovers
// from on the next pass, by re-attempting the same id.
func Claim(townRoot, agent string, id int64) error {
	return withStateLock(townRoot, agent, func(st *ConsumerState) error {
		st.ClaimedID = id
		return nil
	})
}

// Complete idempotently records that id's effect has been durably
// applied. Calling it twice for the same id is a no-op the second time —
// this is the dedup guard: ConsumeOnce checks it before ever invoking the
// effect function again for an id.
func Complete(townRoot, agent string, id int64) error {
	return withStateLock(townRoot, agent, func(st *ConsumerState) error {
		st.CompletedIDs[id] = true
		return nil
	})
}

// IsCompleted reports whether id's effect has already been durably applied.
func IsCompleted(townRoot, agent string, id int64) (bool, error) {
	st, err := LoadState(townRoot, agent)
	if err != nil {
		return false, err
	}
	return st.CompletedIDs[id], nil
}

// Ack advances the replay position to id. It refuses to move the cursor
// backwards (a stale caller acking an already-passed id is a no-op, not
// an error) and refuses to skip ahead of what has actually been
// completed, so AckedID can never outrun CompletedIDs — the invariant
// ConsumeOnce's replay-from-AckedID safety depends on.
func Ack(townRoot, agent string, id int64) error {
	return withStateLock(townRoot, agent, func(st *ConsumerState) error {
		if id <= st.AckedID {
			return nil
		}
		if !st.CompletedIDs[id] {
			return fmt.Errorf("cannot ack event %d: not marked completed", id)
		}
		st.AckedID = id
		// Prune completed ids at or below the new ack position — they are
		// now implied by AckedID and would otherwise grow unbounded.
		for k := range st.CompletedIDs {
			if k <= id {
				delete(st.CompletedIDs, k)
			}
		}
		return nil
	})
}

// AckedPosition returns the current replay position.
func AckedPosition(townRoot, agent string) (int64, error) {
	st, err := LoadState(townRoot, agent)
	if err != nil {
		return 0, err
	}
	return st.AckedID, nil
}

// Heartbeat records that an active consumer polled just now. Called once
// per poll cycle regardless of whether there were events to deliver.
func Heartbeat(townRoot, agent string) error {
	return withStateLock(townRoot, agent, func(st *ConsumerState) error {
		st.HeartbeatAt = time.Now().UTC()
		return nil
	})
}

// IsArmed reports whether the consumer's heartbeat is fresher than
// maxAge. This is the primitive a witness arm-check reads: a stopped or
// crashed `gt events tail` process stops touching the heartbeat, and this
// goes false on its own — it does not depend on the watcher reporting its
// own death.
func IsArmed(townRoot, agent string, maxAge time.Duration) (armed bool, age time.Duration, err error) {
	st, err := LoadState(townRoot, agent)
	if err != nil {
		return false, 0, err
	}
	if st.HeartbeatAt.IsZero() {
		return false, 0, nil
	}
	age = time.Since(st.HeartbeatAt)
	return age <= maxAge, age, nil
}
