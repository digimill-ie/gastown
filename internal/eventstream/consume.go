package eventstream

// ConsumeOnce performs a single ordered pass over unacked events
// (id > AckedID) and, for each: skips it if the effect was already
// completed (dedup guard — this is the case a crash between Complete and
// Ack produces on restart), otherwise durably claims it, invokes apply,
// marks it completed, then acks it, advancing the replay position by
// exactly one event per iteration.
//
// Each event is processed under a single state-lock acquisition covering
// claim, apply, complete and ack together. This is what makes two
// concurrent "gt events tail <target>" processes on the same target safe
// rather than a double-delivery race: the second process blocks on the
// lock until the first finishes an event, then observes it as already
// completed and only catches its own ack cursor up, never re-invoking
// apply. (A version of this loop that called the Claim/Complete/Ack
// helpers as separate lock acquisitions had exactly that race — the
// check and the act were not atomic across processes.)
//
// If apply returns an error, that event's lock callback returns the same
// error, so nothing for it is persisted (not even the claim) and
// ConsumeOnce stops immediately: the event is retried in full on the
// next call, and nothing after it in the pass is touched. This is what
// "ordered replay" means here — events are never acked out of order, so
// a stuck event cannot be silently skipped by a later one succeeding.
//
// delivered lists exactly the events apply was actually invoked for
// (catch-up-only acks from the dedup path are not included) — this is
// the list a caller checks in a restart-recovery test to prove an effect
// was not re-applied.
func ConsumeOnce(townRoot, agent string, apply func(Event) error) (delivered []Event, err error) {
	acked, err := AckedPosition(townRoot, agent)
	if err != nil {
		return nil, err
	}

	pending, err := ReadAfter(townRoot, agent, acked)
	if err != nil {
		return nil, err
	}

	for _, ev := range pending {
		applied, err := processEventLocked(townRoot, agent, ev, apply)
		if err != nil {
			return delivered, err
		}
		if applied {
			delivered = append(delivered, ev)
		}
	}

	return delivered, nil
}

// processEventLocked handles one event's full claim/apply/complete/ack
// lifecycle under a single state-lock acquisition. See ConsumeOnce for
// why this must be one lock cycle rather than four.
func processEventLocked(townRoot, agent string, ev Event, apply func(Event) error) (applied bool, err error) {
	lockErr := withStateLock(townRoot, agent, func(st *ConsumerState) error {
		// A racing consumer (or an earlier iteration in this same pass,
		// for a state file shared across concurrent processes) may have
		// already caught the cursor up past this id.
		if ev.ID <= st.AckedID {
			return nil
		}
		if st.CompletedIDs[ev.ID] {
			// Effect already applied by someone else; just catch up.
			st.AckedID = ev.ID
			pruneCompletedLocked(st, ev.ID)
			return nil
		}

		st.ClaimedID = ev.ID
		if applyErr := apply(ev); applyErr != nil {
			// Nothing is saved: the claim above is discarded along with
			// everything else in this callback, since withStateLock only
			// persists state when fn returns nil. The event is retried
			// from scratch on the next pass.
			return applyErr
		}
		applied = true
		if st.CompletedIDs == nil {
			st.CompletedIDs = map[int64]bool{}
		}
		st.CompletedIDs[ev.ID] = true
		st.AckedID = ev.ID
		pruneCompletedLocked(st, ev.ID)
		return nil
	})
	if lockErr != nil {
		return false, lockErr
	}
	return applied, nil
}
