package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/kerem-kaynak/pier/internal/driver"
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
		{Name: "fix-auth", Repo: "myapp", State: driver.StateWorking, LastActive: time.Now()},
		{Name: "big-build", Repo: "myapp", State: driver.StateRunning, Strained: true},
	}
	v := m.View()
	for _, want := range []string{"fix-auth", "big-build", "working", "strained", "NAME"} {
		if !strings.Contains(v, want) {
			t.Errorf("list view missing %q:\n%s", want, v)
		}
	}
}
