# pier

**Give every agent session its own VM. One command up, zero burn when idle.**

Full dev environments on your own AWS account: your repo, your secrets, your
tools. One command to set up. Detach and it parks itself. Attach and it's
back in ~20 seconds.

![pier demo](docs/demo.gif)

```
pier setup          # once: a short wizard sets up the account groundwork
cd ~/code/myapp
pier fix-login      # new session: your branch, your secrets, your dev env
pier                # the TUI: list, attach, create, resize, destroy
```

## Why pier

Coding agents changed the shape of a dev environment. A session used to be
*you* at one machine; now it's an agent that runs for hours, wants its own
branch, its own ports, its own docker daemon — and you want several at once.
Running them on your laptop means fights over ports and files, and closing
the lid kills the run. Running them on a hosted platform means your code,
your secrets, and your agent's credentials live on someone else's infra,
billed by the seat.

The obvious fix — a cloud dev box — has an obvious flaw: it bills 24/7
whether the agent is thinking or you're asleep, and you still hand-build the
plumbing (repo, auth, secrets, tools) for every new session.

## The solution

pier makes each session a micro-VM that manages its own lifecycle. One
command launches an EC2 instance with your repo on a fresh branch, your
uncommitted edits, your agent harnesses (Claude Code and Codex) already
authenticated, your MCP servers configured, and your dev environment set up
— then attaches you to a tmux session inside it. Your terminal setup stays
untouched; pier hands you tmux over SSH in whatever terminal you already use.

A tiny in-VM supervisor watches tmux clients, agent CPU, and pty activity.
When you detach and the agent goes quiet, the VM shuts itself down: the
instance stops, the disk persists. A parked session costs EBS storage
(~$3–4/month) instead of compute (~$0.03/hour), and resumes in ~20 seconds
with files, branches, and credentials exactly as you left them.

There is no control plane. No server, no database, no daemon on your laptop:
session state lives in EC2 instance tags, and every byte between you and the
VM rides SSH over an SSM tunnel — zero inbound ports, no public SSH, every
connection IAM-authenticated and logged in CloudTrail.

**Why AWS first?** Because most teams are already on it. The account, the
credits, the budget line, the compliance review — they usually exist before
pier arrives, so pier rides them instead of introducing a new vendor and a
new bill. EC2 is also mechanically the right substrate for park/resume:
native stop/start keeps disks intact (measured: parked in 44s, resumed in
21s), SSM gives zero-inbound audited access with no bastion to run, and tags
are a free, consistent state store. A GCP driver is designed and parked —
see the [roadmap](#roadmap).

## Installation

```
git clone https://github.com/kerem-kaynak/pier
cd pier
make install     # cross-compiles the in-VM supervisor, embeds it, installs to $(go env GOPATH)/bin
```

Build with `make`, not `go build` — the supervisor must be embedded.

You need, on the laptop:

- Go 1.24+ (to build)
- `aws` CLI v2 and the [Session Manager plugin](https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html)
- `git` and OpenSSH (already on macOS/Linux)

`pier doctor` checks all of it, plus your AWS auth and account groundwork.

## Setup

```
pier setup
```

The wizard authenticates against your AWS profile, creates the account
groundwork — an IAM role + instance profile carrying only
`AmazonSSMManagedInstanceCore`, and one egress-only security group — writes
`~/.config/pier/config.toml`, detects the agent config and credentials it
will copy into sessions, offers to bake an image for the current repo, and
runs the doctor checks. Everything it creates is tagged and removable with
`pier teardown`.

No IAM permissions? `pier setup --print-admin` prints the handful of
commands for an admin to run once; the wizard then works with what exists.

## Usage

```
pier                      the TUI: sessions live-updating
                          enter attach · n new · d delete · p pin · r refresh · q quit
pier <branch> [base]      new session off base (default HEAD), then attach
    -d, --detach          create without attaching
    --idle <dur|never>    idle self-park timeout (default 30m)
    --cap <dur|never>     unattended runaway cap (default 8h)
    --no-park             shorthand for --idle never
pier ls                   plain list (pipeable)
pier attach <session>     attach; parked sessions auto-resume (~20-30s)
pier rm <session> [-f]    destroy the session and its disk
pier keep <session>       pin: disable idle self-park
pier resize <session> <type>   grow/shrink the VM (~40-60s, same CPU arch)
pier bake                 prebake this repo's session image (~60-90s creates)
pier mcp login <session>  one-time browser approvals for OAuth MCP servers
pier proxy                every running session as <session>.pier (macOS)
pier port <session> <p>   manual port forward (8080:3000 = local:session)
pier doctor               environment + account checks
pier teardown             remove all pier groundwork from the account
```

A day with pier:

```
$ cd ~/code/shop && pier checkout-flow
creating checkout-flow (shop @ HEAD)
  ▸ launching t4g.medium (baked image)
  ▸ pushing secrets + repo (GitHub-first: only secrets ride the tunnel)
  ▸ bootstrap: branch checkout-flow, dirty patch applied, setup running
session checkout-flow ready
[tmux opens — claude is installed, authenticated, on your branch]

# ... detach with C-b d, close the laptop, have dinner ...

$ pier ls
NAME           REPO  STATE   AGE  COST
checkout-flow  shop  parked  3h   ~$3-4/mo (disk only)

$ pier attach checkout-flow
resuming checkout-flow (~20-30s)...
[right where you left it]
```

## Making your repo pier-ready

Three optional files at the repo root, all committed:

| File | Runs / read | Contains |
|---|---|---|
| `.pier-setup.sh` | Every session's first boot, async, in a `setup` tmux window | Repo state: deps, services, migrations, seeds |
| `.pier-include` | At create | Untracked/ignored files to carry (env files, local certs) |
| `.pier-bake.sh` | Once, during `pier bake` | Toolchains beyond the default image (pnpm, python, rust, ...) |

```bash
# .pier-setup.sh — cwd is the repo root; logs to ~/.pier-setup.log
set -euo pipefail
pnpm install
docker compose up -d
pnpm db:migrate
```

```
# .pier-include — one path or glob per line (no **); a directory carries its subtree
.env
apps/*/.env.local
```

```bash
# .pier-bake.sh — agent user, passwordless sudo, NO repo checkout yet
set -euo pipefail
sudo corepack enable
echo COREPACK_ENABLE_DOWNLOAD_PROMPT=0 | sudo tee -a /etc/environment >/dev/null
COREPACK_ENABLE_DOWNLOAD_PROMPT=0 corepack install -g pnpm@10.6.5
```

**Or let your agent write them.** This repo ships a skill —
[`skills/pier-onboard`](skills/pier-onboard/SKILL.md) — that teaches a coding
agent to inspect a repo and write all three files with the right boundaries.
Copy the `pier-onboard` folder into your repo's `.claude/skills/` (or
`~/.claude/skills/` to have it everywhere) and ask your agent to set up pier.

## Features

### Fast repo transfer, GitHub-first

The SSM tunnel moves ~1 MB/s, so pier avoids it for anything big. When the
base commit is on a GitHub origin, the VM fetches straight from GitHub and
only secrets ride the tunnel. Local-only commits travel as a thin delta
bundle; anything else falls back to a full-history bundle. Uncommitted edits
to tracked files ride along as one patch, so the session's working tree
starts exactly as your laptop's sits (staged edits arrive unstaged).

For private repos, pier tries whatever GitHub credential the laptop already
has: the gh CLI's login, git's https credential helper, or plain ssh keys —
with an ssh origin and a running ssh-agent, the agent is forwarded through
the tunnel for the fetch and the key never leaves your machine. Before
skipping the bundle, pier verifies the fetch works with exactly the auth the
session will have; anything doubtful degrades to the slow-but-universal
bundle instead of failing. No credential is ever required — you just get
tunnel-speed transfers and no push.

Pushing from a session follows the same split: with a token, `git push` and
PRs work anytime; with ssh keys only, pushes work while you're attached (the
forwarded agent goes home when you disconnect — exactly the property that
makes it safe).

### Secrets, deliberately boring

Secrets travel once, at create, as an explicit manifest: `~/.claude`,
`~/.codex`, tokens (`gh auth token`, `claude setup-token`), and the repo
files your `.pier-include` lists. Nothing loose ships by default — the
create prints which env files it is *not* carrying. The VM never holds cloud
credentials; the instance role carries SSM and nothing else.

### MCP servers, agents, skills

They travel with their config — auth included when it's static (env vars,
API-key headers). OAuth-backed remote MCPs keep rotating tokens in the OS
keychain, which can't be copied (two machines sharing one revoke each
other), so those need one browser approval each, once per session lifetime.
Creating a session offers to run them on the spot; `pier mcp login
<session>` sweeps whatever still needs auth later.

Headless Chromium ships in the image (playwright's build plus its system
libraries), so browser MCPs and skills — screenshots, web automation — work
out of the box.

### `pier proxy` — sessions as hostnames

```
$ pier proxy
proxy up — running sessions resolve as <session>.pier; ctrl-c to stop
```

Open `http://checkout-flow.pier:3000` in your browser, `psql -h
checkout-flow.pier` in your terminal. Each running session gets a private
loopback IP, and whatever ports it actually listens on are discovered and
mirrored live — start a dev server in the session and the port just appears.
One sudo on first run (a resolver file + loopback aliases — the split-DNS
trick VPNs use); macOS for now. A live connection counts as attachment, so a
session never parks under your open browser tab. `pier port <session> 3000`
is the manual, zero-sudo fallback.

### `pier bake` — per-repo images

A stock create installs the harnesses under cloud-init (minutes). `pier
bake` runs that install once on a throwaway instance, runs your repo's
`.pier-bake.sh` on top, and snapshots the result as an AMI keyed to the repo
— creates drop to ~60–90s. Images are per-repo so one project's toolchain
never bleeds into another's; re-baking supersedes the old image, and
`pier teardown` sweeps them all by tag.

### Setup that can't fail silently

`.pier-setup.sh` runs asynchronously in a tmux window on first boot — after
the checkout, dirty patch, and `.pier-include` files are all in place. The
outcome always surfaces: `pier ls` and the TUI show `(setup running)` /
`(setup failed)`, the log at `~/.pier-setup.log` ends with `pier setup:
done` or `pier setup: FAILED (exit N)`, and a failed window renames to
`setup-failed` and stays open instead of vanishing. Set `PIER_SETUP_SCRIPT`
to run a different script for one create.

### Strain and resize

The supervisor reports sustained cpu/mem pressure (kernel PSI): `pier ls`
shows `working (strained)` and `pier resize checkout-flow t4g.xlarge` grows
the VM through one ~40s park/resume cycle, disk and state intact.
Deliberately not automatic — the VM holds no cloud credentials, and
auto-scaling is a surprise-cost footgun.

### Honest states

EC2 says "running" long before a session is usable, so pier doesn't: a
session lists as `creating` until the bootstrap's last act writes a
`pier:ready` tag. Attaching early is refused with a plain "still setting up"
instead of a raw connection error; TUI creates run in the background and the
row flips when ready; a create that fails cleans up its own instance.

## Caveats

- **The tunnel is ~1 MB/s.** Repos that can't come from GitHub push their
  history through it (a 300 MB history ≈ 5 min, once per create). The
  create output says which transfer mode you got and why.
- **Parking loses processes.** Files, git state, and installed tools
  survive; the tmux server and an in-flight agent run don't. Hibernate
  (park with RAM) is on the roadmap.
- **OAuth MCPs need a browser approval per session.** Keychain-held tokens
  can't be copied safely; this floor is real.
- **ssh-key-only GitHub auth pushes while attached.** The forwarded agent
  disconnects with you. Any token lifts this.
- **`pier proxy` is macOS-only** for now. `pier port` works everywhere.
- **Bake hooks live only in baked images.** A repo with a `.pier-bake.sh`
  that was never baked runs stock — setup then fails loudly, not silently.
- **Resize stays within the CPU arch** (t4g → t4g; providers only allow
  type changes on stopped instances).

## How it works

A session is one EC2 instance plus its EBS disk, tagged and namespaced by
your caller identity (`pier:user` = your STS ARN — a team shares an account
with zero coordination and zero collisions). All state lives in those tags
and on the disk; `pier ls` is a filtered `describe-instances`, and there is
nothing else to operate, back up, or pay for.

```
laptop                              AWS account (yours)
──────                              ───────────────────
pier CLI/TUI ── aws cli ──────────▶ EC2 API        (create/stop/start/tags)
     │                              ┌─────────────────────────┐
     └── ssh ── SSM tunnel ───────▶ │ session VM               │
         (zero inbound ports)       │  tmux ▸ claude / codex   │
                                    │  pier-supervisor         │
                                    │  └─ parks the VM when    │
                                    │     detached and quiet   │
                                    └─────────────────────────┘
```

The supervisor samples every 5s: *attached* (tmux clients, or a live
forwarded TCP connection — your browser on a proxied port), *busy* (agent
process-tree CPU, recent pty output). Attached or busy resets the idle
clock; detached and quiet past `idle_timeout` parks; detached but busy past
`unattended_cap` parks anyway, so a looping agent can't burn compute for
days. Parking is the VM running `shutdown -h now` — the instance is
configured to stop, not terminate. The supervisor holds no credentials and
calls no APIs; it also beacons state (`/run/pier/status.json`) that `ls`,
the TUI, and `pier proxy` read.

Full design, including the settled trade-offs and measured spike numbers:
[docs/SPEC.md](docs/SPEC.md).

## Design decisions

The choices contributors should know before proposing changes
(details in [CONTRIBUTING.md](CONTRIBUTING.md) and [docs/SPEC.md](docs/SPEC.md)):

- **No control plane, ever.** State lives in EC2 tags; adding a server,
  database, or laptop daemon needs an extraordinary reason.
- **The AWS CLI, not the SDK.** The CLI is already required for the SSM
  plugin, and SSO/profiles/MFA come with it for free. v1 has no SDK
  dependency.
- **No cloud credentials in the VM.** Parking is the VM shutting itself
  down; resize is human-triggered. Anything needing account credentials
  happens from the laptop.
- **One transport.** Everything — attach, exec, file push, port forwards —
  is OpenSSH over the SSM tunnel. No second mechanism to secure or debug.
- **Guarded cloud-init, identical on stock and baked images.** Every
  install step is a no-op when the image already has it; baking is an
  optimization, never a requirement. Guards test what the stock image lacks.
- **Images are toolchains; sessions are state.** `pier bake` installs
  tools; deps/migrations/env belong to `.pier-setup.sh` at boot. pier's
  default image chases no language ecosystems — that's what `.pier-bake.sh`
  is for.
- **Dirty state travels as a git patch,** not rsync — binary-safe,
  reviewable, and applied atomically after checkout.
- **Truth over optimism in states.** The `pier:ready` tag, the setup
  status file, the strained flag: pier reports what is, not what EC2 claims.
- **`pier ls` stays plain** (pipeable); the TUI is the pretty view.

## vs. other tools

| | pier | Codespaces / devcontainers | Hosted agent platforms | DIY EC2 + tmux |
|---|---|---|---|---|
| Runs on | your AWS account | GitHub's infra | vendor's sandbox | your AWS account |
| Idle cost | ~$3–4/mo (self-parks) | metered, auto-stop | per-seat / per-task | full rate unless you script it |
| Agent-ready | harnesses + auth + MCP travel | you configure | their agent only | you configure |
| Session lifetime | until you `rm` it | workspace-scoped | task-scoped | until you clean it up |
| Interface | your terminal, tmux | VS Code / web | web UI | your terminal |
| Setup | one wizard | per-repo config | account signup | everything by hand |

Honest framing: Codespaces is more polished for *humans editing in VS
Code*. Hosted agent platforms are simpler when you want *fire-and-forget
tasks* and don't mind whose infra they run on. pier is for keeping
long-lived, stateful agent sessions on infrastructure you already own and
already trust with your code — at storage prices when you're not using them.

## Roadmap

- **GCP driver** — designed (IAP tunnel, labels, TERMINATED-with-disks as
  parked), parked until the AWS driver is fully hardened.
- **Hibernate/suspend parking** — keep RAM, resume mid-agent-run.
- **Linux `pier proxy`.**
- **Custom base AMIs** — bring your own golden image under pier's harness
  layer.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Start with an issue for anything
beyond a small fix — the design ground rules above are load-bearing.

## License

[MIT](LICENSE)
