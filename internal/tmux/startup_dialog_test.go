package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// claudeComposerRule is Claude Code's own horizontal-rule chrome line,
// rendered immediately above its live input box in every permission mode
// (measured live, v2.1.268). Fixtures print it ahead of a "❯" line so the
// structural composer check (isClaudeComposerOpen) recognizes the line as
// the live composer, exactly as a real captured pane would — the numbered-
// option exclusion alone cannot tell it apart from a dialog's own cursor.
const claudeComposerRule = "────────────────────────────────────────────────────────────────────────────"

// TestClassifyStartupDialog covers both polarities required by gtn-k43 /
// hq-ooijo: a specific known dialog is detected by content, and everything
// else — including a working session, a busy/background hint, and a dialog
// already answered — is DialogNone (zero keys).
func TestClassifyStartupDialog(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    StartupDialogKind
	}{
		{
			name:    "no dialog, ordinary shell",
			content: "user@host:~$ ",
			want:    DialogNone,
		},
		{
			name:    "workspace trust dialog visible",
			content: "Quick safety check - do you trust this folder?\n1. Yes, I trust this folder",
			want:    DialogWorkspaceTrust,
		},
		{
			name:    "codex workspace trust dialog visible",
			content: "> You are in /tmp/demo\nDo you trust the contents of this directory?",
			want:    DialogWorkspaceTrust,
		},
		{
			name:    "bypass permissions dialog visible",
			content: "Bypass Permissions mode\n1. No\n2. Yes, I accept",
			want:    DialogBypassPermissions,
		},
		{
			name:    "theme picker visible",
			content: "Choose the text style that looks best with your terminal:\n1. Dark mode\n2. Light mode",
			want:    DialogThemePicker,
		},
		{
			name:    "trust dialog already answered — prompt appears after it",
			content: "Quick safety check - do you trust this folder?\n1. Yes, I trust this folder\n> ",
			want:    DialogNone,
		},
		{
			name:    "bypass dialog already answered — prompt appears after it",
			content: "Bypass Permissions mode\n2. Yes, I accept\nuser@host:~$ ",
			want:    DialogNone,
		},
		{
			name:    "quoted/historical dialog text in scrollback, real prompt below",
			content: "user pasted: \"Bypass Permissions mode\"\n$ ",
			want:    DialogNone, // prompt line after the marker means it's already resolved
		},
		{
			// codex finding on gtn-k43 / hq-ooijo revision 2: an old dialog
			// marker followed by a REAL composer that already has typed
			// text in it must read as resolved. A composer with content
			// does not end with its prompt character ("› review this" does
			// not end in "›"), so a suffix-only prompt check misses it and
			// would send the dialog's Down/Enter into that composer.
			name:    "old bypass marker in scrollback, live composer with typed text below",
			content: "Bypass Permissions mode\n2. Yes, I accept\n› review this",
			want:    DialogNone,
		},
		{
			name:    "old trust marker in scrollback, live empty codex composer below",
			content: "Quick safety check - do you trust this folder?\n1. Yes, I trust this folder\n› ",
			want:    DialogNone,
		},
		{
			name:    "genuinely stalled session, no known dialog text",
			content: "some unrelated hung output\nwith no dialog markers at all",
			want:    DialogNone,
		},
		{
			name:    "empty content",
			content: "",
			want:    DialogNone,
		},
		{
			// codex High, tmux.go:2313, REVISION 2: quoted dialog text and its
			// own resolving composer prefix on ONE line must not authorise a
			// dismissal. The old check only rejected a prompt on a LATER
			// line than the marker; same-line was never covered, so this
			// composer read as "the bypass dialog, not yet answered".
			name:    "composer line quotes bypass dialog text on the same line — must not authorise a dismissal",
			content: "› explain Bypass Permissions mode",
			want:    DialogNone,
		},
		{
			name:    "composer line quotes workspace trust text on the same line — must not authorise a dismissal",
			content: "› what happens if I decline to trust this folder?",
			want:    DialogNone,
		},
		{
			// codex Medium, tmux.go:2145: a dialog's own selection cursor
			// ("❯ 1. Dark mode") shares composerPrefixes' lead glyph with a
			// live composer. Without excluding numbered-option lines, this
			// reads as "a real composer appeared after the dialog marker",
			// i.e. already answered — even though the dialog is still
			// showing. Fixtures previously omitted the cursor glyph
			// entirely (startup_dialog_test.go:38,43 at review time).
			name:    "theme picker with a real selection cursor on an option line",
			content: "Choose the text style that looks best with your terminal:\n❯ 1. Dark mode\n  2. Light mode",
			want:    DialogThemePicker,
		},
		{
			name:    "bypass dialog with a real selection cursor on an option line",
			content: "Bypass Permissions mode\n❯ 1. No\n  2. Yes, I accept",
			want:    DialogBypassPermissions,
		},
		{
			// codex High, tmux.go:2175, REVISION 3: a composer holding
			// user-typed NUMBERED text was excluded from prompt-indicator
			// status by the same pattern that (correctly) excludes a
			// dialog's own "❯ 1. Dark mode" cursor line. That let this
			// composer's own text re-classify as the dialog it merely
			// mentions. The exclusion is now scoped to the '❯' glyph only
			// — never '>' or '›', the real composer leads — so numbered
			// composer text is recognised as a live prompt like any other.
			name:    "composer holding numbered text quoting a dialog marker — must not authorise a dismissal",
			content: "› 1. Explain Quick safety check",
			want:    DialogNone,
		},
		{
			name:    "composer holding numbered text quoting the bypass marker — must not authorise a dismissal",
			content: "> 1. What does Bypass Permissions mode do?",
			want:    DialogNone,
		},
		{
			// codex High, tmux.go:2355, REVISION 3: "the marker is on a
			// later line than the prompt". A multi-line composer entry —
			// one open '›' line followed by more typed/pasted lines with
			// no glyph of their own — is still ALL composer content; only
			// its first line carries the lead glyph. The old check only
			// rejected a marker on a line BEFORE a later prompt; it never
			// rejected a marker appearing AFTER an already-open composer.
			name:    "composer holding a multi-line quotation of the trust prompt — must not authorise a dismissal",
			content: "› what happens if I decline to trust this folder, exactly?\nQuick safety check - do you trust this folder?",
			want:    DialogNone,
		},
		{
			name:    "composer holding a multi-line quotation of the bypass marker — must not authorise a dismissal",
			content: "› explain this to me:\n  Bypass Permissions mode",
			want:    DialogNone,
		},
		{
			// codex review 5637995408, High, tmux.go:2162: Claude's
			// ReadyPromptPrefix IS "❯ " (internal/config/agents.go:251), so
			// a Claude composer holding numbered text renders pixel-for-
			// pixel like a dialog's own cursor line. Text alone cannot
			// distinguish them; structural confirmation (a preceding chrome
			// rule line, isClaudeComposerOpen) is required. Measured live,
			// Claude Code v2.1.268.
			name:    "Claude composer holding numbered text quoting a dialog marker, with its chrome rule — must not authorise a dismissal",
			content: claudeComposerRule + "\n❯ 1. Explain Quick safety check",
			want:    DialogNone,
		},
		{
			name:    "Claude composer holding numbered text quoting the bypass marker, with its chrome rule — must not authorise a dismissal",
			content: claudeComposerRule + "\n❯ 1. What does Bypass Permissions mode do?",
			want:    DialogNone,
		},
		{
			// codex review 5637995408, High, tmux.go:2342: the multi-line
			// suppression (composerOpen) only ever latched for Codex's '›'
			// lead; a Claude '❯' composer's own continuation line — no
			// glyph of its own — fell straight through to the dialog-marker
			// switch.
			name:    "Claude composer holding a multi-line quotation of the bypass marker, with its chrome rule — must not authorise a dismissal",
			content: claudeComposerRule + "\n❯ explain this:\n  Bypass Permissions mode",
			want:    DialogNone,
		},
		{
			// Regression guard for the opposite polarity: WITHOUT a
			// preceding chrome rule, a numbered '❯' line is still read as a
			// dialog's own selection cursor, exactly as the theme-picker
			// and bypass-dialog cases above require. The structural check
			// must never fire on every '❯' line unconditionally.
			name:    "bypass dialog cursor with no preceding rule line still classifies as the dialog",
			content: "Bypass Permissions mode\n❯ 1. No\n  2. Yes, I accept",
			want:    DialogBypassPermissions,
		},
		{
			// codex review 5637995408, Medium, tmux.go:2342 (Codex form):
			// stale scrollback left by a killed process must not
			// permanently block a dialog that genuinely renders later.
			// Claude's version of this is verified directly against
			// isClaudeComposerStale in TestIsClaudeComposerStale below; this
			// case exercises it through the full classifier.
			name: "stale Claude composer from a prior incarnation does not suppress a dialog rendered after a respawn",
			content: claudeComposerRule + "\n❯ \n" + claudeComposerRule + "\n" +
				"  [gastown/polecats/morsov]  dd@host  /tmp/demo  Fable 5.1  18:15\n" +
				"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← 2 agents\n\n" +
				"Choose the text style that looks best with your terminal:\n❯ 1. Dark mode\n  2. Light mode",
			want: DialogThemePicker,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyStartupDialog(tt.content)
			if got != tt.want {
				t.Errorf("classifyStartupDialog(%q) = %q, want %q", tt.content, got, tt.want)
			}
		})
	}
}

// TestContainsBackgroundTaskHint verifies both polarities: the exact text
// measured on gastown/furiosa (gtn-k43) is detected, and ordinary content is not.
func TestContainsBackgroundTaskHint(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "background task hint present",
			content: "Running in the background (down-arrow to manage)",
			want:    true,
		},
		{
			name:    "streaming/interrupt hint present",
			content: "Thinking... (esc to interrupt)",
			want:    true,
		},
		{
			// codex review 5637995408, Medium, tmux.go:2296: this guard
			// duplicated its own "esc to interrupt" substring check instead
			// of reusing hasBusyIndicator, so it went stale the same way —
			// current Claude Code (v2.1.268) renders neither text while
			// thinking. Measured live, gtn-bl1.
			name:    "claude spinner busy, no esc-to-interrupt text anywhere",
			content: "❯ write the essay\n\n· Billowing… (29s · thinking more)\n\n" + claudeComposerRule + "\n❯ ",
			want:    true,
		},
		{
			name:    "ordinary idle prompt",
			content: "> ",
			want:    false,
		},
		{
			name:    "empty content",
			content: "",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ContainsBackgroundTaskHint(tt.content); got != tt.want {
				t.Errorf("ContainsBackgroundTaskHint(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

// TestGetWindowActivity_Basic verifies window_activity reads as a sane
// timestamp on a freshly created session — the field this fix uses instead
// of session_activity, which freezes at creation on every detached session
// (hq-wisp-y46vn).
func TestGetWindowActivity_Basic(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-winactivity-" + t.Name()

	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	activity, err := tm.GetWindowActivity(sessionName)
	if err != nil {
		t.Fatalf("GetWindowActivity: %v", err)
	}
	if activity.IsZero() || activity.Year() < 2000 {
		t.Errorf("GetWindowActivity returned suspicious time: %v", activity)
	}
}

// TestGetWindowActivity_AdvancesOnOutput verifies window_activity moves
// forward when the pane produces output — the property session_activity is
// documented to NOT reliably have on a detached session.
func TestGetWindowActivity_AdvancesOnOutput(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-winactivity-adv-" + t.Name()

	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	first, err := tm.GetWindowActivity(sessionName)
	if err != nil {
		t.Fatalf("GetWindowActivity (first): %v", err)
	}

	time.Sleep(1100 * time.Millisecond)
	if err := tm.SendKeys(sessionName, "echo hello-from-test"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	second, err := tm.GetWindowActivity(sessionName)
	if err != nil {
		t.Fatalf("GetWindowActivity (second): %v", err)
	}

	// Strictly After, not Equal-or-After: an unchanged timestamp must FAIL
	// this test, since the whole point is proving the field moves on real
	// output (codex finding: the prior form accepted a stuck timestamp).
	if !second.After(first) {
		t.Errorf("window_activity did not advance after output: first=%v second=%v", first, second)
	}
}

// TestClassifyVisibleDialog_NoDialog verifies pure detection returns
// DialogNone (zero keys sent — this function never sends keys) for an
// ordinary idle session.
func TestClassifyVisibleDialog_NoDialog(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-classify-none-" + t.Name()

	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	kind, err := tm.ClassifyVisibleDialog(sessionName)
	if err != nil {
		t.Fatalf("ClassifyVisibleDialog: %v", err)
	}
	if kind != DialogNone {
		t.Errorf("kind = %q, want DialogNone for an ordinary shell prompt", kind)
	}
}

// TestClassifyVisibleDialog_DetectsTrustDialog verifies detection of a real
// dialog echoed into the pane, mirroring the existing
// AcceptWorkspaceTrustDialog test's simulation approach.
func TestClassifyVisibleDialog_DetectsTrustDialog(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-classify-trust-" + t.Name()

	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// `read` blocks waiting for input, so the echoed dialog line stays as the
	// pane's last content indefinitely — unlike a plain `echo`, which returns
	// to a shell prompt immediately and would read as "already answered".
	if err := tm.SendKeys(sessionName, "clear; printf '%s\\n' 'Quick safety check - do you trust this folder?'; read -r _dlg"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	time.Sleep(400 * time.Millisecond)

	kind, err := tm.ClassifyVisibleDialog(sessionName)
	if err != nil {
		t.Fatalf("ClassifyVisibleDialog: %v", err)
	}
	if kind != DialogWorkspaceTrust {
		t.Errorf("kind = %q, want %q", kind, DialogWorkspaceTrust)
	}
}

// TestDetectAndDismissKnownDialog_NoDialogSendsNoKeys is the core
// regression test for gtn-k43 / hq-ooijo: an ordinary session with no
// visible dialog must receive ZERO keys. We can't directly observe "no keys
// sent", but we can observe that the composer/prompt is untouched: sending
// nothing leaves an empty shell prompt with no injected newline/command.
func TestDetectAndDismissKnownDialog_NoDialogSendsNoKeys(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-detect-none-" + t.Name()

	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	before, err := tm.CapturePane(sessionName, 30)
	if err != nil {
		t.Fatalf("CapturePane (before): %v", err)
	}

	kind, err := tm.DetectAndDismissKnownDialog(sessionName)
	if err != nil {
		t.Fatalf("DetectAndDismissKnownDialog: %v", err)
	}
	if kind != DialogNone {
		t.Fatalf("kind = %q, want DialogNone", kind)
	}

	time.Sleep(200 * time.Millisecond)
	after, err := tm.CapturePane(sessionName, 30)
	if err != nil {
		t.Fatalf("CapturePane (after): %v", err)
	}
	if before != after {
		t.Errorf("pane content changed with no dialog present — a key was sent\nbefore: %q\nafter:  %q", before, after)
	}
}

// TestDetectAndDismissKnownDialog_QuotedDialogTextInComposerSendsNoKeys is
// the live-tmux regression for the codex High finding on tmux.go:2313: an
// idle composer whose typed text happens to QUOTE a dialog marker, on the
// SAME line as the composer's own lead glyph, must never be read as that
// dialog. `read` blocks so the composer line stays exactly as printed
// (no trailing shell prompt would otherwise land on a later line and mask
// the bug via the old line-order check).
func TestDetectAndDismissKnownDialog_QuotedDialogTextInComposerSendsNoKeys(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-detect-quoted-" + t.Name()

	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	if err := tm.SendKeys(sessionName, "clear; printf '%s' '› explain Bypass Permissions mode'; read -r _dlg"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	time.Sleep(400 * time.Millisecond)

	before, err := tm.CapturePane(sessionName, 30)
	if err != nil {
		t.Fatalf("CapturePane (before): %v", err)
	}

	kind, err := tm.DetectAndDismissKnownDialog(sessionName)
	if err != nil {
		t.Fatalf("DetectAndDismissKnownDialog: %v", err)
	}
	if kind != DialogNone {
		t.Fatalf("kind = %q, want DialogNone (quoted dialog text on the composer's own line must not authorise a dismissal)", kind)
	}

	time.Sleep(200 * time.Millisecond)
	after, err := tm.CapturePane(sessionName, 30)
	if err != nil {
		t.Fatalf("CapturePane (after): %v", err)
	}
	if before != after {
		t.Errorf("pane content changed — a key reached the composer quoting dialog text\nbefore: %q\nafter:  %q", before, after)
	}
}

// TestDetectAndDismissKnownDialog_ClaudeComposerNumberedQuoteSendsNoKeys is
// the Claude runtime's counterpart to the Codex test above, and the exact
// shape codex named for the surviving High (codex review 5637995408:
// "internal/tmux/tmux.go:2162 — ... a Claude composer holding `❯ 1.
// Explain Quick safety check` ... passes classification"). Claude's own
// dialog cursor and its live composer prompt render with the IDENTICAL '❯'
// glyph (internal/config/agents.go:251), so this can only pass once
// recognition is structural (a preceding chrome rule), not textual.
func TestDetectAndDismissKnownDialog_ClaudeComposerNumberedQuoteSendsNoKeys(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-detect-claude-numbered-" + t.Name()

	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	cmd := "clear; printf '%s\\n' '" + claudeComposerRule + "'; printf '%s' '❯ 1. Explain Quick safety check'; read -r _dlg"
	if err := tm.SendKeys(sessionName, cmd); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	time.Sleep(400 * time.Millisecond)

	before, err := tm.CapturePane(sessionName, 30)
	if err != nil {
		t.Fatalf("CapturePane (before): %v", err)
	}

	kind, err := tm.DetectAndDismissKnownDialog(sessionName)
	if err != nil {
		t.Fatalf("DetectAndDismissKnownDialog: %v", err)
	}
	if kind != DialogNone {
		t.Fatalf("kind = %q, want DialogNone (a Claude composer's numbered draft must not authorise a dismissal)", kind)
	}

	time.Sleep(200 * time.Millisecond)
	after, err := tm.CapturePane(sessionName, 30)
	if err != nil {
		t.Fatalf("CapturePane (after): %v", err)
	}
	if before != after {
		t.Errorf("pane content changed — a key reached the Claude composer quoting dialog text\nbefore: %q\nafter:  %q", before, after)
	}
}

// TestDetectAndDismissKnownDialog_DismissesTrustDialog verifies the positive
// case: a real dialog is detected and its specific key sequence is sent.
func TestDetectAndDismissKnownDialog_DismissesTrustDialog(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-detect-trust-" + t.Name()

	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// `read` blocks until a key arrives, holding the dialog text on screen.
	// The first read captures whatever preceded that key raw (so a stray Down
	// or other key sent BEFORE Enter shows up as literal bytes in _line, not
	// silently discarded); a second, non-blocking read with a short timeout
	// then checks for anything sent AFTER Enter. Only when both are empty
	// does the script print the "exact-single-enter" marker — a substring
	// check for a fixed marker string alone cannot distinguish "exactly one
	// Enter" from "Down then Enter" or "Enter then a stray extra key", since
	// both leave that same marker text present somewhere in the pane (codex
	// finding: startup_dialog_test.go:326 — "positive tests assert
	// substrings, not the complete key stream").
	//
	// The second read's timeout is an INTEGER second, not "0.2": the host's
	// bash (3.2.57) rejects a fractional -t value outright ("invalid timeout
	// specification", exit 1) without waiting at all, which silently turned
	// the stray-key check into a no-op — it always saw an empty, never-read
	// $_extra regardless of what was actually sent (codex Medium,
	// startup_dialog_test.go:399, REVISION 3). The check itself now reads
	// $_extra's READ EXIT STATUS, not its string value: a stray bare Enter
	// is a single newline byte, which `read -n1` consumes and then reports
	// as an EMPTY value indistinguishable from "nothing arrived" — only the
	// exit status (0 = something was read, non-zero = the read timed out)
	// tells them apart.
	//
	// A REAL second-long wait now sits between the dismiss key landing and
	// the verdict being knowable, but DismissDialog re-verifies the pane
	// only ~500ms after sending — sooner than that. tmux capture-pane
	// includes scrollback, so `clear` never hides the dialog's own text from
	// that re-check; only a PROMPT-lead line appearing AFTER the dialog text
	// (classifyStartupDialog's "already answered" rule) satisfies it. So the
	// script prints a bare shell-prompt-shaped line ("$ ") immediately after
	// the real key is consumed — independent of the stray-key wait — and
	// writes the actual exact-stream verdict to a FILE once that wait
	// completes, which the test reads separately after giving it time.
	markerPath := filepath.Join(t.TempDir(), "trust.marker")
	script := "#!/bin/bash\n" +
		"clear; printf '%s\\n' 'Quick safety check - do you trust this folder?'\n" +
		"IFS= read -r _line\n" +
		"printf '%s' '$ '\n" +
		"IFS= read -rsn1 -t 1 _extra; _extra_status=$?\n" +
		"if [ -z \"$_line\" ] && [ \"$_extra_status\" -ne 0 ]; then printf 'exact-single-enter' > " + markerPath + "; " +
		"else printf 'unexpected: line=%s extra=%s status=%s' \"$(printf '%s' \"$_line\" | cat -v)\" \"$(printf '%s' \"$_extra\" | cat -v)\" \"$_extra_status\" > " + markerPath + "; fi\n"
	scriptPath := filepath.Join(t.TempDir(), "trust.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	if err := tm.SendKeys(sessionName, "clear; bash "+scriptPath); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	kind, err := tm.DetectAndDismissKnownDialog(sessionName)
	if err != nil {
		t.Fatalf("DetectAndDismissKnownDialog: %v", err)
	}
	if kind != DialogWorkspaceTrust {
		t.Errorf("kind = %q, want %q", kind, DialogWorkspaceTrust)
	}

	// Assert the EXACT key effect, not just the returned status: the trust
	// dialog's own key is a single Enter, nothing before and nothing after.
	// A return status alone cannot tell a correctly-dismissed dialog from
	// one that merely stopped matching the classifier for an unrelated
	// reason (codex finding on this file). The script's own stray-key read
	// waits a full second before writing its verdict, so this wait must
	// clear that 1s mark with margin (~550ms already elapsed inside
	// DetectAndDismissKnownDialog's own post-send sleep).
	time.Sleep(900 * time.Millisecond)
	verdict, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("reading verdict marker: %v", err)
	}
	if string(verdict) != "exact-single-enter" {
		t.Errorf("dismiss did not send exactly one Enter and nothing else: %q", verdict)
	}
}

// TestDetectAndDismissKnownDialog_DismissesBypassDialog verifies the bypass
// permissions dialog's exact key sequence (Down, then Enter) is sent — the
// only case tested for classification alone until now (codex finding: "theme
// and bypass have classifier cases only").
func TestDetectAndDismissKnownDialog_DismissesBypassDialog(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-detect-bypass-" + t.Name()

	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Reads one full LINE (canonical mode): tmux's "Down" sends the raw
	// ESC-[-B escape sequence, which the tty driver does not interpret as a
	// control character — it lands as literal bytes in the line, terminated
	// by the Enter that follows. `cat -v` renders the ESC byte visibly
	// (^[) so the test can assert BOTH keys were sent, in order. A second,
	// non-blocking read with a short timeout then checks for anything sent
	// AFTER that Enter, so a stray extra key following dismissal is caught
	// too — a bare substring check on "^[[B:end" alone would still pass with
	// trailing garbage appended after it (codex finding: startup_dialog_test.go:326
	// — "positive tests assert substrings, not the complete key stream").
	//
	// A script FILE, not an inline multi-line string handed to SendKeys —
	// see the trust dialog test above for why. The timeout is an integer
	// second (bash 3.2 rejects "0.2" outright, silently no-op'ing the wait —
	// see the trust dialog test above), and the stray-key verdict reads the
	// read's EXIT STATUS rather than $_extra's string value: a stray bare
	// Enter is a lone newline byte, which `read -n1` consumes and reports as
	// an empty value indistinguishable from "nothing arrived" (codex Medium,
	// startup_dialog_test.go:399, REVISION 3).
	//
	// A bare "$ " prints IMMEDIATELY after the real key is consumed — not
	// after the stray-key wait — so DismissDialog's own post-send re-check
	// (~700ms after sending Down) reads it as an answered prompt well before
	// this script's 1-second stray-key wait ends; tmux capture-pane includes
	// scrollback, so `clear` alone never hid the dialog text from that
	// re-check (see the trust dialog test above). The exact-stream verdict
	// goes to a FILE once the wait completes, read separately below.
	markerPath := filepath.Join(t.TempDir(), "bypass.marker")
	script := "#!/bin/bash\n" +
		"clear; printf '%s\\n' 'Bypass Permissions mode'\n" +
		"printf '%s\\n' '1. No'\n" +
		"printf '%s\\n' '2. Yes, I accept'\n" +
		"IFS= read -r _line\n" +
		"printf '%s' '$ '\n" +
		"IFS= read -rsn1 -t 1 _extra; _extra_status=$?\n" +
		"{ printf '%s' \"$_line\" | cat -v; printf ':end:'; " +
		"if [ \"$_extra_status\" -eq 0 ]; then printf 'stray-key'; else printf 'clean'; fi; } > " + markerPath + "\n"
	scriptPath := filepath.Join(t.TempDir(), "bypass.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	if err := tm.SendKeys(sessionName, "clear; bash "+scriptPath); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	kind, err := tm.DetectAndDismissKnownDialog(sessionName)
	if err != nil {
		t.Fatalf("DetectAndDismissKnownDialog: %v", err)
	}
	if kind != DialogBypassPermissions {
		t.Errorf("kind = %q, want %q", kind, DialogBypassPermissions)
	}

	// See the trust dialog test above: the script's stray-key read waits a
	// full second before writing its verdict, so this wait must clear that
	// mark with margin.
	time.Sleep(900 * time.Millisecond)
	verdict, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("reading verdict marker: %v", err)
	}
	// Exact equality, not Contains: "^[[B^[[B:end:clean" (an accidental
	// extra Down before the real one) CONTAINS the accepted suffix
	// "^[[B:end:clean" as a substring, so a Contains check here would still
	// pass with a duplicate key on the wire (codex review 5637995408,
	// Medium, startup_dialog_test.go:565).
	if string(verdict) != "^[[B:end:clean" {
		t.Errorf("dismiss did not send exactly Down then Enter and nothing else: %q", verdict)
	}
}

// TestDetectAndDismissKnownDialog_DismissesThemeDialog verifies the theme
// picker's exact key sequence (a single Enter, default pre-highlighted) is
// sent (codex finding: "theme and bypass have classifier cases only").
func TestDetectAndDismissKnownDialog_DismissesThemeDialog(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-detect-theme-" + t.Name()

	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Same exact-stream technique as the trust dialog test above — including
	// running it as a script FILE, not an inline multi-line string handed to
	// SendKeys (same reasoning as that test) — only a single Enter, nothing
	// before it and nothing after, prints the marker (codex finding:
	// startup_dialog_test.go:326 — "positive tests assert substrings, not
	// the complete key stream").
	// The timeout is an integer second and the verdict reads the read's exit
	// status, not $_extra's string value — see the trust dialog test above
	// for why (codex Medium, startup_dialog_test.go:399, REVISION 3). "$ "
	// prints IMMEDIATELY after the real key is consumed — not after the
	// stray-key wait — so DismissDialog's post-send re-check reads it as an
	// answered prompt well before the 1-second wait ends; capture-pane
	// includes scrollback, so `clear` alone never hid the dialog text from
	// that re-check (see the trust dialog test above). The exact-stream
	// verdict goes to a FILE once the wait completes.
	markerPath := filepath.Join(t.TempDir(), "theme.marker")
	script := "#!/bin/bash\n" +
		"clear; printf '%s\\n' 'Choose the text style that looks best with your terminal:'\n" +
		"printf '%s\\n' '1. Dark mode'\n" +
		"printf '%s\\n' '2. Light mode'\n" +
		"IFS= read -r _line\n" +
		"printf '%s' '$ '\n" +
		"IFS= read -rsn1 -t 1 _extra; _extra_status=$?\n" +
		"if [ -z \"$_line\" ] && [ \"$_extra_status\" -ne 0 ]; then printf 'exact-single-enter' > " + markerPath + "; " +
		"else printf 'unexpected: line=%s extra=%s status=%s' \"$(printf '%s' \"$_line\" | cat -v)\" \"$(printf '%s' \"$_extra\" | cat -v)\" \"$_extra_status\" > " + markerPath + "; fi\n"
	scriptPath := filepath.Join(t.TempDir(), "theme.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	if err := tm.SendKeys(sessionName, "clear; bash "+scriptPath); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	kind, err := tm.DetectAndDismissKnownDialog(sessionName)
	if err != nil {
		t.Fatalf("DetectAndDismissKnownDialog: %v", err)
	}
	if kind != DialogThemePicker {
		t.Errorf("kind = %q, want %q", kind, DialogThemePicker)
	}

	// See the trust dialog test above: the script's stray-key read waits a
	// full second before writing its verdict, so this wait must clear that
	// mark with margin.
	time.Sleep(900 * time.Millisecond)
	verdict, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("reading verdict marker: %v", err)
	}
	if string(verdict) != "exact-single-enter" {
		t.Errorf("dismiss did not send exactly one Enter and nothing else: %q", verdict)
	}
}

// TestDismissDialog_PersistsAfterInput covers the polarity codex found
// missing: a dialog that is STILL showing after its dismiss keys are sent
// (e.g. the agent process is wedged, or the wrong keys were sent) must be
// reported as an error, not a false success. Removing DismissDialog's
// post-send verification would leave this green (codex finding on this
// file: "No test rejects a dialog that persists after input").
func TestDismissDialog_PersistsAfterInput(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-dismiss-persists-" + t.Name()

	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Re-prints the SAME dialog text after every keystroke, so the trust
	// dialog's Enter never actually clears it — simulating a stuck dialog.
	loop := `while true; do clear; printf '%s\n' 'Quick safety check - do you trust this folder?'; read -rsn1 _; done`
	if err := tm.SendKeys(sessionName, loop); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	err := tm.DismissDialog(sessionName, DialogWorkspaceTrust)
	if err == nil {
		t.Fatal("expected an error when the dialog is still visible after dismiss keys, got nil")
	}
}

// TestDismissDialog_BypassDialogClearedBetweenDownAndEnter covers the
// revalidation added between Down and Enter for the bypass dialog: if the
// dialog disappears in that gap (another actor, or a race), DismissDialog
// must refuse to send Enter into whatever now has focus rather than send it
// blind (codex finding: "the bypass Enter goes out after an unvalidated
// 200ms gap").
func TestDismissDialog_BypassDialogClearedBetweenDownAndEnter(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-dismiss-bypass-race-" + t.Name()

	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// A real script file (bash, not whatever the interactive login shell's
	// `read` flags happen to be) so the Down keypress is consumed
	// deterministically: the FIRST byte of Down's escape sequence triggers
	// an immediate switch to an unrelated screen ending in a real prompt
	// line (so classifyStartupDialog reads the old dialog marker, now in
	// scrollback, as resolved — same rule as "prompt appears after it"),
	// with a composer waiting below it. If Enter is sent blind (the
	// regression this guards), the composer's empty line is submitted and
	// the marker FILE below is created; a correctly revalidating
	// DismissDialog never lets that happen. Verifying via a file — not
	// screen-scraped pane text — avoids matching this script's own echoed
	// source line for a marker string.
	marker := filepath.Join(t.TempDir(), "enter-reached")
	script := "#!/bin/bash\n" +
		"clear; printf '%s\\n' 'Bypass Permissions mode'; printf '%s\\n' '1. No'; printf '%s\\n' '2. Yes, I accept'\n" +
		"IFS= read -rsn1 _c\n" +
		// Integer timeout — bash 3.2 rejects "0.2" outright (invalid timeout
		// specification, exit 1, no wait at all), same host issue as the
		// exact-key-stream tests above (codex Medium,
		// startup_dialog_test.go:399, REVISION 3). This read only drains any
		// remaining bytes of Down's escape sequence; its value is unused.
		"IFS= read -rsn2 -t 1 _rest\n" +
		"clear; printf '%s\\n' 'unrelated screen, dialog already gone'; printf '%s' '$ '\n" +
		"IFS= read -r _cmd\n" +
		"touch " + marker + "\n"
	scriptPath := filepath.Join(t.TempDir(), "race.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	if err := tm.SendKeys(sessionName, "clear; bash "+scriptPath); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	err := tm.DismissDialog(sessionName, DialogBypassPermissions)
	if err == nil {
		t.Fatal("expected an error when the dialog disappears between Down and Enter, got nil")
	}

	// Confirm no stray Enter reached the unrelated composer — a blind send
	// would have submitted its empty buffer and created the marker file.
	time.Sleep(300 * time.Millisecond)
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("marker file exists — a stray Enter reached the composer that replaced the dialog")
	}
}

// TestDismissDialog_DisappearsBeforeSend covers the "dialog disappears
// between detection and action" polarity: if the dialog is gone by the time
// DismissDialog revalidates, it must refuse to send keys.
func TestDismissDialog_DisappearsBeforeSend(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-dismiss-vanished-" + t.Name()

	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// No dialog was ever shown — DismissDialog is called directly (as if a
	// caller detected DialogWorkspaceTrust a moment ago and the screen has
	// since moved on). Revalidation must find it gone and refuse to act.
	err := tm.DismissDialog(sessionName, DialogWorkspaceTrust)
	if err == nil {
		t.Fatal("expected error when the dialog is no longer visible at send time, got nil")
	}
}

// TestDismissDialog_InvalidKind verifies unknown kinds are rejected rather
// than falling through to a default key sequence.
func TestDismissDialog_InvalidKind(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-dismiss-invalid-" + t.Name()

	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	err := tm.DismissDialog(sessionName, StartupDialogKind("bogus"))
	if err == nil {
		t.Fatal("expected error for unknown dialog kind, got nil")
	}
}

// TestGetSessionID_ReturnsNonEmpty is a sanity check for the new incarnation
// primitive (gtn-qp7 / hq-ooijo revision 3): a live session always has a
// non-empty tmux #{session_id} (e.g. "$3").
func TestGetSessionID_ReturnsNonEmpty(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-session-id-" + t.Name()

	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	id, err := tm.GetSessionID(sessionName)
	if err != nil {
		t.Fatalf("GetSessionID: %v", err)
	}
	if id == "" {
		t.Error("expected a non-empty session id")
	}
}

// TestDismissDialogGated_NotAuthorized_SendsNoKeys covers the primary
// authorization gate (gtn-qp7 / hq-ooijo revision 3, item 4): even with a
// real, currently-visible dialog, an authorized func returning false must
// refuse to send ANY key, never falling back to the unguarded sequence.
func TestDismissDialogGated_NotAuthorized_SendsNoKeys(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-dismiss-gated-unauthorized-" + t.Name()

	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	if err := tm.SendKeys(sessionName, "clear; printf '%s\\n' 'Quick safety check - do you trust this folder?'; read -r _dlg"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	time.Sleep(400 * time.Millisecond)

	before, err := tm.CapturePane(sessionName, 30)
	if err != nil {
		t.Fatalf("CapturePane (before): %v", err)
	}

	err = tm.DismissDialogGated(sessionName, DialogWorkspaceTrust, func() bool { return false })
	if err == nil {
		t.Fatal("expected error when authorized() is always false, got nil")
	}

	after, err := tm.CapturePane(sessionName, 30)
	if err != nil {
		t.Fatalf("CapturePane (after): %v", err)
	}
	if before != after {
		t.Errorf("pane content changed with authorized()=false — a key was sent\nbefore: %q\nafter:  %q", before, after)
	}
}

// TestDismissDialogGated_AuthorizedThroughout_DismissesTrustDialog is the
// positive case: with authorized() always true, DismissDialogGated behaves
// exactly like DismissDialog.
func TestDismissDialogGated_AuthorizedThroughout_DismissesTrustDialog(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-dismiss-gated-authorized-" + t.Name()

	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	if err := tm.SendKeys(sessionName, "clear; printf '%s\\n' 'Quick safety check - do you trust this folder?'; read -r _dlg; clear; echo dialog-dismissed"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	if err := tm.DismissDialogGated(sessionName, DialogWorkspaceTrust, func() bool { return true }); err != nil {
		t.Fatalf("DismissDialogGated: %v", err)
	}
}

// TestDismissDialogGated_BypassDialog_DeauthorizedBetweenDownAndEnter_Aborts
// covers the mid-sequence abort itself (gtn-qp7 / hq-ooijo revision 3, item
// 4): authorized() is true for the pre-Down check and false for every check
// after — the exact shape of an agent writing its first heartbeat (closing
// the startup window) in the 200ms gap between Down and Enter. Enter must
// never be sent. Uses the same blocking-read marker technique as
// TestDismissDialog_BypassDialogClearedBetweenDownAndEnter: the script only
// reaches `touch` if a line (Enter) actually arrives, so a hung/timed-out
// read proves zero keys reached it — sleeping past a timeout could not.
func TestDismissDialogGated_BypassDialog_DeauthorizedBetweenDownAndEnter_Aborts(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-dismiss-gated-bypass-deauth-" + t.Name()

	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	marker := filepath.Join(t.TempDir(), "enter-reached")
	script := "#!/bin/bash\n" +
		"clear; printf '%s\\n' 'Bypass Permissions mode'; printf '%s\\n' '1. No'; printf '%s\\n' '2. Yes, I accept'\n" +
		"IFS= read -rsn1 _c\n" +
		// Integer timeout — bash 3.2 rejects "0.2" outright (invalid timeout
		// specification, exit 1, no wait at all), same host issue noted on
		// the sibling race test above (codex Medium, REVISION 3). Drains the
		// rest of Down's escape sequence; its value is unused.
		"IFS= read -rsn2 -t 1 _rest\n" +
		// Blocks forever unless a line (Enter) actually arrives — a timeout
		// here would let the script fall through to `touch` regardless of
		// whether a key was sent, which is exactly the false-pass this test
		// must not produce.
		"IFS= read -r _maybe_enter\n" +
		"touch " + marker + "\n"
	scriptPath := filepath.Join(t.TempDir(), "gated-deauth.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	if err := tm.SendKeys(sessionName, "clear; bash "+scriptPath); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	calls := 0
	authorized := func() bool {
		calls++
		return calls == 1 // authorized for the pre-Down check only
	}

	err := tm.DismissDialogGated(sessionName, DialogBypassPermissions, authorized)
	if err == nil {
		t.Fatal("expected error when authorization closes between Down and Enter, got nil")
	}
	if calls < 2 {
		t.Fatalf("authorized() called %d times, want at least 2 (pre-Down and pre-Enter)", calls)
	}

	time.Sleep(300 * time.Millisecond)
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("marker file exists — Enter reached the dialog after authorization closed")
	}
}

// TestIsRuleLine pins the structural signature isClaudeComposerOpen and
// isClaudeComposerStale both depend on: Claude Code's own horizontal-rule
// chrome line. If this stops matching the real rule, both structural
// checks silently stop firing and the numbered-composer High (tmux.go:2162)
// reopens.
func TestIsRuleLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{"real chrome rule", claudeComposerRule, true},
		{"real chrome rule with surrounding whitespace", "  " + claudeComposerRule + "  ", true},
		{"short dash run", "──────", false},
		{"ascii hyphens, not the rule glyph", strings.Repeat("-", 80), false},
		{"ordinary text", "❯ 1. Explain Quick safety check", false},
		{"empty", "", false},
		{"theme picker's own divider glyph, not the composer rule", strings.Repeat("╌", 80), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRuleLine(tt.line); got != tt.want {
				t.Errorf("isRuleLine(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

// TestIsClaudeComposerOpen covers both polarities of the structural check
// itself, isolated from the full classifier: a numbered "❯" line reads as
// the live composer ONLY when a chrome rule immediately precedes it —
// otherwise it must still read as a dialog's own selection cursor (codex
// review 5637995408, High, tmux.go:2162).
func TestIsClaudeComposerOpen(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		i     int
		want  bool
	}{
		{"bare composer preceded by rule", []string{claudeComposerRule, "❯ "}, 1, true},
		{"numbered composer preceded by rule", []string{claudeComposerRule, "❯ 1. Explain Quick safety check"}, 1, true},
		{"numbered dialog cursor with NO preceding rule", []string{"Bypass Permissions mode", "❯ 1. No"}, 1, false},
		{"numbered dialog cursor preceded by banner text, not a rule", []string{"Choose the text style:", "❯ 2. Dark mode"}, 1, false},
		{"composer as the very first line — no line to precede it", []string{"❯ hello"}, 0, false},
		{"not a composer lead at all", []string{claudeComposerRule, "  2. Yes, I accept"}, 1, false},
		{"codex composer lead is not this check's concern", []string{claudeComposerRule, "› hello"}, 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isClaudeComposerOpen(tt.lines, tt.i); got != tt.want {
				t.Errorf("isClaudeComposerOpen(%v, %d) = %v, want %v", tt.lines, tt.i, got, tt.want)
			}
		})
	}
}

// TestIsClaudeComposerStale covers both polarities of the respawn-recovery
// fix (codex review 5637995408, Medium, tmux.go:2342): a composer whose own
// closing frame (rule, then status footer) is followed by further content
// is leftover scrollback from a prior process, not the live input box.
func TestIsClaudeComposerStale(t *testing.T) {
	footer := "  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← 2 agents"
	tests := []struct {
		name  string
		lines []string
		i     int
		want  bool
	}{
		{
			name:  "live composer, nothing after its own frame",
			lines: []string{claudeComposerRule, "❯ ", claudeComposerRule, footer},
			i:     1,
			want:  false,
		},
		{
			name:  "live composer, only blank lines after its own frame",
			lines: []string{claudeComposerRule, "❯ ", claudeComposerRule, footer, "", ""},
			i:     1,
			want:  false,
		},
		{
			name: "stale composer followed by a freshly rendered dialog after respawn",
			lines: []string{
				claudeComposerRule, "❯ ", claudeComposerRule, footer, "",
				"Choose the text style that looks best with your terminal:",
				"❯ 1. Dark mode", "  2. Light mode",
			},
			i:    1,
			want: true,
		},
		{
			name:  "no closing rule found within the capture — conservative default",
			lines: []string{claudeComposerRule, "❯ some very long unterminated draft"},
			i:     1,
			want:  false,
		},
		{
			name:  "closing rule present but not followed by the expected footer shape",
			lines: []string{claudeComposerRule, "❯ ", claudeComposerRule, "unexpected content"},
			i:     1,
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isClaudeComposerStale(tt.lines, tt.i); got != tt.want {
				t.Errorf("isClaudeComposerStale(_, %d) = %v, want %v", tt.i, got, tt.want)
			}
		})
	}
}
