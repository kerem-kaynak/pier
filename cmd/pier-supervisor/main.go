// pier-supervisor runs inside every session VM as the agent user (systemd
// unit installed at create). It is the entire "control plane": it decides
// when the session parks itself, with zero cloud credentials — parking is
// just `sudo shutdown -h now`, which stops the instance with the disk intact.
//
// Heuristic, checked every 30s:
//
//	attached := tmux reports >=1 client
//	busy     := any claude/codex process above CPU threshold,
//	            or a pty written to within the last tick
//
// attached or busy resets the idle clock. Detached+quiet past idle_timeout
// parks. Detached but busy continuously past unattended_cap parks too (the
// runaway brake). Config is re-read every tick so `pier keep` edits apply
// live. State beacon: /run/pier/status.json (read by ls/TUI via ssh).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	confPath   = "/etc/pier/supervisor.conf"
	statusPath = "/run/pier/status.json"
	tick       = 30 * time.Second
	cpuBusyPct = 5.0
	// Strain thresholds on the kernel's PSI avg60 (% of the last minute some
	// task sat stalled on the resource). Surfaced by ls/TUI as a resize hint;
	// the supervisor never acts on it — the VM has no cloud credentials.
	cpuStrainPct = 40.0
	memStrainPct = 10.0
)

type status struct {
	State    string    `json:"state"` // attached | working | idle
	Since    time.Time `json:"since"`
	Strained bool      `json:"strained,omitempty"`
	Reason   string    `json:"reason,omitempty"`
}

func main() {
	idleSince := time.Now()
	detachedBusySince := time.Time{}
	last := status{}

	for {
		idleTimeout, cap_ := readConf()
		attached := tmuxAttached()
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
		last.Strained = strained()
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

// ptyActive reports whether any pseudo-terminal was written to since the
// last tick — output flowing to a detached tmux pane counts as activity.
func ptyActive() bool {
	ents, err := filepath.Glob("/dev/pts/[0-9]*")
	if err != nil {
		return false
	}
	for _, p := range ents {
		if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) < tick+5*time.Second {
			return true
		}
	}
	return false
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
