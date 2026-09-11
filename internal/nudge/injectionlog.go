// injectionlog.go provides durable, append-only logging of nudge injection
// failures. Before this existed, the poller discarded its stderr entirely
// (buildPollerCommand sets Stdout/Stderr to nil), so an injection error was
// never recorded anywhere and the first cause of a stuck delivery could not
// be read from a real occurrence (hq-g52db). This is observability only —
// it does not change delivery, retry, or dead-letter behavior.
package nudge

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// injectionLogPath returns the durable per-session log path for injection
// errors. Path: <townRoot>/.runtime/nudge_poller/<session>.log
func injectionLogPath(townRoot, session string) string {
	safe := strings.ReplaceAll(session, "/", "_")
	return filepath.Join(townRoot, constants.DirRuntime, "nudge_poller", safe+".log")
}

// LogInjectionError appends a record of a failed injection attempt,
// including a pane capture, to the session's durable log. Best-effort: a
// logging failure is returned to the caller to report, but must never be
// allowed to block delivery or dead-lettering.
func LogInjectionError(townRoot, session, source string, deliverErr error, paneCapture string) error {
	path := injectionLogPath(townRoot, session)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating nudge-poller log dir: %w", err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("opening nudge-poller log: %w", err)
	}
	defer func() { _ = f.Close() }()

	var b strings.Builder
	fmt.Fprintf(&b, "=== %s source=%s error=%q\n", time.Now().UTC().Format(time.RFC3339Nano), source, deliverErr)
	if paneCapture != "" {
		b.WriteString("--- pane capture (last lines) ---\n")
		b.WriteString(paneCapture)
		if !strings.HasSuffix(paneCapture, "\n") {
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")

	if _, err := f.WriteString(b.String()); err != nil {
		return fmt.Errorf("writing nudge-poller log: %w", err)
	}
	return nil
}
