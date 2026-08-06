package awsec2

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
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
// on a baked AMI the whole thing is the boot + push time. A create that
// fails after launch destroys its own instance — a session either exists
// fully set up or not at all, never as a half-made husk in the list.
func (d *Driver) Create(ctx context.Context, spec driver.CreateSpec) (sess *driver.Session, retErr error) {
	progress := spec.Progress
	if progress == nil {
		progress = func(string) {}
	}
	if err := validateNames(spec); err != nil {
		return nil, err
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
	ami, err := d.resolveAMI(ctx, arch, spec.Image)
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
	// WithoutCancel: the cleanup must run even when the failure IS the ctx
	// being cancelled (ctrl-c mid-create).
	defer func() {
		if retErr == nil {
			return
		}
		progress("create failed — terminating the half-made instance")
		if err := d.Destroy(context.WithoutCancel(ctx), id); err != nil {
			progress("cleanup failed (" + err.Error() + ") — remove it with `pier rm " + spec.Name + "`")
		}
	}()

	// Local prep while the instance boots (~30s). The SSM tunnel moves ~1 MB/s,
	// so ship as little as possible through it: when the base commit is
	// already on a GitHub origin the VM fetches the repo from GitHub directly
	// (~100x faster) and the laptop sends at most a thin local-delta bundle.
	sha, err := gitOut(spec.Repo, "rev-parse", "--verify", baseRef(spec)+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("base ref %q not found", baseRef(spec))
	}
	mode, origin := originInfo(spec.Repo, sha)
	forwardAgent := false
	why := "no github origin has the base"
	if mode != "full" {
		switch {
		case originReachable(ctx, origin, d.SessionEnv["GH_TOKEN"]):
			// https works with the auth the session gets (token or anonymous)
		case sshOriginUsable(ctx, spec.Repo):
			// No https credential, but the laptop's ssh agent can auth: keep
			// the ssh origin and forward the agent for the bootstrap fetch.
			origin, _ = gitOut(spec.Repo, "remote", "get-url", "origin")
			forwardAgent = true
			progress("no github token — the fetch borrows your ssh agent (keys stay on this machine)")
		default:
			progress("github origin not reachable with the auth sessions get — using the full bundle")
			mode, why = "full", "github not reachable with session auth"
		}
	}
	bundle := ""
	if mode != "origin" {
		bundle = filepath.Join(work, "pier.bundle")
		switch err := gitBundle(spec.Repo, sha, bundle, exportRef(spec.Name), mode == "thin"); {
		// Only thin bundles are legitimately empty (stale --contains; origin
		// has it all). A full bundle of real history can't be — treating that
		// as "fetch from origin" once shipped a session pointed at an origin
		// that didn't exist.
		case errors.Is(err, errEmptyBundle) && mode == "thin":
			mode, bundle = "origin", ""
		case err != nil:
			return nil, fmt.Errorf("bundling %s: %w", spec.Repo, err)
		}
	}
	// Dirty tracked state travels only when the session's base IS the
	// laptop's HEAD — branching a session off another commit and grafting
	// today's edits onto it would be a lie about what that base contained.
	patch := ""
	if head, _ := gitOut(spec.Repo, "rev-parse", "HEAD"); head == sha {
		p := filepath.Join(work, "pier-dirty.patch")
		switch ok, err := dirtyPatch(spec.Repo, p); {
		case err != nil:
			return nil, err
		case ok:
			patch = p
		}
	}
	setupSrc, warn := setupScriptOverride(spec.Repo)
	if warn != "" {
		progress(warn)
	}
	filesTar := filepath.Join(work, "pier-files.tar")
	if err := buildFilesTar(filesTar, d.Manifest, spec.Repo, d.SessionEnv, setupSrc); err != nil {
		return nil, err
	}
	if miss := envFilesNotCarried(spec.Repo, pierIncludeFiles(spec.Repo)); len(miss) > 0 {
		name := miss[0]
		if len(miss) > 1 {
			name += fmt.Sprintf(" +%d more", len(miss)-1)
		}
		progress("not carrying " + name + " — env files travel only when .pier-include lists them")
	}
	supPath := filepath.Join(work, "pier-supervisor")
	if err := os.WriteFile(supPath, supervisor, 0o755); err != nil {
		return nil, err
	}
	bootPath := filepath.Join(work, "pier-bootstrap.sh")
	if err := os.WriteFile(bootPath, []byte(renderBootstrap(spec, mode, sha, origin)), 0o755); err != nil {
		return nil, err
	}

	progress("waiting for SSH over SSM (boot ~30s)")
	if err := d.waitSSH(ctx, id, 240*time.Second); err != nil {
		return nil, err
	}

	pushes := []struct{ local, remote string }{
		{supPath, "/tmp/pier-supervisor"},
		{bootPath, "/tmp/pier-bootstrap.sh"},
		{filesTar, "/tmp/pier-files.tar"},
	}
	switch mode {
	case "origin":
		progress("pushing secrets — the repo comes straight from github on the VM")
	case "thin":
		progress(fmt.Sprintf("pushing local-only commits (%s) — the rest comes from github", fileMB(bundle)))
		pushes = append(pushes, struct{ local, remote string }{bundle, "/tmp/pier.bundle"})
	default:
		progress(fmt.Sprintf("pushing workspace (%s — full history; %s)", fileMB(bundle), why))
		pushes = append(pushes, struct{ local, remote string }{bundle, "/tmp/pier.bundle"})
	}
	if patch != "" {
		progress("carrying your uncommitted edits to tracked files")
		pushes = append(pushes, struct{ local, remote string }{patch, "/tmp/pier-dirty.patch"})
	}
	home, _ := os.UserHomeDir()
	if names := OAuthRemotes(home, spec.Repo); len(names) > 0 {
		progress("mcp " + strings.Join(names, ", ") + ": one-time oauth — `pier mcp login " + spec.Name + "` when it's up")
	}
	for _, p := range pushes { // biggest last: its scp meter is the wait
		if err := d.scpTo(ctx, id, p.local, p.remote); err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			// One retry: the SSM tunnel can drop right as the instance
			// settles ("lost connection" seconds after waitSSH passed).
			progress("push interrupted — retrying")
			time.Sleep(2 * time.Second)
			if err := d.scpTo(ctx, id, p.local, p.remote); err != nil {
				return nil, err
			}
		}
	}

	progress("bootstrapping (stock AMI waits for cloud-init here — `pier bake` skips that)")
	var fwd []string
	if forwardAgent {
		fwd = []string{"-A"}
	}
	if out, err := d.sshRunOpts(ctx, id, fwd, "bash /tmp/pier-bootstrap.sh"); err != nil {
		return nil, fmt.Errorf("bootstrap: %w\n%s", err, out)
	}
	// Last act: the ready tag is what List and attach trust — running
	// without it reads as still-creating everywhere. A failed tag write
	// fails the create (defer cleans up) rather than leave a session that
	// looks stuck forever.
	if _, err := d.aws(ctx, "ec2", "create-tags", "--resources", id,
		"--tags", "Key="+TagReady+",Value=1"); err != nil {
		return nil, fmt.Errorf("marking session ready: %w", err)
	}

	return &driver.Session{
		ID: id, Name: spec.Name, Repo: filepath.Base(spec.Repo), Branch: spec.Branch,
		User: me, Driver: d.Name(), State: driver.StateRunning, Created: time.Now(),
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
		{"Key": TagCreated, "Value": time.Now().UTC().Format(time.RFC3339)},
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

// resolveAMI picks the launch image: the caller's baked AMI (repo-specific,
// from config) when given, otherwise the stock Ubuntu the SSM parameter names.
func (d *Driver) resolveAMI(ctx context.Context, arch, image string) (string, error) {
	if image != "" {
		return image, nil
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
  # Container root writes files as agent, the way Docker Desktop translates
  # ownership on a Mac. Without this, any compose service running as root
  # litters bind-mounted repos with root-owned files that break host-side
  # tooling. Written before docker installs, so the daemon starts remapped.
  - path: /etc/docker/daemon.json
    permissions: "0644"
    content: |
      {"userns-remap": "agent"}
runcmd:
  - |
    set -x
    export DEBIAN_FRONTEND=noninteractive
    # unattended-upgrades can hold the dpkg lock right after boot; wait it
    # out instead of failing the install (the block deliberately has no set
    # -e, so a lost race would otherwise skip a harness silently).
    echo 'DPkg::Lock::Timeout "120";' > /etc/apt/apt.conf.d/90pier
    install -d -m 700 -o agent -g agent /home/agent/.ssh
    grep -qxF '{{PUBKEY}}' /home/agent/.ssh/authorized_keys 2>/dev/null || echo '{{PUBKEY}}' >> /home/agent/.ssh/authorized_keys
    chown agent:agent /home/agent/.ssh/authorized_keys && chmod 600 /home/agent/.ssh/authorized_keys
    # userns-remap needs subordinate id ranges: the first line maps container
    # uid/gid 0 to agent (1000), the rest of the range covers other ids.
    for f in /etc/subuid /etc/subgid; do
      grep -q '^agent:1000:1$' "$f" 2>/dev/null && continue
      { echo 'agent:1000:1'; cat "$f" 2>/dev/null; } > "$f.pier" && mv "$f.pier" "$f"
      grep -Eq '^agent:[0-9]+:65536$' "$f" || echo 'agent:300000:65536' >> "$f"
    done
    # Guard on packages stock Ubuntu lacks — it already ships tmux/git/jq/curl.
    # docker.io is the bare engine: compose and buildx are separate packages
    # (Docker Desktop/OrbStack bundle them, so "docker compose up" is table stakes).
    { command -v docker && command -v make && docker compose version && docker buildx version; } >/dev/null 2>&1 || { apt-get update -y && apt-get install -y tmux git curl jq unzip ca-certificates docker.io docker-compose-v2 docker-buildx make; }
    # AMIs baked before the remap existed have docker running unmapped: apply.
    docker info 2>/dev/null | grep -q userns || systemctl restart docker
    command -v node >/dev/null || { curl -fsSL --retry 3 https://deb.nodesource.com/setup_22.x | bash - && apt-get install -y nodejs; }
    command -v gh >/dev/null || { curl -fsSL --retry 3 https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /usr/share/keyrings/githubcli-archive-keyring.gpg && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list && apt-get update -y && apt-get install -y gh; }
    command -v claude >/dev/null || npm install -g @anthropic-ai/claude-code
    command -v codex >/dev/null || npm install -g @openai/codex
    # Headless chromium for browser MCPs/skills (playwright cache + shared libs).
    [ -e /home/agent/.cache/ms-playwright ] || { npx -y playwright install-deps chromium && sudo -Hu agent npx -y playwright install chromium; }
    getent group docker >/dev/null && usermod -aG docker agent
    grep -q 'pier/env' /home/agent/.bashrc || printf '\n[ -f ~/.config/pier/env ] && set -a && . ~/.config/pier/env && set +a\n[ -S ~/.ssh/agent.sock ] && export SSH_AUTH_SOCK=~/.ssh/agent.sock\ncd ~/work/* 2>/dev/null || true\n' >> /home/agent/.bashrc
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

// validateNames gates what create splices into the VM bootstrap (a bash
// script) and into tag values, hostnames, and key file names. A conservative
// charset beats quoting three shell layers deep: a legal-but-hostile git
// branch like "a'; rm -rf ~" must never reach the template, and it fails
// here, before anything launches and bills.
func validateNames(spec driver.CreateSpec) error {
	if err := checkName(spec.Branch); err != nil {
		return fmt.Errorf("branch %q: %w", spec.Branch, err)
	}
	if spec.Name != spec.Branch {
		if err := checkName(spec.Name); err != nil {
			return fmt.Errorf("session name %q: %w", spec.Name, err)
		}
	}
	base := filepath.Base(spec.Repo)
	for _, r := range base {
		if r <= 0x20 || r == 0x7f || strings.ContainsRune("'\"$\\`", r) {
			return fmt.Errorf("repo directory %q contains %q, which breaks the VM setup script it is spliced into — rename the directory first", base, r)
		}
	}
	return nil
}

// checkName allows the git-branch shapes people actually use. Everything else
// is rejected: the name also becomes a proxy hostname, an EC2 tag, a tmux
// target, and a log file name, so exotic characters fail somewhere far worse
// than here.
func checkName(s string) error {
	if s == "" || len(s) > 100 {
		return fmt.Errorf("must be 1-100 characters")
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-', r == '/':
		default:
			return fmt.Errorf("only letters, digits and . _ - / are allowed")
		}
	}
	if s[0] == '-' || s[0] == '.' || s[0] == '/' {
		return fmt.Errorf("cannot start with %q", s[0])
	}
	if strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".") || strings.HasSuffix(s, ".lock") {
		return fmt.Errorf("cannot end with / . or .lock")
	}
	if strings.Contains(s, "..") || strings.Contains(s, "//") {
		return fmt.Errorf("cannot contain .. or //")
	}
	return nil
}

const bootstrapTmpl = `#!/usr/bin/env bash
# pier bootstrap — runs once, as agent, on the fresh instance.
set -euo pipefail

# Stock AMI: harness install still running under cloud-init; wait it out.
# Guard on binaries the stock image LACKS: Ubuntu ships git and tmux, so
# guarding on those skipped the wait everywhere (the same trap the
# user-data idempotency guards document) and .pier-setup.sh raced the
# node/docker/claude installs it depends on.
command -v docker >/dev/null && command -v node >/dev/null && command -v claude >/dev/null || sudo cloud-init status --wait >/dev/null || true

sudo install -m 0755 /tmp/pier-supervisor /usr/local/bin/pier-supervisor
sudo tee /etc/systemd/system/pier-supervisor.service >/dev/null <<'UNIT'
[Unit]
Description=pier session supervisor (self-park watchdog)
After=multi-user.target

[Service]
User=agent
RuntimeDirectory=pier
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
{{GITCONFIG}}
if [ -n '{{ORIGIN}}' ]; then git remote add origin '{{ORIGIN}}'; fi
# ssh origin = fetch rides the laptop's forwarded agent; pre-trust github.
case '{{ORIGIN}}' in git@*|ssh://*) mkdir -p "$HOME/.ssh" && ssh-keyscan github.com >> "$HOME/.ssh/known_hosts" 2>/dev/null || true ;; esac
# gh brokers git credentials for https origin fetches; on a stock AMI it may
# still be installing under cloud-init — wait only when it's load-bearing.
if [ '{{MODE}}' != full ] && [ -n "${GH_TOKEN:-}" ] && ! command -v gh >/dev/null; then sudo cloud-init status --wait >/dev/null || true; fi
{ command -v gh >/dev/null && [ -n "${GH_TOKEN:-}" ] && gh auth setup-git >/dev/null 2>&1; } || true
case '{{MODE}}' in
  origin) git fetch -q --no-tags origin {{SHA}} ;;
  thin)   git fetch -q --no-tags origin && git fetch -q /tmp/pier.bundle {{EXPORTREF}} ;;
  *)      git fetch -q /tmp/pier.bundle {{EXPORTREF}} ;;
esac
git reset -q --hard {{SHA}}
# Uncommitted edits to tracked files, exactly as the laptop had them (a
# failed apply fails the create — better than silently missing work).
if [ -f /tmp/pier-dirty.patch ]; then git apply /tmp/pier-dirty.patch; fi
tar -xf /tmp/pier-files.tar -C . --strip-components=1 repo 2>/dev/null || true

# Codex records folder trust in config.toml; pre-trust the workdir the user
# created this session from (claude's equivalent rides the ~/.claude.json seed).
mkdir -p "$HOME/.codex"
grep -q 'work/{{REPO}}' "$HOME/.codex/config.toml" 2>/dev/null || printf '\n[projects."/home/agent/work/{{REPO}}"]\ntrust_level = "trusted"\n' >> "$HOME/.codex/config.toml"

# Terminal scrollback can't reach into tmux, so without mouse mode a session
# reads as "can't scroll up". Seed only when no ~/.tmux.conf rode the
# manifest; must land before the server starts below.
if [ ! -f "$HOME/.tmux.conf" ]; then
  printf 'set -g mouse on\nset -g history-limit 50000\n' > "$HOME/.tmux.conf"
fi

# SSH_AUTH_SOCK points at the attach-refreshed symlink (dangling until the
# first attach forwards an agent; harmless when it never does).
tmux has-session -t main 2>/dev/null || tmux new-session -d -s main -e "SSH_AUTH_SOCK=$HOME/.ssh/agent.sock" -c "$HOME/work/{{REPO}}"
# Background setup, after checkout + patch + .pier-include extras are all in
# place: the repo's .pier-setup.sh, unless a PIER_SETUP_SCRIPT override rode
# the tar (outer double quotes expand $setup now, into the single-quoted
# bash -c; \$ defers the rest to run time). The outcome must be impossible to
# miss — a failed setup used to vanish with its window: ~/.pier-setup.status
# holds "running" then the exit code (the supervisor beacons it to ls/TUI),
# the log's last line says done/FAILED, and a failed window renames to
# setup-failed and stays open instead of closing. The rename targets its own
# pane id: with a client attached, a bare rename-window can resolve "current
# window" to the attached client's window and mislabel the user's shell.
setup=./.pier-setup.sh
if [ -f "$HOME/.config/pier/setup.sh" ]; then setup="$HOME/.config/pier/setup.sh"; fi
# Presence is the signal, not the exec bit: git only carries +x when the
# author remembered chmod, and gating on -x skipped a committed 0644
# .pier-setup.sh with no trace — the one silent failure setup promises not
# to have. bash runs it either way.
if [ -f "$setup" ]; then
  tmux new-window -d -t main -n setup "bash -c 'set -a; . ~/.config/pier/env 2>/dev/null; set +a; cd ~/work/{{REPO}} || exit 1; echo running > ~/.pier-setup.status; bash $setup 2>&1 | tee ~/.pier-setup.log; c=\${PIPESTATUS[0]}; echo \$c > ~/.pier-setup.status; if [ \$c -eq 0 ]; then echo \"pier setup: done\" >> ~/.pier-setup.log; else echo \"pier setup: FAILED (exit \$c)\" | tee -a ~/.pier-setup.log; tmux rename-window -t \$TMUX_PANE setup-failed; exec sleep infinity; fi'"
fi

# Attach gates on this marker: nobody lands in a half-set-up session. Written
# after the repo checkout and tmux session exist; deliberately NOT after
# .pier-setup.sh, which runs async in its tmux window.
touch "$HOME/.pier-bootstrapped"

rm -f /tmp/pier.bundle /tmp/pier-files.tar /tmp/pier-dirty.patch /tmp/pier-supervisor /tmp/pier-bootstrap.sh
echo bootstrapped
`

func renderBootstrap(spec driver.CreateSpec, mode, sha, origin string) string {
	var gitcfg []string
	line := func(args ...string) {
		out, err := gitOut(spec.Repo, args...)
		if err == nil && out != "" && !strings.Contains(out, "'") {
			switch args[len(args)-1] {
			case "user.name":
				gitcfg = append(gitcfg, "git config user.name '"+out+"'")
			case "user.email":
				gitcfg = append(gitcfg, "git config user.email '"+out+"'")
			}
		}
	}
	line("config", "user.name")
	line("config", "user.email")
	return strings.NewReplacer(
		"{{REPO}}", filepath.Base(spec.Repo),
		"{{BRANCH}}", spec.Branch,
		"{{GITCONFIG}}", strings.Join(gitcfg, "\n"),
		"{{MODE}}", mode,
		"{{SHA}}", sha,
		"{{ORIGIN}}", origin,
		"{{EXPORTREF}}", exportRef(spec.Name),
	).Replace(bootstrapTmpl)
}

// --- workspace bundle + secrets ------------------------------------------------

func gitOut(repo string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

// originInfo decides how the repo reaches the VM. mode "origin": the base
// commit is reachable from an origin ref, so the VM fetches it from GitHub
// and nothing repo-shaped rides the SSM tunnel. "thin": GitHub has the
// shared history, ship only the local-delta bundle. "full": no usable
// GitHub origin — ship everything. The returned url is what the VM's origin
// remote is set to (GitHub ssh forms become https so the GH_TOKEN credential
// helper can auth; when only the laptop's ssh agent can auth, Create swaps
// the ssh URL back in and forwards the agent — ~/.ssh never travels either way).
func originInfo(repo, sha string) (mode, url string) {
	raw, err := gitOut(repo, "remote", "get-url", "origin")
	if err != nil || raw == "" {
		return "full", ""
	}
	fetchable := fetchURL(raw)
	if fetchable == "" {
		return "full", raw // non-GitHub origin: still add the remote, bundle the data
	}
	// --contains reflects the last local fetch: stale-empty just means a
	// bigger bundle; stale-nonempty (force-push) fails the VM fetch loudly.
	if out, err := gitOut(repo, "branch", "-r", "--contains", sha, "--list", "origin/*"); err == nil && out != "" {
		return "origin", fetchable
	}
	return "thin", fetchable
}

// GitHubToken finds a GitHub credential without insisting on any one tool:
// gh's login first, then whatever https credential the user's git already
// pushes with (osxkeychain, credential-store, ...). Never prompts. Sessions
// use it for private-repo fetches and for `git push`/PRs from the VM — the
// only piece of pier that wants a GitHub credential at all.
func GitHubToken() string {
	if out, err := exec.Command("gh", "auth", "token").Output(); err == nil {
		if t := strings.TrimSpace(string(out)); t != "" {
			return t
		}
	}
	fill := exec.Command("git", "credential", "fill")
	fill.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
	fill.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=")
	out, err := fill.Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if t, ok := strings.CutPrefix(strings.TrimSpace(line), "password="); ok && t != "" {
			return t
		}
	}
	return ""
}

// originReachable proves, before skipping the bundle, that the exact fetch
// the VM will run works: same https URL, same auth (GH_TOKEN or anonymous).
// Local credential helpers are disabled so a keychain PAT can't vouch for a
// VM that won't have it. Sessions have open egress, so laptop-success is a
// safe proxy; on failure the bundle path takes over — slower, never wrong.
// Covers: private repo without gh logged in, revoked/SSO-gated tokens, etc.
func originReachable(ctx context.Context, url, token string) bool {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	args := []string{"-c", "credential.helper="} // reset the helper list
	if token != "" {
		args = append(args, "-c", `credential.helper=!f() { echo username=x-access-token; echo "password=$GH_TOKEN"; }; f`)
	}
	cmd := exec.CommandContext(ctx, "git", append(args, "ls-remote", "--heads", url)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "GH_TOKEN="+token)
	return cmd.Run() == nil
}

// sshOriginUsable reports whether a session could fetch the ssh origin with
// the laptop's ssh agent forwarded: an agent is running with keys, and
// ls-remote over ssh succeeds locally without prompting. Tried only when no
// https credential worked. Agent forwarding is a relay, not a copy — the key
// never leaves the laptop; the VM borrows it for the bootstrap fetch (and for
// pushes while attached).
func sshOriginUsable(ctx context.Context, repo string) bool {
	raw, err := gitOut(repo, "remote", "get-url", "origin")
	if err != nil || !isSSHURL(raw) {
		return false
	}
	if exec.Command("ssh-add", "-l").Run() != nil {
		return false // no agent, or no keys in it — nothing to forward
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--heads", raw)
	cmd.Env = append(os.Environ(), "GIT_SSH_COMMAND=ssh -o BatchMode=yes -o ConnectTimeout=10")
	return cmd.Run() == nil
}

func isSSHURL(u string) bool {
	return strings.HasPrefix(u, "git@") || strings.HasPrefix(u, "ssh://")
}

var sshGithub = regexp.MustCompile(`^(?:ssh://)?git@github\.com[:/](.+?)(?:\.git)?$`)

// fetchURL normalizes a GitHub origin to https (empty for non-GitHub hosts).
func fetchURL(raw string) string {
	if m := sshGithub.FindStringSubmatch(raw); m != nil {
		return "https://github.com/" + m[1]
	}
	if strings.HasPrefix(raw, "https://github.com/") {
		return raw
	}
	return ""
}

var errEmptyBundle = fmt.Errorf("empty bundle")

// exportRef names the temporary ref a create's bundle travels under. Derived
// from the session name so concurrent creates in the same repo (two quick
// TUI spawns) don't race on one shared ref — the first create's deferred
// delete used to yank it out from under the second's `git bundle create`.
// Slashes flatten so no ref component can start with a dot the name
// validation only blocks at position zero.
func exportRef(name string) string {
	return "refs/pier/export-" + strings.ReplaceAll(name, "/", "-")
}

// gitBundle packs history reachable from sha into a bundle exposing a single
// ref (the create's exportRef) that the VM fetches — includes uncommitted
// nothing, clean by construction. thin subtracts everything origin already
// has, leaving prerequisites the VM satisfies by fetching origin first.
func gitBundle(repo, sha, dst, ref string, thin bool) error {
	if _, err := gitOut(repo, "update-ref", ref, sha); err != nil {
		return err
	}
	defer gitOut(repo, "update-ref", "-d", ref)
	args := []string{"-C", repo, "bundle", "create", dst, ref}
	if thin {
		args = append(args, "--not", "--remotes=origin")
	}
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		if strings.Contains(strings.ToLower(string(out)), "empty bundle") {
			return errEmptyBundle
		}
		return fmt.Errorf("git bundle: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func fileMB(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "?"
	}
	return fmt.Sprintf("%.1f MB", float64(fi.Size())/1e6)
}

// claudeSeed builds a minimal ~/.claude.json for the VM: theme +
// onboarding-done (skips the theme picker), the MCP servers that can actually
// run on Linux — user-scope ones plus the source repo's project-scoped ones,
// which follow the repo onto the VM workdir (their auth lives in env blocks /
// headers and travels; OAuth'd remotes re-auth in-session) — and pre-trust
// for the workdir: the trust dialog would only re-ask what creating the
// session already answered. The rest of the local state file (history,
// per-path project state) is laptop-specific noise and deliberately stays home.
func claudeSeed(home, srcRepo, workdir string) []byte {
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return nil
	}
	var local struct {
		Theme                  string                     `json:"theme"`
		HasCompletedOnboarding bool                       `json:"hasCompletedOnboarding"`
		McpServers             map[string]json.RawMessage `json:"mcpServers"`
		Projects               map[string]struct {
			McpServers map[string]json.RawMessage `json:"mcpServers"`
		} `json:"projects"`
	}
	if json.Unmarshal(b, &local) != nil || !local.HasCompletedOnboarding {
		return nil
	}
	proj := map[string]any{
		"hasTrustDialogAccepted":        true,
		"hasCompletedProjectOnboarding": true,
	}
	if mcp := portableMCP(local.Projects[srcRepo].McpServers); len(mcp) > 0 {
		proj["mcpServers"] = mcp
	}
	seed := map[string]any{
		"hasCompletedOnboarding": true,
		"projects":               map[string]any{workdir: proj},
	}
	if local.Theme != "" {
		seed["theme"] = local.Theme
	}
	if mcp := portableMCP(local.McpServers); len(mcp) > 0 {
		seed["mcpServers"] = mcp
	}
	out, _ := json.Marshal(seed)
	return out
}

// OAuthRemotes names the seeded remote MCP servers with no static auth
// header. Their OAuth tokens live in the OS keychain and rotate on refresh,
// so copying them would let two machines revoke each other — each session
// instead needs one `pier mcp login <name>` round. Static-auth servers (env
// blocks, Authorization headers) travel whole and never appear here.
func OAuthRemotes(home, srcRepo string) []string {
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return nil
	}
	return OAuthRemoteNames(b, srcRepo)
}

// OAuthRemoteNames is the pure parser behind OAuthRemotes: it reads any
// ~/.claude.json bytes (laptop or session — pass the matching project dir),
// so the CLI can ask a running session what still needs a browser approval.
func OAuthRemoteNames(b []byte, projectDir string) []string {
	var local struct {
		McpServers map[string]json.RawMessage `json:"mcpServers"`
		Projects   map[string]struct {
			McpServers map[string]json.RawMessage `json:"mcpServers"`
		} `json:"projects"`
	}
	if json.Unmarshal(b, &local) != nil {
		return nil
	}
	set := map[string]bool{}
	for _, servers := range []map[string]json.RawMessage{local.McpServers, local.Projects[projectDir].McpServers} {
		for name, raw := range servers {
			var s struct {
				Type    string            `json:"type"`
				Headers map[string]string `json:"headers"`
			}
			if json.Unmarshal(raw, &s) == nil && (s.Type == "http" || s.Type == "sse") && len(s.Headers) == 0 {
				set[name] = true
			}
		}
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// portableMCP drops stdio servers whose command is a macOS-only path — on
// the Linux VM they would just render as failed servers.
func portableMCP(servers map[string]json.RawMessage) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for name, raw := range servers {
		var s struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(raw, &s)
		if c := s.Command; strings.HasPrefix(c, "/Applications/") || strings.HasPrefix(c, "/Users/") ||
			strings.HasPrefix(c, "/opt/homebrew/") || strings.HasPrefix(c, "/System/") {
			continue
		}
		out[name] = raw
	}
	return out
}

// pierIncludeFiles lists the repo files (repo-relative) named by a repo-root
// .pier-include — the ONLY loose-file channel: nothing untracked or ignored
// ships without a line here (tracked content arrives via the fetch, dirty
// edits to it via the patch). One path or glob per line (relative to the
// root; * ? [] per segment, no **), # comments, a directory line carries its
// whole subtree. A listed path travels as it sits on disk — no git-status
// distinction — and the tar extracts after checkout + patch, so listed
// content wins. No file, or an empty one, means nothing extra travels.
func pierIncludeFiles(repoRoot string) []string {
	b, err := os.ReadFile(filepath.Join(repoRoot, ".pier-include"))
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSuffix(strings.TrimSpace(line), "/")
		if line == "" || strings.HasPrefix(line, "#") || !filepath.IsLocal(line) {
			continue
		}
		// WalkDir on a glob match handles files and directories uniformly
		// (a file path walks as just itself); .git and non-regular skipped.
		matches, _ := filepath.Glob(filepath.Join(repoRoot, line))
		for _, m := range matches {
			_ = filepath.WalkDir(m, func(path string, e fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if e.IsDir() {
					if e.Name() == ".git" {
						return filepath.SkipDir
					}
					return nil
				}
				if !e.Type().IsRegular() {
					return nil
				}
				if rel, err := filepath.Rel(repoRoot, path); err == nil && filepath.IsLocal(rel) {
					seen[rel] = true
				}
				return nil
			})
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// dirtyPatch captures uncommitted work on tracked files — edits, staged
// adds, deletions, binary-safe — as one patch the bootstrap applies right
// after checkout, so the session's working tree starts exactly as the
// laptop's (staged edits arrive unstaged). Untracked files are
// .pier-include's business. A clean tree writes nothing.
func dirtyPatch(repoRoot, dst string) (bool, error) {
	out, err := exec.Command("git", "-C", repoRoot, "diff", "--binary", "HEAD").Output()
	if err != nil {
		return false, fmt.Errorf("git diff: %w", err)
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		return false, nil
	}
	return true, os.WriteFile(dst, out, 0o600)
}

// envFilesNotCarried is the fail-loud affordance for "no default env
// transfer": env files the tree has (untracked or ignored, any depth) that
// the tar is NOT carrying. A session whose app dies on a missing env file
// should have said so at create. Wholly-ignored dirs collapse (--directory)
// so node_modules fixtures don't count; tracked ones arrive with the fetch.
func envFilesNotCarried(repoRoot string, carried []string) []string {
	have := map[string]bool{}
	for _, p := range carried {
		have[p] = true
	}
	var miss []string
	for _, args := range [][]string{
		{"ls-files", "-z", "-o", "--exclude-standard"},
		{"ls-files", "-z", "-o", "-i", "--exclude-standard", "--directory"},
	} {
		listing, err := gitOut(repoRoot, args...)
		if err != nil {
			return nil
		}
		for _, p := range strings.Split(listing, "\x00") {
			if p == "" || strings.HasSuffix(p, "/") {
				continue
			}
			if ok, _ := filepath.Match(".env*", filepath.Base(p)); ok && !have[p] {
				miss = append(miss, p)
			}
		}
	}
	sort.Strings(miss)
	return miss
}

// setupScriptOverride resolves PIER_SETUP_SCRIPT: the named
// script travels in the tar and the bootstrap runs it instead of the repo's
// ./.pier-setup.sh. Relative paths resolve against the repo root, ~ against
// home. A set-but-missing path returns a warning, not an error — a typo'd
// env var shouldn't brick creates, but it must not be silent either.
func setupScriptOverride(repoRoot string) (path, warn string) {
	v := os.Getenv("PIER_SETUP_SCRIPT")
	if v == "" {
		return "", ""
	}
	if strings.HasPrefix(v, "~/") {
		home, _ := os.UserHomeDir()
		v = filepath.Join(home, v[2:])
	} else if !filepath.IsAbs(v) {
		v = filepath.Join(repoRoot, v)
	}
	if fi, err := os.Stat(v); err != nil || !fi.Mode().IsRegular() {
		return "", "PIER_SETUP_SCRIPT: " + v + " not found — running the repo's .pier-setup.sh (if any) instead"
	}
	return v, ""
}

// buildFilesTar packs, into one tar: manifest files/dirs under $HOME (prefix
// home/), the repo files named by .pier-include (relative paths kept, prefix
// repo/), the PIER_SETUP_SCRIPT override (as home/.config/pier/setup.sh),
// and a generated home/.config/pier/env with the session tokens. The
// bootstrap extracts the two prefixes to the right places.
func buildFilesTar(dst string, manifest []string, repoRoot string, env map[string]string, setupSrc string) error {
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
			if err != nil {
				return nil // missing manifest entries are fine
			}
			if e.IsDir() {
				if e.Name() == ".git" { // plugin/marketplace checkouts
					return filepath.SkipDir
				}
				return nil
			}
			if !e.Type().IsRegular() {
				return nil // symlinks, sockets
			}
			r, _ := filepath.Rel(home, path)
			info, _ := e.Info()
			return addFile(path, "home/"+r, info.Mode())
		})
		if err != nil {
			return err
		}
	}

	if seed := claudeSeed(home, repoRoot, Workspace+"/"+filepath.Base(repoRoot)); seed != nil {
		if err := tw.WriteHeader(&tar.Header{Name: "home/.claude.json", Mode: 0o600, Size: int64(len(seed))}); err != nil {
			return err
		}
		if _, err := tw.Write(seed); err != nil {
			return err
		}
	}

	for _, rel := range pierIncludeFiles(repoRoot) {
		p := filepath.Join(repoRoot, rel)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			// Widen owner-only modes: the VM's docker daemon is
			// userns-remapped, so container uids are unprivileged host uids
			// and a laptop-tight 0600 .env bind-mounts unreadable — Docker
			// Desktop ignores host perms, so the repo works locally and
			// fails only here. The VM is single-user; o+r gives nothing away.
			mode := fi.Mode().Perm() | 0o444
			if mode&0o100 != 0 {
				mode |= 0o111
			}
			if err := addFile(p, "repo/"+rel, mode); err != nil {
				return err
			}
		}
	}

	if setupSrc != "" {
		// 0755 regardless of the source's mode: the bootstrap gates on -x.
		if err := addFile(setupSrc, "home/.config/pier/setup.sh", 0o755); err != nil {
			return err
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
