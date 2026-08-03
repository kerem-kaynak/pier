package awsec2

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// aws runs the AWS CLI (the tool already requires it for the SSM plugin, so
// v1 has no SDK dependency) and returns trimmed stdout.
func (d *Driver) aws(ctx context.Context, args ...string) (string, error) {
	full := append([]string{}, args...)
	if d.Profile != "" {
		full = append(full, "--profile", d.Profile)
	}
	if d.Region != "" {
		full = append(full, "--region", d.Region)
	}
	cmd := exec.CommandContext(ctx, "aws", full...)
	cmd.Env = append(os.Environ(), "AWS_PAGER=")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		n := min(len(args), 2)
		return "", fmt.Errorf("aws %s: %s", strings.Join(args[:n], " "), strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// --- SSH into the session ----------------------------------------------------
// One mechanism for everything interactive and file-shaped: OpenSSH. No
// standing keys — each session gets its own keypair at create, stored under
// StateDir/keys/<instance-id>.pem. Two transports: by default a plain dial
// of sshd on the instance's public IP (line-rate transfers, raw-RTT
// typing), falling back to an SSM ProxyCommand (works everywhere, no
// inbound ports, slow) whenever direct can't work. aws.direct = false
// forces the tunnel.

func (d *Driver) keyPath(id string) string {
	return filepath.Join(d.StateDir, "keys", id+".pem")
}

func (d *Driver) sshOpts(ctx context.Context, id string) []string {
	opts := []string{
		"-i", d.keyPath(id),
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + filepath.Join(d.StateDir, "known_hosts"),
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		// A dead connection otherwise hangs transfers forever: probe every
		// 15s, give up after 4 misses (~60s).
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
	}
	if ip := d.directIP(ctx, id); ip != "" {
		return append(opts,
			// HostName dials the address while the command line keeps saying
			// agent@<instance-id>; HostKeyAlias keys known_hosts by instance
			// id, so the entry survives the new public IP every park/resume
			// hands out and matches the one the SSM path wrote.
			"-o", "HostName="+ip,
			"-o", "HostKeyAlias="+id,
		)
	}
	pc := "aws ssm start-session --target %h --document-name AWS-StartSSHSession --parameters portNumber=%p"
	if d.Profile != "" {
		pc += " --profile " + d.Profile
	}
	if d.Region != "" {
		pc += " --region " + d.Region
	}
	return append(opts,
		"-o", "ProxyCommand="+pc,
		// The SSM data channel moves ~100KB/s raw. Compression gets ~4x on
		// the text a dev workflow actually pushes through it (JS modules,
		// tmux screens). The direct path skips it: zlib would only burn CPU
		// on a line-rate link.
		"-C",
	)
}

func (d *Driver) sshRun(ctx context.Context, id, script string) (string, error) {
	return d.sshRunOpts(ctx, id, nil, script)
}

// sshRunOpts is sshRun with extra ssh flags — the bootstrap passes -A when
// the workspace fetch rides the laptop's ssh agent.
func (d *Driver) sshRunOpts(ctx context.Context, id string, extra []string, script string) (string, error) {
	args := append(append(d.sshOpts(ctx, id), extra...), "agent@"+id, script)
	out, err := exec.CommandContext(ctx, "ssh", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ssh %s: %s", id, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// sshStream is sshRun with output flowing straight to the terminal — for
// long user-visible steps (the bake hook) where buffered output would look
// like a hang.
func (d *Driver) sshStream(ctx context.Context, id, script string) error {
	args := append(d.sshOpts(ctx, id), "agent@"+id, script)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// extra flags pass through to scp — "-q" silences the progress meter for
// pushes too small to warrant one.
func (d *Driver) scpTo(ctx context.Context, id, local, remote string, extra ...string) error {
	args := append(append(d.sshOpts(ctx, id), extra...), local, "agent@"+id+":"+remote)
	cmd := exec.CommandContext(ctx, "scp", args...)
	// scp draws its progress meter only when stdout is a terminal — so big
	// pushes (the repo bundle) show live progress interactively and stay
	// silent when piped.
	cmd.Stdout = os.Stdout
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("scp %s -> %s: %s", local, id, strings.TrimSpace(errb.String()))
	}
	return nil
}

// newKeypair generates the per-session ed25519 keypair via ssh-keygen and
// returns the public key line.
func (d *Driver) newKeypair(id string) (string, error) {
	dir := filepath.Join(d.StateDir, "keys")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	key := d.keyPath(id)
	_ = os.Remove(key)
	_ = os.Remove(key + ".pub")
	out, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-C", "pier", "-f", key).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ssh-keygen: %s", strings.TrimSpace(string(out)))
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(pub)), nil
}
