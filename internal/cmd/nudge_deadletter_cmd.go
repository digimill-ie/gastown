package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

func init() {
	nudgeCmd.AddCommand(nudgeDeadLetterCmd)
	nudgeDeadLetterCmd.AddCommand(nudgeDeadLetterListCmd)
	nudgeDeadLetterCmd.AddCommand(nudgeDeadLetterReplayCmd)
}

var nudgeDeadLetterCmd = &cobra.Command{
	Use:   "dead-letter",
	Short: "Inspect and replay nudges that failed delivery",
	Long: `A nudge is dead-lettered instead of being retyped forever when delivery
fails in a way that automatic retry cannot fix (a known-dirty composer) or
after retrying once (any other injection error). See hq-g52db.

Dead-lettered entries are never delivered automatically — use "replay" to
put one back in the active queue after fixing whatever caused delivery to
fail (e.g., the target session was genuinely stuck and has since recovered).`,
}

var nudgeDeadLetterListCmd = &cobra.Command{
	Use:   "list <session>",
	Short: "List dead-lettered nudges for a session",
	Args:  cobra.ExactArgs(1),
	RunE:  runNudgeDeadLetterList,
}

var nudgeDeadLetterReplayCmd = &cobra.Command{
	Use:   "replay <session> <id>",
	Short: "Re-enqueue a dead-lettered nudge for delivery",
	Args:  cobra.ExactArgs(2),
	RunE:  runNudgeDeadLetterReplay,
}

func runNudgeDeadLetterList(cmd *cobra.Command, args []string) error {
	sessionName := args[0]

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("cannot find town root: %w", err)
	}

	entries, err := nudge.ListDeadLetters(townRoot, sessionName)
	if err != nil {
		return fmt.Errorf("listing dead letters for %s: %w", sessionName, err)
	}

	if len(entries) == 0 {
		fmt.Printf("%s No dead-lettered nudges for %s\n", style.Dim.Render("○"), sessionName)
		return nil
	}

	fmt.Printf("Dead-lettered nudges for %s (%d):\n\n", sessionName, len(entries))
	for _, e := range entries {
		fmt.Printf("%s %s\n", style.Bold.Render("ID:"), e.ID)
		fmt.Printf("  Dead-lettered: %s (source: %s)\n", e.DeadLetteredAt.Format("2006-01-02T15:04:05Z07:00"), e.Source)
		fmt.Printf("  Sender:  %s\n", e.Sender)
		fmt.Printf("  Attempts: %d, uncertain delivery: %v\n", e.Attempts, e.UncertainDelivery)
		fmt.Printf("  Last error: %s\n", e.LastError)
		fmt.Printf("  Message: %s\n", e.Message)
		if e.PaneCapture != "" {
			fmt.Printf("  Pane capture at failure:\n%s\n", indentLines(e.PaneCapture, "    "))
		}
		fmt.Println()
	}
	fmt.Printf("Replay one: gt nudge dead-letter replay %s <id>\n", sessionName)
	return nil
}

func runNudgeDeadLetterReplay(cmd *cobra.Command, args []string) error {
	sessionName := args[0]
	id := args[1]

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("cannot find town root: %w", err)
	}

	if err := nudge.ReplayDeadLetter(townRoot, sessionName, id); err != nil {
		return fmt.Errorf("replaying %s for %s: %w", id, sessionName, err)
	}

	fmt.Printf("%s Replayed %s for %s — re-enqueued for delivery\n", style.Bold.Render("✓"), id, sessionName)
	return nil
}

// indentLines prefixes every line of s with indent, for nested display.
func indentLines(s, indent string) string {
	out := indent
	for _, r := range s {
		out += string(r)
		if r == '\n' {
			out += indent
		}
	}
	return out
}
