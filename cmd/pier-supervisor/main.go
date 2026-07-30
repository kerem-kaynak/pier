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
)

type status struct {
	State  string    `json:"state"` // attached | working | idle
	Since  time.Time `json:"since"`
	Reason string    `json:"reason,omitempty"`
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
