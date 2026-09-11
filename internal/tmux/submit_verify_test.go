package tmux

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestSubmitNeedle(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 100)
	tests := []struct {
		name    string
		message string
		want    string
	}{
		{"short message", "resume the patrol", "resume the patrol"},
		{"long message truncated", long, long[:submitNeedleMaxRunes]},
		{"multiline uses first line", "first line\nsecond line", "first line"},
		{"leading blanks skipped", "\n  \nreal content", "real content"},
		{"space trimmed", "  hello  ", "hello"},
		{"empty", "", ""},
		{"unicode rune boundary", strings.Repeat("é", 50), strings.Repeat("é", submitNeedleMaxRunes)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := submitNeedle(tt.message); got != tt.want {
				t.Errorf("submitNeedle(%q) = %q, want %q", tt.message, got, tt.want)
			}
		})
	}
}

func TestStripAnsiTrackDim(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		input     string
		wantPlain string
		wantDim   string
	}{
		{"plain", "hello", "hello", "....."},
		{"dim span", "ab\x1b[2mcd\x1b[0mef", "abcdef", "..dd.."},
		{"dim off", "\x1b[2mab\x1b[22mcd", "abcd", "dd.."},
		{"256 color arg 2 is not dim", "\x1b[38;5;2mgreen\x1b[0m", "green", "....."},
		{"truecolor args are not dim", "\x1b[38;2;10;20;30mrgb\x1b[0m", "rgb", "..."},
		{"osc stripped", "\x1b]0;title\x07text", "text", "...."},
		{"unicode preserved", "❯ \x1b[2mghost\x1b[0m", "❯ ghost", "..ddddd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plain, dim := stripAnsiTrackDim(tt.input)
			if string(plain) != tt.wantPlain {
				t.Errorf("plain = %q, want %q", string(plain), tt.wantPlain)
			}
			var got strings.Builder
			for _, d := range dim {
				if d {
					got.WriteByte('d')
				} else {
					got.WriteByte('.')
				}
			}
			if got.String() != tt.wantDim {
				t.Errorf("dim = %q, want %q", got.String(), tt.wantDim)
			}
		})
	}
}

func TestAnalyzeSubmission(t *testing.T) {
	t.Parallel()
	const needle = "resume the patrol"
	const prompt = DefaultReadyPromptPrefix

	tests := []struct {
		name    string
		content string
		want    submitProbe
	}{
		{
			name:    "normal text beats stale busy indicator",
			content: "transcript\n❯ resume the patrol\n· Thinking... (esc to interrupt)",
			want:    probeStranded,
		},
		{
			name:    "busy without composer means turn started",
			content: "agent output\n· Thinking... (esc to interrupt)",
			want:    probeTurnStarted,
		},
		{
			name:    "dim ghost text is cleared",
			content: "transcript\n❯ \x1b[2mresume the patrol\x1b[0m\n",
			want:    probeComposerCleared,
		},
		{
			name:    "empty composer is cleared",
			content: "transcript\n❯\n",
			want:    probeComposerCleared,
		},
		{
			name:    "different normal composer text is dirty",
			content: "transcript\n❯ unrelated draft\n",
			want:    probeComposerDirty,
		},
		{
			name:    "wrapped prefix counts as stranded",
			content: "transcript\n❯ resume the\n patrol\n",
			want:    probeStranded,
		},
		{
			name:    "short prefix is too ambiguous",
			content: "transcript\n❯ resu\n",
			want:    probeComposerDirty,
		},
		{
			name:    "bottom-most prompt line wins",
			content: "❯ resume the patrol\nagent response\n❯\n",
			want:    probeComposerCleared,
		},
		{
			name:    "typed text echoed below prompt is not composer content",
			content: "❯ \nresume the patrol\nresume the patrol\n",
			want:    probeComposerCleared,
		},
		{
			name:    "no prompt or busy is unknown",
			content: "shell output\nwithout prompt",
			want:    probeUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := analyzeSubmission(tt.content, needle, prompt); got != tt.want {
				t.Errorf("analyzeSubmission() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestErrSubmitNotVerifiedWrapping(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("nudge to session: %w", fmt.Errorf("submit: %w", ErrSubmitNotVerified))
	if !errors.Is(wrapped, ErrSubmitNotVerified) {
		t.Fatal("errors.Is did not recognize wrapped ErrSubmitNotVerified")
	}
}

// TestErrComposerDirtyWrapping mirrors TestErrSubmitNotVerifiedWrapping for
// the new sentinel (hq-g52db): callers distinguish "the composer is known
// dirty, do not retype" from every other injection failure via errors.Is on
// ErrComposerDirty specifically, so both wrapped errors must remain
// recognizable through fmt.Errorf %w chains the way submitComposer produces
// them.
func TestErrComposerDirtyWrapping(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("nudge to session %q: %w", "gt-test", fmt.Errorf("%w: %w (composer contains other text after Enter)", ErrSubmitNotVerified, ErrComposerDirty))
	if !errors.Is(wrapped, ErrSubmitNotVerified) {
		t.Error("errors.Is did not recognize wrapped ErrSubmitNotVerified")
	}
	if !errors.Is(wrapped, ErrComposerDirty) {
		t.Error("errors.Is did not recognize wrapped ErrComposerDirty")
	}
}

// TestRecoveryKeystrokesValidatedForAgent covers the runtime table required
// by hq-g52db fix 5: recovery keystrokes (C-j) are only attempted on runtimes
// where their effect is validated. Claude Code is the long-validated target;
// every other known preset — and any unrecognized/custom agent name — must
// default to false rather than risk a destructive keystroke on an
// unfamiliar runtime.
func TestRecoveryKeystrokesValidatedForAgent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		agentName string
		want      bool
	}{
		{"claude", true},
		{"codex", false},
		{"gemini", false},
		{"cursor", false},
		{"copilot", false},
		{"some-unrecognized-custom-agent", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.agentName, func(t *testing.T) {
			if got := recoveryKeystrokesValidatedForAgent(tt.agentName); got != tt.want {
				t.Errorf("recoveryKeystrokesValidatedForAgent(%q) = %v, want %v", tt.agentName, got, tt.want)
			}
		})
	}
}

// TestRecoveryKeystrokesValidatedForSession_LookupFailureFailsClosed covers
// the fix for a lookup FAILURE (tmux error, session gone) being treated the
// same as "no GT_AGENT set" — the latter defaults to true (assume Claude),
// but a failure is a genuinely unknown state and must default to false, not
// fail open into an unvalidated recovery keystroke (codex, tmux.go:3379,
// changes-requested at 08964387). A Tmux pointed at a socket with no real
// server makes GetEnvironment fail deterministically without needing a live
// tmux session.
func TestRecoveryKeystrokesValidatedForSession_LookupFailureFailsClosed(t *testing.T) {
	t.Parallel()
	tm := NewTmuxWithSocket("gt-test-no-such-socket-submit-verify")
	if got := recoveryKeystrokesValidatedForSession(tm, "any-session"); got {
		t.Error("recoveryKeystrokesValidatedForSession() = true on a lookup failure, want false")
	}
}

// TestRecoveryKeystrokesValidatedForPane_UnresolvableTargetFailsClosed is the
// pane-target counterpart: an unresolvable pane (stale, no server) must not
// default to true either (codex, tmux.go:3405, changes-requested at
// 08964387).
func TestRecoveryKeystrokesValidatedForPane_UnresolvableTargetFailsClosed(t *testing.T) {
	t.Parallel()
	tm := NewTmuxWithSocket("gt-test-no-such-socket-submit-verify")
	if got := recoveryKeystrokesValidatedForPane(tm, "%999"); got {
		t.Error("recoveryKeystrokesValidatedForPane() = true for an unresolvable pane, want false")
	}
}

// TestRecoveryProbeError covers the pure classification behind
// recoverStrandedComposer's post-C-j decision, split out specifically so it
// is unit-testable without a live tmux session (codex,
// submit_verify.go:348 / submit_verify_test.go:158, changes-requested at
// 08964387: the prior wrapping tests constructed their own error values
// rather than calling production code, so they could not fail if this
// classification broke).
func TestRecoveryProbeError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		probe           submitProbe
		wantDirty       bool
		wantNotVerified bool
	}{
		{probeStranded, true, true},
		{probeComposerDirty, true, true},
		{probeUnknown, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.probe.String(), func(t *testing.T) {
			err := recoveryProbeError(tt.probe)
			if !errors.Is(err, ErrSubmitNotVerified) {
				t.Errorf("recoveryProbeError(%v) missing ErrSubmitNotVerified: %v", tt.probe, err)
			}
			if got := errors.Is(err, ErrComposerDirty); got != tt.wantDirty {
				t.Errorf("recoveryProbeError(%v) errors.Is(ErrComposerDirty) = %v, want %v", tt.probe, got, tt.wantDirty)
			}
		})
	}
}
