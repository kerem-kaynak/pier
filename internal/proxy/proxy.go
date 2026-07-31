// Package proxy gives every running session its own hostname.
//
// `pier proxy` runs a tiny split-DNS resolver for the .pier domain plus one
// multiplexed ssh master per running session, and mirrors the TCP ports the
// session actually listens on (from the supervisor beacon) onto a
// per-session loopback IP. The result, with zero per-port commands:
//
//	http://payments-retry.pier:3000     the session's dev server
//	psql -h payments-retry.pier         the session's database
//
// Ports appear when something in the session starts listening and vanish
// when it stops. Everything proxy-shaped lives in this package; the rest of
// pier contributes only Driver.SSHTarget (the raw ssh recipe) and the
// supervisor's "listening" beacon field.
//
// Live connections through a mirrored port count as attachment in the
// supervisor, so a session never parks under an open browser tab or psql —
// and a forgotten tab keeping a VM awake is deliberately the user's problem.
// The masters themselves and the beacon polls are park-neutral.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kerem-kaynak/pier/internal/driver"
	"github.com/kerem-kaynak/pier/internal/ui"
)

const (
	domain  = "pier"        // sessions resolve as <name>.pier
	dnsAddr = "127.94.0.53" // the resolver's own loopback alias
	// Not :53 — macOS lets unprivileged processes bind low ports only on the
	// wildcard address, never a specific IP. The resolver file's `port`
	// directive points mDNSResponder here instead.
	dnsPort   = "5533"
	slotBase  = 101 // session IPs: 127.94.0.101 .. +slotCount-1
	slotCount = 32

	listEvery  = 15 * time.Second // session-set refresh (cloud API call)
	portsEvery = 5 * time.Second  // beacon poll per session (rides the mux, no API)
)

type Options struct {
	StateDir string // control sockets live under StateDir/proxy
	Out      io.Writer
}

// Run blocks until ctx is cancelled (ctrl-c), reconciling workers against
// the running-session set. Masters and forwards die with the process; the
// loopback aliases and resolver file persist (inert, gone at reboot).
func Run(ctx context.Context, drv driver.Driver, opt Options) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("pier proxy is macOS-only for now — `pier port <session> <port>` works everywhere")
	}
	if err := ensureNet(opt.Out); err != nil {
		return err
	}
	ctlDir := filepath.Join(opt.StateDir, "proxy")
	if err := os.MkdirAll(ctlDir, 0o700); err != nil {
		return err
	}
	names := &table{m: map[string]net.IP{}}
	dns, err := serveDNS(net.JoinHostPort(dnsAddr, dnsPort), names.lookup)
	if err != nil {
		hint := ""
		if errors.Is(err, syscall.EADDRINUSE) {
			hint = " — is another pier proxy running?"
		}
		return fmt.Errorf("dns: %w%s", err, hint)
	}
	defer dns.Close()
	fmt.Fprintln(opt.Out, ui.Bold.Render("proxy up")+ui.Dim.Render(" — running sessions resolve as <session>."+domain+"; ctrl-c to stop"))

	workers := map[string]*worker{}
	slots := &slots{byID: map[string]int{}}
	var wg sync.WaitGroup
	first := true

	for {
		// Sweep workers that exited on their own (master died: park, rm, a
		// dropped tunnel). Their slot is only reusable now — the dying ssh
		// held the IP's listeners until the process was gone.
		for id, w := range workers {
			select {
			case <-w.done:
				delete(workers, id)
				slots.release(id)
			default:
			}
		}

		sessions, err := drv.List(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
		case err != nil:
			fmt.Fprintln(opt.Out, ui.Warn.Render("! session list failed: "+err.Error()))
		default:
			live := map[string]driver.Session{}
			for _, s := range sessions {
				switch s.State {
				case driver.StateRunning, driver.StateWorking, driver.StateIdle:
					live[s.ID] = s
				}
			}
			if first {
				fmt.Fprintf(opt.Out, ui.Dim.Render("watching %d running session(s)\n"), len(live))
				first = false
			}
			for id, w := range workers {
				if _, ok := live[id]; !ok {
					w.cancel() // exits soon; sweep + slot release next round
				}
			}
			for id, s := range live {
				if _, ok := workers[id]; ok {
					continue
				}
				ip, ok := slots.acquire(id)
				if !ok {
					fmt.Fprintln(opt.Out, ui.Warn.Render("! out of proxy slots ("+fmt.Sprint(slotCount)+") — skipping "+s.Name))
					continue
				}
				sshOpts, dest, err := drv.SSHTarget(ctx, id)
				if err != nil {
					fmt.Fprintln(opt.Out, ui.Warn.Render("! "+s.Name+": "+err.Error()))
					slots.release(id)
					continue
				}
				w := &worker{
					id: id, name: hostname(s.Name), dest: dest, ip: ip,
					ctl:  filepath.Join(ctlDir, id+".ctl"),
					opts: sshOpts, out: opt.Out, names: names,
					done: make(chan struct{}),
				}
				w.ctx, w.cancel = context.WithCancel(ctx)
				workers[id] = w
				wg.Add(1)
				go func() { defer wg.Done(); w.run() }()
			}
		}

		select {
		case <-ctx.Done():
			for _, w := range workers {
				w.cancel()
			}
			wg.Wait()
			fmt.Fprintln(opt.Out, ui.Dim.Render("proxy stopped — forwards closed (loopback aliases persist until reboot)"))
			return nil
		case <-time.After(listEvery):
		}
	}
}

// --- per-session worker --------------------------------------------------

// A worker owns one session's ssh master (-M -S ctl -N over the SSM tunnel),
// its DNS registration, and its live forward set. OpenSSH multiplexing does
// the heavy lifting: beacon polls are channels on the one connection, and
// forwards are added/removed on the fly with `ssh -O forward/cancel` — no
// per-port processes, no SSH library.
type worker struct {
	id, name, dest string
	ip             net.IP
	ctl            string
	opts           []string
	out            io.Writer
	names          *table
	ctx            context.Context
	cancel         context.CancelFunc
	done           chan struct{}
}

func (w *worker) run() {
	defer close(w.done)
	_ = os.Remove(w.ctl) // stale socket from a crashed proxy blocks -M

	var errb bytes.Buffer
	master := exec.CommandContext(w.ctx, "ssh", append(slices.Clone(w.opts), "-M", "-S", w.ctl, "-N", w.dest)...)
	master.Stderr = &errb
	if err := master.Start(); err != nil {
		w.logf(ui.Warn.Render("! %s: ssh: %v"), w.name, err)
		return
	}
	masterDone := make(chan error, 1)
	go func() { masterDone <- master.Wait() }()

	if !w.waitReady(masterDone) {
		if w.ctx.Err() == nil {
			w.logf(ui.Warn.Render("! %s: tunnel didn't come up: %s"), w.name, strings.TrimSpace(errb.String()))
		}
		return
	}
	if !w.names.set(w.name, w.ip) {
		w.logf(ui.Warn.Render("! %s.%s already taken by another session — %s gets no hostname"), w.name, domain, w.id)
		return
	}
	defer w.names.drop(w.name, w.ip)
	w.logf("%s %s", ui.Accent.Render("▸ "+w.name+"."+domain), ui.Dim.Render("→ "+w.ip.String()))
	defer func() {
		if w.ctx.Err() == nil { // master died on its own (parked?), not our shutdown
			w.logf(ui.Dim.Render("▸ %s.%s offline"), w.name, domain)
		}
	}()

	forwards := map[int]bool{}
	warnedOld := false
	tick := time.NewTicker(portsEvery)
	defer tick.Stop()
	for {
		if ports, hasField, err := w.beaconPorts(); err == nil {
			if !hasField && !warnedOld {
				w.logf(ui.Warn.Render("! %s predates port discovery — recreate the session to mirror ports (pier port still works)"), w.name)
				warnedOld = true
			}
			w.sync(ports, forwards)
		}
		select {
		case <-w.ctx.Done():
			return
		case <-masterDone:
			return
		case <-tick.C:
		}
	}
}

// waitReady polls `ssh -O check` until the master answers. SSM tunnels take
// a few seconds; a session that just resumed can take ~20s.
func (w *worker) waitReady(masterDone <-chan error) bool {
	deadline := time.After(45 * time.Second)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-w.ctx.Done():
			return false
		case <-masterDone:
			return false
		case <-deadline:
			return false
		case <-tick.C:
			if w.ctlOp(5*time.Second, "-O", "check") == nil {
				return true
			}
		}
	}
}

// beaconPorts reads the supervisor beacon over the mux. hasField
// distinguishes "no ports" from a pre-port-discovery supervisor.
func (w *worker) beaconPorts() (ports []int, hasField bool, err error) {
	ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
	defer cancel()
	args := append(slices.Clone(w.opts), "-S", w.ctl, w.dest, "cat /run/pier/status.json")
	out, err := exec.CommandContext(ctx, "ssh", args...).Output()
	if err != nil {
		return nil, false, err
	}
	var b struct {
		Listening []int `json:"listening"`
	}
	if json.Unmarshal(out, &b) != nil || b.Listening == nil {
		return nil, false, nil
	}
	return b.Listening, true, nil
}

// sync converges the live forward set on the beacon's port list.
func (w *worker) sync(ports []int, forwards map[int]bool) {
	want := map[int]bool{}
	for _, p := range ports {
		if p > 0 && p < 65536 {
			want[p] = true
		}
	}
	changed := false
	for p := range want {
		if forwards[p] {
			continue
		}
		if err := w.ctlOp(10*time.Second, "-O", "forward", "-L", w.spec(p)); err != nil {
			w.logf(ui.Warn.Render("! %s.%s: mirroring %d failed: %v"), w.name, domain, p, err)
		}
		forwards[p] = true // even on failure: converge, don't spam retries
		changed = true
	}
	for p := range forwards {
		if want[p] {
			continue
		}
		_ = w.ctlOp(10*time.Second, "-O", "cancel", "-L", w.spec(p))
		delete(forwards, p)
		changed = true
	}
	if changed {
		list := "(none)"
		if open := slices.Sorted(maps.Keys(forwards)); len(open) > 0 {
			parts := make([]string, len(open))
			for i, p := range open {
				parts[i] = strconv.Itoa(p)
			}
			list = strings.Join(parts, " ")
		}
		w.logf("  %s %s", ui.Bold.Render(w.name+"."+domain), ui.Dim.Render("ports: "+list))
	}
}

func (w *worker) spec(port int) string {
	return fmt.Sprintf("%s:%d:localhost:%d", w.ip, port, port)
}

// ctlOp runs a control operation against the master's socket with a timeout.
func (w *worker) ctlOp(timeout time.Duration, args ...string) error {
	ctx, cancel := context.WithTimeout(w.ctx, timeout)
	defer cancel()
	full := append(append(slices.Clone(w.opts), "-S", w.ctl), append(args, w.dest)...)
	out, err := exec.CommandContext(ctx, "ssh", full...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (w *worker) logf(format string, a ...any) {
	fmt.Fprintf(w.out, format+"\n", a...)
}

// --- small pieces ----------------------------------------------------------

// table maps hostnames to IPs for the DNS responder. Workers register on
// ready and deregister on exit; drop is guarded by IP so a collision loser
// can never evict the winner.
type table struct {
	mu sync.RWMutex
	m  map[string]net.IP
}

func (t *table) lookup(name string) (net.IP, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	ip, ok := t.m[name]
	return ip, ok
}

func (t *table) set(name string, ip net.IP) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, taken := t.m[name]; taken {
		return false
	}
	t.m[name] = ip
	return true
}

func (t *table) drop(name string, ip net.IP) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if have, ok := t.m[name]; ok && have.Equal(ip) {
		delete(t.m, name)
	}
}

// slots hands out the per-session loopback IPs (lowest free). Sized well
// above any realistic concurrent-session count.
type slots struct {
	byID map[string]int
	used [slotCount]bool
}

func (s *slots) acquire(id string) (net.IP, bool) {
	if i, ok := s.byID[id]; ok {
		return slotIP(i), true
	}
	for i := range s.used {
		if !s.used[i] {
			s.used[i] = true
			s.byID[id] = i
			return slotIP(i), true
		}
	}
	return nil, false
}

func (s *slots) release(id string) {
	if i, ok := s.byID[id]; ok {
		s.used[i] = false
		delete(s.byID, id)
	}
}

func slotIP(i int) net.IP { return net.IPv4(127, 94, 0, byte(slotBase+i)) }

var unsafeHost = regexp.MustCompile(`[^a-z0-9-]+`)

// hostname turns a session name (may contain "/") into a DNS label, the same
// way session VMs derive their hostname.
func hostname(name string) string {
	s := unsafeHost.ReplaceAllString(strings.ToLower(name), "-")
	s = strings.Trim(s, "-")
	if len(s) > 60 {
		s = s[:60]
	}
	if s == "" {
		s = "session"
	}
	return s
}
