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
- Default VPC + public IP + `pier-egress-only` SG (direct connect, the
  default, reconciles one TCP-22 rule per caller, /32-scoped to their
  current public IP; `aws.direct = false` keeps it zero inbound). Orgs
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

Default fast path: connections dial sshd on the instance's public IP
(HostKeyAlias keeps known_hosts keyed by instance id, so the entry survives
the IP change every park/resume brings). The tunnel moves ~100KB/s-1MB/s and
adds a service hop to every keystroke; direct is line-rate and raw-RTT. pier
keeps one ingress rule per caller — TCP 22 from their current public IP /32,
recognized by rule description, stale addresses revoked — and falls back to
the tunnel only when direct can never work (no public IP, network blocks
outbound 22). Dial failures within 3 minutes of launch count as still-booting
and re-probe every few seconds instead, so post-resume connections flip to
direct the moment sshd listens; park/resume/resize drop the probe cache so
nothing dials the previous public IP. `aws.direct = false` forces the tunnel
for every connection. Teardown deletes the SG, rules included.

## 5. Identity & teams

The caller's cloud identity (STS `GetCallerIdentity` ARN; GCP authed
principal) is written to `pier:user` at create and filtered on at list. Many
devs share one account with zero coordination: no name collisions, no shared
state, nothing to configure. `pier ls --all` can show teammates' sessions
(read-only visibility); everything else operates only on your own.

## 6. Parking policy (supervisor)

`pier-supervisor` runs in every VM with **zero cloud credentials** — parking
is the VM shutting itself down. Every 5s (fast enough that the beacon's port
list feels live to `pier proxy`; the activity windows still span ~30s) it
derives:

- `attached` — tmux has ≥1 client, **or a live forwarded TCP connection**
  (established sshd-owned sockets that aren't the :22 transport — your
  browser or psql on a mirrored port). A session never parks under an open
  connection; a forgotten tab keeping it awake is deliberately the user's
  problem. App→app loopback traffic (dev server holding a DB pool) doesn't
  count — only sshd's forward dials do.
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

The beacon additionally lists the session's listening TCP ports (one
`sudo ss -Htnap` pass; sshd and systemd-resolved excluded) — this is how
`pier proxy` knows what to mirror without the user declaring anything.

A session either exists fully set up or not at all — no half-states:

- The create's **last act** is a `pier:ready` tag on the instance (written
  after the VM-side bootstrap touches `~/.pier-bootstrapped`; the marker
  stays as the raw-ssh backstop, before the async `.pier-setup.sh`
  finishes). EC2 reports `running` well before the session is usable — SSM
  registration alone lags ~30s — so ls/TUI read running-without-the-tag as
  `creating`: truthful state from the first second, no probe, no SSM
  dependency. Attach and other session commands **refuse cleanly** on a
  creating session ("still setting up — try again when it shows running")
  instead of spawning ssh into a raw `TargetNotConnected` or waiting to
  drop the user into an empty $HOME — which would also steal the `main`
  tmux session away from its workdir. (The supervisor still starts early on
  purpose: even a half-made instance parks itself.)
- A create that fails after launch **destroys its own instance** (ctrl-c
  included — the cleanup runs on a cancel-immune context). Transient scp
  drops right after boot get one retry first.

### 6.1 Hostnames: `pier proxy`

`pier proxy` (foreground, ctrl-c to stop) gives every **running** session its
own hostname: `http://<session>.pier:3000` in the browser, `psql -h
<session>.pier` — real port numbers, any TCP client, zero per-port commands.
All machinery lives in `internal/proxy`; the rest of the product contributes
only `Driver.SSHTarget` (the raw ssh recipe) and the beacon's port list.

- **Names**: a ~100-line UDP responder answers A queries for `*.pier`,
  wired in system-wide via `/etc/resolver/pier` — the split-DNS mechanism
  VPNs use, honored by browsers and getaddrinfo alike. AAAA/HTTPS queries
  get NOERROR-empty (never NXDOMAIN, which would negative-cache the name).
- **IPs**: each session gets a stable private loopback IP (127.94.0.101+,
  32 slots; the resolver sits on 127.94.0.53:5533 — a high port plus a `port`
  directive in the resolver file, because macOS lets unprivileged processes
  bind low ports only on the wildcard address). macOS needs `ifconfig lo0
  alias` per IP — the one sudo, disclosed before it runs; aliases are inert
  /32s that vanish at reboot. Per-session IPs are what let two sessions both
  serve :3000 without colliding — pier's most normal situation.
- **Forwards**: one multiplexed OpenSSH master per running session
  (`-M -S <ctl> -N` over the SSM tunnel). Beacon polls ride the mux as exec
  channels (no new SSM sessions, no AWS API); forwards are added/removed
  live with `ssh -O forward/cancel -L <ip>:<port>:localhost:<port>` as the
  port list changes. Stock OpenSSH, shell-out only — no SSH library.
- **localhost too**: every mirrored port also binds `127.0.0.1:<port>`,
  because origin allow-lists (OAuth dashboards, CORS configs) trust
  `http://localhost:<port>` and nothing else — apps with strict auth
  callbacks work with zero dashboard changes. First session wins a
  contested port (notice printed); the `.pier` name is the tiebreaker.
- **Accelerated HTTP**: the relay sniffs each connection; HTTP is answered
  by a per-port accelerator instead of piped. It caches what the dev server
  marks cacheable (immutable dep chunks forever — the URL hash changes when
  they do — ETagged files with revalidation), and scans JS/HTML responses
  for imports, prefetching the module graph 64-wide over pooled backend
  connections before the browser asks. Vite-style dev servers (1500 files,
  6 browser connections, one WAN round trip each = ~36s) load in seconds;
  a reload becomes one parallel revalidation burst. WebSockets, POSTs,
  streaming and non-HTTP traffic pass through untouched.
- **Park-neutral by design**: masters and beacon polls don't touch tmux,
  ptys, or forwarded sockets, so watching a session doesn't keep it awake —
  only actual connections do (§6). Parked sessions' names stop resolving
  rather than auto-resuming: a stray tab must not wake a stopped VM.
- Sessions created before the port-discovery supervisor get a one-time
  warning (recreate to mirror); `pier port` remains the manual, zero-sudo,
  any-OS fallback. macOS-only for now (Linux would need an /etc/hosts block
  or resolved glue — cut until asked for).

## 7. Speed

Targets: **create → attached 60–90s; resume → attached ~30s** (measured
numbers in §14).

1. **`pier bake`** — prebaked **per-repo** image: agent user, tmux, git, gh,
   docker (with compose + buildx — docker.io alone is the bare engine; the
   daemon runs userns-remapped to agent, so container-root writes on bind
   mounts land agent-owned — the ownership translation Docker Desktop does
   on a Mac, without which compose stacks litter repos with root-owned
   files), make,
   claude + codex, headless chromium (playwright build + system
   deps, for browser MCPs/skills), supervisor preinstalled — plus whatever
   the repo's `.pier-bake.sh` installs on top (run on the bake instance as
   agent with passwordless sudo, no repo checkout present: toolchains like
   pnpm/python belong here, repo state in `.pier-setup.sh`; a failed hook
   aborts the bake). Pier deliberately doesn't chase language ecosystems in
   the default image — the hook is the user's channel. Images are keyed by
   repo basename in config (`[aws.baked_amis]`), tagged `pier:repo`; a
   re-bake supersedes and deregisters the repo's previous image, and
   teardown sweeps every `pier:managed`-tagged image. ~$1/mo snapshot
   storage per repo. Offered as the wizard's last step when it runs inside
   a repo; `pier bake` refreshes it.
2. **Overlapped create** — launch the instance first; build the git bundle +
   secrets tar while it boots; push and bootstrap the moment sshd answers;
   `.pier-setup.sh` runs asynchronously in a background tmux window while you
   type to the agent — it starts only after the checkout, dirty patch, and
   `.pier-include` extras are all in place. `PIER_SETUP_SCRIPT`
   points it at a different script — relative to the repo root or `~` —
   which travels in the tar and takes precedence over the repo's own.
   The outcome is never silent: the window writes `~/.pier-setup.status`
   ("running", then the exit code — the supervisor beacons it, so `ls`/TUI
   show "(setup running)"/"(setup failed)"), ends `~/.pier-setup.log` with
   `pier setup: done`/`FAILED (exit N)`, and on failure renames itself to
   `setup-failed` and stays open. The status file lives on the disk, so a
   failure stays visible across park/resume.
   (On a stock AMI the bootstrap must wait for cloud-init's
   harness install — minutes, exactly once; bake removes it.)
3. **Origin-first workspace fetch** — the SSM tunnel moves ~1 MB/s, so the
   repo avoids it whenever possible. If the base commit is reachable from a
   GitHub origin ref, the VM fetches straight from GitHub (~100× faster;
   auth via the GH_TOKEN credential helper — ssh origins are rewritten to
   https, which also makes push work in sessions). Local-only commits on top
   of pushed history travel as a thin delta bundle (KBs). Before skipping the
   bundle, a preflight `ls-remote` proves the fetch works with exactly the
   auth a session gets (GH_TOKEN or anonymous; local credential helpers
   disabled). With no usable https credential but a working ssh origin +
   local ssh-agent, the ssh URL is kept and the **agent is forwarded** (`-A`)
   for the bootstrap fetch — the key never leaves the laptop. Attach also
   forwards the agent (symlink-stabilized `SSH_AUTH_SOCK` for tmux), so
   ssh-key users can push while attached; detached pushes still need a token,
   because a persistent capability needs a persistent credential. The
   full-history bundle over SSM remains the universal fallback: no origin,
   non-GitHub host, never-pushed history, all preflights failed. Whatever the
   mode, **uncommitted edits to tracked files** ride along too, as one
   binary-safe patch (`git diff HEAD`) applied right after the checkout — the
   session's working tree starts exactly as the laptop's, staged edits
   arriving unstaged. (Only when the session's base is the laptop's HEAD; a
   session created off another commit carries no dirty state.) Untracked and
   ignored files travel **only** when a repo-root **`.pier-include`** names
   them: one path or glob per line, matched against the disk with no
   git-status distinction — listed = travels, extracted after checkout +
   patch so listed content wins. Nothing loose ships by default — no env
   auto-transfer; the create prints which env files it is *not* carrying so
   a missing one fails loud at create, not deep in `make dev`.

Cut for v1 (numbers didn't justify the moving parts): warm pools, mid-create
quota polling.

## 8. Secrets

One-way copy from the laptop at create; never stored anywhere else, never
written back. Sources (wizard-detected, confirmed into the manifest):

- repo files named by a repo-root `.pier-include` (path or glob per line) —
  the **only** loose-file channel: nothing untracked or ignored ships without
  a line here (env files included; the create warns about ones left behind).
  Tracked content arrives with the fetch, dirty edits to it via the patch.
- `~/.codex/` (auth.json, config.toml)
- `~/.claude/` settings, `CLAUDE.md`, agents
- a GitHub credential for git push/PRs and private-repo fetch: `gh auth
  token` if gh is logged in, else the laptop's https git credential
  (`git credential fill`), else the ssh agent relayed via forwarding (fetch
  always, push while attached) — never required, doctor reports which was
  found. `~/.ssh` itself never travels.
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
  remotes) carry their declaration but not their tokens — they rotate on
  refresh, so copying them would let two machines revoke each other. Instead:
  `pier mcp login <session>` asks the session which servers still lack a
  token (seeded config minus its credential store) and runs `claude mcp
  login` for each, sequentially, with the OAuth callback port forwarded
  through the SSM tunnel — one browser approval per server on the laptop
  completes each flow inside the session; tokens persist on its disk across
  park/resume, and re-runs skip what's done. The interactive create offers
  this sweep right before the first attach. (Remotes that accept static keys
  can be declared locally with an `Authorization` header, which travels
  whole — zero approvals.)

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
pier                    TUI: list / attach / new / delete / pin (new = background create,
                        listed as "creating" until the create writes its ready tag)
pier <branch> [base]    create from cwd repo (branch off base, default HEAD) and attach
pier ls                 list own sessions
pier attach <match>     reattach; resumes if parked
pier mcp login <match> [server]  browser-auth every MCP server that still needs it, sequentially
                        (callback rides the tunnel; server arg = redo just that one)
pier proxy              every running session as <session>.pier, listening ports mirrored
                        live onto a per-session loopback IP and onto localhost, HTTP
                        accelerated (§6.1; macOS, one sudo)
pier port <match> <p> [p...]  manual port forwards, zero-sudo any-OS fallback (3000 or 8080:3000)
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

The throwaway spike scripts that produced these numbers have been retired;
the verdicts stand as the record.

| bet                                                        | spike       | status   |
|------------------------------------------------------------|-------------|----------|
| `shutdown -h now` parks (EC2 → stopped, not terminated)    | aws spike | **PASS** — stopped in 44s |
| boot → SSM-ready time supports 60–90s TTFI                 | aws spike | **PASS** — 29s stock AMI |
| stop → start → ready ≈ 30s resume                          | aws spike | **PASS** — 21s |
| SSM interactive TTY feels like ssh (manual, `--keep` mode) | aws spike | superseded — v1 uses real ssh over the SSM tunnel |
| GCE: same four on IAP + TERMINATED-with-disks              | gcp spike | parked with the GCP driver |

Measured 2026-07-30 in eu-central-1.

Full v1 E2E (same day, same region, t4g.medium): stock create → ready 42s
(harnesses finish async under cloud-init); idle self-park fired at
idle_timeout+tick; resume → ready 23s; `pier bake` 8m19s once; **baked create
→ attach-ready 52s** — inside the 60-90s TTFI target. keep/pin, ls state
enrichment (beacon), rm, and teardown all verified; account swept clean after.
