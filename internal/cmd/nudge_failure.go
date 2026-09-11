package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Sources identify which caller handled a failed injection, for the
// observability log and dead-letter records.
const (
	sourceNudgePoller = "nudge-poller"
	sourceIdleWatcher = "idle-watcher"
	sourceWaitIdle    = "wait-idle"
)

// errAttemptsExhausted marks an entry whose Attempts already reached
// nudge.MaxInjectionAttempts on a prior cycle — via the dead-letter-write
// durability fallback below, which requeues rather than losing the message
// even though the bound was already hit. Callers must route such entries
// here WITHOUT attempting a live tmux injection first: retyping an
// already-exhausted entry on every poll cycle until TTL expiry is exactly
// the retry storm nudge.MaxInjectionAttempts exists to prevent (codex,
// nudge_failure.go:66/nudge_poller.go:134, changes-requested at 08964387).
var errAttemptsExhausted = errors.New("nudge injection attempts exhausted, not retyping")

// errPriorAttemptUnresolved marks an entry whose most recent injection
// attempt (Claim.MarkAttempt persisted Attempts and QueuedNudge.InFlight
// BEFORE that attempt) never reported an outcome — the poller or idle
// watcher process was killed after starting the attempt but before
// handleFailedInjection or a successful ack recorded what happened.
// Attempts alone cannot tell this apart from an ordinary, safe-to-retry
// failure: on restart the orphan sweep restores such a claim with its
// InFlight flag still true, and Attempts may be well under the bound.
// Because some or all of the message text may already have reached the
// composer, this is handled with the same zero-retype policy as an
// unverified tmux submission (codex, nudge_failure.go:152,
// changes-requested at REVISION 3 — High 2). See partitionForInjection.
var errPriorAttemptUnresolved = errors.New("nudge injection attempt from a prior cycle never resolved, not retyping")

// handleFailedInjection is the shared failure policy for every caller that
// drains the nudge queue and injects via tmux: the poller, the idle watcher,
// and the wait-idle fallback. It replaces the old requeueDrainedNudges, which
// unconditionally retyped a failed entry on every future poll — the
// self-feeding loop that produced dozens of duplicate copies of one mail in
// one turn (hq-g52db).
//
// Policy:
//   - An unverified submission (errors.Is(deliverErr, tmux.ErrSubmitNotVerified)
//     — covers both a known-dirty composer and every other "typed but could
//     not confirm it left the composer" state, e.g. an acknowledgement lost
//     after typing, or a stranded-composer recovery that itself could not be
//     confirmed) is dead-lettered immediately: retyping when we cannot prove
//     the prior attempt failed risks duplicating content, so zero retypes
//     are allowed. REVISION 3 requires this for dirty AND uncertain
//     deliveries alike, not composer-dirty alone.
//   - Any other (verified-failed) injection error is bounded: an entry is
//     requeued for one more attempt, then dead-lettered once
//     nudge.MaxInjectionAttempts is reached. Attempts persist on disk
//     (QueuedNudge.Attempts), so the bound holds across a poller restart too.
//
// n.Attempts is NOT incremented here — this function trusts the Attempts
// value already on each entry. A real injection attempt persists its own
// increment BEFORE the attempt via Claim.MarkAttempt (see nudge_poller.go
// and watchAndDeliver), so a crash mid-injection still leaves the correct
// count behind; an errAttemptsExhausted entry never attempted injection
// this cycle at all, so incrementing here would count an attempt that
// never happened (codex, nudge_failure.go:80, changes-requested at
// 08964387/95f841e6 rework).
//
// UncertainDelivery is always recorded as true in the dead-letter entry: no
// failure path here can PROVE the message did not reach the agent (a dirty
// snapshot proves nothing about what happened before the other content
// appeared, and a generic injection error is no more conclusive) — recording
// false as "certain non-delivery" would be an unproven claim.
//
// Every failure is first written to the durable per-session log
// (nudge.LogInjectionError) so the actual error is never silently discarded,
// unlike before this existed (poller.go redirects stdout/stderr to nil).
//
// Claims (not bare QueuedNudge values) are the caller's responsibility: this
// function only decides and performs the dead-letter/requeue writes, and
// returns the entries whose outcome did NOT durably land anywhere: a
// bounded entry whose dead-letter write failed AND whose requeue-fallback
// write also failed, OR an unverified entry whose dead-letter write failed
// (the double fault — R2, hq-g52db REVISION 3: retained, never dropped).
// Callers draining via nudge.DrainClaims must Ack every OTHER original
// claim (see nudge.AckClaims) only AFTER this function returns — an entry
// in the returned slice must be left un-acked, so a crash-equivalent write
// failure leaves the original claim recoverable via the orphan sweep
// instead of lost the moment the caller acks it anyway (codex,
// nudge_poller.go:157, changes-requested at 08964387/95f841e6 rework).
func handleFailedInjection(t *tmux.Tmux, townRoot, sessionName, source string, drained []nudge.QueuedNudge, deliverErr error) (unresolved []nudge.QueuedNudge) {
	paneCapture, _ := t.CapturePane(t.ResolveAgentTarget(sessionName), 25)

	if err := nudge.LogInjectionError(townRoot, sessionName, source, deliverErr, paneCapture); err != nil {
		fmt.Fprintf(os.Stderr, "%s: injection-error log for %s failed: %v\n", source, sessionName, err)
	}

	// "Zero retypes allowed" applies to both an unverified tmux submission
	// AND an entry whose prior attempt never reported an outcome at all
	// (errPriorAttemptUnresolved) — the two are indistinguishable in what
	// they risk (unproven partial delivery), so they share the same
	// dead-letter-immediately, retain-on-double-fault policy below.
	unverified := errors.Is(deliverErr, tmux.ErrSubmitNotVerified) || errors.Is(deliverErr, errPriorAttemptUnresolved)

	// errAttemptsExhausted and errPriorAttemptUnresolved are ROUTING
	// sentinels, not real injection errors — partitionForInjection uses
	// them to say "route this entry to dead-letter without attempting a
	// live injection this cycle" (no deliverErr from an actual attempt
	// exists for these entries THIS cycle). Recording the sentinel's
	// generic text as LastError would destroy the real diagnostic error
	// already on the entry from its last actual attempt (persisted across
	// Requeue) — exactly what an operator inspecting a dead-letter entry
	// needs (codex, nudge_failure.go:95, changes-requested at REVISION 3
	// — Medium).
	isRoutingSentinel := errors.Is(deliverErr, errAttemptsExhausted) || errors.Is(deliverErr, errPriorAttemptUnresolved)

	var toRequeue []nudge.QueuedNudge
	for _, n := range drained {
		lastError := deliverErr.Error()
		if isRoutingSentinel && n.LastError != "" {
			lastError = n.LastError
		}
		n.LastError = lastError

		if unverified || n.Attempts >= nudge.MaxInjectionAttempts {
			dlPath, dlErr := nudge.DeadLetter(townRoot, sessionName, n, source, lastError, paneCapture, true)
			if dlErr != nil {
				if !unverified {
					// Durability fallback for a bounded (verified-failed)
					// entry: don't lose the message because the dead-letter
					// write itself failed — requeue instead. Safe because a
					// verified-failed delivery carries no duplication risk;
					// the caller's pre-injection attempts check (see
					// errAttemptsExhausted) keeps this from retyping forever.
					fmt.Fprintf(os.Stderr, "%s: dead-letter for %s failed, requeuing instead: %v\n", source, sessionName, dlErr)
					toRequeue = append(toRequeue, n)
					continue
				}
				// An unverified entry must NOT requeue even here: requeuing
				// would resume the exact retype-into-uncertain-composer loop
				// this fix exists to stop the moment the entry is drained
				// again. This double fault (unverified submission AND
				// dead-letter unwritable, e.g. a full disk) is RETAINED,
				// never dropped (R2, hq-g52db REVISION 3 mayor ruling — the
				// prior version of this comment called the drop
				// "deliberate" and acked the original claim anyway, which
				// is exactly the loss R2 prohibits). The entry is added to
				// unresolved so the caller leaves its claim un-acked: it
				// stays on disk as a live claim, and a future orphan sweep
				// restores it once staleClaimThreshold passes. The
				// already-persisted Attempts count (Claim.MarkAttempt,
				// before this cycle's injection) is what bounds how many
				// more live retypes it can still get — this path itself
				// retypes nothing, it only keeps retrying the durability
				// write. A nudge-free alert goes to the rig witness and the
				// mayor so a human notices a dead-letter store that can't
				// be written to.
				fmt.Fprintf(os.Stderr, "%s: CRITICAL: dead-letter for unverified entry %s on %s failed, retaining claim (will NOT be dropped): %v\n", source, n.ID, sessionName, dlErr)
				alertDoubleFault(townRoot, sessionName, source, n, dlErr)
				unresolved = append(unresolved, n)
				continue
			}
			fmt.Fprintf(os.Stderr, "%s: dead-lettered entry %s for %s (%s)\n", source, n.ID, sessionName, dlPath)
			alertDeadLetter(townRoot, sessionName, source, n)
			continue
		}

		toRequeue = append(toRequeue, n)
	}

	// Merge into (not replace) unresolved: a batch can contain BOTH an
	// unverified double-fault entry (already appended to unresolved above)
	// AND a separately-bounded entry that only failed its ordinary
	// requeue below — returning `failed` alone, or a bare `nil` when
	// toRequeue was empty, silently dropped the double-fault entries from
	// the caller's view, undoing the R2 retention fix above (found by
	// TestPipeline_UnverifiedDoubleFault_RetainsClaim).
	if len(toRequeue) == 0 {
		return unresolved
	}
	failed, err := nudge.Requeue(townRoot, sessionName, toRequeue)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: requeue for %s failed for %d/%d entries: %v\n", source, sessionName, len(failed), len(toRequeue), err)
	}
	unresolved = append(unresolved, failed...)
	return unresolved
}

// partitionForInjection splits claimed nudges into three groups: still
// eligible for a live tmux injection attempt (toInject); already exhausted
// nudge.MaxInjectionAttempts on a prior cycle, reached only via the
// dead-letter-write durability fallback in handleFailedInjection, which
// requeues past the bound rather than losing the message (exhausted); and
// entries whose most recent attempt never reported an outcome at all —
// InFlight still true, restored by the orphan sweep after a crash mid-
// attempt (staleInFlight). Both exhausted and staleInFlight entries must be
// routed to handleFailedInjection directly — never retyped — but with
// DIFFERENT sentinel errors (errAttemptsExhausted vs
// errPriorAttemptUnresolved) since they carry different risk: an exhausted
// entry's prior failures were each definitively observed, while a
// staleInFlight entry's last attempt might have partially typed (codex,
// nudge_poller.go:134 / nudge_failure.go:152, changes-requested at
// 08964387 and REVISION 3 — High 2).
func partitionForInjection(claims []nudge.Claim) (toInject, exhausted, staleInFlight []nudge.Claim) {
	for _, c := range claims {
		switch {
		case c.Nudge.Attempts >= nudge.MaxInjectionAttempts:
			exhausted = append(exhausted, c)
		case c.Nudge.InFlight:
			staleInFlight = append(staleInFlight, c)
		default:
			toInject = append(toInject, c)
		}
	}
	return toInject, exhausted, staleInFlight
}

// claimNudges extracts the QueuedNudge payloads from a slice of claims, in
// order.
func claimNudges(claims []nudge.Claim) []nudge.QueuedNudge {
	nudges := make([]nudge.QueuedNudge, len(claims))
	for i, c := range claims {
		nudges[i] = c.Nudge
	}
	return nudges
}

// ackClaims resolves every claim in claims whose outcome is durable —
// successful delivery, or a durable dead-letter/requeue write via
// handleFailedInjection — by removing its underlying file. unresolved (as
// returned by handleFailedInjection) names the entries that are NOT yet
// durable anywhere; their claims are deliberately left un-acked so a future
// orphan sweep can recover them instead of losing them the moment this
// function acks the original claim anyway.
func ackClaims(source, sessionName string, claims []nudge.Claim, unresolved []nudge.QueuedNudge) {
	nudge.AckClaims(claims, unresolved, func(c nudge.Claim, err error) {
		fmt.Fprintf(os.Stderr, "%s: failed to ack nudge claim for %s: %v\n", source, sessionName, err)
	})
}

// markAttempts persists an incremented Attempts count (see nudge.Claim.
// MarkAttempt) on every claim in claims BEFORE the caller attempts a live
// batch injection, and returns two slices: marked (mutated in place, so the
// caller's subsequent claimNudges(...) extraction carries the already-
// persisted Attempts/InFlight values) and failed (claims whose persist
// write itself failed).
//
// A claim in failed must NOT be injected this cycle: attempting a live
// delivery with no durable record that the attempt happened at all means a
// crash mid-injection leaves a claim reading Attempts=0/InFlight=false —
// indistinguishable from a never-attempted entry, which defeats the whole
// point of persisting the increment before the attempt. The caller should
// leave failed's claims un-acked (not dead-lettered or requeued either —
// there is no delivery outcome to report here, only a filesystem write
// failure) so a future orphan sweep retries them once the underlying
// problem clears (codex, nudge_failure.go:193, changes-requested at
// REVISION 3 — High 2, "ignores persist errors").
func markAttempts(source, sessionName string, claims []nudge.Claim) (marked, failed []nudge.Claim) {
	for i := range claims {
		if err := claims[i].MarkAttempt(); err != nil {
			fmt.Fprintf(os.Stderr, "%s: failed to persist attempt state for %s, skipping injection this cycle for entry %s: %v\n", source, sessionName, claims[i].Nudge.ID, err)
			failed = append(failed, claims[i])
			continue
		}
		marked = append(marked, claims[i])
	}
	return marked, failed
}

// alertDeadLetter sends a nudge-free (--no-notify equivalent) mail to the
// rig witness and the mayor when an entry is dead-lettered. SuppressNotify
// skips the router's proactive tmux/queue nudge to the recipient (router.go
// notifyRecipient) — that is the point: the bead asks for a "nudge-free
// alert", precisely so a dead-lettered entry does not itself become another
// injection into a possibly-still-busy session. The alert is durable and
// shows up in the recipient's ordinary `gt mail inbox`; it is not a proactive
// interruption, and does not make the dead-letter store itself unnecessary to
// check. Errors are logged, not returned — a failed alert must not block
// dead-lettering (which has already durably persisted the entry).
//
// Uses the given townRoot directly rather than findMailWorkDir's cwd/env
// detection: mail.NewRouter falls back to GT_TOWN_ROOT/GT_ROOT when its
// workDir isn't a real workspace, which would silently redirect an alert (or
// a test run against a scratch townRoot) into whatever town those env vars
// happen to point at. Requiring townRoot to already be a real workspace
// avoids that and is a no-op everywhere it would otherwise misfire.
func alertDeadLetter(townRoot, sessionName, source string, entry nudge.QueuedNudge) {
	subject := fmt.Sprintf("Nudge dead-lettered: %s", sessionName)
	body := fmt.Sprintf(`Session: %s
Source: %s
Sender: %s
Entry ID: %s
Attempts: %d
Last error: %s

Inspect: gt nudge dead-letter list %s
Replay:  gt nudge dead-letter replay %s %s`,
		sessionName, source, entry.Sender, entry.ID, entry.Attempts, entry.LastError,
		sessionName, sessionName, entry.ID)

	sendNudgeAlert(townRoot, sessionName, source, subject, body)
}

// alertDoubleFault sends a nudge-free alert when an unverified entry's
// dead-letter write itself fails (the double fault handleFailedInjection
// retains rather than drops — R2, hq-g52db REVISION 3). Distinct from
// alertDeadLetter: the entry named here was NOT durably dead-lettered, it
// is still a live, un-acked claim, and dlErr is the write failure that
// prevented dead-lettering it.
func alertDoubleFault(townRoot, sessionName, source string, entry nudge.QueuedNudge, dlErr error) {
	subject := fmt.Sprintf("Nudge dead-letter write failed (claim retained): %s", sessionName)
	body := fmt.Sprintf(`Session: %s
Source: %s
Sender: %s
Entry ID: %s
Attempts: %d
Delivery error: %s
Dead-letter write error: %s

This entry could not be dead-lettered and was NOT dropped: its claim is
retained un-acked and will be restored to the active queue by a future
orphan sweep. If the dead-letter store's underlying problem (e.g. disk
full) persists, this entry will keep failing to dead-letter on every
retry.`,
		sessionName, source, entry.Sender, entry.ID, entry.Attempts, entry.LastError, dlErr)

	sendNudgeAlert(townRoot, sessionName, source, subject, body)
}

// sendNudgeAlert sends a nudge-free (--no-notify equivalent) mail to the
// rig witness and the mayor. SuppressNotify skips the router's proactive
// tmux/queue nudge to the recipient (router.go notifyRecipient) — that is
// the point: an alert about nudge delivery trouble must not itself become
// another injection into a possibly-still-busy or still-broken session. The
// alert is durable and shows up in the recipient's ordinary `gt mail
// inbox`; it is not a proactive interruption. Errors are logged, not
// returned — a failed alert must not block the caller's own durable write,
// which has already happened by the time this is called.
//
// Uses the given townRoot directly rather than findMailWorkDir's cwd/env
// detection: mail.NewRouter falls back to GT_TOWN_ROOT/GT_ROOT when its
// workDir isn't a real workspace, which would silently redirect an alert (or
// a test run against a scratch townRoot) into whatever town those env vars
// happen to point at. Requiring townRoot to already be a real workspace
// avoids that and is a no-op everywhere it would otherwise misfire.
func sendNudgeAlert(townRoot, sessionName, source, subject, body string) {
	if ok, _ := workspace.IsWorkspace(townRoot); !ok {
		fmt.Fprintf(os.Stderr, "%s: alert for %s: %q is not a Gas Town workspace, skipping\n", source, sessionName, townRoot)
		return
	}

	rig := ""
	if identity, err := session.ParseSessionName(sessionName); err == nil {
		rig = identity.Rig
	}

	targets := []string{"mayor"}
	if rig != "" {
		targets = append(targets, rig+"/witness")
	}

	router := mail.NewRouter(townRoot)
	defer router.WaitPendingNotifications()
	for _, to := range targets {
		msg := mail.NewMessage("nudge-poller", to, subject, body)
		msg.SuppressNotify = true
		if err := router.Send(msg); err != nil {
			fmt.Fprintf(os.Stderr, "%s: alert to %s failed: %v\n", source, to, err)
		}
	}
}
