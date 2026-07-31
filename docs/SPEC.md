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
delete disk. Resize = the same park/resume cycle with an instance-type change
in between (providers only allow type changes while stopped — so a strained
session grows with ~40s of downtime, same CPU arch only). This is why VMs beat
the container services for this product:

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
- Tags: `pier:managed`, `pier:user` (STS ARN), `pier:session`, `pier:repo`,
  `pier:branch`.
- No AWS SDK: the driver shells out to `aws --output json` (the CLI is already
  required for the SSM plugin, and SSO/profiles/MFA come for free).

### gcp-gce (parked — implemented after AWS is fully E2E-tested)
- e2-medium default (~$0.033/h), Ubuntu 24.04, pd-balanced 40GB.
- Default network + public IP; single firewall rule `pier-allow-iap-ssh`
  (35.235.240.0/20 → :22, target tag `pier-session`).
- Instances run with **no service account, no scopes**.
- `shutdown -h now` lands in TERMINATED with disks intact (= parked).
- Labels mirror the AWS tags (`pier-managed`, `pier-user`, ...).

## 4. Attach (and every other byte to the VM)

One mechanism: **OpenSSH over the SSM tunnel** — `ProxyCommand aws ssm
start-session --document-name AWS-StartSSHSession`. Attach is `ssh -t agent@<id>
tmux new -A -s main`; exec is one-shot ssh; file push is scp. Each session
gets its own ed25519 keypair, generated at create, pubkey injected via
cloud-init, private key under `~/.config/pier/keys/`. (GCP equivalent later:
`gcloud compute ssh --tunnel-through-iap`.)

Zero inbound networking; every connection is IAM-authenticated and audited
(CloudTrail). Inside the VM, work happens as user `agent` in a tmux session
under `/home/agent/work/<repo>`.

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

The beacon also carries a `strained` flag — kernel PSI (`/proc/pressure/cpu`
avg60 ≥ 40, memory ≥ 10) — which ls/TUI render as `working (strained)`, the
nudge toward `pier resize`. The supervisor never resizes anything itself: the
VM has no cloud credentials (invariant), and auto-scaling is a surprise-cost
footgun. The human is the trigger; the fix is one command.

## 7. Speed

Targets: **create → attached 60–90s; resume → attached ~30s** (measured
numbers live in spike/README.md).

1. **`pier bake`** — prebaked per-driver image: agent user, tmux, git, gh,
   docker, mise, claude + codex, supervisor preinstalled. ~$1/mo snapshot
   storage. Offered as the wizard's last step; `pier bake` refreshes it.
2. **Overlapped create** — launch the instance first; build the git bundle +
   secrets tar while it boots; push and bootstrap the moment sshd answers;
   `.pier-setup.sh` runs asynchronously in a background tmux window while you
   type to the agent. (On a stock AMI the bootstrap must wait for cloud-init's
   harness install — minutes, exactly once; bake removes it.)
3. **Origin-first workspace fetch** — the SSM tunnel moves ~1 MB/s, so the
   repo avoids it whenever possible. If the base commit is reachable from a
   GitHub origin ref, the VM fetches straight from GitHub (~100× faster;
   auth via the GH_TOKEN credential helper — ssh origins are rewritten to
   https, which also makes push work in sessions). Local-only commits on top
   of pushed history travel as a thin delta bundle (KBs). Before skipping the
   bundle, a preflight `ls-remote` proves the fetch works with exactly the
   auth a session gets (GH_TOKEN or anonymous; local credential helpers
   disabled) — private repos without a usable token degrade automatically.
   The full-history bundle over SSM remains the universal fallback: no
   origin, non-GitHub host, never-pushed history, failed preflight.

Cut for v1 (numbers didn't justify the moving parts): warm pools, mid-create
quota polling.

## 8. Secrets

One-way copy from the laptop at create; never stored anywhere else, never
written back. Sources (wizard-detected, confirmed into the manifest):

- repo `.env*` (create-time globs)
- `~/.codex/` (auth.json, config.toml)
- `~/.claude/` settings, `CLAUDE.md`, agents
- gh token (`gh auth token`) for git push/PRs
- Claude subscription auth on macOS lives in the Keychain → one-time
  `claude setup-token` during the wizard, injected as an env var in sessions.
  (Foundry/API-key setups need no token: their auth rides in
  `~/.claude/settings.json` with the manifest — the wizard detects this.)
- a minimal `~/.claude.json` seed: onboarding-done + theme (skips the theme
  picker), the MCP servers that can run on Linux — user-scope ones plus the
  source repo's project-scoped ones, which follow the repo onto the VM
  workdir (auth rides in their env blocks; macOS-binary servers are
  dropped) — and pre-trust for the session workdir so the folder-trust
  dialog never fires. Codex gets the workdir pre-trusted via a `[projects]`
  append to its copied config.toml. History and laptop-path project state
  stay home. MCP servers whose auth lives in the macOS Keychain (OAuth-based
  remotes like Linear/Notion) carry their declaration but need a one-time
  `claude mcp login <name>` in the session — tokens rotate on refresh, so
  copying them would let two machines revoke each other. Create lists the
  affected servers. Remotes that accept static keys (Linear does) can instead
  be declared locally with an `Authorization` header, which travels whole —
  then nothing ever re-asks.

## 9. Setup wizard

`pier setup` — plain stdin prompts (no TUI form), four phases, under 3
minutes. v1 is AWS-only:

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
pier ls                 list own sessions
pier attach <match>     reattach; resumes if parked
pier rm <match>         destroy (instance + disk)
pier keep <match>       disable auto-park for a session
pier resize <match> <type>  change VM size (running: park→modify→resume; same arch)
pier setup              wizard (--print-admin for the no-IAM-rights path)
pier bake               build/refresh the prebaked image
pier doctor             checks
pier teardown           remove account groundwork
```

## 12. Config (`~/.config/pier/config.toml`)

```toml
driver         = "aws-ec2"
idle_timeout   = "30m"        # or "never"
unattended_cap = "8h"

[aws]
profile       = "default"
region        = "eu-central-1"
instance_type = "t4g.medium"
disk_gib      = 40
# subnet      = "subnet-..."  # only for orgs without a default VPC
# baked_ami   = "ami-..."     # written by `pier bake`

[secrets]
manifest = [".codex/auth.json", ".codex/config.toml", ".claude/settings.json", ".claude/CLAUDE.md"]
# claude_oauth_token = "..."  # from `claude setup-token` (macOS Keychain path)
```

## 13. v1 cut line

In: AWS driver, TUI, wizard (+print-admin, teardown), bake, overlapped create,
supervisor parking (+keep/pin), one-way secrets copy, doctor, quota UX.
Out (v1.1+): GCP driver (parked until AWS is fully E2E-tested), warm pools,
hibernate/suspend park, k8s driver, `ls --all`, Windows.

## 14. Load-bearing bets → spikes

| bet                                                        | spike       | status   |
|------------------------------------------------------------|-------------|----------|
| `shutdown -h now` parks (EC2 → stopped, not terminated)    | spike/aws.sh | **PASS** — stopped in 44s |
| boot → SSM-ready time supports 60–90s TTFI                 | spike/aws.sh | **PASS** — 29s stock AMI |
| stop → start → ready ≈ 30s resume                          | spike/aws.sh | **PASS** — 21s |
| SSM interactive TTY feels like ssh (manual, `--keep` mode) | spike/aws.sh | superseded — v1 uses real ssh over the SSM tunnel |
| GCE: same four on IAP + TERMINATED-with-disks              | spike/gcp.sh | parked with the GCP driver |

Measured 2026-07-30 in eu-central-1; details in spike/README.md.

Full v1 E2E (same day, same region, t4g.medium): stock create → ready 42s
(harnesses finish async under cloud-init); idle self-park fired at
idle_timeout+tick; resume → ready 23s; `pier bake` 8m19s once; **baked create
→ attach-ready 52s** — inside the 60-90s TTFI target. keep/pin, ls state
enrichment (beacon), rm, and teardown all verified; account swept clean after.
