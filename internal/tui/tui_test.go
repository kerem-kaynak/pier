package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/kerem-kaynak/pier/internal/config"
	"github.com/kerem-kaynak/pier/internal/driver"
	"github.com/kerem-kaynak/pier/internal/driver/awsec2"
)

// View smoke tests: the load-flash bug was View rendering "0 sessions"
// before the first fetch landed, so pin the three distinct states.
func TestViewStates(t *testing.T) {
	m := model{loading: true}
	if v := m.View(); !strings.Contains(v, "fetching") {
		t.Errorf("pre-fetch view must show a loading line, not a session count:\n%s", v)
	}
	if v := m.View(); strings.Contains(v, "0 session") || strings.Contains(v, "no sessions") {
		t.Errorf("pre-fetch view leaked an empty-state:\n%s", v)
	}

	m.loaded = true
	if v := m.View(); !strings.Contains(v, "no sessions") {
		t.Errorf("loaded-empty view must say no sessions:\n%s", v)
	}

	m.sessions = []driver.Session{
		{Name: "fix-auth", Repo: "myapp", State: driver.StateWorking, Created: time.Now()},
		{Name: "big-build", Repo: "myapp", State: driver.StateRunning, Strained: true},
		{Name: "bad-deps", Repo: "myapp", State: driver.StateIdle, Setup: "failed"},
	}
	v := m.View()
	for _, want := range []string{"fix-auth", "big-build", "working", "strained", "(setup failed)", "NAME"} {
		if !strings.Contains(v, want) {
			t.Errorf("list view missing %q:\n%s", want, v)
		}
	}
}

// The settings page renders every settable key and rejects bad values while
// editing (staying in edit so the value can be fixed) without touching disk.
func TestSettingsPage(t *testing.T) {
	cfg := config.Default()
	m := model{mode: modeSettings, cfg: &cfg}

	v := m.View()
	for _, want := range []string{"pier settings", "idle_timeout", "aws.instance_type", "t4g.medium"} {
		if !strings.Contains(v, want) {
			t.Errorf("settings view missing %q:\n%s", want, v)
		}
	}

	// Move to idle_timeout, edit, type a bad duration, try to save.
	m.setIdx = 1 // idle_timeout
	got, _ := m.updateSettings(tea.KeyMsg{Type: tea.KeyEnter})
	m = got.(model)
	if !m.editing || m.setInput != "30m" {
		t.Fatalf("enter must start editing with the current value, got editing=%v input=%q", m.editing, m.setInput)
	}
	m.setInput = "not-a-duration"
	got, _ = m.updateSettings(tea.KeyMsg{Type: tea.KeyEnter})
	m = got.(model)
	if !m.editing || !m.statusBad {
		t.Errorf("bad duration must stay in edit with an error, got editing=%v status=%q", m.editing, m.status)
	}
	if cfg.IdleTimeout != "30m" {
		t.Errorf("bad value leaked into config: %q", cfg.IdleTimeout)
	}
}

// The m key opens the resize picker preselected on the session's current
// type, and enter on that same type is a no-op notice, not a resize call.
func TestResizePicker(t *testing.T) {
	resized := ""
	m := model{loaded: true,
		sessions: []driver.Session{{Name: "fix-auth", Repo: "myapp", State: driver.StateRunning, InstanceType: "t4g.medium"}},
		opts: Options{
			Fetch:    func() ([]driver.Session, error) { return nil, nil },
			Resize:   func(s driver.Session, itype string) error { resized = itype; return nil },
			Machines: func(s driver.Session) []driver.Machine { return awsec2.Machines(s.InstanceType) },
		}}
	got, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")})
	m = got.(model)
	if m.mode != modeResize {
		t.Fatalf("m must open the picker, got mode=%v status=%q", m.mode, m.status)
	}
	if cur := m.machines[m.machIdx].Type; cur != "t4g.medium" {
		t.Errorf("picker must preselect the current type, got %q", cur)
	}
	v := m.View()
	for _, want := range []string{"resize fix-auth", "t4g.large", "(current)", "vCPU", "GiB"} {
		if !strings.Contains(v, want) {
			t.Errorf("picker view missing %q:\n%s", want, v)
		}
	}

	// Enter on the current type: notice, no resize.
	got, cmd := m.updateResize(tea.KeyMsg{Type: tea.KeyEnter})
	m = got.(model)
	if cmd != nil || resized != "" {
		t.Errorf("enter on the current type must not resize, got resized=%q", resized)
	}
	if m.mode != modeList || !strings.Contains(m.status, "already") {
		t.Errorf("want an already-that-size notice back on the list, got mode=%v status=%q", m.mode, m.status)
	}

	// Reopen, move down one, confirm: the resize command fires.
	got, _ = m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")})
	m = got.(model)
	got, _ = m.updateResize(tea.KeyMsg{Type: tea.KeyDown})
	m = got.(model)
	want := m.machines[m.machIdx].Type
	got, cmd = m.updateResize(tea.KeyMsg{Type: tea.KeyEnter})
	m = got.(model)
	if cmd == nil {
		t.Fatal("enter on a different type must return the resize command")
	}
	if batch, ok := cmd().(tea.BatchMsg); ok { // Batch wraps, doesn't run
		for _, c := range batch {
			c()
		}
	}
	if resized != want {
		t.Errorf("resize called with %q, want %q", resized, want)
	}
	if !m.loading || !strings.Contains(m.status, "resizing fix-auth") {
		t.Errorf("want spinner + resizing status, got loading=%v status=%q", m.loading, m.status)
	}
}

// Enter on a still-creating session must not leave the TUI to spawn ssh —
// mid-create attaches used to dump a raw TargetNotConnected. It shows a
// notice and stays put instead.
func TestEnterOnCreatingSession(t *testing.T) {
	m := model{loaded: true, sessions: []driver.Session{
		{Name: "half-built", Repo: "myapp", State: driver.StateCreating},
	}}
	got, cmd := m.updateList(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatal("enter on a creating session must not quit to attach")
	}
	gm := got.(model)
	if gm.action.Kind == ActionAttach {
		t.Error("attach action set for a creating session")
	}
	if !strings.Contains(gm.status, "still setting up") || gm.statusBad {
		t.Errorf("want a friendly notice, got status=%q bad=%v", gm.status, gm.statusBad)
	}
}
