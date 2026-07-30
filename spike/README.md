# spikes

By-hand verification of the load-bearing bets in [docs/SPEC.md §14](../docs/SPEC.md)
before any real implementation. Each script is self-contained, uses only
throwaway resources named `pier-spike`, cleans up after itself, and costs
about a cent per run.

| script  | verifies                                                        | status   |
|---------|-----------------------------------------------------------------|----------|
| aws.sh  | boot→SSM time, cmd RTT, `shutdown -h now` → **stopped**, resume | pending  |
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

*(fill in after each run: date, region, numbers)*
