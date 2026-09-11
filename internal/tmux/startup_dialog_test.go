package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
	// A real SCRIPT FILE, run via `bash <path>` — not a multi-line string
	// handed straight to SendKeys — because SendKeys pastes its whole
	// argument as one literal burst: a `read` mid-script would consume the
	// NEXT queued line of that same paste as its own input instead of
	// blocking on a separately-sent key, corrupting the intended sequence
	// (same reasoning as TestDismissDialog_BypassDialogClearedBetweenDownAndEnter
	// below).
	script := "#!/bin/bash\n" +
		"clear; printf '%s\\n' 'Quick safety check - do you trust this folder?'\n" +
		"IFS= read -r _line\n" +
		"IFS= read -rsn1 -t 0.2 _extra\n" +
		"clear\n" +
		"if [ -z \"$_line\" ] && [ -z \"$_extra\" ]; then printf 'exact-single-enter\\n'; " +
		"else printf 'unexpected: line=%s extra=%s\\n' \"$(printf '%s' \"$_line\" | cat -v)\" \"$(printf '%s' \"$_extra\" | cat -v)\"; fi\n"
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
	// reason (codex finding on this file).
	time.Sleep(400 * time.Millisecond)
	after, err := tm.CapturePane(sessionName, 30)
	if err != nil {
		t.Fatalf("CapturePane (after): %v", err)
	}
	if !strings.Contains(after, "exact-single-enter") {
		t.Errorf("dismiss did not send exactly one Enter and nothing else\npane: %q", after)
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
	// see the trust dialog test above for why.
	script := "#!/bin/bash\n" +
		"clear; printf '%s\\n' 'Bypass Permissions mode'\n" +
		"printf '%s\\n' '1. No'\n" +
		"printf '%s\\n' '2. Yes, I accept'\n" +
		"IFS= read -r _line\n" +
		"IFS= read -rsn1 -t 0.2 _extra\n" +
		"clear\n" +
		"printf '%s' \"$_line\" | cat -v\n" +
		"printf ':end:'\n" +
		"if [ -n \"$_extra\" ]; then printf 'stray-key'; else printf 'clean'; fi\n" +
		"printf '\\n'\n"
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

	time.Sleep(400 * time.Millisecond)
	after, err := tm.CapturePane(sessionName, 30)
	if err != nil {
		t.Fatalf("CapturePane (after): %v", err)
	}
	if !strings.Contains(after, "^[[B:end:clean") {
		t.Errorf("dismiss did not send exactly Down then Enter and nothing else\npane: %q", after)
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
	script := "#!/bin/bash\n" +
		"clear; printf '%s\\n' 'Choose the text style that looks best with your terminal:'\n" +
		"printf '%s\\n' '1. Dark mode'\n" +
		"printf '%s\\n' '2. Light mode'\n" +
		"IFS= read -r _line\n" +
		"IFS= read -rsn1 -t 0.2 _extra\n" +
		"clear\n" +
		"if [ -z \"$_line\" ] && [ -z \"$_extra\" ]; then printf 'exact-single-enter\\n'; " +
		"else printf 'unexpected: line=%s extra=%s\\n' \"$(printf '%s' \"$_line\" | cat -v)\" \"$(printf '%s' \"$_extra\" | cat -v)\"; fi\n"
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

	time.Sleep(400 * time.Millisecond)
	after, err := tm.CapturePane(sessionName, 30)
	if err != nil {
		t.Fatalf("CapturePane (after): %v", err)
	}
	if !strings.Contains(after, "exact-single-enter") {
		t.Errorf("dismiss did not send exactly one Enter and nothing else\npane: %q", after)
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
		"IFS= read -rsn2 -t 0.2 _rest\n" +
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
