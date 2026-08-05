// Package driver defines the contract every cloud driver implements.
//
// Design invariants (see docs/SPEC.md):
//   - No cloud credentials ever live inside a session VM. Self-park is the VM
//     shutting itself down; everything else is client-initiated.
//   - Session state is derived from provider APIs + tags/labels. No database,
//     no server, no background process on the laptop.
//   - The caller's cloud identity (STS ARN / GCP principal) namespaces every
//     resource, so many devs share one account without collisions.
package driver

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type State string

const (
	StateCreating State = "creating" // provisioning / first boot
	StateRunning  State = "running"  // reachable, a client is attached
	StateWorking  State = "working"  // reachable, detached, agent actively working
	StateIdle     State = "idle"     // reachable, quiet; park countdown running
	StateParked   State = "parked"   // instance stopped, disk persists
	StateDead     State = "dead"     // terminated/crashed outside our control
)

// Session is one instance + its persistent disk.
type Session struct {
	ID           string // provider instance ID
	Name         string // branch-derived session name
	Repo         string
	Branch       string
	User         string // derived from cloud caller identity, never configured
	Driver       string
	State        State
	Strained     bool      // sustained cpu/mem pressure (supervisor beacon) — resize hint
	Setup        string    // repo setup script: "" | "running" | "failed" (supervisor beacon)
	InstanceType string    // provider machine type; feeds the TUI resize picker
	Created      time.Time // session creation, not last boot — AGE must never go backward
	CostNote     string    // honest money: "$3/mo parked", "$0.03/h running"
}

// Machine is one row in the TUI's resize picker: a curated instance type
// with its shape and rough on-demand cost, so nobody memorizes type names.
type Machine struct {
	Type string
	CPU  string // vCPUs
	Mem  string // GiB
	Cost string
}

// CreateSpec describes a new session. Create returns as soon as the instance
// is attach-ready; code push and .pier-setup.sh continue asynchronously in
// the session (the async create pipeline) with steps streamed via Progress.
type CreateSpec struct {
	Name          string
	Repo          string        // local repo root; the bundle source
	Branch        string        // new branch, off BaseRef
	BaseRef       string        // default: HEAD
	Image         string        // the repo's baked image ID; "" = stock (guarded cloud-init installs everything)
	IdleTimeout   time.Duration // 0 = never auto-park
	UnattendedCap time.Duration // 0 = no runaway cap
	Progress      func(step string)
}

// BakeSpec describes one repo's image bake. Images are repo-specific: the
// default install serves pier and the harnesses; whatever a repo's toolchain
// needs on top (pnpm, python, ...) comes from its .pier-bake.sh.
type BakeSpec struct {
	RepoName string   // repo basename; keys the image to its repo
	HookPath string   // local path to the repo's .pier-bake.sh; "" = none
	Replaces []string // images this bake supersedes (previous bake, legacy shared image)
}

// BakeHook returns the repo's .pier-bake.sh, "" when absent. It runs on the
// bake instance (agent user, passwordless sudo, no repo checkout — bake
// predates any session): toolchains belong here, repo state in .pier-setup.sh.
func BakeHook(repoRoot string) string {
	p := filepath.Join(repoRoot, ".pier-bake.sh")
	if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	return p
}

type Check struct {
	Name   string
	OK     bool
	Detail string
}

type SetupReport struct {
	Created []string // resources created this run
	Existed []string // groundwork found and adopted (second-dev path)
}

type Quota struct {
	Used   int
	Limit  int
	Detail string
}

// Driver is implemented by awsec2 and gcpgce (later: k8s, fly, docker).
type Driver interface {
	Name() string

	// SetupOnce creates the per-account groundwork (idempotent, tiny: on AWS
	// an instance role + egress-only SG; on GCP two API enables + one IAP
	// firewall rule). Second devs find everything in Existed.
	SetupOnce(ctx context.Context) (SetupReport, error)
	Teardown(ctx context.Context) error
	Doctor(ctx context.Context) []Check

	Create(ctx context.Context, spec CreateSpec) (*Session, error)
	Resume(ctx context.Context, id string) error
	Park(ctx context.Context, id string) error // client-initiated; normal parking is the VM's own shutdown
	Destroy(ctx context.Context, id string) error

	// Resize changes the instance size (vertical scaling). Providers only
	// allow this on stopped instances, and park is exactly a stop — so a
	// running session rides one park+resume cycle (~40s down); a parked one
	// stays parked. Same CPU architecture only: the disk's binaries live on.
	Resize(ctx context.Context, id, instanceType string) error

	// List returns only the caller's sessions (identity-filtered tags/labels).
	List(ctx context.Context) ([]Session, error)

	// AttachCommand hands the terminal over: `aws ssm start-session` via
	// session-manager-plugin, or `gcloud compute ssh --tunnel-through-iap`.
	AttachCommand(ctx context.Context, id string) (*exec.Cmd, error)

	// MCPLoginCommand runs `claude mcp login <server>` inside the session
	// with the OAuth callback port tunneled back to the laptop, so
	// browser-based MCP auth completes with one approval — no URL copying.
	MCPLoginCommand(ctx context.Context, id, server string, port int) (*exec.Cmd, error)

	// PortForwardCommand holds local→session port forwards open until the
	// process is interrupted: the app in the session on your laptop's
	// browser, its database in your local psql. pairs are {local, remote}.
	PortForwardCommand(ctx context.Context, id string, pairs [][2]int) (*exec.Cmd, error)

	// SSHTarget exposes the raw OpenSSH recipe for a session — option args
	// and destination — for features that manage their own ssh processes
	// (pier proxy's multiplexed masters and live-adjusted forwards). Pier's
	// one remote mechanism is OpenSSH, so this is a contract, not a leak.
	SSHTarget(ctx context.Context, id string) (opts []string, dest string, err error)

	// Exec runs a one-shot command (status reads, push bootstrap) without a TTY.
	Exec(ctx context.Context, id string, command string) (string, error)

	// Bake builds one repo's prebaked session image (harnesses + the repo's
	// .pier-bake.sh toolchains), cutting that repo's cold create to ~60-90s.
	Bake(ctx context.Context, spec BakeSpec) (imageID string, err error)

	// Headroom reports account capacity (vCPU quota) for the create-time
	// opportunistic check and the TUI header.
	Headroom(ctx context.Context) (Quota, error)
}
