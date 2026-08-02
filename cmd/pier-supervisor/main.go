// pier-supervisor runs inside every session VM as the agent user (systemd
// unit installed at create). It is the entire "control plane": it decides
// when the session parks itself, with zero cloud credentials — parking is
// just `sudo shutdown -h now`, which stops the instance with the disk intact.
//
// Heuristic, checked every 5s (a fast tick keeps the beacon's port list
// fresh for `pier proxy`; the activity windows below still span ~30s):
//
//	attached := tmux reports >=1 client, or a live forwarded TCP connection
//	            (someone's browser/psql on a mirrored port — never park
//	            under an open connection; a forgotten tab keeping the VM
//	            awake is the user's call)
//	busy     := any claude/codex process above CPU threshold,
//	            or a pty written to within the last ~30s
//
// attached or busy resets the idle clock. Detached+quiet past idle_timeout
// parks. Detached but busy continuously past unattended_cap parks too (the
// runaway brake). Config is re-read every tick so `pier keep` edits apply
// live. State beacon: /run/pier/status.json (read by ls/TUI via ssh) — also
// carries the listening TCP ports so `pier proxy` knows what to mirror.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	confPath   = "/etc/pier/supervisor.conf"
	statusPath = "/run/pier/status.json"
	tick       = 5 * time.Second
	ptyWindow  = 35 * time.Second
	cpuBusyPct = 5.0
	// Strain thresholds on the kernel's PSI avg60 (% of the last minute some
	// task sat stalled on the resource). Surfaced by ls/TUI as a resize hint;
	// the supervisor never acts on it — the VM has no cloud credentials.
	cpuStrainPct = 40.0
	memStrainPct = 10.0
)

type status struct {
	State string    `json:"state"` // attached | working | idle
	Since time.Time `json:"since"`
	// Listening is every user-relevant TCP port the session listens on
	// (sshd/resolved excluded) — `pier proxy` mirrors exactly these. Always
	// present, even empty: its absence means a pre-port-discovery supervisor.
	Listening []int `json:"listening"`
	Tunnels   int   `json:"tunnels,omitempty"` // live forwarded connections
	// Bootstrapping is true until the create's bootstrap writes its marker
	// (repo fetched, tmux up) — ls/TUI keep showing "creating" meanwhile.
	// omitempty keeps old-session beacons (no marker ever) reading as false.
	Bootstrapping bool `json:"bootstrapping,omitempty"`
	Strained      bool `json:"strained,omitempty"`
	// Setup is the repo setup script's outcome: "running" | "failed", empty
	// when there is no script (or it succeeded — quiet is the happy path).
	Setup  string `json:"setup,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func main() {
	idleSince := time.Now()
	detachedBusySince := time.Time{}
	last := status{}

	for {
		idleTimeout, cap_ := readConf()
		listening, tunnels := netSnapshot()
		attached := tmuxAttached() || tunnels > 0
		busy := agentBusy() || ptyActive()

		now := time.Now()
		st := "idle"
		switch {
		case attached:
			st = "attached"
		case busy:
			st = "working"
		}
		if attached || busy {
			idleSince = now
		}
		if !attached && busy {
			if detachedBusySince.IsZero() {
				detachedBusySince = now
			}
		} else {
			detachedBusySince = time.Time{}
		}
		if last.State != st {
			last = status{State: st, Since: now}
		}
		last.Listening, last.Tunnels = listening, tunnels
		last.Bootstrapping = bootstrapping()
		last.Strained = strained()
		last.Setup = setupState()
		writeStatus(last)

		if idleTimeout > 0 && !attached && !busy && now.Sub(idleSince) > idleTimeout {
			park("idle for " + idleTimeout.String())
		}
		if cap_ > 0 && !detachedBusySince.IsZero() && now.Sub(detachedBusySince) > cap_ {
			park("unattended cap (" + cap_.String() + ") hit while working detached")
		}

		time.Sleep(tick)
	}
}

// readConf parses KEY=VALUE lines: idle_timeout=30m, unattended_cap=8h.
// "never"/"0"/absent key = disabled. Unreadable file = never park (safe).
func readConf() (idle, cap_ time.Duration) {
	b, err := os.ReadFile(confPath)
	if err != nil {
		return 0, 0
	}
	get := func(key string) time.Duration {
		for _, line := range strings.Split(string(b), "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
			if !ok || k != key || v == "never" || v == "0" {
				continue
			}
			if d, err := time.ParseDuration(v); err == nil {
				return d
			}
		}
		return 0
	}
	return get("idle_timeout"), get("unattended_cap")
}

func tmuxAttached() bool {
	out, err := exec.Command("tmux", "list-clients").Output()
	return err == nil && len(strings.TrimSpace(string(out))) > 0
}

// agentBusy scans ps for claude/codex processes above the CPU threshold.
func agentBusy() bool {
	out, err := exec.Command("ps", "-eo", "pcpu,args").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		args := strings.Join(f[1:], " ")
		if !strings.Contains(args, "claude") && !strings.Contains(args, "codex") {
			continue
		}
		if strings.Contains(args, "pier-supervisor") {
			continue
		}
		if pcpu, err := strconv.ParseFloat(f[0], 64); err == nil && pcpu >= cpuBusyPct {
			return true
		}
	}
	return false
}

// ptyActive reports whether any pseudo-terminal was written to recently —
// output flowing to a detached tmux pane counts as activity. The window is
// fixed (not tied to the tick) so the faster tick didn't tighten what
// "recently" means.
func ptyActive() bool {
	ents, err := filepath.Glob("/dev/pts/[0-9]*")
	if err != nil {
		return false
	}
	for _, p := range ents {
		if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) < ptyWindow {
			return true
		}
	}
	return false
}

// bootstrapping reports whether the create's bootstrap has yet to write its
// done-marker. New supervisors only ship on sessions whose bootstrap writes
// it, so a missing marker means "still setting up" (or a create that died —
// either way, not a session to present as ready).
func bootstrapping() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(home, ".pier-bootstrapped"))
	return err != nil
}

// setupState reads the setup script's outcome marker, written by the
// bootstrap's tmux window: "running" while the script runs, its exit code
// once it finishes. The file lives in $HOME, so unlike this beacon it
// survives park/resume — a failure stays visible. No marker means no setup
// script (or a pre-marker session): nothing to report.
func setupState() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(home, ".pier-setup.status"))
	if err != nil {
		return ""
	}
	switch strings.TrimSpace(string(b)) {
	case "running":
		return "running"
	case "0", "":
		return ""
	default:
		return "failed"
	}
}

// netSnapshot surveys the session's TCP state in one `ss` pass: which ports
// are listening (for the proxy to mirror) and how many live forwarded
// connections exist (they block parking). Process names need root — the
// agent user's NOPASSWD sudo covers it. Any failure reads as "no ports, no
// tunnels": the proxy just sees nothing and parking falls back to the
// tmux/CPU heuristics.
func netSnapshot() ([]int, int) {
	out, err := exec.Command("sudo", "-n", "ss", "-Htnap").Output()
	if err != nil {
		return []int{}, 0
	}
	return parseSS(string(out))
}

// parseSS reads `ss -Htnap` output. Listening ports exclude the plumbing
// (sshd, systemd-resolved) and dedupe v4/v6. A "tunnel" is an established
// connection owned by sshd that isn't the :22 transport itself — exactly the
// loopback dials sshd makes to serve a forwarded port, and never app→app
// traffic (a dev server holding a pooled DB connection must not block
// parking forever).
func parseSS(out string) (listening []int, tunnels int) {
	seen := map[int]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		local, peer, proc := f[3], f[4], strings.Join(f[5:], " ")
		switch f[0] {
		case "LISTEN":
			if strings.Contains(proc, `"sshd"`) || strings.Contains(proc, `"systemd-resolve`) {
				continue
			}
			if p := addrPort(local); p > 0 && p != 22 && !seen[p] {
				seen[p] = true
				listening = append(listening, p)
			}
		case "ESTAB":
			if strings.Contains(proc, `"sshd"`) && addrPort(local) != 22 && addrPort(peer) != 22 {
				tunnels++
			}
		}
	}
	sort.Ints(listening)
	if listening == nil {
		listening = []int{}
	}
	return listening, tunnels
}

// addrPort pulls the port off ss address forms: 0.0.0.0:22, *:3000,
// 127.0.0.53%lo:53, [::1]:8080.
func addrPort(addr string) int {
	i := strings.LastIndexByte(addr, ':')
	if i < 0 {
		return 0
	}
	p, err := strconv.Atoi(addr[i+1:])
	if err != nil {
		return 0
	}
	return p
}

// strained reports sustained resource pressure. avg60 already smooths over a
// minute, so no extra hysteresis. No /proc/pressure (kernel without PSI) or
// parse trouble reads as 0 = not strained.
func strained() bool {
	return psiAvg60("/proc/pressure/cpu") >= cpuStrainPct ||
		psiAvg60("/proc/pressure/memory") >= memStrainPct
}

func psiAvg60(path string) float64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return parsePSIAvg60(string(b))
}

// parsePSIAvg60 pulls avg60 off the "some" line:
//
//	some avg10=0.00 avg60=12.34 avg300=5.00 total=123456
func parsePSIAvg60(s string) float64 {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "some" {
			continue
		}
		for _, kv := range f[1:] {
			if v, ok := strings.CutPrefix(kv, "avg60="); ok {
				if x, err := strconv.ParseFloat(v, 64); err == nil {
					return x
				}
			}
		}
	}
	return 0
}

func writeStatus(s status) {
	_ = os.MkdirAll(filepath.Dir(statusPath), 0o755)
	b, _ := json.Marshal(s)
	_ = os.WriteFile(statusPath, b, 0o644)
}

func park(reason string) {
	writeStatus(status{State: "parking", Since: time.Now(), Reason: reason})
	fmt.Println("pier-supervisor: parking:", reason)
	_ = exec.Command("sudo", "shutdown", "-h", "now").Run()
	os.Exit(0)
}
