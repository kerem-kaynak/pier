// Package awsec2 implements the pier driver on EC2 + EBS.
//
// Shape (settled in docs/SPEC.md): session = one EC2 instance + gp3 root
// volume; native stop/start = park/resume; everything (attach, file push,
// exec) is OpenSSH straight to the instance's public IP by default, with
// SSH over the SSM tunnel as the automatic fallback (see direct.go); instance role
// carries AmazonSSMManagedInstanceCore and nothing else; self-park is the
// in-VM supervisor running `shutdown -h now` (InstanceInitiatedShutdown-
// Behavior=stop — verified by spike/aws.sh). All state lives in EC2 tags.
package awsec2

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/kerem-kaynak/pier/internal/driver"
)

const (
	RoleName      = "pier-session"
	ProfileName   = "pier-session"
	SecurityGroup = "pier-egress-only"
	TagManaged    = "pier:managed"
	TagUser       = "pier:user"
	TagSession    = "pier:session"
	TagRepo       = "pier:repo"
	TagBranch     = "pier:branch"
	TagReady      = "pier:ready"   // create's last act: bootstrap done, attachable
	TagCreated    = "pier:created" // RFC3339 create time; launch time resets on every resume
	Workspace     = "/home/agent/work"
	amiParamBase  = "/aws/service/canonical/ubuntu/server/24.04/stable/current/%s/hvm/ebs-gp3/ami-id"
)

type Driver struct {
	Profile      string
	Region       string
	InstanceType string
	DiskGiB      int
	Subnet       string // optional: orgs without a default VPC
	// Direct (the default): dial sshd on the instance's public IP, opening
	// TCP 22 to this machine's /32 only. false forces the SSM tunnel for
	// every connection. See direct.go.
	Direct bool

	// StateDir holds per-session ssh keys + known_hosts (the config dir).
	StateDir string
	// Manifest: $HOME-relative files/dirs copied into each session.
	Manifest []string
	// SessionEnv is written to ~/.config/pier/env in the session (sourced by
	// bashrc): GH_TOKEN, CLAUDE_CODE_OAUTH_TOKEN, ...
	SessionEnv map[string]string
	// SupervisorBin returns the embedded pier-supervisor binary for an arch
	// ("arm64"/"amd64").
	SupervisorBin func(arch string) ([]byte, error)

	callerARN string // cached

	// direct-connect probe state (direct.go)
	dmu       sync.Mutex
	dprobe    map[string]directProbe
	myIP      string
	myIPUntil time.Time
	ensured   string // the cidr whose SG rule this process reconciled
	noticed   map[string]bool
}

var _ driver.Driver = (*Driver)(nil)

func (d *Driver) Name() string { return "aws-ec2" }

// user returns the caller identity used for tag namespacing. Assumed-role
// ARNs get their per-login session name stripped so the value is stable.
func (d *Driver) user(ctx context.Context) (string, error) {
	if d.callerARN != "" {
		return d.callerARN, nil
	}
	arn, err := d.aws(ctx, "sts", "get-caller-identity", "--query", "Arn", "--output", "text")
	if err != nil {
		return "", err
	}
	if i := strings.Index(arn, ":assumed-role/"); i >= 0 {
		parts := strings.Split(arn, "/")
		if len(parts) == 3 {
			arn = strings.Join(parts[:2], "/")
		}
	}
	d.callerARN = arn
	return arn, nil
}

type ec2Instance struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Launch string `json:"launch"`
	IType  string `json:"itype"`
	Tags   []struct {
		Key   string `json:"Key"`
		Value string `json:"Value"`
	} `json:"tags"`
}

func (d *Driver) List(ctx context.Context) ([]driver.Session, error) {
	me, err := d.user(ctx)
	if err != nil {
		return nil, err
	}
	out, err := d.aws(ctx, "ec2", "describe-instances",
		"--filters", "Name=tag:"+TagManaged+",Values=1", "Name=tag:"+TagUser+",Values="+me,
		"Name=instance-state-name,Values=pending,running,stopping,stopped",
		"--query", "Reservations[].Instances[].{id:InstanceId,state:State.Name,launch:LaunchTime,itype:InstanceType,tags:Tags}",
		"--output", "json")
	if err != nil {
		return nil, err
	}
	var raw []ec2Instance
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, err
	}
	var sessions []driver.Session
	for _, in := range raw {
		s := driver.Session{ID: in.ID, User: me, Driver: d.Name(), InstanceType: in.IType}
		ready := false
		for _, t := range in.Tags {
			switch t.Key {
			case TagSession:
				s.Name = t.Value
			case TagRepo:
				s.Repo = t.Value
			case TagBranch:
				s.Branch = t.Value
			case TagReady:
				ready = true
			case TagCreated:
				if ts, err := time.Parse(time.RFC3339, t.Value); err == nil {
					s.Created = ts
				}
			}
		}
		switch in.State {
		case "pending":
			s.State = driver.StateCreating
		case "running":
			// EC2 says running long before the session is usable (SSM
			// registration, secrets push, bootstrap). The ready tag is the
			// create's last act, so its absence means still-creating — a
			// truthful state with no probe and no SSM dependency.
			s.State = driver.StateRunning // enriched to working/idle by the caller
			if !ready {
				s.State = driver.StateCreating
			}
		case "stopping", "stopped":
			s.State = driver.StateParked
		default:
			s.State = driver.StateDead
		}
		// Launch time is the fallback for pre-tag sessions; it resets on
		// every resume, so the created tag wins whenever present.
		if s.Created.IsZero() {
			if ts, err := time.Parse(time.RFC3339, in.Launch); err == nil {
				s.Created = ts
			}
		}
		s.CostNote = costNote(s.State)
		sessions = append(sessions, s)
	}
	return sessions, nil
}

func costNote(st driver.State) string {
	switch st {
	case driver.StateParked:
		return "~$3-4/mo"
	case driver.StateDead:
		return ""
	default:
		return "~$0.03/h"
	}
}

func (d *Driver) Resume(ctx context.Context, id string) error {
	if _, err := d.aws(ctx, "ec2", "start-instances", "--instance-ids", id); err != nil {
		return err
	}
	d.dropProbe(id) // the VM comes back on a fresh public IP
	return d.waitSSH(ctx, id, 240*time.Second)
}

func (d *Driver) Park(ctx context.Context, id string) error {
	_, err := d.aws(ctx, "ec2", "stop-instances", "--instance-ids", id)
	if err == nil {
		d.dropProbe(id) // the public IP dies with the stop
	}
	return err
}

// Resize: EC2 permits instance-type changes only while stopped, and park is
// exactly a stop — so resize is park → modify → resume. A parked session is
// resized in place and stays parked (costs nothing to leave it that way).
func (d *Driver) Resize(ctx context.Context, id, itype string) error {
	out, err := d.aws(ctx, "ec2", "describe-instances", "--instance-ids", id,
		"--query", "Reservations[0].Instances[0].[State.Name,Architecture,InstanceType]",
		"--output", "text")
	if err != nil {
		return err
	}
	f := strings.Fields(out)
	if len(f) != 3 {
		return fmt.Errorf("unexpected describe-instances output: %q", out)
	}
	state, curArch, curType := f[0], f[1], f[2]
	if curType == itype {
		return fmt.Errorf("session is already a %s", itype)
	}
	if curArch == "x86_64" {
		curArch = "amd64"
	}
	newArch, err := d.archOf(ctx, itype)
	if err != nil {
		return err
	}
	if newArch != curArch {
		return fmt.Errorf("cannot resize across architectures: the disk is %s, %s is %s — pick a same-arch type", curArch, itype, newArch)
	}

	wasRunning := false
	switch state {
	case "running", "pending":
		wasRunning = true
		if _, err := d.aws(ctx, "ec2", "stop-instances", "--instance-ids", id); err != nil {
			return err
		}
		d.dropProbe(id)
	case "stopping", "stopped":
		// already parked (or on its way); resize in place
	default:
		return fmt.Errorf("session is %s — nothing to resize", state)
	}
	if _, err := d.aws(ctx, "ec2", "wait", "instance-stopped", "--instance-ids", id); err != nil {
		return err
	}
	if _, err := d.aws(ctx, "ec2", "modify-instance-attribute", "--instance-id", id,
		"--instance-type", "Value="+itype); err != nil {
		return err
	}
	if wasRunning {
		return d.Resume(ctx, id)
	}
	return nil
}

func (d *Driver) Destroy(ctx context.Context, id string) error {
	if _, err := d.aws(ctx, "ec2", "terminate-instances", "--instance-ids", id); err != nil {
		return err
	}
	_ = os.Remove(d.keyPath(id))
	_ = os.Remove(d.keyPath(id) + ".pub")
	return nil
}

// AttachCommand forwards the laptop's ssh agent (a no-op when none runs) and
// refreshes ~/.ssh/agent.sock before tmux: each attach gets a fresh forwarded
// socket path, while long-lived tmux panes hold the old one — bashrc points
// them at the symlink instead, so `git push` over ssh keeps working across
// re-attaches. Keys never leave the laptop; detached sessions can't use them.
//
// It also refuses on a missing bootstrap marker: attaching mid-create would
// land in an empty $HOME (no repo yet) and steal the `main` tmux session
// away from its workdir. The ready tag stops pier's own commands well before
// this, so it's a backstop for races and raw ssh users — refuse, don't wait.
func (d *Driver) AttachCommand(ctx context.Context, id string) (*exec.Cmd, error) {
	const remote = `[ -S "$SSH_AUTH_SOCK" ] && ln -sf "$SSH_AUTH_SOCK" ~/.ssh/agent.sock
[ -e "$HOME/.pier-bootstrapped" ] || { echo "pier: this session is still setting up — attach again when it shows running in pier ls" >&2; exit 1; }
exec tmux new-session -A -s main`
	args := append(d.sshOpts(ctx, id), "-t", "-o", "ForwardAgent=yes", "agent@"+id, remote)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd, nil
}

// MCPLoginCommand: interactive `claude mcp login` with the OAuth callback
// port forwarded through the SSM tunnel. The auth URL prints in the user's
// terminal (BROWSER=echo keeps headless claude from skipping straight to
// paste mode); opening it on the laptop completes the provider's
// localhost redirect INTO the VM's waiting listener — one browser approval,
// no URL copy-paste. The token then lives on the session disk, so this is
// once per session, surviving park/resume. Same local/remote port: the
// redirect URL embeds the port claude registered on the VM.
func (d *Driver) MCPLoginCommand(ctx context.Context, id, server string, port int) (*exec.Cmd, error) {
	remote := fmt.Sprintf(
		"set -a; . ~/.config/pier/env 2>/dev/null; set +a; BROWSER=echo exec claude mcp login '%s' --callback-port %d",
		server, port)
	args := append(d.sshOpts(ctx, id),
		"-t", "-L", fmt.Sprintf("%d:localhost:%d", port, port), "agent@"+id, remote)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd, nil
}

// PortForwardCommand: plain ssh -L forwards, -N so no remote shell is
// taken. Runs until interrupted. Over the SSM tunnel this is not fast
// (~100KB/s-1MB/s raw; sshOpts' -C buys ~4x on dev-server text) — with
// aws.direct the forwards run at line rate.
func (d *Driver) PortForwardCommand(ctx context.Context, id string, pairs [][2]int) (*exec.Cmd, error) {
	args := d.sshOpts(ctx, id)
	for _, p := range pairs {
		args = append(args, "-L", fmt.Sprintf("%d:localhost:%d", p[0], p[1]))
	}
	args = append(args, "-N", "agent@"+id)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd, nil
}

func (d *Driver) SSHTarget(ctx context.Context, id string) ([]string, string, error) {
	return d.sshOpts(ctx, id), "agent@" + id, nil
}

func (d *Driver) Exec(ctx context.Context, id string, command string) (string, error) {
	return d.sshRun(ctx, id, command)
}

// waitSSH polls until SSH answers (the SSM tunnel, or the direct path when
// enabled — direct needs only sshd up, so it usually answers first).
func (d *Driver) waitSSH(ctx context.Context, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, cancel := context.WithTimeout(ctx, 20*time.Second)
		_, err := d.sshRun(c, id, "true")
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("session %s not reachable after %s", id, timeout)
}
