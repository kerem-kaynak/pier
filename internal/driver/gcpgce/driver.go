// Package gcpgce implements the pier driver on GCE + Persistent Disk.
//
// Shape (settled in docs/SPEC.md):
//   - Session = one GCE instance (default e2-medium, Ubuntu 24.04) with a
//     pd-balanced root disk (default 40GB). Instance stop/start = park/resume;
//     an in-VM `shutdown -h now` lands in TERMINATED with disks intact
//     (restartable) — verified by spike/gcp.sh.
//   - Networking: default network, public IP, one firewall rule allowing only
//     Google's IAP range (35.235.240.0/20) -> :22, target-tagged to pier VMs.
//   - Instances run with NO service account and no scopes: zero cloud
//     permissions inside the VM, ever.
//   - Attach: `gcloud compute ssh --tunnel-through-iap` (shell-out in v1).
//   - Identity: the authed principal -> label pier-user; List filters on it.
package gcpgce

import (
	"context"
	"errors"
	"os/exec"

	"github.com/kerem-kaynak/pier/internal/driver"
)

const (
	FirewallRule    = "pier-allow-iap-ssh"
	IAPRange        = "35.235.240.0/20"
	NetworkTag      = "pier-session"
	LabelManaged    = "pier-managed"
	LabelUser       = "pier-user"
	LabelSession    = "pier-session"
	LabelRepo       = "pier-repo"
	DefaultType     = "e2-medium"
	DefaultDiskGiB  = 40
	WorkspacePrefix = "/home/agent/work"
)

var errNotImplemented = errors.New("gcpgce: not implemented yet")

type Driver struct {
	Project string
	Zone    string
}

var _ driver.Driver = (*Driver)(nil)

func (d *Driver) Name() string { return "gcp-gce" }

func (d *Driver) SetupOnce(ctx context.Context) (driver.SetupReport, error) {
	// TODO: enable compute + IAP APIs, ensure FirewallRule (IAPRange -> :22,
	// target tag NetworkTag). Nothing else is ever created account-wide.
	return driver.SetupReport{}, errNotImplemented
}

func (d *Driver) Teardown(ctx context.Context) error { return errNotImplemented }

func (d *Driver) Doctor(ctx context.Context) []driver.Check {
	// TODO: ADC/gcloud auth valid, APIs enabled, regional CPU quota headroom
	// (regions describe -> usage/limit), gcloud present for attach shell-out.
	return nil
}

func (d *Driver) Create(ctx context.Context, spec driver.CreateSpec) (*driver.Session, error) {
	// TODO: launch (baked image if present) with labels + NetworkTag +
	// --no-service-account --no-scopes, then the same async pipeline as awsec2.
	return nil, errNotImplemented
}

func (d *Driver) Resume(ctx context.Context, id string) error  { return errNotImplemented }
func (d *Driver) Park(ctx context.Context, id string) error    { return errNotImplemented }
func (d *Driver) Destroy(ctx context.Context, id string) error { return errNotImplemented }

func (d *Driver) List(ctx context.Context) ([]driver.Session, error) {
	return nil, errNotImplemented
}

func (d *Driver) AttachCommand(ctx context.Context, id string) (*exec.Cmd, error) {
	// TODO: gcloud compute ssh <name> --tunnel-through-iap -- -t 'sudo -iu agent tmux attach'
	return nil, errNotImplemented
}

func (d *Driver) Exec(ctx context.Context, id string, command string) (string, error) {
	return "", errNotImplemented
}

func (d *Driver) Bake(ctx context.Context) (string, error) { return "", errNotImplemented }

func (d *Driver) Headroom(ctx context.Context) (driver.Quota, error) {
	return driver.Quota{}, errNotImplemented
}
