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

// handleFailedInjection is the shared failure policy for every caller that
// drains the nudge queue and injects via tmux: the poller, the idle watcher,
// and the wait-idle fallback. It replaces the old requeueDrainedNudges, which
// unconditionally retyped a failed entry on every future poll — the
// self-feeding loop that produced dozens of duplicate copies of one mail in
// one turn (hq-g52db).
//
// Policy:
//   - A composer-dirty failure (tmux.ErrComposerDirty) is dead-lettered
//     immediately: retyping into a known-dirty composer duplicates content,
//     it never fixes anything, so zero retypes are allowed.
//   - Any other injection error is bounded: an entry is requeued for one more
//     attempt, then dead-lettered once nudge.MaxInjectionAttempts is reached.
//     Attempts persist on disk (QueuedNudge.Attempts), so the bound holds
//     across a poller restart too.
//
// Every failure is first written to the durable per-session log
// (nudge.LogInjectionError) so the actual error is never silently discarded,
// unlike before this existed (poller.go redirects stdout/stderr to nil).
func handleFailedInjection(t *tmux.Tmux, townRoot, sessionName, source string, drained []nudge.QueuedNudge, deliverErr error) {
	paneCapture, _ := t.CapturePane(sessionName, 25)

	if err := nudge.LogInjectionError(townRoot, sessionName, source, deliverErr, paneCapture); err != nil {
		fmt.Fprintf(os.Stderr, "%s: injection-error log for %s failed: %v\n", source, sessionName, err)
	}

	dirty := errors.Is(deliverErr, tmux.ErrComposerDirty)

	var toRequeue []nudge.QueuedNudge
	for _, n := range drained {
		n.Attempts++
		n.LastError = deliverErr.Error()

		if dirty || n.Attempts >= nudge.MaxInjectionAttempts {
			dlPath, dlErr := nudge.DeadLetter(townRoot, sessionName, n, source, deliverErr.Error(), paneCapture, !dirty)
			if dlErr != nil {
				if !dirty {
					// Durability fallback for a bounded (non-dirty) failure:
					// don't lose the message because the dead-letter write
					// itself failed — requeue instead. Safe because the next
					// cycle retries an ordinary injection, not a retype into
					// a known-dirty composer.
					fmt.Fprintf(os.Stderr, "%s: dead-letter for %s failed, requeuing instead: %v\n", source, sessionName, dlErr)
					toRequeue = append(toRequeue, n)
					continue
				}
				// A dirty entry must NOT requeue even here: requeuing would
				// resume the exact retype-into-dirty-composer loop this fix
				// exists to stop the moment the entry is drained again. This
				// double fault (composer dirty AND dead-letter unwritable,
				// e.g. a full disk) drops the message — logged loudly — in
				// preference to reproducing the duplicate-spam bug.
				fmt.Fprintf(os.Stderr, "%s: CRITICAL: dead-letter for known-dirty entry %s on %s failed and it will NOT be requeued (would resume the retype loop): %v\n", source, n.ID, sessionName, dlErr)
				continue
			}
			fmt.Fprintf(os.Stderr, "%s: dead-lettered entry %s for %s (%s)\n", source, n.ID, sessionName, dlPath)
			alertDeadLetter(townRoot, sessionName, source, n)
			continue
		}

		toRequeue = append(toRequeue, n)
	}

	if len(toRequeue) == 0 {
		return
	}
	if err := nudge.Requeue(townRoot, sessionName, toRequeue); err != nil {
		fmt.Fprintf(os.Stderr, "%s: requeue for %s failed: %v\n", source, sessionName, err)
	}
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
	if ok, _ := workspace.IsWorkspace(townRoot); !ok {
		fmt.Fprintf(os.Stderr, "%s: dead-letter alert for %s: %q is not a Gas Town workspace, skipping\n", source, sessionName, townRoot)
		return
	}

	rig := ""
	if identity, err := session.ParseSessionName(sessionName); err == nil {
		rig = identity.Rig
	}

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
			fmt.Fprintf(os.Stderr, "%s: dead-letter alert to %s failed: %v\n", source, to, err)
		}
	}
}
