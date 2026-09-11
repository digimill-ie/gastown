package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/eventstream"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Smallest-slice event delivery (gtn-2ml / hq-6u258): one opt-in Claude
// agent, one event type (queued_nudge), a durable per-agent stream with
// ids and an acknowledged replay position, subscribed to via the Monitor
// tool. This is an additive channel — it changes no default nudge or
// mail path. See internal/eventstream for the durable contract.

var (
	eventsEmitTypeFlag     string
	eventsEmitMessageFlag  string
	eventsEmitSenderFlag   string
	eventsTailIntervalFlag time.Duration
	eventsTailOnceFlag     bool
	eventsStatusJSONFlag   bool
	eventsStatusMaxAgeFlag time.Duration
)

func init() {
	rootCmd.AddCommand(eventsCmd)
	eventsCmd.AddCommand(eventsEmitCmd)
	eventsCmd.AddCommand(eventsTailCmd)
	eventsCmd.AddCommand(eventsStatusCmd)
	eventsCmd.AddCommand(eventsMonitorCmdCmd)

	eventsEmitCmd.Flags().StringVar(&eventsEmitTypeFlag, "type", eventstream.TypeQueuedNudge, "Event type (the smallest slice only has a consumer for queued_nudge)")
	eventsEmitCmd.Flags().StringVarP(&eventsEmitMessageFlag, "message", "m", "", "Message payload (for type=queued_nudge)")
	eventsEmitCmd.Flags().StringVar(&eventsEmitSenderFlag, "sender", "", "Sender identity (defaults to the caller's detected address)")

	eventsTailCmd.Flags().DurationVar(&eventsTailIntervalFlag, "interval", 2*time.Second, "Poll interval between passes")
	eventsTailCmd.Flags().BoolVar(&eventsTailOnceFlag, "once", false, "Run a single consume pass and exit (for scripting/testing, not for Monitor use)")

	eventsStatusCmd.Flags().BoolVar(&eventsStatusJSONFlag, "json", false, "Output JSON")
	eventsStatusCmd.Flags().DurationVar(&eventsStatusMaxAgeFlag, "max-age", 30*time.Second, "Heartbeat staleness threshold for the armed verdict")
}

var eventsCmd = &cobra.Command{
	Use:     "events",
	GroupID: GroupComm,
	Short:   "Durable per-agent event stream (reactive delivery via the Monitor tool)",
	Long: `Reactive event delivery, smallest slice (gtn-2ml, mirrors hq-6u258).

A durable, per-agent, append-only event stream with stable ids and an
explicit claim -> complete -> ack lifecycle. One opt-in Claude agent
subscribes with 'gt events tail <target>' under the Monitor tool; new
lines arrive as notifications without cancelling any in-flight tool call.

This is additive: it does not replace 'gt nudge' or the UserPromptSubmit
hook drain, and nothing here changes a default delivery path.

Subcommands:
  emit          Append an event to a target's stream
  tail          Consume the stream (the Monitor-tool command)
  status        Report replay position, pending count, and whether the
                tail consumer's heartbeat is fresh (armed) or stale
  monitor-cmd   Print the exact command to run under the Monitor tool`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	},
}

var eventsEmitCmd = &cobra.Command{
	Use:         "emit <target> [message]",
	Annotations: map[string]string{AnnotationPolecatSafe: "true"},
	Short:       "Append an event to a target agent's durable stream",
	Long: `Appends one durable event to <target>'s event stream.

<target> accepts the same role shortcuts as 'gt nudge' (witness, refinery,
mayor, deacon) plus a literal session name. The default event type,
queued_nudge, is the one type the smallest-slice consumer ('gt events
tail') understands; other types are appended but render generically.

Examples:
  gt events emit witness "Check your mail"
  gt events emit gastown/witness -m "Check your mail"`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runEventsEmit,
}

var eventsTailCmd = &cobra.Command{
	Use:         "tail <target>",
	Annotations: map[string]string{AnnotationPolecatSafe: "true"},
	Short:       "Consume <target>'s event stream (the Monitor-tool command)",
	Long: `Runs the durable consumer loop for <target>'s event stream: each
pass claims the next unacked event, applies its effect (prints a
notification line), marks it complete, then acks it — advancing the
replay position by exactly one event. A crash between complete and ack
is recovered on the next pass without re-applying the effect (see
internal/eventstream for the contract).

This is the command a Claude Code agent arms with the Monitor tool at
prime. It runs until interrupted (SIGINT/SIGTERM) or --once completes a
single pass. Every pass touches a heartbeat regardless of whether there
were events, independent of delivery — this is what 'gt events status'
reads to report the consumer as armed or stale.`,
	Args: cobra.ExactArgs(1),
	RunE: runEventsTail,
}

var eventsStatusCmd = &cobra.Command{
	Use:         "status <target>",
	Annotations: map[string]string{AnnotationPolecatSafe: "true"},
	Short:       "Report replay position and whether the tail consumer is armed",
	Long: `Reports the current ack position, pending event count, and whether
'gt events tail' has touched its heartbeat within --max-age (default 30s).

This is the arm-check primitive: it observes the WATCHER's liveness, not
whether events are flowing, so a dead 'gt events tail' process is
reported independently of the underlying event source — the failure
mode the design calls out as needing to be independently observable
rather than silent.`,
	Args: cobra.ExactArgs(1),
	RunE: runEventsStatus,
}

var eventsMonitorCmdCmd = &cobra.Command{
	Use:         "monitor-cmd <target>",
	Annotations: map[string]string{AnnotationPolecatSafe: "true"},
	Short:       "Print the exact command to arm with the Monitor tool",
	Args:        cobra.ExactArgs(1),
	RunE:        runEventsMonitorCmd,
}

// resolveEventStreamTarget expands the role shortcuts 'gt nudge' accepts
// (witness, refinery, mayor, deacon) to a session name; any other value
// is treated as a literal stream key (a session name, or any stable
// identifier a caller already has).
func resolveEventStreamTarget(target string) (string, error) {
	target = strings.TrimSuffix(target, "/")
	switch target {
	case constants.RoleMayor:
		return session.MayorSessionName(), nil
	case constants.RoleDeacon:
		return session.DeaconSessionName(), nil
	case constants.RoleWitness, constants.RoleRefinery:
		roleInfo, err := GetRole()
		if err != nil {
			return "", fmt.Errorf("cannot determine rig for %s shortcut: %w", target, err)
		}
		if roleInfo.Rig == "" {
			return "", fmt.Errorf("cannot determine rig for %s shortcut (not in a rig context)", target)
		}
		rigPrefix := session.PrefixFor(roleInfo.Rig)
		if target == constants.RoleWitness {
			return session.WitnessSessionName(rigPrefix), nil
		}
		return session.RefinerySessionName(rigPrefix), nil
	default:
		return target, nil
	}
}

func runEventsEmit(cmd *cobra.Command, args []string) error {
	stream, err := resolveEventStreamTarget(args[0])
	if err != nil {
		return err
	}

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("gt events requires a Gas Town workspace: %w", err)
	}

	message := eventsEmitMessageFlag
	if message == "" && len(args) == 2 {
		message = args[1]
	}
	if eventsEmitTypeFlag == eventstream.TypeQueuedNudge && message == "" {
		return fmt.Errorf("message required for type=%s: use -m or provide as second argument", eventstream.TypeQueuedNudge)
	}

	sender := eventsEmitSenderFlag
	if sender == "" {
		sender = detectSender()
	}

	payload := map[string]interface{}{
		"sender":  sender,
		"message": message,
	}

	ev, err := eventstream.Append(townRoot, stream, eventsEmitTypeFlag, payload)
	if err != nil {
		return fmt.Errorf("emitting event: %w", err)
	}

	fmt.Printf("✓ Emitted event %d (type=%s) to %s\n", ev.ID, ev.Type, stream)
	return nil
}

func runEventsTail(cmd *cobra.Command, args []string) error {
	stream, err := resolveEventStreamTarget(args[0])
	if err != nil {
		return err
	}

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("gt events requires a Gas Town workspace: %w", err)
	}

	apply := func(ev eventstream.Event) error {
		fmt.Println(eventstream.FormatNotification(ev))
		return nil
	}

	// pass reports the consume error to the caller rather than swallowing
	// it, so --once (scripting/tests) surfaces failure via exit code.
	// Heartbeat is always attempted, independent of whether the consume
	// succeeded or anything was delivered — liveness, not delivery, is
	// what 'status' reads.
	pass := func() error {
		_, consumeErr := eventstream.ConsumeOnce(townRoot, stream, apply)
		if hbErr := eventstream.Heartbeat(townRoot, stream); hbErr != nil {
			fmt.Fprintf(os.Stderr, "gt events tail: heartbeat error: %v\n", hbErr)
		}
		return consumeErr
	}

	if eventsTailOnceFlag {
		return pass()
	}

	// The persistent loop does not exit on a pass error: the failed event
	// stays claimed-unacked and is retried next pass. Only --once (above)
	// propagates it as a command failure.
	logPassError := func() {
		if err := pass(); err != nil {
			fmt.Fprintf(os.Stderr, "gt events tail: consume pass error: %v\n", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logPassError()
	ticker := time.NewTicker(eventsTailIntervalFlag)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			logPassError()
		}
	}
}

func runEventsStatus(cmd *cobra.Command, args []string) error {
	stream, err := resolveEventStreamTarget(args[0])
	if err != nil {
		return err
	}

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("gt events requires a Gas Town workspace: %w", err)
	}

	acked, err := eventstream.AckedPosition(townRoot, stream)
	if err != nil {
		return fmt.Errorf("reading acked position: %w", err)
	}
	pending, err := eventstream.ReadAfter(townRoot, stream, acked)
	if err != nil {
		return fmt.Errorf("reading pending events: %w", err)
	}
	armed, age, err := eventstream.IsArmed(townRoot, stream, eventsStatusMaxAgeFlag)
	if err != nil {
		return fmt.Errorf("reading heartbeat: %w", err)
	}

	if eventsStatusJSONFlag {
		result := map[string]interface{}{
			"target":          args[0],
			"stream":          stream,
			"acked_position":  acked,
			"pending":         len(pending),
			"armed":           armed,
			"heartbeat_age_s": age.Seconds(),
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	armedLabel := "ARMED"
	if !armed {
		armedLabel = "NOT ARMED (stale or no heartbeat)"
	}
	fmt.Printf("Stream: %s\n", stream)
	fmt.Printf("Acked position: %d\n", acked)
	fmt.Printf("Pending: %d\n", len(pending))
	fmt.Printf("Tail consumer: %s (heartbeat age %s)\n", armedLabel, age.Round(time.Second))
	return nil
}

func runEventsMonitorCmd(cmd *cobra.Command, args []string) error {
	stream, err := resolveEventStreamTarget(args[0])
	if err != nil {
		return err
	}
	fmt.Printf("gt events tail %s\n", stream)
	return nil
}
