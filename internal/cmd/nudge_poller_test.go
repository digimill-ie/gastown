package cmd

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/tmux"
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

	const session = "testrig-crew-helm"
	if err := nudge.Enqueue(townRoot, session, nudge.QueuedNudge{Sender: "test", Message: "hello"}); err != nil {
		t.Fatal(err)
	}

	hasPromptDetection, promptPrefix, opts := resolveNudgePollerAgentOptions("claude-helm", townRoot, rigPath)
	if !hasPromptDetection {
		t.Fatal("custom Claude alias should enable prompt detection")
	}
	if promptPrefix != config.GetAgentPresetByName("claude").ReadyPromptPrefix {
		t.Fatalf("custom Claude alias prompt prefix = %q, want Claude preset prefix", promptPrefix)
	}
	if opts.SkipEscape {
		t.Fatal("Claude alias should not skip Escape")
	}
	if !shouldSkipDrainUntilIdle(hasPromptDetection, errors.New("busy pane")) {
		t.Fatal("busy pane should leave the nudge queued")
	}
	if pending, err := nudge.Pending(townRoot, session); err != nil || pending != 1 {
		t.Fatalf("busy pane pending nudges = %d, err %v; want 1, nil", pending, err)
	}
	if shouldSkipDrainUntilIdle(hasPromptDetection, nil) {
		t.Fatal("idle pane should allow the queued nudge to drain")
	}
	drained, err := nudge.Drain(townRoot, session)
	if err != nil {
		t.Fatal(err)
	}
	if len(drained) != 1 {
		t.Fatalf("idle pane drained %d nudges, want 1", len(drained))
	}
	drained, err = nudge.Drain(townRoot, session)
	if err != nil {
		t.Fatal(err)
	}
	if len(drained) != 0 {
		t.Fatalf("second idle drain delivered %d nudges, want 0", len(drained))
	}
}

func TestResolveNudgePollerAgentOptionsUnknownAlias(t *testing.T) {
	hasPromptDetection, promptPrefix, opts := resolveNudgePollerAgentOptions("unknown-agent", t.TempDir(), "")
	if hasPromptDetection {
		t.Fatal("unknown alias should preserve best-effort behavior")
	}
	if promptPrefix != tmux.DefaultReadyPromptPrefix {
		t.Fatalf("unknown alias prompt prefix = %q, want default %q", promptPrefix, tmux.DefaultReadyPromptPrefix)
	}
	if opts.SkipEscape {
		t.Fatal("unknown alias should use default nudge options")
	}
	if shouldSkipDrainUntilIdle(hasPromptDetection, errors.New("busy pane")) {
		t.Fatal("unknown alias should preserve existing busy-pane behavior")
	}
}
