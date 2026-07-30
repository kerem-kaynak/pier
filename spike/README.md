# spikes

By-hand verification of the load-bearing bets in [docs/SPEC.md §14](../docs/SPEC.md)
before any real implementation. Each script is self-contained, uses only
throwaway resources named `pier-spike`, cleans up after itself, and costs
about a cent per run.

| script  | verifies                                                        | status   |
|---------|-----------------------------------------------------------------|----------|
| aws.sh  | boot→SSM time, cmd RTT, `shutdown -h now` → **stopped**, resume | **PASS** (2026-07-30) |
| gcp.sh  | boot→IAP-ssh time, `shutdown -h now` → **TERMINATED**+disk, resume | written, UNTESTED |

Modes (both scripts): no args = measure + clean up; `--keep` = leave the
instance running for the manual interactive-TTY feel test (the one thing a
script can't judge); `--cleanup` = remove leftovers.

## Pass criteria

- Self-park: in-VM shutdown must land in a *restartable stopped* state with
  the disk intact — never terminated. If this fails the whole
  credential-free parking design is out.
- Cold boot to reachable ≤ ~90s stock (the baked image only improves it).
- Resume to reachable ≈ 30s.

## Measured results

**aws.sh — 2026-07-30, eu-central-1, t4g.medium, Ubuntu 24.04 arm64 (stock AMI, no bake):**

| measurement                    | result        | verdict |
|--------------------------------|---------------|---------|
| launch → SSM online            | **29s**       | cold-create floor well under the 60–90s attach target |
| send-command RTT (×3)          | 4s / 1s / 1s  | polling upper bound; fine |
| `shutdown -h now` → state      | **stopped** in 44s | self-park bet **holds** — disk intact, restartable |
| start → command-ready (resume) | **21s**       | beats the ~30s resume target |

Notes: IAM instance-profile propagation needed one 5s retry at launch (the
retry loop in the driver is mandatory, not defensive). The interactive TTY
feel test (`--keep` mode + `aws ssm start-session`) hasn't been done yet.
