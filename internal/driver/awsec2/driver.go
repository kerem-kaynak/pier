// Package awsec2 implements the pier driver on EC2 + EBS.
//
// Shape (settled in docs/SPEC.md): session = one EC2 instance + gp3 root
// volume; native stop/start = park/resume; zero-inbound SG with everything
// (attach, file push, exec) riding SSH over the SSM tunnel; instance role
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
	Workspace     = "/home/agent/work"
	amiParamBase  = "/aws/service/canonical/ubuntu/server/24.04/stable/current/%s/hvm/ebs-gp3/ami-id"
)

type Driver struct {
	Profile      string
	Region       string
	InstanceType string
	DiskGiB      int
	Subnet       string // optional: orgs without a default VPC
	BakedAMI     string

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
		"--query", "Reservations[].Instances[].{id:InstanceId,state:State.Name,launch:LaunchTime,tags:Tags}",
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
		s := driver.Session{ID: in.ID, User: me, Driver: d.Name()}
		for _, t := range in.Tags {
			switch t.Key {
			case TagSession:
				s.Name = t.Value
			case TagRepo:
				s.Repo = t.Value
			case TagBranch:
				s.Branch = t.Value
			}
		}
		switch in.State {
		case "pending":
			s.State = driver.StateCreating
		case "running":
			s.State = driver.StateRunning // enriched to working/idle by the caller
		case "stopping", "stopped":
			s.State = driver.StateParked
		default:
			s.State = driver.StateDead
		}
		if ts, err := time.Parse(time.RFC3339, in.Launch); err == nil {
			s.LastActive = ts
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
	return d.waitSSH(ctx, id, 240*time.Second)
}

func (d *Driver) Park(ctx context.Context, id string) error {
	_, err := d.aws(ctx, "ec2", "stop-instances", "--instance-ids", id)
	return err
}

func (d *Driver) Destroy(ctx context.Context, id string) error {
	if _, err := d.aws(ctx, "ec2", "terminate-instances", "--instance-ids", id); err != nil {
		return err
	}
	_ = os.Remove(d.keyPath(id))
	_ = os.Remove(d.keyPath(id) + ".pub")
	return nil
}

func (d *Driver) AttachCommand(ctx context.Context, id string) (*exec.Cmd, error) {
	args := append(d.sshOpts(id), "-t", "agent@"+id, "tmux new-session -A -s main")
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd, nil
}

func (d *Driver) Exec(ctx context.Context, id string, command string) (string, error) {
	return d.sshRun(ctx, id, command)
}

// waitSSH polls until SSH over the SSM tunnel answers.
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
