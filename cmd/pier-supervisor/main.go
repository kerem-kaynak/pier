// pier-supervisor runs inside every session VM (installed by cloud-init /
// baked image). It is the entire "control plane": it decides when the session
// parks itself, and it needs zero cloud credentials to do so.
//
// Loop (every 30s):
//
//	attached := tmux has >=1 client
//	busy     := claude/codex process tree CPU above threshold within window,
//	            or recent pty output (heuristic — harnesses stay stock, no hooks)
//	if attached || busy       -> reset idle clock
//	if !attached && !busy     -> idle += tick; idle > IdleTimeout -> park
//	if !attached && busy for  -> longer than UnattendedCap        -> park
//	                             (runaway brake; TUI shows the reason)
//
// park = write reason+status file for the TUI, then `shutdown -h now`.
// On EC2 (shutdown-behavior=stop) and GCE this stops the instance with the
// disk intact — verified by the spike scripts.
//
// Config arrives via /etc/pier/supervisor.conf (written at create; per-session
// overrides like --idle / --no-park / `pier keep` rewrite it live via Exec).
package main

import (
	"fmt"
	"os"
)

func main() {
	// TODO: implement the loop above. Status beacon: /run/pier/status.json
	// (state, since, reason) — read on demand by the TUI via one-shot Exec.
	fmt.Fprintln(os.Stderr, "pier-supervisor: not implemented yet")
	os.Exit(1)
}
