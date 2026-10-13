package cmd

import (
	"strings"
	"testing"
)

// Serving without an instance password must fail fast with a
// human-readable error, before any listener is opened.
func TestServeRequiresInstancePassword(t *testing.T) {
	cmd := &ServeCmd{}
	err := cmd.Run(&Deps{Version: "test"})
	if err == nil {
		t.Fatal("want error without password")
	}
	if !strings.Contains(err.Error(), "password") {
		t.Fatalf("error should mention the password: %v", err)
	}
}

func TestResolveStateDirFlagWins(t *testing.T) {
	dir, err := resolveStateDir("/tmp/custom-state")
	if err != nil {
		t.Fatal(err)
	}
	if dir != "/tmp/custom-state" {
		t.Fatalf("dir = %q", dir)
	}
}

func TestResolveStateDirDefaultUnderUserState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/xdg-state")
	dir, err := resolveStateDir("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, "/tmp/xdg-state/") || !strings.HasSuffix(dir, appName) {
		t.Fatalf("dir = %q, want under XDG_STATE_HOME/%s", dir, appName)
	}
}
