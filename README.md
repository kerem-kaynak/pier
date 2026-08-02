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
verified spike numbers: [docs/SPEC.md](docs/SPEC.md).

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
- `pier bake` (run inside a repo) prebakes that repo's AMI: the harnesses,
  plus whatever a repo-root `.pier-bake.sh` installs on top — toolchains like
  pnpm or python that pier deliberately doesn't chase per-ecosystem. The hook
  runs on the bake instance (agent user, passwordless sudo, no repo checkout
  yet), so it's for tools, not repo state — deps and migrations stay in
  `.pier-setup.sh`. Images are per-repo so one project's toolchain never
  bleeds into another's. The same cloud-init runs on stock and baked images —
  every default step is guarded, so baking stays optional (hook contents,
  though, exist only in baked sessions).
- Your repo reaches the VM GitHub-first: when the base commit is already on
  a GitHub origin, the VM fetches straight from GitHub and only secrets ride
  the tunnel. Local-only commits travel as a thin delta bundle; anything else
  falls back to a full-history git bundle. Uncommitted edits to tracked
  files ride along as one patch — the session's working tree starts as
  yours sits, staged edits arriving unstaged. Untracked and ignored files
  travel only when a repo-root `.pier-include` lists them (one path or glob
  per line, matched against the disk, no git-status distinction). Nothing
  loose — env files included — ships by default; the create prints which
  env files it is *not* carrying.
- Secrets travel as an explicit manifest (`~/.claude`, `~/.codex`, the
  repo files your `.pier-include` lists) plus tokens (`gh auth token`,
  `claude setup-token`).
- MCP servers, agents, skills, and plugins travel with their config — auth
  included when it's static (env vars, API-key headers). OAuth-backed remote
  MCPs keep rotating tokens in the OS keychain, which can't be copied (two
  machines sharing one revoke each other) — those need **one browser approval
  each, once per session lifetime**. Creating a session offers to run them
  on the spot; `pier mcp login <session>` sweeps whatever still needs auth
  later (done servers are skipped — no server names to remember).
- Headless Chromium is in the image (playwright's build plus its system
  libraries), so browser MCPs and skills — playwright, screenshots, web
  automation — work out of the box. Agents drive it headless; it's a server.
- `pier proxy` gives every running session its own hostname: open
  `http://payments-retry.pier:3000` in your browser, `psql -h
  payments-retry.pier` in your terminal. Each session gets a private
  loopback IP, and whatever ports it actually listens on are discovered and
  mirrored live — start a dev server in the session and the port just
  appears. One sudo on first run (a resolver file + loopback aliases, the
  same split-DNS trick VPNs use); macOS for now. A live connection counts
  as attachment, so a session never parks under your open browser tab or
  psql — and a forgotten tab keeping a VM awake is your call to close.
  `pier port <session> 3000` stays as the manual, zero-sudo fallback
  (`8080:3000` maps local:session).
- Creating from the TUI doesn't block: the session builds in the background
  and sits in the list as `creating` until its bootstrap finishes (a
  `pier:ready` tag written as the create's last act — EC2 says "running"
  long before a session is usable, so pier doesn't). Attaching early is
  refused with a plain "still setting up — try again when it shows running";
  you never land in a session before its repo does, never see a raw AWS
  connection error, and a create that fails cleans up its own instance.
- If the repo has a `.pier-setup.sh`, it runs asynchronously in a tmux window
  on first boot (deps, migrations, seeds) — after the checkout, dirty patch,
  and `.pier-include` files are all in place. Set `PIER_SETUP_SCRIPT` to run
  a different script instead (resolved relative to the repo root or `~`; it
  travels with the create). The outcome is never silent: `pier ls` shows
  `(setup running)` / `(setup failed)`, the log at `~/.pier-setup.log` ends
  with `pier setup: done` or `pier setup: FAILED (exit N)`, and a failed
  window renames to `setup-failed` and stays open instead of vanishing.

## Start times

A baked create is ~60s: ~30s of EC2 boot, then secrets + bootstrap. When a
create is much slower than that, it's one of two things:

- **No baked AMI for this repo** — a stock image installs node, the
  harnesses, gh, docker (with compose + buildx), and make under cloud-init on
  every create (minutes). Run `pier bake` once per repo.
- **The repo couldn't come from GitHub** — pier then pushes the full git
  history through the SSM tunnel at ~1 MB/s (a 300 MB history ≈ 5 min). The
  create output says which mode you got and why. The fast path needs:
  - the repo hosted on GitHub, with the base commit pushed, and
  - for **private** repos: any GitHub credential on the laptop, tried in
    order — the gh CLI's login, whatever your git https credential helper
    pushes with, or plain **ssh keys**: with an ssh origin and a running
    ssh-agent, pier forwards the agent through the tunnel for the fetch. The
    key never leaves your machine; the VM borrows it, like agent forwarding
    always has. Public repos need nothing.

  Before skipping the bundle, pier verifies the GitHub fetch works with
  exactly the auth the session will have — anything doubtful degrades to the
  slow-but-universal bundle instead of failing. No GitHub credential is ever
  *required*: sessions run fine without one, just with tunnel-speed transfers
  and no push. `pier doctor` shows what was found.

  Pushing from a session follows the same split: with a token, `git push`
  and PRs work anytime; with ssh keys only, pushes work **while you're
  attached** (the forwarded agent goes home when you disconnect — exactly
  the property that makes it safe).

## Build

```
make          # cross-compiles the in-VM supervisor, embeds it, builds ./pier
make install  # install to $(go env GOPATH)/bin
```

Requires: `aws` CLI v2, `session-manager-plugin`, `git`, OpenSSH. `pier doctor`
checks all of it.
