package cmd

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		Agents: map[string]*config.RuntimeConfig{
			"claude-helm":  {Command: "claude"},
			"codex-crew":   {Command: "codex", Tmux: &config.RuntimeTmuxConfig{ReadyPromptPrefix: "CUSTOM> "}},
			"copilot-crew": {Command: "copilot", Tmux: &config.RuntimeTmuxConfig{ReadyPromptPrefix: "CUSTOM> "}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	const session = "testrig-crew-helm"
	if err := nudge.Enqueue(townRoot, session, nudge.QueuedNudge{Sender: "test", Message: "hello"}); err != nil {
		t.Fatal(err)
	}

	options := resolveNudgePollerAgentOptions("claude-helm", townRoot, rigPath)
	if !options.hasPromptDetection {
		t.Fatal("custom Claude alias should enable prompt detection")
	}
	if options.readyPromptPrefix != config.GetAgentPresetByName("claude").ReadyPromptPrefix {
		t.Fatalf("custom Claude alias prompt prefix = %q, want Claude preset prefix", options.readyPromptPrefix)
	}
	if options.nudgeOpts.SkipEscape {
		t.Fatal("Claude alias should not skip Escape")
	}
	if !shouldSkipDrainUntilIdle(options.hasPromptDetection, errors.New("busy pane")) {
		t.Fatal("busy pane should leave the nudge queued")
	}
	if pending, err := nudge.Pending(townRoot, session); err != nil || pending != 1 {
		t.Fatalf("busy pane pending nudges = %d, err %v; want 1, nil", pending, err)
	}
	if shouldSkipDrainUntilIdle(options.hasPromptDetection, nil) {
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

func TestResolveNudgePollerAgentOptionsUsesExplicitAliasPrompt(t *testing.T) {
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "testrig")
	if err := config.SaveRigSettings(config.RigSettingsPath(rigPath), &config.RigSettings{
		Type: "rig-settings", Version: config.CurrentRigSettingsVersion,
		Agents: map[string]*config.RuntimeConfig{
			"codex-crew":      {Command: "codex", Tmux: &config.RuntimeTmuxConfig{ReadyPromptPrefix: "CUSTOM> "}},
			"copilot-crew":    {Command: "copilot", Tmux: &config.RuntimeTmuxConfig{ReadyPromptPrefix: "COPILOT> "}},
			"copilot-default": {Command: "copilot"},
			"codex":           {Command: "copilot"},
			"codex-prompt":    {Command: "copilot", Tmux: &config.RuntimeTmuxConfig{ReadyPromptPrefix: "OVERRIDE> "}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	options := resolveNudgePollerAgentOptions("codex-crew", townRoot, rigPath)
	if !options.hasPromptDetection || options.readyPromptPrefix != "CUSTOM> " {
		t.Fatalf("Codex alias = (%v, %q), want explicit custom prompt", options.hasPromptDetection, options.readyPromptPrefix)
	}

	options = resolveNudgePollerAgentOptions("copilot-crew", townRoot, rigPath)
	if !options.hasPromptDetection || options.readyPromptPrefix != "COPILOT> " {
		t.Fatalf("Copilot alias = (%v, %q), want explicit custom prompt", options.hasPromptDetection, options.readyPromptPrefix)
	}

	options = resolveNudgePollerAgentOptions("copilot-default", townRoot, rigPath)
	copilotPreset := config.GetAgentPresetByName("copilot")
	if copilotPreset == nil || copilotPreset.ReadyPromptPrefix != "" {
		t.Fatal("test requires Copilot built-in preset to have no prompt prefix")
	}
	if options.hasPromptDetection || options.readyPromptPrefix != tmux.DefaultReadyPromptPrefix {
		t.Fatalf("Copilot alias = (%v, %q), want no prompt detection and default fallback", options.hasPromptDetection, options.readyPromptPrefix)
	}
	if shouldSkipDrainUntilIdle(options.hasPromptDetection, errors.New("timeout")) {
		t.Fatal("agent without a built-in prompt should preserve best-effort draining")
	}

	options = resolveNudgePollerAgentOptions("codex", townRoot, rigPath)
	if options.hasPromptDetection || options.nudgeOpts.SkipEscape {
		t.Fatalf("custom codex alias = (%v, skip escape %v), want promptless Copilot behavior", options.hasPromptDetection, options.nudgeOpts.SkipEscape)
	}

	options = resolveNudgePollerAgentOptions("codex-prompt", townRoot, rigPath)
	if !options.hasPromptDetection || options.readyPromptPrefix != "OVERRIDE> " || options.nudgeOpts.SkipEscape {
		t.Fatalf("custom codex prompt alias = (%v, %q, skip escape %v), want explicit prompt and Copilot behavior", options.hasPromptDetection, options.readyPromptPrefix, options.nudgeOpts.SkipEscape)
	}

	options = resolveNudgePollerAgentOptions("codex", townRoot, filepath.Join(townRoot, "other-rig"))
	codexPreset := config.GetAgentPresetByName("codex")
	if codexPreset == nil {
		t.Fatal("test requires Codex built-in preset")
	}
	if !options.hasPromptDetection || options.readyPromptPrefix != codexPreset.ReadyPromptPrefix {
		t.Fatalf("built-in Codex = (%v, %q), want built-in prompt %q", options.hasPromptDetection, options.readyPromptPrefix, codexPreset.ReadyPromptPrefix)
	}
}

func TestResolveNudgePollerAgentOptionsForSessionRefreshesAgent(t *testing.T) {
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "testrig")
	if err := config.SaveRigSettings(config.RigSettingsPath(rigPath), &config.RigSettings{
		Type: "rig-settings", Version: config.CurrentRigSettingsVersion,
		Agents: map[string]*config.RuntimeConfig{
			"claude-helm":  {Command: "claude"},
			"copilot-crew": {Command: "copilot"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	currentAgent := "claude-helm"
	getEnvironment := func(key string) (string, error) {
		switch key {
		case "GT_AGENT":
			return currentAgent, nil
		case "GT_RIG":
			return "testrig", nil
		default:
			return "", errors.New("unexpected environment key")
		}
	}

	options := resolveNudgePollerAgentOptionsForSession(getEnvironment, townRoot)
	if !options.hasPromptDetection {
		t.Fatal("Claude alias should enable prompt detection on the first tick")
	}

	currentAgent = "copilot-crew"
	options = resolveNudgePollerAgentOptionsForSession(getEnvironment, townRoot)
	if options.hasPromptDetection || options.readyPromptPrefix != tmux.DefaultReadyPromptPrefix {
		t.Fatalf("updated alias = (%v, %q), want best-effort prompt fallback", options.hasPromptDetection, options.readyPromptPrefix)
	}
}

func TestResolveNudgePollerAgentOptionsUnknownAlias(t *testing.T) {
	options := resolveNudgePollerAgentOptions("unknown-agent", t.TempDir(), "")
	if options.hasPromptDetection {
		t.Fatal("unknown alias should preserve best-effort behavior")
	}
	if options.readyPromptPrefix != tmux.DefaultReadyPromptPrefix {
		t.Fatalf("unknown alias prompt prefix = %q, want default %q", options.readyPromptPrefix, tmux.DefaultReadyPromptPrefix)
	}
	if options.nudgeOpts.SkipEscape {
		t.Fatal("unknown alias should use default nudge options")
	}
	if shouldSkipDrainUntilIdle(options.hasPromptDetection, errors.New("busy pane")) {
		t.Fatal("unknown alias should preserve existing busy-pane behavior")
	}
}

type fakeNudgePollerTmux struct {
	agent         string
	rig           string
	waitErrors    []error
	waitPrefixes  []string
	injected      []string
	injectedNudge []tmux.NudgeOpts
}

func (f *fakeNudgePollerTmux) HasSession(string) (bool, error) { return true, nil }

func (f *fakeNudgePollerTmux) GetEnvironment(_ string, key string) (string, error) {
	switch key {
	case "GT_AGENT":
		return f.agent, nil
	case "GT_RIG":
		return f.rig, nil
	default:
		return "", errors.New("unexpected environment key")
	}
}

func (f *fakeNudgePollerTmux) WaitForIdleWithPromptPrefix(_ string, prefix string, _ time.Duration) error {
	f.waitPrefixes = append(f.waitPrefixes, prefix)
	index := len(f.waitPrefixes) - 1
	if index < len(f.waitErrors) {
		return f.waitErrors[index]
	}
	return nil
}

func (f *fakeNudgePollerTmux) NudgeSessionWithOpts(_ string, message string, opts tmux.NudgeOpts) error {
	f.injected = append(f.injected, message)
	f.injectedNudge = append(f.injectedNudge, opts)
	return nil
}

func TestProcessNudgePollTickUsesResolvedPromptBeforeDraining(t *testing.T) {
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "testrig")
	if err := config.SaveRigSettings(config.RigSettingsPath(rigPath), &config.RigSettings{
		Type: "rig-settings", Version: config.CurrentRigSettingsVersion,
		Agents: map[string]*config.RuntimeConfig{
			"claude-helm": {Command: "claude", Tmux: &config.RuntimeTmuxConfig{ReadyPromptPrefix: "CUSTOM> "}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	const session = "testrig-crew-helm"
	if err := nudge.Enqueue(townRoot, session, nudge.QueuedNudge{Sender: "test", Message: "hello"}); err != nil {
		t.Fatal(err)
	}
	tmuxFake := &fakeNudgePollerTmux{agent: "claude-helm", rig: "testrig", waitErrors: []error{errors.New("busy"), nil}}
	warned := make(map[string]struct{})

	if !processNudgePollTick(tmuxFake, filesystemNudgePollerQueue{}, session, townRoot, time.Second, warned) {
		t.Fatal("poll tick reported a live session as gone")
	}
	if len(tmuxFake.waitPrefixes) != 1 || tmuxFake.waitPrefixes[0] != "CUSTOM> " {
		t.Fatalf("idle wait prefixes = %q, want custom alias prefix", tmuxFake.waitPrefixes)
	}
	if pending, err := nudge.Pending(townRoot, session); err != nil || pending != 1 {
		t.Fatalf("busy tick pending = %d, err %v; want queued nudge retained", pending, err)
	}
	if len(tmuxFake.injected) != 0 {
		t.Fatalf("busy tick injected %d nudges, want none", len(tmuxFake.injected))
	}

	if !processNudgePollTick(tmuxFake, filesystemNudgePollerQueue{}, session, townRoot, time.Second, warned) {
		t.Fatal("second poll tick reported a live session as gone")
	}
	if pending, err := nudge.Pending(townRoot, session); err != nil || pending != 0 {
		t.Fatalf("idle tick pending = %d, err %v; want empty queue", pending, err)
	}
	if len(tmuxFake.injected) != 1 {
		t.Fatalf("idle tick injected %d nudges, want one", len(tmuxFake.injected))
	}
}

func TestWarnUnknownNudgePollerAgentOnce(t *testing.T) {
	var output bytes.Buffer
	warned := make(map[string]struct{})
	warnUnknownNudgePollerAgent("unknown-alias", warned, &output)
	warnUnknownNudgePollerAgent("unknown-alias", warned, &output)
	if got := strings.Count(output.String(), "could not resolve agent"); got != 1 {
		t.Fatalf("warning count = %d, want one; output: %q", got, output.String())
	}
}
