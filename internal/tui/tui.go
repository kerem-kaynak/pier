// Package tui is the bare `pier` screen: a session list you can attach to,
// plus new/delete/pin/refresh. Deliberately plain — one hand-rolled list,
// no styling framework.
package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kerem-kaynak/pier/internal/driver"
)

type Options struct {
	Quota   string // e.g. "12/32 vCPUs in use"; "" hides it
	Fetch   func() ([]driver.Session, error)
	Destroy func(driver.Session) error
	Pin     func(driver.Session) error
}

type ActionKind int

const (
	ActionNone ActionKind = iota
	ActionAttach
	ActionNew
)

// Action is what the user picked; attach/new run after the TUI returns the
// terminal (ssh needs it).
type Action struct {
	Kind    ActionKind
	Session driver.Session
	Branch  string
}

func Run(opts Options) (Action, error) {
	final, err := tea.NewProgram(model{opts: opts, loading: true}).Run()
	if err != nil {
		return Action{}, err
	}
	return final.(model).action, nil
}

type mode int

const (
	modeList mode = iota
	modeNew
	modeConfirm
)

type model struct {
	opts     Options
	sessions []driver.Session
	cursor   int
	mode     mode
	input    string // branch name being typed in modeNew
	status   string // transient line: errors, confirmations
	loading  bool
	action   Action
}

type sessionsMsg struct {
	sessions []driver.Session
	err      error
}

func (m model) fetch() tea.Msg {
	ss, err := m.opts.Fetch()
	return sessionsMsg{ss, err}
}

func (m model) Init() tea.Cmd { return m.fetch }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case sessionsMsg:
		m.loading = false
		if msg.err != nil {
			m.status = msg.err.Error()
			return m, nil
		}
		m.sessions = msg.sessions
		if m.cursor >= len(m.sessions) {
			m.cursor = max(0, len(m.sessions)-1)
		}
		return m, nil
	case tea.KeyMsg:
		switch m.mode {
		case modeNew:
			return m.updateNew(msg)
		case modeConfirm:
			return m.updateConfirm(msg)
		}
		return m.updateList(msg)
	}
	return m, nil
}

func (m model) updateList(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.status = ""
	switch msg.String() {
	case "q", "ctrl+c", "esc":
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.sessions)-1 {
			m.cursor++
		}
	case "enter":
		if len(m.sessions) > 0 {
			m.action = Action{Kind: ActionAttach, Session: m.sessions[m.cursor]}
			return m, tea.Quit
		}
	case "n":
		m.mode = modeNew
		m.input = ""
	case "d":
		if len(m.sessions) > 0 {
			m.mode = modeConfirm
		}
	case "p":
		if len(m.sessions) > 0 {
			s := m.sessions[m.cursor]
			m.loading = true
			return m, func() tea.Msg {
				if err := m.opts.Pin(s); err != nil {
					return sessionsMsg{nil, err}
				}
				return m.fetch()
			}
		}
	case "r":
		m.loading = true
		return m, m.fetch
	}
	return m, nil
}

func (m model) updateNew(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.mode = modeList
	case "enter":
		if m.input != "" {
			m.action = Action{Kind: ActionNew, Branch: m.input}
			return m, tea.Quit
		}
	case "backspace":
		if len(m.input) > 0 {
			m.input = m.input[:len(m.input)-1]
		}
	default:
		if msg.Type == tea.KeyRunes && !strings.ContainsRune(string(msg.Runes), ' ') {
			m.input += string(msg.Runes)
		}
	}
	return m, nil
}

func (m model) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y":
		s := m.sessions[m.cursor]
		m.mode = modeList
		m.loading = true
		return m, func() tea.Msg {
			if err := m.opts.Destroy(s); err != nil {
				return sessionsMsg{nil, err}
			}
			return m.fetch()
		}
	default:
		m.mode = modeList
	}
	return m, nil
}

func (m model) View() string {
	var b strings.Builder
	head := fmt.Sprintf("pier — %d session(s)", len(m.sessions))
	if m.opts.Quota != "" {
		head += " · " + m.opts.Quota
	}
	if m.loading {
		head += " · ..."
	}
	b.WriteString(head + "\n\n")

	if len(m.sessions) == 0 && !m.loading {
		b.WriteString("  no sessions — press n to start one\n")
	}
	nameW, repoW := 4, 4
	for _, s := range m.sessions {
		nameW = max(nameW, len(s.Name))
		repoW = max(repoW, len(s.Repo))
	}
	for i, s := range m.sessions {
		marker := "  "
		if i == m.cursor {
			marker = "▸ "
		}
		fmt.Fprintf(&b, "%s%-*s  %-*s  %-8s  %-4s  %s\n",
			marker, nameW, s.Name, repoW, s.Repo, s.State, age(s.LastActive), s.CostNote)
	}
	b.WriteString("\n")

	switch m.mode {
	case modeNew:
		b.WriteString("new session branch: " + m.input + "█   (enter create · esc cancel)\n")
	case modeConfirm:
		fmt.Fprintf(&b, "destroy %q and its disk? (y/n)\n", m.sessions[m.cursor].Name)
	default:
		if m.status != "" {
			b.WriteString("! " + m.status + "\n")
		}
		b.WriteString("enter attach · n new · d delete · p pin · r refresh · q quit\n")
	}
	return b.String()
}

func age(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
