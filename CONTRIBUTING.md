# Contributing to pier

Thanks for pitching in. pier is deliberately small; the fastest way to land a
change is to keep it that way.

## Build and test

```
make          # cross-compiles the in-VM supervisor, embeds it, builds ./pier
make install  # install to $(go env GOPATH)/bin
go test ./...
```

Always build with `make`, not `go build` — the supervisor binaries must be
embedded or session creation fails at runtime.

Requires: Go, `aws` CLI v2, `session-manager-plugin`, `git`, OpenSSH.
`pier doctor` checks the runtime dependencies.

## Design ground rules

[docs/SPEC.md](docs/SPEC.md) is the source of truth for architecture and
open decisions — TODOs in code point there. The short version:

- **Lean over general.** No server, no daemon, no database: all state lives
  in EC2 tags and on the session's disk. Changes that add moving parts need
  a strong reason.
- **The AWS CLI, not the SDK.** pier already requires the `aws` CLI for the
  SSM plugin, so the driver shells out to it and v1 carries no SDK
  dependency.
- **No cloud credentials in the VM.** The instance role carries SSM and
  nothing else. Anything that would put account credentials on a session
  box is out.
- **Guarded cloud-init.** The same user-data runs on stock and baked images;
  every install step is guarded so it no-ops when the image already has it.
  Guards must test something the stock image *lacks*.
- **Images are toolchains, sessions are state.** `pier bake` and
  `.pier-bake.sh` install tools; repo state (deps, migrations, env) belongs
  in `.pier-setup.sh` at session boot. pier does not chase language
  ecosystems in its default image.
- **Failures are loud.** Setup outcomes, bake failures, unreachable
  sessions — every failure names itself and says where to look next.

## Code conventions

- Command output goes through `internal/ui` (one accent color, ANSI palette,
  lots of dim). Long-running commands print a bold header, `ui.Step` lines,
  and a green completion. `pier ls` output stays plain so it pipes cleanly;
  the TUI is the pretty view.
- Comments explain *why*, not *what*. Match the density already there.
- `go vet ./...` and `gofmt` clean before sending a PR. Some `modernize`
  suggestions (e.g. `SplitSeq`) are deliberately not applied.

## Sending changes

Open an issue first for anything beyond a small fix, so design questions get
settled before code review. PRs should include tests where behavior changed
and a one-paragraph description of the why. CI runs gofmt, `make test`, and
`make build` on every PR and must be green.

## Releasing (maintainers)

Releases are tag-driven. Homebrew builds from the source tarball, so there
are no prebuilt artifacts to manage.

1. Make sure CI is green on main.
2. Tag and push:
   `git tag -a vX.Y.Z -m "pier vX.Y.Z" && git push origin vX.Y.Z`.
   The release workflow re-runs the checks and publishes the GitHub release.
3. Bump the Homebrew formula in
   [kerem-kaynak/homebrew-tap](https://github.com/kerem-kaynak/homebrew-tap):
   point `url` at the new tag and update `sha256`
   (`curl -sL <tarball-url> | shasum -a 256`).

Versioning is semver. While pier is 0.x, breaking changes bump the minor
version.
