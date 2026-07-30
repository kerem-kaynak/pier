# pier — v1 design spec

Status: settled design, pre-implementation (2026-07-30). This document is the
source of truth; TODOs in code point here.

## 1. Product

Coding-agent sessions (Claude Code / Codex) as micro-VMs on **your own AWS or
GCP account**, parking to near-zero cost when idle and resuming where they
left off. The daily loop:

```
cd ~/code/myapp
pier payments-retry     # new cloud session on branch payments-retry, attached in ~60-90s
# ... work with claude/codex in the remote tmux, detach or close the tab ...
# the session parks itself once it goes quiet
pier                    # TUI: sessions + states; pick one to reattach (~30s from parked)
```

A session comes prepacked with: the repo on a fresh branch, the dev
environment (repo's `.pier-setup.sh`, run automatically), the user's secrets
(one-way copy from the laptop), and both agent harnesses installed. Everything
else is bloat.

**Hard requirements:** runs on AWS *and* GCP easily; scale-to-(near)zero with
pick-back-up; one-command TUI; one-time wizard setup; teams share an account
without collisions; low time-to-first-interaction.

**Non-goals (v1):** sharing/collab features, hosted control plane, web UI,
model-auth brokering (harness configs are copied, nothing more), Windows.

## 2. Session model

Session = **one micro-VM + its persistent disk**. Park = native instance stop
(disk persists, RAM lost). Resume = instance start. Destroy = terminate +
delete disk. This is why VMs beat the container services for this product:

- ECS/Fargate: tasks are immutable and can't stop/resume with local state.
- Cloud Run: no interactive TTY semantics, no persistent local disk.
- Kubernetes: presupposes a cluster (violates easy-setup), no clean
  scale-from-zero per session, docker-in-session is painful. Deferred as a
  possible third driver for teams already living in k8s.

| state    | meaning                                  | cost            |
|----------|------------------------------------------|-----------------|
| creating | provisioning / first boot                | running rate    |
| running  | reachable, a client attached             | ~$0.03/h        |
| working  | reachable, detached, agent busy          | ~$0.03/h        |
| idle     | reachable, quiet; park countdown running | ~$0.03/h        |
| parked   | instance stopped, disk only              | ~$3–4/mo (40GB) |
| dead     | terminated outside pier                  | $0              |

Honest seam: in v1 a park loses running processes (including the tmux server
and any in-flight agent run) — files, git state, and installed tools survive.
v1.1 upgrade: EC2 hibernate / GCE suspend to preserve RAM across park.

## 3. Drivers

One Go interface (`internal/driver`), two v1 implementations. All state lives
in provider APIs + tags/labels — **no server, no database, no laptop daemon**.

### aws-ec2
- t4g.medium default (2 vCPU / 4GB, ~$0.034/h), Ubuntu 24.04 arm64 (SSM
  parameter-resolved AMI, or the baked AMI), gp3 root 40GB.
- Default VPC + public IP + `pier-egress-only` SG (zero inbound). Orgs
  without a default VPC set `subnet` in config.
- Instance profile `pier-session`: `AmazonSSMManagedInstanceCore` only.
- `InstanceInitiatedShutdownBehavior=stop` → in-VM `shutdown -h now` parks.
- Tags: `pier:managed`, `pier:user` (STS ARN), `pier:session`, `pier:repo`.

### gcp-gce
- e2-medium default (~$0.033/h), Ubuntu 24.04, pd-balanced 40GB.
- Default network + public IP; single firewall rule `pier-allow-iap-ssh`
  (35.235.240.0/20 → :22, target tag `pier-session`).
- Instances run with **no service account, no scopes**.
- `shutdown -h now` lands in TERMINATED with disks intact (= parked).
- Labels mirror the AWS tags (`pier-managed`, `pier-user`, ...).

## 4. Attach

- AWS: `aws ssm start-session` (interactive-command document →
  `sudo -iu agent tmux attach`) shelling out to `session-manager-plugin`.
- GCP: `gcloud compute ssh <name> --tunnel-through-iap -- -t '... tmux attach'`.

Zero inbound networking on both clouds; every attach is IAM-authenticated and
audited (CloudTrail / IAP logs). Inside the VM, work happens as user `agent`
in a tmux session under `/home/agent/work/<repo>`.

## 5. Identity & teams

The caller's cloud identity (STS `GetCallerIdentity` ARN; GCP authed
principal) is written to `pier:user` at create and filtered on at list. Many
devs share one account with zero coordination: no name collisions, no shared
state, nothing to configure. `pier ls --all` can show teammates' sessions
(read-only visibility); everything else operates only on your own.

## 6. Parking policy (supervisor)

`pier-supervisor` runs in every VM with **zero cloud credentials** — parking
is the VM shutting itself down. Every 30s it derives:

- `attached` — tmux has ≥1 client
- `busy` — agent process-tree CPU above threshold in window, or recent pty
  output (heuristic; harnesses stay stock, no hooks)

Rules: attached or busy resets the idle clock; detached+quiet past
`idle_timeout` → park; detached+busy past `unattended_cap` → park (runaway
brake, so a looping agent can't burn compute for days). Reason lands in
`/run/pier/status.json` for the TUI.

Configurable at wizard time and in config: `idle_timeout` (default 30m,
`"never"` allowed), `unattended_cap` (default 8h, disable-able). Per-session:
`--idle`, `--no-park`, `--cap`, and `pier keep <match>` to pin a session.

## 7. Speed

Targets: **create → attached 60–90s; resume → attached ~30s** (measured
numbers live in spike/README.md).

1. **`pier bake`** — prebaked per-driver image: agent user, tmux, git, gh,
   docker, mise, claude + codex, supervisor preinstalled. ~$1/mo snapshot
   storage. Offered as the wizard's last step; `pier bake` refreshes it.
2. **Async create pipeline** — launch the instance first; prepare the git
   bundle + secrets while it boots; attach the moment SSM/IAP answers; push
   code and run `.pier-setup.sh` in a background tmux pane with progress
   visible in the session. You're typing to the agent while deps install.
3. **`warm_pool = N`** (optional) — N blank parked VMs; create becomes
   resume+claim (~35–45s). Off by default until the spike proves it earns
   its keep.

## 8. Secrets

One-way copy from the laptop at create; never stored anywhere else, never
written back. Sources (wizard-detected, confirmed into the manifest):

- repo `.env*` (create-time globs)
- `~/.codex/` (auth.json, config.toml)
- `~/.claude/` settings, `CLAUDE.md`, agents
- gh token (`gh auth token`) for git push/PRs
- Claude subscription auth on macOS lives in the Keychain → one-time
  `claude setup-token` during the wizard, injected as an env var in sessions.

## 9. Setup wizard

`pier setup` — four phases, under 3 minutes:

1. **Detect** — CLIs, profiles/projects, plugin, gh, harness configs; all
   found values become defaults.
2. **Ask** — ≤6 questions (cloud, profile/region or project/zone, size,
   parking policy, secrets manifest, bake now?).
3. **Apply** — idempotent groundwork, printed before it's touched:
   - AWS: role+profile `pier-session` (SSM core policy), SG
     `pier-egress-only` in the default VPC.
   - GCP: enable compute + IAP APIs, firewall `pier-allow-iap-ssh`.
   `--print-admin` emits exactly this list for an admin when the dev lacks
   IAM rights. `pier teardown` reverses it.
4. **Doctor** — quota headroom, connectivity, plugin; writes
   `~/.config/pier/config.toml`; prints `cd <repo> && pier <branch>`.

Second dev on a prepared account: detect finds groundwork (`Existed`),
creates nothing, done in ~90s.

## 10. Quota & scaling UX

Doctor checks vCPU headroom at setup; every create re-checks opportunistically
during the boot wait (costs nothing). Quota-exceeded errors are caught and
answered with a one-keystroke service-quota increase request. The TUI header
shows headroom (e.g. `12/32 vCPU`).

## 11. CLI

```
pier                    TUI: list / attach / new / delete / pin
pier <branch> [base]    create from cwd repo (branch off base, default HEAD) and attach
pier ls                 list own sessions (--all: whole account)
pier attach <match>     reattach; resumes if parked
pier rm <match>         destroy (instance + disk)
pier keep <match>       disable auto-park for a session
pier setup              wizard (--print-admin for the no-IAM-rights path)
pier bake               build/refresh the prebaked image
pier doctor             checks
pier teardown           remove account groundwork
```

## 12. Config (`~/.config/pier/config.toml`)

```toml
default_driver = "aws-ec2"
idle_timeout   = "30m"        # or "never"
unattended_cap = "8h"
warm_pool      = 0

[aws]
profile       = "default"
region        = "eu-central-1"
instance_type = "t4g.medium"
disk_gib      = 40
# subnet      = "subnet-..."  # only for orgs without a default VPC
# baked_ami   = "ami-..."     # written by `pier bake`

[gcp]
project      = "my-project"
zone         = "europe-west3-a"
machine_type = "e2-medium"
disk_gib     = 40

[secrets]
manifest = ["~/.codex/", "~/.claude/settings.json", "~/.claude/CLAUDE.md"]
```

## 13. v1 cut line

In: both drivers, TUI, wizard (+print-admin, teardown), bake, async create
pipeline, supervisor parking (+keep/pin), one-way secrets copy, doctor, quota
UX. Out (v1.1+): hibernate/suspend park, k8s driver, warm-pool auto-tuning,
`ls --all` write ops, Windows.

## 14. Load-bearing bets → spikes

| bet                                                        | spike       | status   |
|------------------------------------------------------------|-------------|----------|
| `shutdown -h now` parks (EC2 → stopped, not terminated)    | spike/aws.sh | pending |
| boot → SSM-ready time supports 60–90s TTFI                 | spike/aws.sh | pending |
| stop → start → ready ≈ 30s resume                          | spike/aws.sh | pending |
| SSM interactive TTY feels like ssh (manual, `--keep` mode) | spike/aws.sh | pending |
| GCE: same four on IAP + TERMINATED-with-disks              | spike/gcp.sh | pending (script untested) |
