package cmd

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
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

type nudgePollerAgentOptions struct {
	hasPromptDetection bool
	readyPromptPrefix  string
	nudgeOpts          tmux.NudgeOpts
	unknownAgent       bool
	agentName          string
}

type nudgePollerTmux interface {
	HasSession(string) (bool, error)
	GetEnvironment(string, string) (string, error)
	WaitForIdleWithPromptPrefix(string, string, time.Duration) error
	NudgeSessionWithOpts(string, string, tmux.NudgeOpts) error
}

type nudgePollerQueue interface {
	Pending(string, string) (int, error)
	Drain(string, string) ([]nudge.QueuedNudge, error)
}

type filesystemNudgePollerQueue struct{}

func (filesystemNudgePollerQueue) Pending(root, session string) (int, error) {
	return nudge.Pending(root, session)
}

func (filesystemNudgePollerQueue) Drain(root, session string) ([]nudge.QueuedNudge, error) {
	return nudge.Drain(root, session)
}

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

	// Set up signal handling for graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	warnedUnknownAgents := make(map[string]struct{})

	for {
		select {
		case <-sigCh:
			return nil // graceful shutdown

		case <-ticker.C:
			alive := processNudgePollTick(t, filesystemNudgePollerQueue{}, sessionName, townRoot, idleTimeout, warnedUnknownAgents)
			if !alive {
				return nil // session gone, exit
			}
		}
	}
}

func processNudgePollTick(t nudgePollerTmux, queue nudgePollerQueue, sessionName, townRoot string, idleTimeout time.Duration, warnedUnknownAgents map[string]struct{}) bool {
	if exists, _ := t.HasSession(sessionName); !exists {
		return false
	}
	if n, _ := queue.Pending(townRoot, sessionName); n == 0 {
		return true
	}

	options := resolveNudgePollerAgentOptionsForSession(
		func(key string) (string, error) { return t.GetEnvironment(sessionName, key) },
		townRoot,
	)
	if options.unknownAgent {
		warnUnknownNudgePollerAgent(options.agentName, warnedUnknownAgents, os.Stderr)
	}

	// Prompt-aware runtimes must be idle before the queue is drained.
	waitErr := t.WaitForIdleWithPromptPrefix(sessionName, options.readyPromptPrefix, idleTimeout)
	if shouldSkipDrainUntilIdle(options.hasPromptDetection, waitErr) {
		return true
	}

	drained, err := queue.Drain(townRoot, sessionName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nudge-poller: drain error for %s: %v\n", sessionName, err)
		return true
	}
	if len(drained) == 0 {
		return true
	}

	if err := t.NudgeSessionWithOpts(sessionName, nudge.FormatForInjection(drained), options.nudgeOpts); err != nil {
		fmt.Fprintf(os.Stderr, "nudge-poller: injection error for %s: %v\n", sessionName, err)
		requeueDrainedNudges(townRoot, sessionName, "nudge-poller", drained)
	}
	return true
}

func warnUnknownNudgePollerAgent(agentName string, warned map[string]struct{}, output io.Writer) {
	if _, alreadyWarned := warned[agentName]; alreadyWarned {
		return
	}
	fmt.Fprintf(output, "nudge-poller: could not resolve agent %q; using best-effort delivery\n", agentName)
	warned[agentName] = struct{}{}
}

func resolveNudgePollerAgentOptionsForSession(getEnvironment func(string) (string, error), townRoot string) nudgePollerAgentOptions {
	agentName, err := getEnvironment("GT_AGENT")
	if err != nil || agentName == "" {
		return nudgePollerAgentOptions{readyPromptPrefix: tmux.DefaultReadyPromptPrefix}
	}

	rigName, _ := getEnvironment("GT_RIG")
	rigPath := ""
	if rigName != "" {
		rigPath = filepath.Join(townRoot, rigName)
	}
	options := resolveNudgePollerAgentOptions(agentName, townRoot, rigPath)
	options.agentName = agentName
	return options
}

func resolveNudgePollerAgentOptions(agentName, townRoot, rigPath string) nudgePollerAgentOptions {
	resolved := config.ResolveAgentConfigByName(agentName, townRoot, rigPath)
	if resolved == nil {
		return nudgePollerAgentOptions{readyPromptPrefix: tmux.DefaultReadyPromptPrefix, unknownAgent: true}
	}

	if resolved.Tmux != nil && resolved.Tmux.ReadyPromptPrefix != "" {
		presetName := resolved.Provider
		if presetName == "" {
			presetName = resolved.Command
		}
		preset := config.GetAgentPresetByName(presetName)
		return nudgePollerAgentOptions{
			hasPromptDetection: true,
			readyPromptPrefix:  resolved.Tmux.ReadyPromptPrefix,
			nudgeOpts:          tmux.NudgeOpts{SkipEscape: preset != nil && preset.EscapeCancelsRequest},
		}
	}

	presetName := resolved.Provider
	if presetName == "" {
		presetName = resolved.Command
	}
	preset := config.GetAgentPresetByName(presetName)
	if preset == nil || preset.ReadyPromptPrefix == "" {
		return nudgePollerAgentOptions{readyPromptPrefix: tmux.DefaultReadyPromptPrefix, nudgeOpts: tmux.NudgeOpts{SkipEscape: preset != nil && preset.EscapeCancelsRequest}}
	}
	return nudgePollerAgentOptions{hasPromptDetection: true, readyPromptPrefix: preset.ReadyPromptPrefix, nudgeOpts: tmux.NudgeOpts{SkipEscape: preset.EscapeCancelsRequest}}
}

func shouldSkipDrainUntilIdle(hasPromptDetection bool, waitErr error) bool {
	return hasPromptDetection && waitErr != nil
}
