# pier

Coding-agent sessions (Claude Code / Codex) as micro-VMs on **your own AWS
account** (GCP planned). Each session is a full dev environment — your repo on
its own branch, your secrets, your tools — that parks itself to near-zero cost
when idle and resumes with files, branches, and credentials exactly as you
left them. Your terminal setup stays untouched: pier hands you a tmux session
over SSH in whatever terminal you already use.

```
pier setup             # once: wizard creates the tiny account groundwork + bakes the image
cd ~/code/myapp
pier payments-retry    # new session: your branch, your secrets, your dev env (~60-90s baked)
pier                   # TUI: list / attach / new / delete / pin
pier attach payments   # reattach anytime; parked sessions resume in ~20-30s
```

**Status: v1 implemented (AWS), E2E hardening in progress.** Design and
verified spike numbers: [docs/SPEC.md](docs/SPEC.md), [spike/](spike/).

## How it works

- A session is one EC2 instance + its EBS disk, tagged and namespaced by your
  caller identity. No database, no server, no daemon — state lives in EC2.
- Everything rides **SSH over an SSM tunnel**: zero inbound ports, no public
  IPs, per-session ed25519 keys. The instance role carries SSM and nothing else;
  **no cloud credentials ever enter the VM**.
- A tiny in-VM supervisor watches tmux clients, agent CPU, and pty activity;
  when you detach and the agent goes quiet, the VM shuts itself down —
  EC2-stop, disk intact (≈ $3-4/mo instead of ~$0.03/h). `pier attach` starts
  it again. A runaway cap parks even "busy" sessions after N hours detached.
- The supervisor also reports sustained cpu/mem pressure (kernel PSI):
  `pier ls` shows `working (strained)` and `pier resize <session> t4g.xlarge`
  grows the VM through one ~40s park/resume cycle, disk and state intact.
  Deliberately not automatic — the VM holds no cloud credentials.
- `pier bake` prebakes an AMI with the harnesses installed, cutting session
  creation from minutes to ~60-90s. The same cloud-init runs on stock and
  baked images — every step is guarded, so baking is optional.
- Your repo travels as a git bundle (full history, committed state only);
  secrets travel as an explicit manifest (`~/.claude`, `~/.codex`, repo
  `.env*`) plus tokens (`gh auth token`, `claude setup-token`).
- If the repo has a `.pier-setup.sh`, it runs asynchronously in a tmux window
  on first boot (deps, migrations, seeds).

## Build

```
make          # cross-compiles the in-VM supervisor, embeds it, builds ./pier
make install  # cp to $(go env GOPATH)/bin
```

Requires: `aws` CLI v2, `session-manager-plugin`, `git`, OpenSSH. `pier doctor`
checks all of it.
