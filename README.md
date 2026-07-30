# pier

Coding-agent sessions (Claude Code / Codex) as micro-VMs on **your own AWS or GCP
account**. Each session is a full dev environment — your repo on its own branch, your
secrets, your tools — that parks to near-zero cost when idle and resumes with files,
branches, and credentials exactly as you left them. One bare command opens a TUI to
attach, create, and delete sessions. Your terminal setup stays untouched: pier just
hands you a remote shell in whatever terminal or multiplexer you already use.

**Status: pre-alpha scaffold.** The design is settled — see [docs/SPEC.md](docs/SPEC.md).
Nothing works yet.

```
cd ~/code/myapp
pier payments-retry    # cloud session: your branch, your secrets, your dev env, ~60-90s
pier                   # TUI: list / attach / new / delete
```

Groundwork experiments live in [spike/](spike/) — by-hand verification of the three
load-bearing bets (self-park via OS shutdown, attach latency, cold-create time).
