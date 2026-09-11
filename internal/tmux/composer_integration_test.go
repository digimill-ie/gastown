package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeComposerScript is a minimal Python program that renders a static
// Claude-Code-like composer line ("❯ <needle>") and never clears it on its
// own — simulating a stranded/dirty composer exactly as submitComposer
// would see one over a real tmux capture-pane.
//
// tmux's "C-j" key and "Enter" key transmit DIFFERENT bytes — C-j is LF
// (0x0A / "\n"), Enter is CR (0x0D / "\r") — but the prior version of this
// script treated them identically (`ch in ("\n", "\r")`), clearing on
// either with no distinguishing signal. That means this test could pass
// even if recoverStrandedComposer's C-j send were removed entirely: the
// composer would still clear from submitComposer's own leading
// sendEnterVerified Enter, or the retype's trailing Enter (codex,
// changes-requested at REVISION 3 — Medium: "StrandedComposerRecovered
// passes without C-j"). This version keys on the byte itself: only LF
// prints a "RECOVERED" marker line before the fresh empty prompt, so a
// test can assert that marker actually appeared — proving a keystroke
// shaped exactly like C-j, not an Enter, is what cleared the composer.
//
// This exists because submitComposer's classification (submit_verify.go)
// operates entirely on a real `tmux capture-pane` snapshot: testing it
// faithfully — not just the pure analyzeSubmission function on a synthetic
// string, which TestAnalyzeSubmission already covers — means giving it a
// REAL pane whose content we control precisely (codex,
// submit_verify_test.go:158, changes-requested at 08964387/95f841e6
// rework: prior tests in this area constructed synthetic errors/strings
// rather than exercising the production capture-pane path end to end).
const fakeComposerScript = `
import sys
sys.stdout.write("❯ " + sys.argv[1])
sys.stdout.flush()
while True:
    ch = sys.stdin.read(1)
    if not ch:
        break
    if ch == "\n":
        # tmux's "C-j" key transmits LF (0x0A) — the specific recovery
        # keystroke. Mark it distinctly so a test can tell it apart from
        # an ordinary Enter.
        sys.stdout.write("\nRECOVERED\n❯ \n")
        sys.stdout.flush()
    elif ch == "\r":
        # tmux's "Enter" key transmits CR (0x0D) — an ordinary submit.
        sys.stdout.write("\n❯ \n")
        sys.stdout.flush()
`

// fakeStaticComposerScript renders "❯ <needle>" once and never reacts to
// stdin at all — unlike fakeComposerScript, an Enter (or any other
// keystroke) sent to it changes nothing on screen. This simulates a
// composer that holds content belonging to someone/something else entirely
// (a concurrent unrelated write), which a real Enter sent looking for a
// DIFFERENT needle must not be able to clear.
const fakeStaticComposerScript = `
import sys
sys.stdout.write("❯ " + sys.argv[1])
sys.stdout.flush()
sys.stdin.read()
`

func hasPython3() bool {
	_, err := exec.LookPath("python3")
	return err == nil
}

// startFakeComposerSession launches a tmux session running the given fake
// composer script (fakeComposerScript or fakeStaticComposerScript) as its
// sole process (no shell), pre-rendering "❯ <needle>" as a static composer
// line. Returns the session name; the caller is responsible for killing it.
func startFakeComposerSession(t *testing.T, tm *Tmux, script, needle string) string {
	t.Helper()
	if !hasPython3() {
		t.Skip("python3 not installed")
	}

	scriptPath := filepath.Join(t.TempDir(), "fake_composer.py")
	if err := os.WriteFile(scriptPath, []byte(script), 0644); err != nil {
		t.Fatalf("writing fake composer script: %v", err)
	}

	sessionName := "gt-test-composer-" + fmt.Sprintf("%d", time.Now().UnixNano()%1000000)
	// respawn-pane hands this whole string to the user's shell, so the
	// needle argument must be quoted — otherwise a multi-word needle like
	// "an unrelated draft" is word-split into three argv entries and the
	// script only ever sees "an".
	cmd := fmt.Sprintf("python3 %s '%s'", scriptPath, needle)
	if err := tm.NewSessionWithCommand(sessionName, t.TempDir(), cmd); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillSession(sessionName) })

	// Let the interpreter start and print the initial stranded line.
	time.Sleep(400 * time.Millisecond)
	return sessionName
}

// TestSubmitComposer_DirtyStateHeldAfterEnter is one of the six tests
// REVISION 3 required and codex found NOT MET: a composer holding content
// that is neither the sent needle nor its dim/cleared form must be reported
// as dirty — and it must STAY reported as dirty after a real Enter is sent,
// not read as success just because Enter itself didn't error (codex,
// nudge_failure.go:76, changes-requested at 08964387/95f841e6 rework).
//
// The fake composer here never reacts to the "unrelated" needle we ask
// submitComposer to look for — it only clears when it independently sees a
// newline on stdin, which submitComposer's own sendEnterVerified step does
// send, so this exercises Enter genuinely being delivered. The rendered
// content is a STATIC pane, so the post-Enter clear is invisible to a probe
// still checking for a DIFFERENT needle — matching the case where a
// concurrent, unrelated write left the composer holding something else.
func TestSubmitComposer_DirtyStateHeldAfterEnter(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := startFakeComposerSession(t, tm, fakeStaticComposerScript, "an unrelated draft")

	err := tm.submitComposer(sessionName, "resume the patrol", DefaultReadyPromptPrefix, false)
	if err == nil {
		t.Fatal("submitComposer() = nil, want an error for a dirty composer")
	}
	if !errors.Is(err, ErrSubmitNotVerified) {
		t.Errorf("submitComposer() = %v, want wrapped ErrSubmitNotVerified", err)
	}
	if !errors.Is(err, ErrComposerDirty) {
		t.Errorf("submitComposer() = %v, want wrapped ErrComposerDirty", err)
	}
}

// TestSubmitComposer_StrandedComposerRecovered is the second of the six
// required tests: a stranded composer (the sent needle still visibly
// sitting there, un-dimmed) must be recoverable via the validated C-j path
// — recoverStrandedComposer sends C-j, observes the composer clear, retypes
// the message, and confirms it lands.
func TestSubmitComposer_StrandedComposerRecovered(t *testing.T) {
	tm := newTestTmux(t)
	const needle = "resume the patrol"
	sessionName := startFakeComposerSession(t, tm, fakeComposerScript, needle)

	// recoveryValidated=true: this is the runtime-validated path (the
	// production caller gates this on recoveryKeystrokesValidatedForSession,
	// tested separately).
	err := tm.submitComposer(sessionName, needle, DefaultReadyPromptPrefix, true)
	if err != nil {
		t.Fatalf("submitComposer() = %v, want nil (recovered via C-j)", err)
	}

	content, err := tm.CapturePane(sessionName, 10)
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
	// Prove recovery specifically used a C-j-shaped keystroke (a bare
	// newline with nothing typed first), not the retype's own eventual
	// Enter landing early — see fakeComposerScript's doc comment (codex,
	// changes-requested at REVISION 3 — Medium: this test previously
	// passed even when nothing recognizable as C-j was ever sent).
	if !strings.Contains(content, "RECOVERED") {
		t.Errorf("post-recovery pane = %q, want a RECOVERED marker proving a bare (C-j-shaped) keystroke cleared the composer", content)
	}
	// The fake composer must show the RETYPED needle after recovery
	// cleared it and recoverStrandedComposer sent it again.
	if !strings.Contains(content, needle) {
		t.Errorf("post-recovery pane = %q, want it to contain retyped needle %q", content, needle)
	}
}
