// Package config holds the per-machine configuration written by `pier setup`.
// Path: ~/.config/pier/config.toml (TOML lib lands with the wizard build-out).
package config

import "time"

type Config struct {
	DefaultDriver string // "aws-ec2" | "gcp-gce"

	AWS *AWSConfig
	GCP *GCPConfig

	// Parking policy (wizard question; per-session flags override).
	IdleTimeout   time.Duration // 0 = never auto-park
	UnattendedCap time.Duration // 0 = no runaway cap; default 8h

	// WarmPool keeps N blank parked VMs per driver for ~35-45s creates.
	WarmPool int

	// Secrets manifest: files copied one-way into every session at create.
	// Defaults assembled by the wizard from what it detects on this machine:
	// ~/.codex/, Claude settings/CLAUDE.md/agents, gh token, and per-repo
	// .env* globs added at create time.
	Manifest []string
}

type AWSConfig struct {
	Profile      string
	Region       string
	InstanceType string // default t4g.medium
	DiskGiB      int    // default 40
	Subnet       string // optional: orgs without a default VPC
	BakedAMI     string // set by `pier bake`
}

type GCPConfig struct {
	Project     string
	Zone        string
	MachineType string // default e2-medium
	DiskGiB     int    // default 40
	BakedImage  string // set by `pier bake`
}
