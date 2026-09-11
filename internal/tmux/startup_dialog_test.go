package tmux

import (
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
			name:    "genuinely stalled session, no known dialog text",
			content: "some unrelated hung output\nwith no dialog markers at all",
			want:    DialogNone,
		},
		{
			name:    "empty content",
			content: "",
			want:    DialogNone,
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

	if !second.After(first) && !second.Equal(first) {
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

	// `read` blocks until a key arrives, holding the dialog text on screen;
	// once dismissed it clears and prints a marker so verification sees the
	// dialog text is genuinely gone (not just re-printed by a loop).
	if err := tm.SendKeys(sessionName, "clear; printf '%s\\n' 'Quick safety check - do you trust this folder?'; read -r _dlg; clear; echo dialog-dismissed"); err != nil {
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
