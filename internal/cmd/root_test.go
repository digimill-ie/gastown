package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
)

func TestCheckHelpFlag(t *testing.T) {
	// Create a test command
	testCmd := &cobra.Command{
		Use:   "test",
		Short: "Test command",
		Long:  "This is a test command for testing checkHelpFlag.",
	}

	tests := []struct {
		name        string
		args        []string
		wantHelped  bool
		description string
	}{
		{
			name:        "--help as first arg",
			args:        []string{"--help"},
			wantHelped:  true,
			description: "should show help when --help is first argument",
		},
		{
			name:        "-h as first arg",
			args:        []string{"-h"},
			wantHelped:  true,
			description: "should show help when -h is first argument",
		},
		{
			name:        "--help with other args after",
			args:        []string{"--help", "something"},
			wantHelped:  true,
			description: "should show help when --help is first, ignoring rest",
		},
		{
			name:        "no args",
			args:        []string{},
			wantHelped:  false,
			description: "should not show help with no args",
		},
		{
			name:        "regular args",
			args:        []string{"abc123", "--json"},
			wantHelped:  false,
			description: "should not show help with regular args",
		},
		{
			name:        "--help NOT first - false positive prevention",
			args:        []string{"-m", "--help"},
			wantHelped:  false,
			description: "should NOT show help when --help is not first (e.g., commit -m '--help')",
		},
		{
			name:        "-h NOT first - false positive prevention",
			args:        []string{"something", "-h"},
			wantHelped:  false,
			description: "should NOT show help when -h is not first",
		},
		{
			name:        "--help after -- separator",
			args:        []string{"--", "--help"},
			wantHelped:  false,
			description: "should NOT show help when --help is after -- (passed to underlying tool)",
		},
		{
			name:        "similar but not help flag",
			args:        []string{"--helper"},
			wantHelped:  false,
			description: "should not match --helper as help flag",
		},
		{
			name:        "help without dashes",
			args:        []string{"help"},
			wantHelped:  false,
			description: "should not match 'help' without dashes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			helped, err := checkHelpFlag(testCmd, tt.args)
			if err != nil {
				t.Errorf("checkHelpFlag() returned error: %v", err)
			}
			if helped != tt.wantHelped {
				t.Errorf("checkHelpFlag(%v) helped = %v, want %v (%s)",
					tt.args, helped, tt.wantHelped, tt.description)
			}
		})
	}
}

func TestCheckHelpFlag_EdgeCases(t *testing.T) {
	testCmd := &cobra.Command{
		Use:   "test",
		Short: "Test command",
	}

	// Test that we correctly handle edge cases that could cause panics or unexpected behavior
	t.Run("nil-like empty slice", func(t *testing.T) {
		var args []string
		helped, err := checkHelpFlag(testCmd, args)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if helped {
			t.Error("should not show help for nil/empty args")
		}
	})

	t.Run("single empty string arg", func(t *testing.T) {
		args := []string{""}
		helped, err := checkHelpFlag(testCmd, args)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if helped {
			t.Error("should not show help for empty string arg")
		}
	})
}

func TestPersistentPreRunLoadsAgentRegistry(t *testing.T) {
	// Regression test: persistentPreRun must load settings/agents.json so that
	// GetProcessNames (used by IsAgentAlive, daemon heartbeat, cleanup) respects
	// user-configured process_names overrides.
	//
	// Without this, NixOS users whose Claude binary is ".claude-unwrapped" get
	// their sessions killed every 3 minutes because the builtin preset only
	// lists ["node", "claude"].
	//
	// NOTE: cannot use t.Parallel() — mutates cwd and global agent registry.
	config.ResetRegistryForTesting()
	t.Cleanup(config.ResetRegistryForTesting)

	// Build a minimal fake town root with mayor/town.json (PrimaryMarker)
	// and settings/agents.json containing a process_names override.
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "settings"), 0755); err != nil {
		t.Fatal(err)
	}

	registry := config.AgentRegistry{
		Version: config.CurrentAgentRegistryVersion,
		Agents: map[string]*config.AgentPresetInfo{
			"claude": {
				Name:         "claude",
				Command:      "claude",
				Args:         []string{"--dangerously-skip-permissions"},
				ProcessNames: []string{"node", "claude", ".claude-unwrapped"},
			},
		},
	}
	data, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "settings", "agents.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	// cd into the fake town root so workspace.FindFromCwd() finds it.
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(townRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	// Run persistentPreRun (the function under test).
	cmd := &cobra.Command{Use: "version"}
	if err := persistentPreRun(cmd, nil); err != nil {
		t.Fatalf("persistentPreRun: %v", err)
	}

	// Verify GetProcessNames returns the override from settings/agents.json.
	got := config.GetProcessNames("claude")
	want := []string{"node", "claude", ".claude-unwrapped"}
	if len(got) != len(want) {
		t.Fatalf("GetProcessNames(claude) = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("GetProcessNames(claude)[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestPersistentPreRunMalformedAgentRegistry(t *testing.T) {
	// Verify that malformed settings/agents.json does not block persistentPreRun
	// and that the builtin defaults are preserved (graceful fallback).
	//
	// NOTE: cannot use t.Parallel() — mutates cwd and global agent registry.
	config.ResetRegistryForTesting()
	t.Cleanup(config.ResetRegistryForTesting)

	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "settings"), 0755); err != nil {
		t.Fatal(err)
	}
	// Write invalid JSON to settings/agents.json.
	if err := os.WriteFile(filepath.Join(townRoot, "settings", "agents.json"), []byte("{malformed"), 0644); err != nil {
		t.Fatal(err)
	}

	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(townRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	// persistentPreRun should succeed despite malformed agents.json.
	cmd := &cobra.Command{Use: "version"}
	if err := persistentPreRun(cmd, nil); err != nil {
		t.Fatalf("persistentPreRun should not fail on malformed agents.json: %v", err)
	}

	// Builtin defaults should still be in effect.
	got := config.GetProcessNames("claude")
	if len(got) < 2 || got[0] != "node" || got[1] != "claude" {
		t.Fatalf("GetProcessNames(claude) after malformed registry = %v, want builtin [node claude ...]", got)
	}
}

// TestIsDryRunInvocation covers both polarities of the generic --dry-run
// detector persistentPreRun uses to suppress shared mutation side effects
// (gtn-m7s / hq-ooijo revision 2): a command with no dry-run flag, one with
// it registered but false, and one with it true.
func TestIsDryRunInvocation(t *testing.T) {
	tests := []struct {
		name    string
		build   func() *cobra.Command
		wantDry bool
	}{
		{
			name: "no dry-run flag registered",
			build: func() *cobra.Command {
				return &cobra.Command{Use: "version"}
			},
			wantDry: false,
		},
		{
			name: "dry-run flag registered but false",
			build: func() *cobra.Command {
				cmd := &cobra.Command{Use: "scan"}
				cmd.Flags().Bool("dry-run", false, "")
				return cmd
			},
			wantDry: false,
		},
		{
			name: "dry-run flag registered and set true",
			build: func() *cobra.Command {
				cmd := &cobra.Command{Use: "scan"}
				cmd.Flags().Bool("dry-run", false, "")
				if err := cmd.Flags().Set("dry-run", "true"); err != nil {
					t.Fatal(err)
				}
				return cmd
			},
			wantDry: true,
		},
		{
			name:    "nil command",
			build:   func() *cobra.Command { return nil },
			wantDry: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDryRunInvocation(tt.build()); got != tt.wantDry {
				t.Errorf("isDryRunInvocation() = %v, want %v", got, tt.wantDry)
			}
		})
	}
}

// TestPersistentPreRunDryRunSuppressesSharedMutations proves that a command
// declaring --dry-run=true suppresses persistentPreRun's own mutations, not
// only its RunE logic: the rigs.json fallback copy (session.InitRegistry's
// write side effect) and this session's own heartbeat touch. Both
// polarities, run against identical fixtures: dry-run writes neither file;
// the same setup without dry-run writes both (gtn-m7s / hq-ooijo revision 2,
// codex finding on root.go:133,242 — "a dry-run flag must suppress ALL scan
// mutations").
//
// NOTE: cannot use t.Parallel() — mutates cwd, env, and global registries.
func TestPersistentPreRunDryRunSuppressesSharedMutations(t *testing.T) {
	config.ResetRegistryForTesting()
	t.Cleanup(config.ResetRegistryForTesting)

	// Bypass the darwin unsigned-binary guard so this test observes the
	// logic under test rather than an unrelated local-build artifact (the
	// same guard already makes TestPersistentPreRunLoadsAgentRegistry fail
	// under a plain `go test` on macOS — restored after the test).
	origBuild, origBuiltProperly := Build, BuiltProperly
	Build, BuiltProperly = "test", "test"
	t.Cleanup(func() { Build, BuiltProperly = origBuild, origBuiltProperly })

	newFixture := func(t *testing.T) string {
		t.Helper()
		townRoot := t.TempDir()
		if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}"), 0644); err != nil {
			t.Fatal(err)
		}
		// The canonical rigs.json is what BuildPrefixRegistryFromTown reads
		// before deciding to write the town-root fallback copy — without
		// this file present, the write-triggering branch is never reached
		// and the assertions below would pass vacuously either way.
		rigsJSON := `{"rigs":{"testrig":{"beads":{"prefix":"gtn"}}}}`
		if err := os.WriteFile(filepath.Join(townRoot, "mayor", "rigs.json"), []byte(rigsJSON), 0644); err != nil {
			t.Fatal(err)
		}
		origDir, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chdir(origDir) })
		if err := os.Chdir(townRoot); err != nil {
			t.Fatal(err)
		}
		return townRoot
	}

	fallbackRigsPath := func(townRoot string) string { return filepath.Join(townRoot, "rigs.json") }
	heartbeatPath := func(townRoot, sessionName string) string {
		return filepath.Join(townRoot, ".runtime", "heartbeats", sessionName+".json")
	}

	t.Run("dry-run writes neither file", func(t *testing.T) {
		townRoot := newFixture(t)
		t.Setenv("GT_SESSION", "gt-test-dryrun-session")
		t.Setenv("GT_ROLE", "polecat")

		cmd := &cobra.Command{Use: "scan"}
		cmd.Flags().Bool("dry-run", false, "")
		if err := cmd.Flags().Set("dry-run", "true"); err != nil {
			t.Fatal(err)
		}

		if err := persistentPreRun(cmd, nil); err != nil {
			t.Fatalf("persistentPreRun: %v", err)
		}

		if _, err := os.Stat(fallbackRigsPath(townRoot)); !os.IsNotExist(err) {
			t.Errorf("rigs.json fallback copy present after --dry-run (stat err=%v) — dry-run must write nothing", err)
		}
		if _, err := os.Stat(heartbeatPath(townRoot, "gt-test-dryrun-session")); !os.IsNotExist(err) {
			t.Errorf("heartbeat file present after --dry-run (stat err=%v) — dry-run must not touch the caller's own heartbeat", err)
		}
	})

	t.Run("without dry-run, the identical setup writes both (control)", func(t *testing.T) {
		townRoot := newFixture(t)
		t.Setenv("GT_SESSION", "gt-test-live-session")
		t.Setenv("GT_ROLE", "polecat")

		cmd := &cobra.Command{Use: "scan"}
		cmd.Flags().Bool("dry-run", false, "") // left false

		if err := persistentPreRun(cmd, nil); err != nil {
			t.Fatalf("persistentPreRun: %v", err)
		}

		if _, err := os.Stat(fallbackRigsPath(townRoot)); err != nil {
			t.Errorf("rigs.json fallback copy missing without --dry-run: %v — this control proves the dry-run assertions above test something real, not a vacuous fixture", err)
		}
		if _, err := os.Stat(heartbeatPath(townRoot, "gt-test-live-session")); err != nil {
			t.Errorf("heartbeat file missing without --dry-run: %v", err)
		}
	})
}
