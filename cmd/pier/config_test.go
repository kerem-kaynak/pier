package main

import (
	"strings"
	"testing"

	"github.com/kerem-kaynak/pier/internal/config"
)

func TestApplyConfigSet(t *testing.T) {
	cfg := config.Default()

	if err := applyConfigSet(&cfg, "aws.instance_type", "t4g.xlarge"); err != nil {
		t.Fatal(err)
	}
	if cfg.AWS.InstanceType != "t4g.xlarge" {
		t.Errorf("instance_type = %q", cfg.AWS.InstanceType)
	}

	if err := applyConfigSet(&cfg, "idle_timeout", "never"); err != nil {
		t.Fatal(err)
	}
	if err := applyConfigSet(&cfg, "idle_timeout", "not-a-duration"); err == nil {
		t.Error("bad duration must not apply")
	}

	if err := applyConfigSet(&cfg, "aws.disk_gib", "7"); err == nil {
		t.Error("disk below 8 GiB must not apply")
	}
	if err := applyConfigSet(&cfg, "aws.disk_gib", "80"); err != nil {
		t.Fatal(err)
	}
	if cfg.AWS.DiskGiB != 80 {
		t.Errorf("disk_gib = %d", cfg.AWS.DiskGiB)
	}

	// Secrets stay out of the set path: the error must name the valid keys.
	err := applyConfigSet(&cfg, "secrets.claude_oauth_token", "x")
	if err == nil || !strings.Contains(err.Error(), "settable:") {
		t.Errorf("secret key must be rejected with the settable list, got %v", err)
	}
}
