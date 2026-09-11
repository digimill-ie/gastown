package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/nudge"
)

// failingWriter always errors, simulating a closed stdout.
type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) {
	return 0, errors.New("simulated write failure (closed stdout)")
}

// TestDrainAndPrintQueuedNudges_WriteFailureRetainsClaim covers High 5
// (mail_check.go:106): the hook drain must claim, deliver, THEN ack — not
// ack immediately on read (the old nudge.Drain call). If the write fails
// (a closed stdout, or any other I/O error), the claim must be left
// un-acked so a future orphan sweep can recover it, instead of being lost
// the instant it was read.
func TestDrainAndPrintQueuedNudges_WriteFailureRetainsClaim(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-mailcheck-writefail"

	if err := nudge.Enqueue(townRoot, session, nudge.QueuedNudge{
		ID: "wf-1", Sender: "test", Message: "must survive a failed print",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	drainAndPrintQueuedNudges(townRoot, session, failingWriter{})

	// Not acked: the claim must still exist (as a retained claim), not a
	// fresh pending .json (Drain used to delete it before ever trying to
	// print it).
	has, err := nudge.PendingOrClaimed(townRoot, session)
	if err != nil {
		t.Fatalf("PendingOrClaimed: %v", err)
	}
	if !has {
		t.Fatal("claim was lost when the write failed — drainAndPrintQueuedNudges must retain it for recovery")
	}
	if pending, _ := nudge.Pending(townRoot, session); pending != 0 {
		t.Fatalf("Pending = %d, want 0 (the claim must stay claimed, not be visible as a plain pending entry)", pending)
	}
}

// TestDrainAndPrintQueuedNudges_SuccessAcksClaim is the true-case
// companion: a successful write must ack the claim so the nudge is not
// redelivered on the next check.
func TestDrainAndPrintQueuedNudges_SuccessAcksClaim(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-mailcheck-success"

	if err := nudge.Enqueue(townRoot, session, nudge.QueuedNudge{
		ID: "ok-1", Sender: "test", Message: "delivered nudge",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	var buf strings.Builder
	drainAndPrintQueuedNudges(townRoot, session, &buf)

	if !strings.Contains(buf.String(), "delivered nudge") {
		t.Fatalf("output = %q, want it to contain the nudge message", buf.String())
	}
	has, err := nudge.PendingOrClaimed(townRoot, session)
	if err != nil {
		t.Fatalf("PendingOrClaimed: %v", err)
	}
	if has {
		t.Fatal("claim was left behind after a successful print — must be acked")
	}
}

func TestFormatInjectOutput(t *testing.T) {
	// Helper to build test messages with a given priority.
	msg := func(id, from, subject string, priority mail.Priority) *mail.Message {
		return &mail.Message{
			ID:       id,
			From:     from,
			Subject:  subject,
			Priority: priority,
		}
	}

	tests := []struct {
		name     string
		messages []*mail.Message
		// Strings that MUST appear in the output.
		wantContains []string
		// Strings that must NOT appear in the output.
		wantAbsent []string
	}{
		{
			name: "urgent only",
			messages: []*mail.Message{
				msg("m1", "mayor/", "Deploy now", mail.PriorityUrgent),
			},
			wantContains: []string{
				"<system-reminder>",
				"</system-reminder>",
				"URGENT: 1 urgent message(s)",
				"m1 from mayor/: Deploy now",
				"gt mail read <id>",
			},
			wantAbsent: []string{
				"high-priority",
				"additional",
			},
		},
		{
			name: "high only",
			messages: []*mail.Message{
				msg("m2", "gastown/wolf", "Review PR", mail.PriorityHigh),
			},
			wantContains: []string{
				"<system-reminder>",
				"1 high-priority message(s)",
				"m2 from gastown/wolf: Review PR",
				"process these messages",
				"before going idle",
			},
			wantAbsent: []string{
				"URGENT",
				"additional",
			},
		},
		{
			name: "normal only",
			messages: []*mail.Message{
				msg("m3", "gastown/toast", "FYI update", mail.PriorityNormal),
			},
			wantContains: []string{
				"<system-reminder>",
				"1 unread message(s)",
				"m3 from gastown/toast: FYI update",
				"check these messages",
				"before going idle",
			},
			wantAbsent: []string{
				"URGENT",
				"high-priority",
			},
		},
		{
			name: "low priority treated as normal tier",
			messages: []*mail.Message{
				msg("m4", "gastown/nux", "Backlog item", mail.PriorityLow),
			},
			wantContains: []string{
				"1 unread message(s)",
				"m4 from gastown/nux: Backlog item",
				"check these messages",
			},
			wantAbsent: []string{
				"URGENT",
				"high-priority",
			},
		},
		{
			name: "urgent + high: high listed separately",
			messages: []*mail.Message{
				msg("m5", "mayor/", "Emergency", mail.PriorityUrgent),
				msg("m6", "gastown/wolf", "Important review", mail.PriorityHigh),
			},
			wantContains: []string{
				"URGENT: 1 urgent message(s)",
				"m5 from mayor/: Emergency",
				"1 high-priority message(s)",
				"m6 from gastown/wolf: Important review",
				"process before going idle",
				"gt mail read <id>",
			},
			wantAbsent: []string{
				// High-priority should NOT be folded into a generic "non-urgent" count.
				"non-urgent",
			},
		},
		{
			name: "urgent + high + normal: all tiers shown",
			messages: []*mail.Message{
				msg("m7", "mayor/", "Fire", mail.PriorityUrgent),
				msg("m8", "gastown/wolf", "Review ASAP", mail.PriorityHigh),
				msg("m9", "gastown/toast", "Newsletter", mail.PriorityNormal),
			},
			wantContains: []string{
				"URGENT: 1 urgent message(s)",
				"m7 from mayor/: Fire",
				"1 high-priority message(s)",
				"m8 from gastown/wolf: Review ASAP",
				"1 additional message(s)",
			},
			wantAbsent: []string{
				"normal-priority",
				"non-urgent",
			},
		},
		{
			name: "urgent + normal (no high): normal shown as additional",
			messages: []*mail.Message{
				msg("m10", "mayor/", "Alert", mail.PriorityUrgent),
				msg("m11", "gastown/nux", "Low item", mail.PriorityLow),
				msg("m12", "gastown/toast", "Info", mail.PriorityNormal),
			},
			wantContains: []string{
				"URGENT: 1 urgent message(s)",
				"2 additional message(s)",
			},
			wantAbsent: []string{
				"high-priority",
				"normal-priority",
			},
		},
		{
			name: "high + normal: normal shown as additional",
			messages: []*mail.Message{
				msg("m13", "gastown/wolf", "Review", mail.PriorityHigh),
				msg("m14", "gastown/toast", "FYI", mail.PriorityNormal),
				msg("m15", "gastown/nux", "Backlog", mail.PriorityLow),
			},
			wantContains: []string{
				"1 high-priority message(s)",
				"2 additional message(s)",
			},
			wantAbsent: []string{
				"URGENT",
				"normal-priority",
			},
		},
		{
			name: "multiple urgent messages",
			messages: []*mail.Message{
				msg("m16", "mayor/", "Fire 1", mail.PriorityUrgent),
				msg("m17", "deacon/", "Fire 2", mail.PriorityUrgent),
			},
			wantContains: []string{
				"URGENT: 2 urgent message(s)",
				"m16 from mayor/: Fire 1",
				"m17 from deacon/: Fire 2",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output := formatInjectOutput(tt.messages)

			for _, want := range tt.wantContains {
				if !strings.Contains(output, want) {
					t.Errorf("output should contain %q\n\nGot:\n%s", want, output)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(output, absent) {
					t.Errorf("output should NOT contain %q\n\nGot:\n%s", absent, output)
				}
			}
		})
	}
}
