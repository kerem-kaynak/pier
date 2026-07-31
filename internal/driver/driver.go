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
	"os/exec"
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
	ID         string // provider instance ID
	Name       string // branch-derived session name
	Repo       string
	Branch     string
	User       string // derived from cloud caller identity, never configured
	Driver     string
	State      State
	Strained   bool // sustained cpu/mem pressure (supervisor beacon) — resize hint
	LastActive time.Time
	CostNote   string // honest money: "$3/mo parked", "$0.03/h running"
}

// CreateSpec describes a new session. Create returns as soon as the instance
// is attach-ready; code push and .pier-setup.sh continue asynchronously in
// the session (the async create pipeline) with steps streamed via Progress.
type CreateSpec struct {
	Name          string
	Repo          string        // local repo root; the bundle source
	Branch        string        // new branch, off BaseRef
	BaseRef       string        // default: HEAD
	IdleTimeout   time.Duration // 0 = never auto-park
	UnattendedCap time.Duration // 0 = no runaway cap
	Progress      func(step string)
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

	// Exec runs a one-shot command (status reads, push bootstrap) without a TTY.
	Exec(ctx context.Context, id string, command string) (string, error)

	// Bake builds the prebaked session image (harnesses preinstalled) that
	// cuts cold create to ~60-90s. Offered as the wizard's last step.
	Bake(ctx context.Context) (imageID string, err error)

	// Headroom reports account capacity (vCPU quota) for the create-time
	// opportunistic check and the TUI header.
	Headroom(ctx context.Context) (Quota, error)
}
