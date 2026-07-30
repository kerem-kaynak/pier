package awsec2

import (
	"strings"
	"testing"
	"time"

	"github.com/kerem-kaynak/pier/internal/driver"
)

func TestSanitize(t *testing.T) {
	for in, want := range map[string]string{
		"fix-auth":          "fix-auth",
		"feat/big_Refactor": "feat-big-refactor",
		"//weird//":         "weird",
		"":                  "session",
	} {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDurConf(t *testing.T) {
	if got := durConf(0); got != "never" {
		t.Errorf("durConf(0) = %q, want never", got)
	}
	if got := durConf(30 * time.Minute); got != "30m0s" {
		t.Errorf("durConf(30m) = %q, want 30m0s", got)
	}
}

func TestRenderUserDataGuards(t *testing.T) {
	spec := driver.CreateSpec{Name: "feat/x", IdleTimeout: 30 * time.Minute}
	ud := renderUserData(spec, "ssh-ed25519 AAAA pier")
	if !strings.HasPrefix(ud, "#cloud-config\n") {
		t.Fatal("user-data must start with #cloud-config")
	}
	if !strings.Contains(ud, "hostname: feat-x") {
		t.Error("hostname not sanitized")
	}
	if !strings.Contains(ud, "idle_timeout=30m0s") {
		t.Error("supervisor conf not rendered")
	}
	// Every install step must be guarded so the same user-data is a fast
	// no-op on a baked AMI.
	for _, guard := range []string{"command -v tmux", "command -v node", "command -v gh",
		"command -v claude", "command -v codex", "grep -q 'pier/env'"} {
		if !strings.Contains(ud, guard) {
			t.Errorf("user-data missing idempotency guard %q", guard)
		}
	}
}
