package awsec2

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kerem-kaynak/pier/internal/driver"
)

// Create: launch first, prep the workspace bundle while the instance boots,
// push over SSH-over-SSM as soon as sshd answers, bootstrap, done. On a stock
// AMI the bootstrap waits for cloud-init's harness install (minutes, once);
// on a baked AMI the whole thing is the boot + push time.
func (d *Driver) Create(ctx context.Context, spec driver.CreateSpec) (*driver.Session, error) {
	progress := spec.Progress
	if progress == nil {
		progress = func(string) {}
	}
	me, err := d.user(ctx)
	if err != nil {
		return nil, err
	}

	arch, err := d.archOf(ctx, d.InstanceType)
	if err != nil {
		return nil, err
	}
	supervisor, err := d.SupervisorBin(arch)
	if err != nil {
		return nil, err
	}
	ami, err := d.resolveAMI(ctx, arch)
	if err != nil {
		return nil, err
	}

	// The keypair is instance-id-keyed but must exist pre-launch (pubkey goes
	// into user-data), so generate under a temp name and rename after launch.
	tmpID := "new-" + sanitize(spec.Name)
	pubkey, err := d.newKeypair(tmpID)
	if err != nil {
		return nil, err
	}

	work, err := os.MkdirTemp("", "pier-create-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	userData := renderUserData(spec, pubkey)
	udPath := filepath.Join(work, "user-data.yaml")
	if err := os.WriteFile(udPath, []byte(userData), 0o600); err != nil {
		return nil, err
	}

	id, err := d.launch(ctx, spec, me, ami, udPath)
	if err != nil {
		return nil, err
	}
	os.Rename(d.keyPath(tmpID), d.keyPath(id))
	os.Rename(d.keyPath(tmpID)+".pub", d.keyPath(id)+".pub")
	progress(fmt.Sprintf("launched %s (%s, %s)", id, d.InstanceType, arch))

	// Local prep while the instance boots (~30s).
	bundle := filepath.Join(work, "pier.bundle")
	if err := gitBundle(spec.Repo, baseRef(spec), bundle); err != nil {
		return nil, fmt.Errorf("bundling %s: %w", spec.Repo, err)
	}
	filesTar := filepath.Join(work, "pier-files.tar")
	if err := buildFilesTar(filesTar, d.Manifest, spec.Repo, d.SessionEnv); err != nil {
		return nil, err
	}
	supPath := filepath.Join(work, "pier-supervisor")
	if err := os.WriteFile(supPath, supervisor, 0o755); err != nil {
		return nil, err
	}
	bootPath := filepath.Join(work, "pier-bootstrap.sh")
	if err := os.WriteFile(bootPath, []byte(renderBootstrap(spec)), 0o755); err != nil {
		return nil, err
	}
	progress("packed workspace bundle + secrets")

	progress("waiting for SSH over SSM (boot ~30s)")
	if err := d.waitSSH(ctx, id, 240*time.Second); err != nil {
		return nil, err
	}

	progress("pushing workspace")
	for local, remote := range map[string]string{
		bundle:   "/tmp/pier.bundle",
		filesTar: "/tmp/pier-files.tar",
		supPath:  "/tmp/pier-supervisor",
		bootPath: "/tmp/pier-bootstrap.sh",
	} {
		if err := d.scpTo(ctx, id, local, remote); err != nil {
			return nil, err
		}
	}

	progress("bootstrapping (stock AMI waits for cloud-init here — `pier bake` skips that)")
	if out, err := d.sshRun(ctx, id, "bash /tmp/pier-bootstrap.sh"); err != nil {
		return nil, fmt.Errorf("bootstrap: %w\n%s", err, out)
	}

	return &driver.Session{
		ID: id, Name: spec.Name, Repo: filepath.Base(spec.Repo), Branch: spec.Branch,
		User: me, Driver: d.Name(), State: driver.StateRunning, LastActive: time.Now(),
		CostNote: costNote(driver.StateRunning),
	}, nil
}

func baseRef(spec driver.CreateSpec) string {
	if spec.BaseRef == "" {
		return "HEAD"
	}
	return spec.BaseRef
}

// launch retries on IAM instance-profile propagation lag (the spike showed
// one retry is usually needed right after `pier setup`).
func (d *Driver) launch(ctx context.Context, spec driver.CreateSpec, me, ami, udPath string) (string, error) {
	sg, err := d.securityGroupID(ctx)
	if err != nil {
		return "", err
	}
	if sg == "" {
		return "", fmt.Errorf("security group %s not found — run `pier setup`", SecurityGroup)
	}
	rootDev, err := d.aws(ctx, "ec2", "describe-images", "--image-ids", ami,
		"--query", "Images[0].RootDeviceName", "--output", "text")
	if err != nil {
		return "", err
	}

	tags, _ := json.Marshal([]map[string]any{
		{"ResourceType": "instance", "Tags": tagList(spec, me, "pier-"+spec.Name)},
		{"ResourceType": "volume", "Tags": tagList(spec, me, "pier-"+spec.Name)},
	})
	bdm := fmt.Sprintf(`[{"DeviceName":%q,"Ebs":{"VolumeSize":%d,"VolumeType":"gp3","DeleteOnTermination":true}}]`,
		rootDev, d.DiskGiB)

	args := []string{"ec2", "run-instances",
		"--image-id", ami,
		"--instance-type", d.InstanceType,
		"--iam-instance-profile", "Name=" + ProfileName,
		"--security-group-ids", sg,
		"--instance-initiated-shutdown-behavior", "stop", // in-VM shutdown = park
		"--metadata-options", "HttpTokens=required,HttpEndpoint=enabled",
		"--block-device-mappings", bdm,
		"--tag-specifications", string(tags),
		"--user-data", "file://" + udPath,
		"--query", "Instances[0].InstanceId",
	}
	if d.Subnet != "" {
		args = append(args, "--subnet-id", d.Subnet)
	}

	var lastErr error
	for range 10 {
		out, err := d.aws(ctx, append(args, "--output", "text")...)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if strings.Contains(err.Error(), "Invalid IAM Instance Profile") {
			time.Sleep(3 * time.Second) // IAM propagation
			continue
		}
		if strings.Contains(err.Error(), "VcpuLimitExceeded") {
			return "", fmt.Errorf("vCPU quota exceeded — request a raise:\n"+
				"  aws service-quotas request-service-quota-increase --service-code ec2 --quota-code L-1216C47A --desired-value 32\n(%w)", err)
		}
		return "", err
	}
	return "", lastErr
}

func tagList(spec driver.CreateSpec, me, name string) []map[string]string {
	return []map[string]string{
		{"Key": "Name", "Value": name},
		{"Key": TagManaged, "Value": "1"},
		{"Key": TagUser, "Value": me},
		{"Key": TagSession, "Value": spec.Name},
		{"Key": TagRepo, "Value": filepath.Base(spec.Repo)},
		{"Key": TagBranch, "Value": spec.Branch},
	}
}

// archOf maps the instance type to arm64/amd64 (selects AMI + supervisor build).
func (d *Driver) archOf(ctx context.Context, itype string) (string, error) {
	out, err := d.aws(ctx, "ec2", "describe-instance-types", "--instance-types", itype,
		"--query", "InstanceTypes[0].ProcessorInfo.SupportedArchitectures[0]", "--output", "text")
	if err != nil {
		return "", err
	}
	if out == "x86_64" {
		return "amd64", nil
	}
	return out, nil
}

func (d *Driver) resolveAMI(ctx context.Context, arch string) (string, error) {
	if d.BakedAMI != "" {
		return d.BakedAMI, nil
	}
	return d.aws(ctx, "ssm", "get-parameter",
		"--name", fmt.Sprintf(amiParamBase, arch),
		"--query", "Parameter.Value", "--output", "text")
}

// --- user data ---------------------------------------------------------------
// One template for stock and baked AMIs: every install is guarded, so on a
// baked image runcmd is per-instance config only (seconds).

const userDataTmpl = `#cloud-config
hostname: {{HOSTNAME}}
users:
  - name: agent
    shell: /bin/bash
    sudo: "ALL=(ALL) NOPASSWD:ALL"
    ssh_authorized_keys:
      - {{PUBKEY}}
write_files:
  - path: /etc/pier/supervisor.conf
    permissions: "0644"
    content: |
      idle_timeout={{IDLE}}
      unattended_cap={{CAP}}
runcmd:
  - |
    set -x
    export DEBIAN_FRONTEND=noninteractive
    install -d -m 700 -o agent -g agent /home/agent/.ssh
    grep -qxF '{{PUBKEY}}' /home/agent/.ssh/authorized_keys 2>/dev/null || echo '{{PUBKEY}}' >> /home/agent/.ssh/authorized_keys
    chown agent:agent /home/agent/.ssh/authorized_keys && chmod 600 /home/agent/.ssh/authorized_keys
    command -v tmux >/dev/null || { apt-get update -y && apt-get install -y tmux git curl jq unzip ca-certificates docker.io; }
    command -v node >/dev/null || { curl -fsSL https://deb.nodesource.com/setup_22.x | bash - && apt-get install -y nodejs; }
    command -v gh >/dev/null || { curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /usr/share/keyrings/githubcli-archive-keyring.gpg && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list && apt-get update -y && apt-get install -y gh; }
    command -v claude >/dev/null || npm install -g @anthropic-ai/claude-code
    command -v codex >/dev/null || npm install -g @openai/codex
    getent group docker >/dev/null && usermod -aG docker agent
    grep -q 'pier/env' /home/agent/.bashrc || printf '\n[ -f ~/.config/pier/env ] && set -a && . ~/.config/pier/env && set +a\ncd ~/work/* 2>/dev/null || true\n' >> /home/agent/.bashrc
`

func renderUserData(spec driver.CreateSpec, pubkey string) string {
	return strings.NewReplacer(
		"{{HOSTNAME}}", sanitize(spec.Name),
		"{{PUBKEY}}", pubkey,
		"{{IDLE}}", durConf(spec.IdleTimeout),
		"{{CAP}}", durConf(spec.UnattendedCap),
	).Replace(userDataTmpl)
}

func durConf(d time.Duration) string {
	if d <= 0 {
		return "never"
	}
	return d.String()
}

var unsafeHost = regexp.MustCompile(`[^a-z0-9-]+`)

// sanitize turns a session name (may contain "/") into a hostname/filename.
func sanitize(name string) string {
	s := unsafeHost.ReplaceAllString(strings.ToLower(name), "-")
	s = strings.Trim(s, "-")
	if len(s) > 60 {
		s = s[:60]
	}
	if s == "" {
		s = "session"
	}
	return s
}

// --- bootstrap ----------------------------------------------------------------

const bootstrapTmpl = `#!/usr/bin/env bash
# pier bootstrap — runs once, as agent, on the fresh instance.
set -euo pipefail

# Stock AMI: harness install still running under cloud-init; wait it out.
command -v git >/dev/null && command -v tmux >/dev/null || sudo cloud-init status --wait >/dev/null || true

sudo install -m 0755 /tmp/pier-supervisor /usr/local/bin/pier-supervisor
sudo tee /etc/systemd/system/pier-supervisor.service >/dev/null <<'UNIT'
[Unit]
Description=pier session supervisor (self-park watchdog)
After=multi-user.target

[Service]
User=agent
ExecStart=/usr/local/bin/pier-supervisor
Restart=always

[Install]
WantedBy=multi-user.target
UNIT
sudo systemctl daemon-reload
sudo systemctl enable --now pier-supervisor.service

tar -xf /tmp/pier-files.tar -C "$HOME" --strip-components=1 home 2>/dev/null || true
set -a; . "$HOME/.config/pier/env" 2>/dev/null || true; set +a

mkdir -p "$HOME/work/{{REPO}}"
cd "$HOME/work/{{REPO}}"
git init -q -b '{{BRANCH}}'
git fetch -q /tmp/pier.bundle refs/pier/export
git reset -q --hard FETCH_HEAD
{{GITCONFIG}}
tar -xf /tmp/pier-files.tar -C . --strip-components=1 repo 2>/dev/null || true
{ command -v gh >/dev/null && [ -n "${GH_TOKEN:-}" ] && gh auth setup-git >/dev/null 2>&1; } || true

tmux has-session -t main 2>/dev/null || tmux new-session -d -s main -c "$HOME/work/{{REPO}}"
if [ -x ./.pier-setup.sh ]; then
  tmux new-window -d -t main -n setup "bash -c 'set -a; . ~/.config/pier/env 2>/dev/null; set +a; cd ~/work/{{REPO}} && ./.pier-setup.sh 2>&1 | tee ~/.pier-setup.log'"
fi

rm -f /tmp/pier.bundle /tmp/pier-files.tar /tmp/pier-supervisor /tmp/pier-bootstrap.sh
echo bootstrapped
`

func renderBootstrap(spec driver.CreateSpec) string {
	var gitcfg []string
	line := func(args ...string) {
		out, err := gitOut(spec.Repo, args...)
		if err == nil && out != "" && !strings.Contains(out, "'") {
			switch args[len(args)-1] {
			case "user.name":
				gitcfg = append(gitcfg, "git config user.name '"+out+"'")
			case "user.email":
				gitcfg = append(gitcfg, "git config user.email '"+out+"'")
			case "origin":
				gitcfg = append(gitcfg, "git remote add origin '"+out+"'")
			}
		}
	}
	line("config", "user.name")
	line("config", "user.email")
	line("remote", "get-url", "origin")
	return strings.NewReplacer(
		"{{REPO}}", filepath.Base(spec.Repo),
		"{{BRANCH}}", spec.Branch,
		"{{GITCONFIG}}", strings.Join(gitcfg, "\n"),
	).Replace(bootstrapTmpl)
}

// --- workspace bundle + secrets ------------------------------------------------

func gitOut(repo string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

// gitBundle packs full history up to baseRef into a bundle exposing a single
// ref (refs/pier/export) that the VM fetches — includes uncommitted nothing,
// clean by construction.
func gitBundle(repo, ref, dst string) error {
	sha, err := gitOut(repo, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return fmt.Errorf("base ref %q not found", ref)
	}
	if _, err := gitOut(repo, "update-ref", "refs/pier/export", sha); err != nil {
		return err
	}
	defer gitOut(repo, "update-ref", "-d", "refs/pier/export")
	if out, err := exec.Command("git", "-C", repo, "bundle", "create", dst, "refs/pier/export").CombinedOutput(); err != nil {
		return fmt.Errorf("git bundle: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// buildFilesTar packs, into one tar: manifest files/dirs under $HOME (prefix
// home/), repo-local .env* (prefix repo/), and a generated home/.config/pier/env
// with the session tokens. The bootstrap extracts the two prefixes to the
// right places.
func buildFilesTar(dst string, manifest []string, repoRoot string, env map[string]string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	defer tw.Close()

	home, _ := os.UserHomeDir()
	addFile := func(path, name string, mode fs.FileMode) error {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: int64(mode.Perm()), Size: int64(len(b))}); err != nil {
			return err
		}
		_, err = tw.Write(b)
		return err
	}

	for _, m := range manifest {
		p := m
		if strings.HasPrefix(p, "~/") {
			p = filepath.Join(home, p[2:])
		} else if !filepath.IsAbs(p) {
			p = filepath.Join(home, p)
		}
		rel, err := filepath.Rel(home, p)
		if err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("manifest entry %q is outside $HOME", m)
		}
		err = filepath.WalkDir(p, func(path string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || !e.Type().IsRegular() {
				return nil // skip missing entries, dirs, symlinks
			}
			r, _ := filepath.Rel(home, path)
			info, _ := e.Info()
			return addFile(path, "home/"+r, info.Mode())
		})
		if err != nil {
			return err
		}
	}

	envFiles, _ := filepath.Glob(filepath.Join(repoRoot, ".env*"))
	for _, p := range envFiles {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			if err := addFile(p, "repo/"+filepath.Base(p), fi.Mode()); err != nil {
				return err
			}
		}
	}

	if len(env) > 0 {
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			b.WriteString(k + "='" + strings.ReplaceAll(env[k], "'", `'\''`) + "'\n")
		}
		if err := tw.WriteHeader(&tar.Header{Name: "home/.config/pier/env", Mode: 0o600, Size: int64(b.Len())}); err != nil {
			return err
		}
		if _, err := tw.Write([]byte(b.String())); err != nil {
			return err
		}
	}
	return nil
}
