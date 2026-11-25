package cmd

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

func TestShouldSkipDrainUntilIdle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		hasPromptDetection bool
		waitErr            error
		want               bool
	}{
		{"prompt aware idle", true, nil, false},
		{"prompt aware busy", true, errors.New("timeout"), true},
		{"no prompt detection busy", false, errors.New("timeout"), false},
		{"no prompt detection idle", false, nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldSkipDrainUntilIdle(tt.hasPromptDetection, tt.waitErr); got != tt.want {
				t.Errorf("shouldSkipDrainUntilIdle(%v, %v) = %v, want %v", tt.hasPromptDetection, tt.waitErr, got, tt.want)
			}
		})
	}
}

func TestResolveNudgePollerAgentOptionsCustomAlias(t *testing.T) {
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "testrig")
	if err := config.SaveRigSettings(config.RigSettingsPath(rigPath), &config.RigSettings{
		Type: "rig-settings", Version: config.CurrentRigSettingsVersion,
		Agents: map[string]*config.RuntimeConfig{"claude-helm": {Command: "claude"}},
	}); err != nil {
		t.Fatal(err)
	}

	hasPromptDetection, opts := resolveNudgePollerAgentOptions("claude-helm", townRoot, rigPath)
	if !hasPromptDetection {
		t.Fatal("custom Claude alias should enable prompt detection")
	}
	if opts.SkipEscape {
		t.Fatal("Claude alias should not skip Escape")
	}
	if !shouldSkipDrainUntilIdle(hasPromptDetection, errors.New("busy pane")) {
		t.Fatal("busy pane should leave the nudge queued")
	}
	if shouldSkipDrainUntilIdle(hasPromptDetection, nil) {
		t.Fatal("idle pane should allow the queued nudge to drain")
	}
}

func TestResolveNudgePollerAgentOptionsUnknownAlias(t *testing.T) {
	hasPromptDetection, opts := resolveNudgePollerAgentOptions("unknown-agent", t.TempDir(), "")
	if hasPromptDetection {
		t.Fatal("unknown alias should preserve best-effort behavior")
	}
	if opts.SkipEscape {
		t.Fatal("unknown alias should use default nudge options")
	}
	if shouldSkipDrainUntilIdle(hasPromptDetection, errors.New("busy pane")) {
		t.Fatal("unknown alias should preserve existing busy-pane behavior")
	}
}
