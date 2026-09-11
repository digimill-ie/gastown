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

			// Check if there are queued nudges.
			if n, _ := nudge.Pending(townRoot, sessionName); n == 0 {
				continue
			}

			// For runtimes with prompt detection, defer delivery until the session
			// is actually idle. Runtimes without prompt detection preserve the old
			// best-effort behavior and drain on the poll interval.
			waitErr := t.WaitForIdle(sessionName, idleTimeout)
			if shouldSkipDrainUntilIdle(hasPromptDetection, waitErr) {
				continue
			}

			// Drain and inject.
			drained, err := nudge.Drain(townRoot, sessionName)
			if err != nil {
				fmt.Fprintf(os.Stderr, "nudge-poller: drain error for %s: %v\n", sessionName, err)
				continue
			}
			if len(drained) == 0 {
				continue // someone else drained it
			}

			formatted := nudge.FormatForInjection(drained)
			if err := t.NudgeSessionWithOpts(sessionName, formatted, nudgeOpts); err != nil {
				fmt.Fprintf(os.Stderr, "nudge-poller: injection error for %s: %v\n", sessionName, err)
				deadLetterDrainedNudges(t, townRoot, sessionName, drained, err)
			}
		}
	}
}

func shouldSkipDrainUntilIdle(hasPromptDetection bool, waitErr error) bool {
	return hasPromptDetection && waitErr != nil
}

// deadLetterDrainedNudges is the poller's injection error path: an entry
// that failed tmux delivery is never retyped, so it cannot duplicate
// content or loop forever. It is (a) logged to the session's durable
// injection-error log with a pane capture, then (b) written once to the
// dead-letter store for later inspection and manual replay
// (`gt nudge dead-letter list/replay`). There is no requeue and no retry: a
// crash between the failed injection and this write still loses the
// message, exactly as at base (hq-g52db).
func deadLetterDrainedNudges(t *tmux.Tmux, townRoot, sessionName string, drained []nudge.QueuedNudge, deliverErr error) {
	const source = "nudge-poller"

	paneCapture, _ := t.CapturePane(sessionName, 25)
	if err := nudge.LogInjectionError(townRoot, sessionName, source, deliverErr, paneCapture); err != nil {
		fmt.Fprintf(os.Stderr, "%s: injection-error log for %s failed: %v\n", source, sessionName, err)
	}

	for _, n := range drained {
		if _, err := nudge.DeadLetter(townRoot, sessionName, n, source, deliverErr.Error()); err != nil {
			fmt.Fprintf(os.Stderr, "%s: dead-letter write for %s failed, message lost: %v\n", source, sessionName, err)
		}
	}
}
