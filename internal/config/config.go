// Package config loads/saves ~/.config/pier/config.toml.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Driver        string  `toml:"driver"`
	IdleTimeout   string  `toml:"idle_timeout"`   // duration or "never"
	UnattendedCap string  `toml:"unattended_cap"` // duration or "never"
	AWS           AWS     `toml:"aws"`
	Secrets       Secrets `toml:"secrets"`
}

type AWS struct {
	Profile      string `toml:"profile"`
	Region       string `toml:"region"`
	InstanceType string `toml:"instance_type"`
	DiskGiB      int    `toml:"disk_gib"`
	Subnet       string `toml:"subnet"` // optional: orgs without a default VPC
	// BakedAMI is the legacy shared image (pre repo-specific bakes) — still
	// used as a fallback, deregistered and cleared by the next `pier bake`.
	BakedAMI string `toml:"baked_ami,omitempty"`
	// BakedAMIs: repo basename -> image, written by `pier bake` (run from the
	// repo). Each repo bakes its own image so .pier-bake.sh toolchains don't
	// bleed across projects.
	BakedAMIs map[string]string `toml:"baked_amis,omitempty"`
}

type Secrets struct {
	// Manifest: files/dirs under $HOME copied one-way into each session's
	// home at create. Repo files listed in a repo-root .pier-include travel
	// additionally (uncommitted tracked edits ride separately, as a patch).
	Manifest []string `toml:"manifest"`
	// ClaudeOAuthToken: from `claude setup-token` (macOS Keychain escape
	// hatch); injected as CLAUDE_CODE_OAUTH_TOKEN in sessions.
	ClaudeOAuthToken string `toml:"claude_oauth_token"`
}

func Default() Config {
	return Config{
		Driver:        "aws-ec2",
		IdleTimeout:   "30m",
		UnattendedCap: "8h",
		AWS: AWS{
			InstanceType: "t4g.medium",
			DiskGiB:      40,
		},
	}
}

func Path() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "pier", "config.toml")
}

func Dir() string { return filepath.Dir(Path()) }

func Load() (Config, error) {
	c := Default()
	if _, err := toml.DecodeFile(Path(), &c); err != nil {
		if os.IsNotExist(err) {
			return c, fmt.Errorf("no config at %s — run `pier setup` first", Path())
		}
		return c, err
	}
	return c, nil
}

func (c Config) Save() error {
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(Path(), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(c)
}

// ParkDuration parses "30m" / "8h" / "never" (or "0") into a duration;
// 0 means disabled.
func ParkDuration(s string) (time.Duration, error) {
	if s == "" || s == "never" || s == "0" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

// Setting is one field the TUI settings page can edit.
type Setting struct {
	Key  string
	Hint string
}

// Settings lists the settable keys in display order. Secrets and baked AMIs
// are deliberately absent: those are managed by `pier setup` and `pier bake`.
var Settings = []Setting{
	{"driver", ""},
	{"idle_timeout", "detached and quiet this long → park"},
	{"unattended_cap", "parks even while busy"},
	{"aws.profile", ""},
	{"aws.region", ""},
	{"aws.instance_type", "machine for new sessions"},
	{"aws.disk_gib", ""},
	{"aws.subnet", "optional"},
}

// Get returns the current value of a settable key ("" for unknown keys).
func Get(c Config, key string) string {
	switch key {
	case "driver":
		return c.Driver
	case "idle_timeout":
		return c.IdleTimeout
	case "unattended_cap":
		return c.UnattendedCap
	case "aws.profile":
		return c.AWS.Profile
	case "aws.region":
		return c.AWS.Region
	case "aws.instance_type":
		return c.AWS.InstanceType
	case "aws.disk_gib":
		return strconv.Itoa(c.AWS.DiskGiB)
	case "aws.subnet":
		return c.AWS.Subnet
	}
	return ""
}

// Set mutates one whitelisted scalar, validating durations and disk size.
// Changes apply to new sessions only.
func Set(c *Config, key, val string) error {
	switch key {
	case "driver":
		c.Driver = val
	case "idle_timeout", "unattended_cap":
		if _, err := ParkDuration(val); err != nil {
			return fmt.Errorf("%s: %v (want a duration like 30m or 8h, or never)", key, err)
		}
		if key == "idle_timeout" {
			c.IdleTimeout = val
		} else {
			c.UnattendedCap = val
		}
	case "aws.profile":
		c.AWS.Profile = val
	case "aws.region":
		c.AWS.Region = val
	case "aws.instance_type":
		c.AWS.InstanceType = val
	case "aws.disk_gib":
		n, err := strconv.Atoi(val)
		if err != nil || n < 8 {
			return fmt.Errorf("aws.disk_gib: want a whole number of GiB, at least 8 (got %q)", val)
		}
		c.AWS.DiskGiB = n
	case "aws.subnet":
		c.AWS.Subnet = val
	default:
		keys := make([]string, len(Settings))
		for i, s := range Settings {
			keys[i] = s.Key
		}
		return fmt.Errorf("unknown key %q — settable: %s", key, strings.Join(keys, ", "))
	}
	return nil
}
