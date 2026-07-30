// Package awsec2 implements the pier driver on EC2 + EBS.
//
// Shape (settled in docs/SPEC.md):
//   - Session = one EC2 instance (default t4g.medium, Ubuntu 24.04 arm64) with
//     a gp3 root volume (default 40GB). Stop/start is the park/resume semantic.
//   - Networking: default VPC, public IP, zero-inbound SG; attach rides SSM
//     over outbound 443. Orgs without a default VPC set config `subnet`.
//   - Instance role carries AmazonSSMManagedInstanceCore and nothing else.
//   - Self-park: in-VM supervisor runs `shutdown -h now`; EBS-backed instances
//     with InstanceInitiatedShutdownBehavior=stop park rather than terminate
//     (verified by spike/aws.sh).
//   - Identity: sts GetCallerIdentity ARN -> tag pier:user; List filters on it.
package awsec2

import (
	"context"
	"errors"
	"os/exec"

	"github.com/kerem-kaynak/pier/internal/driver"
)

const (
	RoleName        = "pier-session"
	ProfileName     = "pier-session"
	SecurityGroup   = "pier-egress-only"
	TagManaged      = "pier:managed"
	TagUser         = "pier:user"
	TagSession      = "pier:session"
	TagRepo         = "pier:repo"
	DefaultType     = "t4g.medium"
	DefaultDiskGiB  = 40
	UbuntuAMIParam  = "/aws/service/canonical/ubuntu/server/24.04/stable/current/arm64/hvm/ebs-gp3/ami-id"
	WorkspacePrefix = "/home/agent/work"
)

var errNotImplemented = errors.New("awsec2: not implemented yet")

type Driver struct {
	Profile string
	Region  string
	// Subnet overrides default-VPC discovery for orgs that deleted it.
	Subnet string
}

var _ driver.Driver = (*Driver)(nil)

func (d *Driver) Name() string { return "aws-ec2" }

func (d *Driver) SetupOnce(ctx context.Context) (driver.SetupReport, error) {
	// TODO: idempotently ensure RoleName(+profile, SSM core policy) and
	// SecurityGroup in the default VPC; print exact IAM needed beforehand.
	return driver.SetupReport{}, errNotImplemented
}

func (d *Driver) Teardown(ctx context.Context) error { return errNotImplemented }

func (d *Driver) Doctor(ctx context.Context) []driver.Check {
	// TODO: creds valid, default VPC present, SSM reachable, plugin installed,
	// vCPU quota headroom, bake image freshness.
	return nil
}

func (d *Driver) Create(ctx context.Context, spec driver.CreateSpec) (*driver.Session, error) {
	// TODO: resolve AMI (baked image if present, else UbuntuAMIParam), launch
	// with tags + zero-inbound SG + shutdown-behavior=stop, then pipeline:
	// poll SSM online -> attach-ready; push bundle + secrets; run
	// .pier-setup.sh in a background tmux pane. Quota check runs during the
	// boot wait.
	return nil, errNotImplemented
}

func (d *Driver) Resume(ctx context.Context, id string) error  { return errNotImplemented }
func (d *Driver) Park(ctx context.Context, id string) error    { return errNotImplemented }
func (d *Driver) Destroy(ctx context.Context, id string) error { return errNotImplemented }

func (d *Driver) List(ctx context.Context) ([]driver.Session, error) {
	// TODO: DescribeInstances filtered by TagManaged + TagUser(=caller ARN),
	// map EC2 states -> driver.State; rich working/idle read via Exec on demand.
	return nil, errNotImplemented
}

func (d *Driver) AttachCommand(ctx context.Context, id string) (*exec.Cmd, error) {
	// TODO: aws ssm start-session --target id --document-name
	// AWS-StartInteractiveCommand --parameters command='sudo -iu agent tmux attach'
	return nil, errNotImplemented
}

func (d *Driver) Exec(ctx context.Context, id string, command string) (string, error) {
	return "", errNotImplemented
}

func (d *Driver) Bake(ctx context.Context) (string, error) { return "", errNotImplemented }

func (d *Driver) Headroom(ctx context.Context) (driver.Quota, error) {
	return driver.Quota{}, errNotImplemented
}
