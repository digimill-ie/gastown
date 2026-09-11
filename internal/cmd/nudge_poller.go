package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	nudgePollerIntervalFlag string
	nudgePollerIdleFlag     string
)

func init() {
	rootCmd.AddCommand(nudgePollerCmd)
	nudgePollerCmd.Flags().StringVar(&nudgePollerIntervalFlag, "interval", nudge.DefaultPollInterval, "Poll interval (e.g., 10s, 30s)")
	nudgePollerCmd.Flags().StringVar(&nudgePollerIdleFlag, "idle-timeout", nudge.DefaultIdleTimeout, "How long to wait for agent idle before skipping")
}

var nudgePollerCmd = &cobra.Command{
	Use:    "nudge-poller <session>",
	Short:  "Background nudge queue poller for non-Claude agents",
	Hidden: true, // Internal command — launched by crew manager, not by users.
	Long: `Polls the nudge queue for a tmux session and drains it when the agent
is idle. This is the background equivalent of Claude's UserPromptSubmit hook
drain — it ensures queued nudges are delivered to agents that lack
turn-boundary hooks (Gemini, Codex, Cursor, etc.).

This command runs as a long-lived background process. It exits when:
  - The target tmux session dies
  - It receives SIGTERM (from StopPoller or session teardown)
  - The poll loop encounters an unrecoverable error

Normally launched automatically by 'gt crew start' for non-Claude agents.
Not intended for direct user invocation.`,
	Args: cobra.ExactArgs(1),
	RunE: runNudgePoller,
}

func runNudgePoller(cmd *cobra.Command, args []string) error {
	sessionName := args[0]

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("cannot find town root: %w", err)
	}

	pollInterval, err := time.ParseDuration(nudgePollerIntervalFlag)
	if err != nil {
		return fmt.Errorf("invalid --interval: %w", err)
	}

	idleTimeout, err := time.ParseDuration(nudgePollerIdleFlag)
	if err != nil {
		return fmt.Errorf("invalid --idle-timeout: %w", err)
	}

	t := tmux.NewTmux()

	// Verify session exists before starting the loop.
	if exists, _ := t.HasSession(sessionName); !exists {
		return fmt.Errorf("session %q not found", sessionName)
	}

	// Resolve nudge options once at startup: if the target agent uses Escape
	// as cancel (e.g., Gemini CLI), skip the Escape keystroke during delivery
	// to avoid canceling in-flight generation. (GH#gt-wasn)
	nudgeOpts := tmux.NudgeOpts{}
	agentName := ""
	hasPromptDetection := false
	if name, err := t.GetEnvironment(sessionName, "GT_AGENT"); err == nil && name != "" {
		agentName = name
		if preset := config.GetAgentPresetByName(agentName); preset != nil {
			hasPromptDetection = preset.ReadyPromptPrefix != ""
			if preset.EscapeCancelsRequest {
				nudgeOpts.SkipEscape = true
			}
		}
	}

	// Set up signal handling for graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-sigCh:
			return nil // graceful shutdown

		case <-ticker.C:
			// Check if session still exists.
			if exists, _ := t.HasSession(sessionName); !exists {
				return nil // session gone, exit
			}

			// Check if there is anything at all in the queue — a fresh
			// entry, or a leftover .claimed file from a crashed drainer.
			// Pending alone (counts .json only) would never trigger the
			// DrainClaims call below when the queue holds only .claimed
			// files, so the orphan sweep inside DrainClaims (the only thing
			// that restores a stale claim) would never run (codex,
			// nudge_poller.go:111, changes-requested at 08964387/95f841e6
			// rework).
			if has, _ := nudge.PendingOrClaimed(townRoot, sessionName); !has {
				continue
			}

			// For runtimes with prompt detection, defer delivery until the session
			// is actually idle. Runtimes without prompt detection preserve the old
			// best-effort behavior and drain on the poll interval.
			waitErr := t.WaitForIdle(sessionName, idleTimeout)
			if shouldSkipDrainUntilIdle(hasPromptDetection, waitErr) {
				continue
			}

			// Drain via claims, not Drain: the claim files stay on disk
			// until explicitly acked below, so a crash between draining and
			// the dead-letter/requeue write leaves the entry recoverable
			// instead of lost (nudge.DrainClaims, hq-g52db rework).
			claims, err := nudge.DrainClaims(townRoot, sessionName)
			if err != nil {
				fmt.Fprintf(os.Stderr, "nudge-poller: drain error for %s: %v\n", sessionName, err)
				continue
			}
			if len(claims) == 0 {
				continue // someone else drained it
			}

			// Entries that already exhausted MaxInjectionAttempts on a prior
			// cycle (reached only via the dead-letter-write durability
			// fallback), or whose prior attempt never reported an outcome
			// at all (InFlight, restored by the orphan sweep after a
			// crash), must not be retyped again — route them straight to
			// another dead-letter attempt.
			toInject, exhausted, staleInFlight := partitionForInjection(claims)
			var unresolved []nudge.QueuedNudge
			if len(exhausted) > 0 {
				unresolved = append(unresolved, handleFailedInjection(t, townRoot, sessionName, sourceNudgePoller, claimNudges(exhausted), errAttemptsExhausted)...)
			}
			if len(staleInFlight) > 0 {
				unresolved = append(unresolved, handleFailedInjection(t, townRoot, sessionName, sourceNudgePoller, claimNudges(staleInFlight), errPriorAttemptUnresolved)...)
			}
			if len(toInject) > 0 {
				// Persist the incremented Attempts count to each claim
				// BEFORE the batch injection attempt — see Claim.MarkAttempt
				// — so a crash mid-injection leaves the correct count
				// behind instead of a stale, pre-attempt one. A claim whose
				// persist itself fails is excluded from injection this
				// cycle (see markAttempts) and left un-acked.
				marked, failedPersist := markAttempts(sourceNudgePoller, sessionName, toInject)
				if len(failedPersist) > 0 {
					unresolved = append(unresolved, claimNudges(failedPersist)...)
				}
				if len(marked) > 0 {
					formatted := nudge.FormatForInjection(claimNudges(marked))
					if err := t.NudgeSessionWithOpts(sessionName, formatted, nudgeOpts); err != nil {
						fmt.Fprintf(os.Stderr, "nudge-poller: injection error for %s: %v\n", sessionName, err)
						unresolved = append(unresolved, handleFailedInjection(t, townRoot, sessionName, sourceNudgePoller, claimNudges(marked), err)...)
					}
				}
			}

			// Ack every RESOLVED claim only now: handleFailedInjection has
			// already durably dead-lettered or requeued each entry (or, for
			// the documented double-fault, dropped it with a loud log), so
			// the original claim is redundant for those. An entry in
			// unresolved did NOT durably land anywhere (its requeue write
			// itself failed) — its claim is left un-acked so the orphan
			// sweep can recover it instead of losing it here.
			ackClaims(sourceNudgePoller, sessionName, claims, unresolved)
		}
	}
}

func shouldSkipDrainUntilIdle(hasPromptDetection bool, waitErr error) bool {
	return hasPromptDetection && waitErr != nil
}
