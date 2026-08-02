// Package tui is the bare `pier` screen: a session list you can attach to,
// plus new/delete/pin/refresh. Inline (no full-screen takeover), one accent
// color, states color-coded. Quota loads asynchronously so the screen opens
// instantly; a `loaded` flag separates "fetching" from "genuinely empty" so
// the list never flashes "0 sessions" before the first fetch lands.
package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/kerem-kaynak/pier/internal/driver"
	"github.com/kerem-kaynak/pier/internal/ui"
)

type Options struct {
	FetchQuota func() string // e.g. "12/32 vCPUs in use"; nil/"" hides it
	Fetch      func() ([]driver.Session, error)
	Destroy    func(driver.Session) error
	Pin        func(driver.Session) error
	// CreateDetached starts a background create and returns its log path;
	// the TUI stays open and the session appears in the list as "creating".
	// nil falls back to quit-and-create-in-foreground (ActionNew).
	CreateDetached func(branch string) (logPath string, err error)
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
	opts      Options
	sessions  []driver.Session
	quota     string
	cursor    int
	mode      mode
	input     string // branch name being typed in modeNew
	status    string // transient line: errors, confirmations
	statusBad bool   // status is an error (red) vs a notice (accent)
	loading   bool
	loaded    bool // first fetch has landed
	polling   bool // an auto-refresh poll is scheduled (creates in flight)
	watch     int  // poll rounds left after a spawn (until it's listable)
	frame     int  // spinner frame
	action    Action
}

func anyCreating(sessions []driver.Session) bool {
	for _, s := range sessions {
		if s.State == driver.StateCreating {
			return true
		}
	}
	return false
}

type sessionsMsg struct {
	sessions []driver.Session
	err      error
}

type quotaMsg string

type tickMsg struct{}

// pollMsg drives the slow auto-refresh that runs while a session is creating,
// so a background create's progress lands in the list without pressing r.
type pollMsg struct{}

// spawnedMsg reports a detached create kicked off (or failed to).
type spawnedMsg struct {
	branch string
	log    string
	err    error
}

func tickCmd() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

func pollCmd() tea.Cmd {
	return tea.Tick(5*time.Second, func(time.Time) tea.Msg { return pollMsg{} })
}

func (m model) fetch() tea.Msg {
	ss, err := m.opts.Fetch()
	return sessionsMsg{ss, err}
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.fetch, tickCmd()}
	if m.opts.FetchQuota != nil {
		cmds = append(cmds, func() tea.Msg { return quotaMsg(m.opts.FetchQuota()) })
	}
	return tea.Batch(cmds...)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case sessionsMsg:
		m.loading = false
		m.loaded = true
		if msg.err != nil {
			m.status, m.statusBad = msg.err.Error(), true
			return m, nil
		}
		m.sessions = msg.sessions
		if m.cursor >= len(m.sessions) {
			m.cursor = max(0, len(m.sessions)-1)
		}
		// While anything is still creating, keep refreshing on our own so a
		// background create walks to "running" without the user pressing r.
		if !m.polling && anyCreating(m.sessions) {
			m.polling = true
			return m, pollCmd()
		}
		return m, nil
	case pollMsg:
		m.polling = false
		if m.watch > 0 {
			m.watch--
		}
		// watch keeps the chain alive right after a spawn, before the new
		// instance is visible in the provider's list.
		if m.watch > 0 || anyCreating(m.sessions) {
			m.polling = true
			return m, tea.Batch(m.fetch, pollCmd()) // silent refresh: no spinner flicker
		}
		return m, nil
	case spawnedMsg:
		if msg.err != nil {
			m.status, m.statusBad = msg.err.Error(), true
			return m, nil
		}
		m.status, m.statusBad = "creating "+msg.branch+" in the background — it appears below shortly (log: "+msg.log+")", false
		m.watch = 12 // ~60s of polling even before the instance shows up
		if !m.polling {
			m.polling = true
			return m, pollCmd()
		}
		return m, nil
	case quotaMsg:
		m.quota = string(msg)
		return m, nil
	case tickMsg:
		if m.loading {
			m.frame++
			return m, tickCmd()
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
			if s := m.sessions[m.cursor]; s.State == driver.StateCreating {
				m.status, m.statusBad = s.Name+" is still setting up — attach when it shows running", false
				return m, nil
			}
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
			return m, tea.Batch(func() tea.Msg {
				if err := m.opts.Pin(s); err != nil {
					return sessionsMsg{nil, err}
				}
				return m.fetch()
			}, tickCmd())
		}
	case "r":
		m.loading = true
		return m, tea.Batch(m.fetch, tickCmd())
	}
	return m, nil
}

func (m model) updateNew(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.mode = modeList
	case "enter":
		if m.input == "" {
			return m, nil
		}
		if m.opts.CreateDetached == nil { // legacy: quit, create in foreground
			m.action = Action{Kind: ActionNew, Branch: m.input}
			return m, tea.Quit
		}
		for _, s := range m.sessions {
			if s.Name == m.input {
				m.mode = modeList
				m.status, m.statusBad = fmt.Sprintf("session %q already exists", m.input), true
				return m, nil
			}
		}
		branch := m.input
		m.mode, m.input = modeList, ""
		return m, func() tea.Msg {
			log, err := m.opts.CreateDetached(branch)
			return spawnedMsg{branch, log, err}
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
		return m, tea.Batch(func() tea.Msg {
			if err := m.opts.Destroy(s); err != nil {
				return sessionsMsg{nil, err}
			}
			return m.fetch()
		}, tickCmd())
	default:
		m.mode = modeList
	}
	return m, nil
}

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (m model) View() string {
	var b strings.Builder

	head := " " + ui.Title.Render("⚓ pier")
	var meta []string
	if m.loaded {
		meta = append(meta, fmt.Sprintf("%d session(s)", len(m.sessions)))
	}
	if m.quota != "" {
		meta = append(meta, m.quota)
	}
	if len(meta) > 0 {
		head += ui.Dim.Render("  " + strings.Join(meta, " · "))
	}
	if m.loading {
		head += " " + ui.Accent.Render(spinner[m.frame%len(spinner)])
	}
	b.WriteString("\n" + head + "\n\n")

	switch {
	case !m.loaded:
		b.WriteString(ui.Dim.Render("   fetching sessions…") + "\n")
	case len(m.sessions) == 0:
		b.WriteString(ui.Dim.Render("   no sessions — press n to start one") + "\n")
	default:
		b.WriteString(m.table())
	}
	b.WriteString("\n")

	switch m.mode {
	case modeNew:
		b.WriteString(" " + ui.Dim.Render("new session branch") + "\n")
		b.WriteString(ui.Box.Render(ui.Accent.Render("❯ ")+m.input+ui.Accent.Render("▌")) + "\n")
		b.WriteString(" " + ui.Keys("enter", "create", "esc", "cancel") + "\n")
	case modeConfirm:
		b.WriteString(" " + ui.Warn.Render(fmt.Sprintf("destroy %q and its disk? (y/n)", m.sessions[m.cursor].Name)) + "\n")
	default:
		if m.status != "" {
			if m.statusBad {
				b.WriteString(" " + ui.Bad.Render("! "+m.status) + "\n")
			} else {
				b.WriteString(" " + ui.Accent.Render("▸ "+m.status) + "\n")
			}
		}
		b.WriteString(" " + ui.Keys("enter", "attach", "n", "new", "d", "delete", "p", "pin", "r", "refresh", "q", "quit") + "\n")
	}
	return b.String()
}

// table renders the session rows: dim column header, accent cursor, states
// color-coded. Cells are padded as plain text first, then styled — ANSI
// escapes would defeat %-*s width math.
func (m model) table() string {
	nameW, repoW, stateW := 4, 4, 5
	states := make([]string, len(m.sessions))
	for i, s := range m.sessions {
		states[i] = stateCell(s)
		nameW = max(nameW, len(s.Name))
		repoW = max(repoW, len(s.Repo))
		stateW = max(stateW, len([]rune(states[i])))
	}

	var b strings.Builder
	b.WriteString(ui.Dim.Render(fmt.Sprintf("   %-*s  %-*s  %-*s  %-4s  %s",
		nameW, "NAME", repoW, "REPO", stateW, "STATE", "AGE", "COST")) + "\n")
	for i, s := range m.sessions {
		marker := "   "
		name := fmt.Sprintf("%-*s", nameW, s.Name)
		if i == m.cursor {
			marker = " " + ui.Accent.Render("▸") + " "
			name = ui.Bold.Render(name)
		}
		pad := stateW - len([]rune(states[i]))
		state := stateStyle(s).Render(states[i]) + strings.Repeat(" ", pad)
		fmt.Fprintf(&b, "%s%s  %s  %s  %-4s  %s\n",
			marker, name,
			ui.Dim.Render(fmt.Sprintf("%-*s", repoW, s.Repo)),
			state, age(s.LastActive), ui.Dim.Render(s.CostNote))
	}
	return b.String()
}

func stateCell(s driver.Session) string {
	dots := map[driver.State]string{
		driver.StateCreating: "◐",
		driver.StateRunning:  "●",
		driver.StateWorking:  "●",
		driver.StateIdle:     "●",
		driver.StateParked:   "◌",
		driver.StateDead:     "✗",
	}
	dot, ok := dots[s.State]
	if !ok {
		dot = "?"
	}
	cell := dot + " " + string(s.State)
	if s.Strained {
		cell += " ▲strained"
	}
	switch s.Setup {
	case "running":
		cell += " ⋯setup"
	case "failed":
		cell += " ✗setup"
	}
	return cell
}

func stateStyle(s driver.Session) lipgloss.Style {
	if s.Setup == "failed" {
		return ui.Bad
	}
	if s.Strained {
		return ui.Strain
	}
	switch s.State {
	case driver.StateRunning:
		return ui.OK
	case driver.StateWorking:
		return ui.Accent
	case driver.StateCreating, driver.StateIdle:
		return ui.Warn
	case driver.StateDead:
		return ui.Bad
	default: // parked
		return ui.Dim
	}
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
