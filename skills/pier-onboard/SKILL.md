---
name: pier-onboard
description: Set up a repository for pier — inspect the project and write its .pier-setup.sh, .pier-include, and .pier-bake.sh so pier sessions boot ready to work. Use when asked to set up, onboard, or configure pier for a repo.
---

# Onboard a repository to pier

pier runs coding-agent sessions as micro-VMs on the user's own AWS account.
When a session is created, the repo is checked out on the VM, uncommitted
edits arrive as a patch, and three optional repo-root files control the rest:

| File | When it runs / travels | What belongs in it |
|---|---|---|
| `.pier-bake.sh` | Once, during `pier bake`, on a throwaway instance that becomes the repo's AMI | **Toolchains** — language runtimes, package managers |
| `.pier-setup.sh` | On every session's first boot, async, after the repo lands | **Repo state** — dependency install, services, migrations, seeds |
| `.pier-include` | List read at create time; matching files ride to the VM | **Untracked/ignored files** dev needs — env files, local certs |

Your job: inspect this repo, write the files that apply, and tell the user
what to run next. All three are optional — write only what the repo needs.

## Step 1 — inspect the repo

Look at (where present): `package.json` (especially the `packageManager`
field), lockfiles, `go.mod`, `pyproject.toml` / `requirements.txt`,
`Cargo.toml`, `Gemfile`, `Makefile`, `docker-compose*.yml`, the README's
dev-setup section, `.gitignore`, and any existing `.env*` files on disk.
From that, determine:

1. **Toolchains beyond the default image.** The image already ships:
   node 22 + npm, git, gh, make, docker (with compose and buildx), tmux,
   jq, curl, unzip, and headless Chromium (playwright). Do **not**
   reinstall these. Anything else the build needs — pnpm/yarn, python,
   uv, rust, java — goes in `.pier-bake.sh`.
2. **The dev-setup sequence** — the commands a human runs after a fresh
   clone (install deps, start services, migrate, seed). That's
   `.pier-setup.sh`.
3. **Files git doesn't carry** that dev needs — usually `.env*`. That's
   `.pier-include`. Nothing untracked or gitignored ships unless listed
   here; pier prints which env files it is *not* carrying at create time.

## Step 2 — write `.pier-bake.sh` (only if toolchains are needed)

Runs as the `agent` user with passwordless sudo on the bake instance.
**There is no repo checkout yet** — bake predates any session — so nothing
here may reference repo files; pin versions inline. A nonzero exit aborts
the bake. Everything must be non-interactive.

Example for a repo whose `package.json` pins `"packageManager": "pnpm@10.6.5"`:

```bash
#!/usr/bin/env bash
# pier bake hook: runs once on the bake instance (agent user, passwordless
# sudo, NO repo checkout). Toolchains only; repo state goes in .pier-setup.sh.
set -euo pipefail

# corepack ships with node 22 but prompts on TTYs before downloading —
# silence it machine-wide, then prefetch the pinned pnpm.
sudo corepack enable
echo COREPACK_ENABLE_DOWNLOAD_PROMPT=0 | sudo tee -a /etc/environment >/dev/null
COREPACK_ENABLE_DOWNLOAD_PROMPT=0 corepack install -g pnpm@10.6.5
```

For apt installs, use `sudo DEBIAN_FRONTEND=noninteractive apt-get install -y …`.

## Step 3 — write `.pier-setup.sh`

Runs asynchronously in a `setup` tmux window on the session's first boot,
with the repo root as cwd, after the checkout, dirty patch, and
`.pier-include` files are all in place. The user's secrets env
(`~/.config/pier/env`) is loaded. Output logs to `~/.pier-setup.log`; a
nonzero exit shows as `(setup failed)` in `pier ls`, so **let failures
fail** — start with `set -euo pipefail`, don't swallow errors.

Prefer the repo's own entry point (`make install`, `make dev-setup`) over
duplicating its steps. Typical shape:

```bash
#!/usr/bin/env bash
# pier setup: runs async in the session's "setup" tmux window on first
# boot, cwd = repo root. Logs to ~/.pier-setup.log.
set -euo pipefail
pnpm install
docker compose up -d
pnpm db:migrate
```

Everything must be non-interactive — no prompts, no `sudo` that asks, no
watch-mode/foreground processes. Run services with `docker compose up -d`
where possible. A bare background process must fully detach —
`setsid cmd </dev/null >log 2>&1 &` — because the setup tmux window closes
when the script ends and SIGHUPs its process group (`nohup` alone does not
detach it).

## Step 4 — write `.pier-include` (only if loose files are needed)

One path or glob per line, relative to the repo root. `*`, `?`, `[]` match
within a path segment — **no `**`**. A directory line carries its whole
subtree. `#` starts a comment. Listed files travel exactly as they sit on
disk and win over the checkout. Example:

```
# env files docker compose and the apps read
.env
apps/*/.env.local
```

Only list what a dev session genuinely needs — everything listed leaves
the laptop for the VM.

## Step 5 — finish

- These files are meant to be committed (they name paths, not secrets).
  pier runs both scripts with bash, so the exec bit is optional.
- Tell the user: if you wrote or changed `.pier-bake.sh`, run `pier bake`
  in the repo (~8 min, once per hook change). Then create a session and
  watch `pier ls` — `(setup running)` should clear; if it shows
  `(setup failed)`, attach and read `~/.pier-setup.log`.
