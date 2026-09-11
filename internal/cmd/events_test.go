package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/session"
)

func TestResolveEventStreamTarget_RoleShortcuts(t *testing.T) {
	tests := []struct {
		name   string
		target string
		want   string
	}{
		{"mayor", constants.RoleMayor, session.MayorSessionName()},
		{"deacon", constants.RoleDeacon, session.DeaconSessionName()},
		{"literal session name passes through", "gt-gastown-witness", "gt-gastown-witness"},
		{"agent address passes through unresolved", "gastown/witness", "gastown/witness"},
		{"trailing slash is trimmed", "mayor/", session.MayorSessionName()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveEventStreamTarget(tt.target)
			if err != nil {
				t.Fatalf("resolveEventStreamTarget(%q): %v", tt.target, err)
			}
			if got != tt.want {
				t.Errorf("resolveEventStreamTarget(%q) = %q, want %q", tt.target, got, tt.want)
			}
		})
	}
}
