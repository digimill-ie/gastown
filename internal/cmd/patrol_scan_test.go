package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/witness"
)

type progressDiagnostics struct {
	bytes.Buffer
	sawProgress chan struct{}
	once        sync.Once
}

func (d *progressDiagnostics) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "still running") {
		d.once.Do(func() { close(d.sawProgress) })
	}
	return d.Buffer.Write(p)
}

func TestPatrolScanOutputJSON(t *testing.T) {
	output := PatrolScanOutput{
		Rig:       "gastown",
		Timestamp: "2026-03-17T12:00:00Z",
		Zombies: &PatrolScanZombieOutput{
			Checked: 3,
			Found:   1,
			Zombies: []PatrolScanZombieItem{
				{
					Polecat:        "alpha",
					Classification: "session-dead-active",
					AgentState:     "working",
					HookBead:       "gas-abc",
					Action:         "restarted",
					WasActive:      true,
				},
			},
		},
		Receipts: []witness.PatrolReceipt{
			{
				Rig:               "gastown",
				Polecat:           "alpha",
				Verdict:           witness.PatrolVerdictStale,
				RecommendedAction: "restarted",
				Evidence: witness.PatrolReceiptEvidence{
					AgentState:     "working",
					Classification: witness.ZombieSessionDeadActive,
					HookBead:       "gas-abc",
				},
			},
		},
	}

	data, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("failed to marshal output: %v", err)
	}

	var parsed PatrolScanOutput
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal output: %v", err)
	}

	if parsed.Rig != "gastown" {
		t.Errorf("Rig = %q, want %q", parsed.Rig, "gastown")
	}
	if parsed.Zombies.Found != 1 {
		t.Errorf("Zombies.Found = %d, want 1", parsed.Zombies.Found)
	}
	if parsed.Zombies.Checked != 3 {
		t.Errorf("Zombies.Checked = %d, want 3", parsed.Zombies.Checked)
	}
	if len(parsed.Zombies.Zombies) != 1 {
		t.Fatalf("len(Zombies) = %d, want 1", len(parsed.Zombies.Zombies))
	}
	z := parsed.Zombies.Zombies[0]
	if z.Polecat != "alpha" {
		t.Errorf("zombie Polecat = %q, want %q", z.Polecat, "alpha")
	}
	if z.Classification != "session-dead-active" {
		t.Errorf("zombie Classification = %q, want %q", z.Classification, "session-dead-active")
	}
	if !z.WasActive {
		t.Error("zombie WasActive = false, want true")
	}
	if len(parsed.Receipts) != 1 {
		t.Fatalf("len(Receipts) = %d, want 1", len(parsed.Receipts))
	}
	if parsed.Receipts[0].Verdict != witness.PatrolVerdictStale {
		t.Errorf("receipt Verdict = %q, want %q", parsed.Receipts[0].Verdict, witness.PatrolVerdictStale)
	}
}

func TestCountActiveWorkZombies(t *testing.T) {
	result := &witness.DetectZombiePolecatsResult{
		Zombies: []witness.ZombieResult{
			{PolecatName: "alpha", WasActive: true},
			{PolecatName: "beta", WasActive: false},
			{PolecatName: "gamma", WasActive: true},
		},
	}

	got := countActiveWorkZombies(result)
	if got != 2 {
		t.Errorf("countActiveWorkZombies() = %d, want 2", got)
	}
}

func TestCountActiveWorkZombies_Empty(t *testing.T) {
	result := &witness.DetectZombiePolecatsResult{}
	got := countActiveWorkZombies(result)
	if got != 0 {
		t.Errorf("countActiveWorkZombies() = %d, want 0", got)
	}
}

func TestRunPatrolScanPhaseEmitsProgressDiagnostics(t *testing.T) {
	oldInterval := patrolScanProgressInterval
	patrolScanProgressInterval = 10 * time.Millisecond
	defer func() { patrolScanProgressInterval = oldInterval }()

	diagnostics := &progressDiagnostics{sawProgress: make(chan struct{})}
	release := make(chan struct{})
	go func() {
		select {
		case <-diagnostics.sawProgress:
		case <-time.After(time.Second):
		}
		close(release)
	}()

	got := runPatrolScanPhase(diagnostics, "slow phase", func() string {
		<-release
		return "ok"
	})

	if got != "ok" {
		t.Fatalf("runPatrolScanPhase result = %q, want ok", got)
	}

	output := diagnostics.String()
	select {
	case <-diagnostics.sawProgress:
	default:
		t.Fatalf("diagnostics %q never emitted progress", output)
	}
	for _, want := range []string{
		"gt patrol scan: starting slow phase",
		"gt patrol scan: still running slow phase after",
		"gt patrol scan: finished slow phase in",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("diagnostics %q missing %q", output, want)
		}
	}
}

func TestRunPatrolScanPhaseZeroIntervalSkipsProgressTicks(t *testing.T) {
	oldInterval := patrolScanProgressInterval
	patrolScanProgressInterval = 0
	defer func() { patrolScanProgressInterval = oldInterval }()

	var diagnostics bytes.Buffer
	got := runPatrolScanPhase(&diagnostics, "fast phase", func() int {
		return 42
	})

	if got != 42 {
		t.Fatalf("runPatrolScanPhase result = %d, want 42", got)
	}

	output := diagnostics.String()
	if strings.Contains(output, "still running") {
		t.Fatalf("diagnostics should not include progress tick when interval is disabled: %q", output)
	}
	for _, want := range []string{
		"gt patrol scan: starting fast phase",
		"gt patrol scan: finished fast phase in",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("diagnostics %q missing %q", output, want)
		}
	}
}

func TestPatrolScanZombieItemSerialization(t *testing.T) {
	item := PatrolScanZombieItem{
		Polecat:        "obsidian",
		Classification: "agent-dead-in-session",
		AgentState:     "working",
		HookBead:       "gas-xyz",
		CleanupStatus:  "has_uncommitted",
		Action:         "restarted-dirty (cleanup_status=has_uncommitted, wisp=gas-wisp-123)",
		WasActive:      true,
		Error:          "restart failed: tmux error",
	}

	data, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("failed to marshal item: %v", err)
	}

	var parsed PatrolScanZombieItem
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal item: %v", err)
	}

	if parsed.Polecat != "obsidian" {
		t.Errorf("Polecat = %q, want %q", parsed.Polecat, "obsidian")
	}
	if parsed.CleanupStatus != "has_uncommitted" {
		t.Errorf("CleanupStatus = %q, want %q", parsed.CleanupStatus, "has_uncommitted")
	}
	if parsed.Error != "restart failed: tmux error" {
		t.Errorf("Error = %q, want %q", parsed.Error, "restart failed: tmux error")
	}
}

// TestPatrolScanDryRunFlag verifies --dry-run is registered, defaults to
// false, and is wired to the same patrolScanDryRun variable runPatrolScan
// reads to skip zombie/completion mutations and all notifications (gtn-k43 /
// hq-ooijo: a witness must be able to survey without acting).
func TestPatrolScanDryRunFlag(t *testing.T) {
	flag := patrolScanCmd.Flags().Lookup("dry-run")
	if flag == nil {
		t.Fatal("expected --dry-run flag to be registered on `gt patrol scan`")
	}
	if flag.DefValue != "false" {
		t.Errorf("--dry-run default = %q, want %q", flag.DefValue, "false")
	}

	old := patrolScanDryRun
	defer func() { patrolScanDryRun = old }()

	if err := flag.Value.Set("true"); err != nil {
		t.Fatalf("setting --dry-run: %v", err)
	}
	if !patrolScanDryRun {
		t.Error("patrolScanDryRun = false after setting --dry-run=true")
	}
}

// TestPatrolScan_DryRun_NeverSendsKeys actually RUNS the scan through
// runPatrolScan (the CLI entry point), not just the underlying library call —
// TestPatrolScanDryRunFlag above only checks flag wiring, so removing the
// zombie/completion/notification guards inside runPatrolScan would leave it
// green (codex finding on this file, gtn-m7s / hq-ooijo revision 2). Both
// polarities against a real tmux dialog: --dry-run leaves it standing;
// without it, the identical setup dismisses it.
func TestPatrolScan_DryRun_NeverSendsKeys(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	// NOTE: cannot use t.Parallel() — mutates cwd, env, and package-level
	// patrolScan* flag variables.

	newFixture := func(t *testing.T) (townRoot, rigName, sessionName string, tm *tmux.Tmux) {
		t.Helper()
		townRoot = t.TempDir()
		rigName = "testrig"
		polecatName := "alpha"

		if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(townRoot, rigName, "polecats", polecatName), 0o755); err != nil {
			t.Fatal(err)
		}

		// A private, test-unique tmux socket — never the inherited
		// GT_TMUX_SOCKET, which on a real gastown host can point at the
		// live town server (stall_detection_test.go finding, gtn-m7s).
		t.Setenv("GT_TMUX_SOCKET", "")

		if err := session.InitRegistry(townRoot); err != nil {
			t.Logf("session.InitRegistry (non-fatal for a bare temp town): %v", err)
		}
		sessionName = session.PolecatSessionName(session.PrefixFor(rigName), polecatName)

		tm = tmux.NewTmux()
		if alive, _ := tm.HasSession(sessionName); alive {
			t.Fatalf("refusing to reuse pre-existing tmux session %q", sessionName)
		}
		if err := tm.NewSession(sessionName, ""); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		t.Cleanup(func() { _ = tm.KillSession(sessionName) })
		if err := tm.SetEnvironment(sessionName, "GT_PROCESS_NAMES", "zsh,bash,sh"); err != nil {
			t.Fatalf("SetEnvironment GT_PROCESS_NAMES: %v", err)
		}

		// `read` keeps the dialog text as the pane's last content
		// indefinitely, so a session old enough to be considered for stall
		// detection still shows it (no stall-threshold config here — the
		// production default is 90s, so this fixture instead asserts on
		// the CLI *not sending keys to a currently-visible dialog*, which
		// only requires the pane content path, not session age).
		if err := tm.SendKeys(sessionName, "clear; printf '%s\\n' 'Quick safety check - do you trust this folder?'; read -r _dlg; clear; echo dialog-dismissed"); err != nil {
			t.Fatalf("SendKeys: %v", err)
		}
		time.Sleep(400 * time.Millisecond)

		return townRoot, rigName, sessionName, tm
	}

	runScan := func(t *testing.T, townRoot, rigName string, dryRun bool) {
		t.Helper()
		origDir, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chdir(townRoot); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chdir(origDir) })

		origRig, origDryRun, origJSON := patrolScanRig, patrolScanDryRun, patrolScanJSON
		patrolScanRig, patrolScanDryRun, patrolScanJSON = rigName, dryRun, false
		t.Cleanup(func() { patrolScanRig, patrolScanDryRun, patrolScanJSON = origRig, origDryRun, origJSON })

		if err := runPatrolScan(patrolScanCmd, nil); err != nil {
			t.Fatalf("runPatrolScan (dryRun=%v): %v", dryRun, err)
		}
	}

	t.Run("dry-run leaves the dialog standing", func(t *testing.T) {
		townRoot, rigName, sessionName, tm := newFixture(t)

		// Give the fast test settle time; stall threshold is production
		// default (90s) here, so classification alone (not the age gate)
		// is what this asserts — the config write below shrinks it so the
		// scan actually reaches ClassifyVisibleDialog.
		writeZeroStallThresholds(t, townRoot)

		runScan(t, townRoot, rigName, true)

		kind, err := tm.ClassifyVisibleDialog(sessionName)
		if err != nil {
			t.Fatalf("ClassifyVisibleDialog: %v", err)
		}
		if kind != tmux.DialogWorkspaceTrust {
			t.Errorf("dialog no longer visible after `gt patrol scan --dry-run` — a key was sent (kind=%q)", kind)
		}
	})

	t.Run("without dry-run, the identical setup dismisses it (control)", func(t *testing.T) {
		townRoot, rigName, sessionName, tm := newFixture(t)
		writeZeroStallThresholds(t, townRoot)

		runScan(t, townRoot, rigName, false)

		kind, err := tm.ClassifyVisibleDialog(sessionName)
		if err != nil {
			t.Fatalf("ClassifyVisibleDialog: %v", err)
		}
		if kind != tmux.DialogNone {
			t.Errorf("dialog still visible after `gt patrol scan` (no --dry-run): %q — this control proves the dry-run assertion above tests something real", kind)
		}
	})
}

// writeZeroStallThresholds writes operational config with zero stall/activity
// thresholds so a freshly-created test session is immediately eligible for
// stall classification instead of waiting out the 90s/60s production defaults.
func writeZeroStallThresholds(t *testing.T, townRoot string) {
	t.Helper()
	settingsDir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"operational":{"witness":{"startup_stall_threshold":"0s","startup_activity_grace":"0s"}}}`
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPatrolScan_DryRun_SkipsZombieAndCompletionPhases closes the gap codex
// found in the dry-run coverage above: TestPatrolScan_DryRun_NeverSendsKeys
// only exercises the stall/dialog path (DetectStalledPolecats runs
// unconditionally and is itself dryRun-aware), so it cannot fail if the
// `if !patrolScanDryRun` guard around zombie detection, completion discovery,
// or the zombie notification were removed — those phases have their own,
// separate mutation-heavy paths (reap/restart, bead/mail routing) that
// TestPatrolScanDryRunFlag (flag wiring only) also does not reach (codex
// finding, patrol_scan_test.go:355). Rather than construct a live zombie or
// completion fixture, this asserts on the phase's own start/skip diagnostic
// line — a direct read of which branch actually ran, not an inference from
// its absence of side effects.
func TestPatrolScan_DryRun_SkipsZombieAndCompletionPhases(t *testing.T) {
	newEmptyRigFixture := func(t *testing.T) (townRoot, rigName string) {
		t.Helper()
		townRoot = t.TempDir()
		rigName = "testrig"
		if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Empty polecats dir: DetectZombiePolecats and DiscoverCompletions
		// both os.ReadDir it and return immediately on an empty result, so
		// this never touches Dolt/bd — only whether the phase ran at all is
		// under test here, not what it finds.
		if err := os.MkdirAll(filepath.Join(townRoot, rigName, "polecats"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GT_TMUX_SOCKET", "")
		return townRoot, rigName
	}

	runScanCapturingDiagnostics := func(t *testing.T, townRoot, rigName string, dryRun bool) string {
		t.Helper()
		origDir, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chdir(townRoot); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chdir(origDir) })

		origRig, origDryRun, origJSON := patrolScanRig, patrolScanDryRun, patrolScanJSON
		patrolScanRig, patrolScanDryRun, patrolScanJSON = rigName, dryRun, false
		t.Cleanup(func() { patrolScanRig, patrolScanDryRun, patrolScanJSON = origRig, origDryRun, origJSON })

		var stderr bytes.Buffer
		patrolScanCmd.SetErr(&stderr)
		t.Cleanup(func() { patrolScanCmd.SetErr(nil) })

		if err := runPatrolScan(patrolScanCmd, nil); err != nil {
			t.Fatalf("runPatrolScan (dryRun=%v): %v", dryRun, err)
		}
		return stderr.String()
	}

	t.Run("dry-run skips both phases", func(t *testing.T) {
		townRoot, rigName := newEmptyRigFixture(t)
		diagnostics := runScanCapturingDiagnostics(t, townRoot, rigName, true)

		if !strings.Contains(diagnostics, "skipping zombie detection (--dry-run)") {
			t.Errorf("dry-run did not skip zombie detection\ndiagnostics: %s", diagnostics)
		}
		if !strings.Contains(diagnostics, "skipping completion discovery (--dry-run)") {
			t.Errorf("dry-run did not skip completion discovery\ndiagnostics: %s", diagnostics)
		}
		if strings.Contains(diagnostics, "starting zombie detection") {
			t.Errorf("dry-run started zombie detection anyway\ndiagnostics: %s", diagnostics)
		}
		if strings.Contains(diagnostics, "starting completion discovery") {
			t.Errorf("dry-run started completion discovery anyway\ndiagnostics: %s", diagnostics)
		}
	})

	t.Run("without dry-run, the identical setup runs both phases (control)", func(t *testing.T) {
		townRoot, rigName := newEmptyRigFixture(t)
		diagnostics := runScanCapturingDiagnostics(t, townRoot, rigName, false)

		if !strings.Contains(diagnostics, "starting zombie detection") {
			t.Errorf("zombie detection did not run without --dry-run — this control proves the skip assertions above test something real\ndiagnostics: %s", diagnostics)
		}
		if !strings.Contains(diagnostics, "starting completion discovery") {
			t.Errorf("completion discovery did not run without --dry-run\ndiagnostics: %s", diagnostics)
		}
	})
}
