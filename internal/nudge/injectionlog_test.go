package nudge

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// TestLogInjectionError covers hq-g52db fix 1: every injection failure must
// be written to a durable per-session log with its pane capture, since the
// poller discards its own stderr (buildPollerCommand sets it to nil) and
// previously the actual error was never recorded anywhere.
func TestLogInjectionError(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-log"

	err := LogInjectionError(townRoot, session, "nudge-poller", errors.New("composer dirty"), "❯ some stray text")
	if err != nil {
		t.Fatalf("LogInjectionError: %v", err)
	}

	path := injectionLogPath(townRoot, session)
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("reading log file: %v", readErr)
	}
	content := string(data)
	if !strings.Contains(content, "nudge-poller") {
		t.Error("log missing source")
	}
	if !strings.Contains(content, "composer dirty") {
		t.Error("log missing error text")
	}
	if !strings.Contains(content, "❯ some stray text") {
		t.Error("log missing pane capture")
	}
}

// TestLogInjectionErrorAppends verifies the log is append-only: a poller
// runs for a long time and must accumulate a history, not overwrite it on
// each failure.
func TestLogInjectionErrorAppends(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-log-append"

	if err := LogInjectionError(townRoot, session, "nudge-poller", errors.New("first failure"), ""); err != nil {
		t.Fatalf("LogInjectionError 1: %v", err)
	}
	if err := LogInjectionError(townRoot, session, "nudge-poller", errors.New("second failure"), ""); err != nil {
		t.Fatalf("LogInjectionError 2: %v", err)
	}

	data, err := os.ReadFile(injectionLogPath(townRoot, session))
	if err != nil {
		t.Fatalf("reading log file: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "first failure") || !strings.Contains(content, "second failure") {
		t.Errorf("log should contain both entries, got:\n%s", content)
	}
}

func TestInjectionLogPathSanitizesSession(t *testing.T) {
	got := injectionLogPath("/town", "some/session")
	if strings.Contains(strings.TrimPrefix(got, "/town/.runtime/nudge_poller/"), "/") {
		t.Errorf("injectionLogPath(%q) = %q, want slashes replaced with underscores", "some/session", got)
	}
}
