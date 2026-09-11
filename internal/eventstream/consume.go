package eventstream

// ConsumeOnce performs a single ordered pass over unacked events
// (id > AckedID) and, for each: skips it if the effect was already
// completed (dedup guard — this is the case a crash between Complete and
// Ack produces on restart), otherwise durably claims it, invokes apply,
// marks it completed, then acks it, advancing the replay position by
// exactly one event per iteration.
//
// If apply returns an error, ConsumeOnce stops and returns immediately:
// the failed event stays claimed-but-not-completed-or-acked, so the next
// ConsumeOnce call retries it before considering anything after it. This
// is what "ordered replay" means here — events are never acked out of
// order, so a stuck event cannot be silently skipped by a later one
// succeeding.
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
		completed, err := IsCompleted(townRoot, agent, ev.ID)
		if err != nil {
			return delivered, err
		}
		if completed {
			// Effect already applied in a prior run that crashed before
			// acking. Catch the cursor up without re-invoking apply.
			if err := Ack(townRoot, agent, ev.ID); err != nil {
				return delivered, err
			}
			continue
		}

		if err := Claim(townRoot, agent, ev.ID); err != nil {
			return delivered, err
		}
		if err := apply(ev); err != nil {
			// Leave claimed, uncompleted, unacked — retried on the next pass.
			return delivered, err
		}
		if err := Complete(townRoot, agent, ev.ID); err != nil {
			return delivered, err
		}
		if err := Ack(townRoot, agent, ev.ID); err != nil {
			return delivered, err
		}
		delivered = append(delivered, ev)
	}

	return delivered, nil
}
