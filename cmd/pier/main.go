// pier — coding agent sessions as park-when-idle micro-VMs on your own cloud.
package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/kerem-kaynak/pier/internal/config"
	"github.com/kerem-kaynak/pier/internal/driver"
	"github.com/kerem-kaynak/pier/internal/driver/awsec2"
	"github.com/kerem-kaynak/pier/internal/tui"
	"github.com/kerem-kaynak/pier/internal/ui"
	"github.com/kerem-kaynak/pier/internal/wizard"
)

//go:embed assets
var assets embed.FS

func supervisorBin(arch string) ([]byte, error) {
	b, err := assets.ReadFile("assets/pier-supervisor-linux-" + arch)
	if err != nil {
		return nil, fmt.Errorf("supervisor for %s not embedded — build with `make`, not `go build`", arch)
	}
	return b, nil
}

const usage = `pier — coding agent sessions as park-when-idle micro-VMs on your own cloud

usage:
  pier                      interactive session list
  pier <branch> [base]      new session off base (default HEAD), attach
      -d, --detach            create without attaching
      --idle <dur|never>      idle self-park timeout (default from config)
      --cap <dur|never>       unattended runaway cap
      --no-park               shorthand for --idle never
  pier ls                   list sessions
  pier attach <session>     attach (auto-resumes if parked)
  pier mcp login <session> [server]   one-time OAuth for an MCP server in the session
                            (no server: list the session's MCP servers + status)
  pier rm <session> [-f]    destroy session and its disk
  pier keep <session>       pin: disable idle self-park
  pier resize <session> <type>  grow/shrink the VM (running: ~40s park+resume; same arch only)
  pier setup                first-run wizard (creates cloud groundwork)
      --print-admin           print the admin-runnable setup commands instead
  pier doctor               environment + account checks
  pier bake                 prebake the session image (~60-90s creates)
  pier teardown             remove all pier groundwork from the account
`

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		cmdTUI()
		return
	}
	switch args[0] {
	case "ls":
		cmdLS()
	case "attach":
		cmdAttach(args[1:])
	case "mcp":
		cmdMCP(args[1:])
	case "rm":
		cmdRM(args[1:])
	case "keep":
		cmdKeep(args[1:])
	case "resize":
		cmdResize(args[1:])
	case "setup":
		cmdSetup(args[1:])
	case "doctor":
		cmdDoctor()
	case "bake":
		cmdBake()
	case "teardown":
		cmdTeardown()
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		cmdNew(args)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, ui.Bad.Render("pier:"), err)
	os.Exit(1)
}

func newDriver(cfg config.Config) (driver.Driver, error) {
	switch cfg.Driver {
	case "", "aws-ec2":
		return &awsec2.Driver{
			Profile:       cfg.AWS.Profile,
			Region:        cfg.AWS.Region,
			InstanceType:  cfg.AWS.InstanceType,
			DiskGiB:       cfg.AWS.DiskGiB,
			Subnet:        cfg.AWS.Subnet,
			BakedAMI:      cfg.AWS.BakedAMI,
			StateDir:      config.Dir(),
			Manifest:      cfg.Secrets.Manifest,
			SessionEnv:    sessionEnv(cfg),
			SupervisorBin: supervisorBin,
		}, nil
	case "gcp-gce":
		return nil, fmt.Errorf("the gcp-gce driver is parked for v1 — set driver = \"aws-ec2\"")
	default:
		return nil, fmt.Errorf("unknown driver %q", cfg.Driver)
	}
}

// sessionEnv builds ~/.config/pier/env for new sessions: a GitHub credential
// from wherever the laptop already has one (gh login or git's credential
// helper), Claude token from config (macOS keychain escape hatch).
func sessionEnv(cfg config.Config) map[string]string {
	env := map[string]string{}
	if t := cfg.Secrets.ClaudeOAuthToken; t != "" {
		env["CLAUDE_CODE_OAUTH_TOKEN"] = t
	}
	if t := awsec2.GitHubToken(); t != "" {
		env["GH_TOKEN"] = t
	}
	return env
}

func loadDriver() (config.Config, driver.Driver) {
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	drv, err := newDriver(cfg)
	if err != nil {
		fatal(err)
	}
	return cfg, drv
}

// --- new session ---------------------------------------------------------------

func cmdNew(args []string) {
	// Split positionals from flags so `pier my-branch -d` works (Go's flag
	// package stops at the first positional).
	takesValue := map[string]bool{"-idle": true, "--idle": true, "-cap": true, "--cap": true}
	var pos, flagArgs []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			if takesValue[a] && i+1 < len(args) {
				i++
				flagArgs = append(flagArgs, args[i])
			}
		} else {
			pos = append(pos, a)
		}
	}
	fs := flag.NewFlagSet("new", flag.ExitOnError)
	var detach, noPark bool
	fs.BoolVar(&detach, "d", false, "")
	fs.BoolVar(&detach, "detach", false, "")
	fs.BoolVar(&noPark, "no-park", false, "")
	idleS := fs.String("idle", "", "")
	capS := fs.String("cap", "", "")
	fs.Parse(flagArgs)
	if len(pos) < 1 || len(pos) > 2 {
		fmt.Print(usage)
		os.Exit(1)
	}
	branch := pos[0]
	base := "HEAD"
	if len(pos) == 2 {
		base = pos[1]
	}

	cfg, drv := loadDriver()
	repo := repoRoot()
	ctx := context.Background()

	sessions, err := drv.List(ctx)
	if err != nil {
		fatal(err)
	}
	for _, s := range sessions {
		if s.Name == branch {
			fatal(fmt.Errorf("session %q already exists — `pier attach %s`", branch, branch))
		}
	}

	idle, err := config.ParkDuration(or(*idleS, cfg.IdleTimeout))
	if err != nil {
		fatal(fmt.Errorf("--idle: %w", err))
	}
	if noPark {
		idle = 0
	}
	cap_, err := config.ParkDuration(or(*capS, cfg.UnattendedCap))
	if err != nil {
		fatal(fmt.Errorf("--cap: %w", err))
	}

	fmt.Printf("%s %s\n", ui.Bold.Render("creating "+branch),
		ui.Dim.Render(fmt.Sprintf("(%s @ %s)", filepath.Base(repo), base)))
	sess, err := drv.Create(ctx, driver.CreateSpec{
		Name: branch, Repo: repo, Branch: branch, BaseRef: base,
		IdleTimeout: idle, UnattendedCap: cap_,
		Progress: func(step string) { fmt.Println(ui.Accent.Render("  ▸"), step) },
	})
	if err != nil {
		fatal(err)
	}
	fmt.Println(ui.OK.Render("session " + sess.Name + " ready"))
	if detach {
		fmt.Println(ui.Dim.Render("attach with: pier attach " + sess.Name))
		return
	}
	attach(drv, sess.ID)
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func repoRoot() string {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		fatal(fmt.Errorf("not inside a git repository"))
	}
	return strings.TrimSpace(string(out))
}

func attach(drv driver.Driver, id string) {
	cmd, err := drv.AttachCommand(context.Background(), id)
	if err != nil {
		fatal(err)
	}
	fmt.Println(ui.Dim.Render("attaching — detach with C-b d (session keeps running)"))
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "pier: attach:", err)
	}
}

// --- ls / attach / rm / keep -----------------------------------------------------

func cmdLS() {
	_, drv := loadDriver()
	sessions, err := drv.List(context.Background())
	if err != nil {
		fatal(err)
	}
	enrich(drv, sessions)
	if len(sessions) == 0 {
		fmt.Println("no sessions — start one with `pier <branch>`")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tREPO\tSTATE\tAGE\tCOST")
	anyStrained := false
	for _, s := range sessions {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.Name, s.Repo, stateLabel(s), age(s.LastActive), s.CostNote)
		anyStrained = anyStrained || s.Strained
	}
	w.Flush()
	if anyStrained {
		fmt.Println("\n! strained = sustained cpu/mem pressure — grow with `pier resize <session> <type>`")
	}
}

// stateLabel renders the state plus the supervisor's strain flag.
func stateLabel(s driver.Session) string {
	if s.Strained {
		return string(s.State) + " (strained)"
	}
	return string(s.State)
}

// enrich upgrades StateRunning to working/idle by reading each running
// session's supervisor beacon in parallel.
func enrich(drv driver.Driver, sessions []driver.Session) {
	var wg sync.WaitGroup
	for i := range sessions {
		if sessions[i].State != driver.StateRunning {
			continue
		}
		wg.Add(1)
		go func(s *driver.Session) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			out, err := drv.Exec(ctx, s.ID, "cat /run/pier/status.json 2>/dev/null")
			if err != nil {
				return
			}
			var st struct {
				State    string    `json:"state"`
				Since    time.Time `json:"since"`
				Strained bool      `json:"strained"`
			}
			if json.Unmarshal([]byte(out), &st) != nil {
				return
			}
			switch st.State {
			case "working":
				s.State = driver.StateWorking
			case "idle":
				s.State = driver.StateIdle
			}
			s.Strained = st.Strained
			if !st.Since.IsZero() {
				s.LastActive = st.Since
			}
		}(&sessions[i])
	}
	wg.Wait()
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

func match(drv driver.Driver, query string) driver.Session {
	sessions, err := drv.List(context.Background())
	if err != nil {
		fatal(err)
	}
	var hits []driver.Session
	for _, s := range sessions {
		if s.Name == query {
			return s
		}
		if strings.Contains(s.Name, query) {
			hits = append(hits, s)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0]
	case 0:
		fatal(fmt.Errorf("no session matching %q", query))
	default:
		var names []string
		for _, h := range hits {
			names = append(names, h.Name)
		}
		fatal(fmt.Errorf("%q is ambiguous: %s", query, strings.Join(names, ", ")))
	}
	panic("unreachable")
}

func cmdAttach(args []string) {
	if len(args) != 1 {
		fatal(fmt.Errorf("usage: pier attach <session>"))
	}
	_, drv := loadDriver()
	s := match(drv, args[0])
	resumeIfParked(drv, s)
	attach(drv, s.ID)
}

var mcpServerName = regexp.MustCompile(`^[A-Za-z0-9._:@-]+$`)

// cmdMCP: `pier mcp login <session> [server]` — one-time OAuth for MCP
// servers whose tokens live in the laptop's keychain and can't be copied
// (they rotate; two machines sharing one revoke each other). The callback
// port rides the existing SSM tunnel, so the browser approval on the laptop
// completes the flow inside the VM: click, approve, done — no URL copying.
func cmdMCP(args []string) {
	if len(args) < 2 || args[0] != "login" {
		fatal(fmt.Errorf("usage: pier mcp login <session> [server]"))
	}
	ctx := context.Background()
	_, drv := loadDriver()
	s := match(drv, args[1])
	resumeIfParked(drv, s)

	if len(args) == 2 { // no server named: show what the session has
		fmt.Println(ui.Dim.Render("  asking the session for its MCP servers..."))
		out, err := drv.Exec(ctx, s.ID, "claude mcp list 2>&1 || true")
		if err != nil {
			fatal(err)
		}
		fmt.Println(out)
		fmt.Println("\nauthenticate one with " + ui.Accent.Render("pier mcp login "+s.Name+" <server>"))
		return
	}
	server := args[2]
	if !mcpServerName.MatchString(server) {
		fatal(fmt.Errorf("implausible server name %q", server))
	}
	port, err := freePort()
	if err != nil {
		fatal(err)
	}
	fmt.Println("  " + ui.Bold.Render("authenticating "+server) +
		ui.Dim.Render(" — open the URL below in your browser and approve"))
	fmt.Println(ui.Dim.Render("  the callback tunnels back into the session; the token lives on its disk across park/resume"))
	cmd, err := drv.MCPLoginCommand(ctx, s.ID, server, port)
	if err != nil {
		fatal(err)
	}
	if err := cmd.Run(); err != nil {
		fatal(fmt.Errorf("mcp login: %w", err))
	}
}

// freePort grabs an OS-assigned port. Local and remote must match: the OAuth
// redirect URL embeds the one port claude registers inside the VM.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func resumeIfParked(drv driver.Driver, s driver.Session) {
	if s.State == driver.StateParked {
		fmt.Printf("resuming %s (~20-30s)...\n", s.Name)
		if err := drv.Resume(context.Background(), s.ID); err != nil {
			fatal(err)
		}
	}
}

func cmdRM(args []string) {
	force := false
	var names []string
	for _, a := range args {
		if a == "-f" || a == "--force" {
			force = true
		} else {
			names = append(names, a)
		}
	}
	if len(names) != 1 {
		fatal(fmt.Errorf("usage: pier rm <session> [-f]"))
	}
	_, drv := loadDriver()
	s := match(drv, names[0])
	if !force && !confirm(fmt.Sprintf("destroy session %q and its disk?", s.Name), false) {
		return
	}
	if err := drv.Destroy(context.Background(), s.ID); err != nil {
		fatal(err)
	}
	fmt.Printf("destroyed %s\n", s.Name)
}

func cmdKeep(args []string) {
	if len(args) != 1 {
		fatal(fmt.Errorf("usage: pier keep <session>"))
	}
	_, drv := loadDriver()
	s := match(drv, args[0])
	if err := pin(drv, s); err != nil {
		fatal(err)
	}
	fmt.Printf("%s pinned — no idle self-park (unattended cap still applies)\n", s.Name)
}

// pin disables idle self-park; the supervisor re-reads its conf every tick,
// so this applies live.
func pin(drv driver.Driver, s driver.Session) error {
	if s.State == driver.StateParked {
		return fmt.Errorf("%s is parked — `pier attach %s` first", s.Name, s.Name)
	}
	_, err := drv.Exec(context.Background(), s.ID,
		"sudo sed -i 's/^idle_timeout=.*/idle_timeout=never/' /etc/pier/supervisor.conf")
	return err
}

func cmdResize(args []string) {
	if len(args) != 2 {
		fatal(fmt.Errorf("usage: pier resize <session> <instance-type>"))
	}
	_, drv := loadDriver()
	s := match(drv, args[0])
	itype := args[1]
	if s.State == driver.StateParked {
		fmt.Printf("resizing parked %s to %s (stays parked)\n", s.Name, itype)
	} else {
		fmt.Printf("resizing %s to %s — parks, resizes, resumes (~40-60s)\n", s.Name, itype)
	}
	if err := drv.Resize(context.Background(), s.ID, itype); err != nil {
		fatal(err)
	}
	fmt.Printf("%s is now a %s\n", s.Name, itype)
}

// --- setup / doctor / bake / teardown ---------------------------------------------

func cmdSetup(args []string) {
	printAdmin := len(args) > 0 && args[0] == "--print-admin"
	if err := wizard.Run(newDriver, printAdmin); err != nil {
		fatal(err)
	}
}

func cmdDoctor() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Println("! no config yet — checking with defaults (run `pier setup`)")
		cfg = config.Default()
	}
	drv, err := newDriver(cfg)
	if err != nil {
		fatal(err)
	}
	if !printChecks(drv.Doctor(context.Background())) {
		os.Exit(1)
	}
}

func printChecks(checks []driver.Check) bool {
	allOK := true
	for _, c := range checks {
		allOK = allOK && c.OK
		line := "  " + ui.Mark(c.OK) + " " + c.Name
		if c.Detail != "" {
			line += ui.Dim.Render(" — " + c.Detail)
		}
		fmt.Println(line)
	}
	return allOK
}

func cmdBake() {
	cfg, drv := loadDriver()
	fmt.Println("baking the session image: one temporary instance (~5 min), then an AMI (~$1-2/mo storage)")
	ami, err := drv.Bake(context.Background())
	if err != nil {
		fatal(err)
	}
	cfg.AWS.BakedAMI = ami
	if err := cfg.Save(); err != nil {
		fatal(err)
	}
	fmt.Printf("baked %s — new sessions now cold-start in ~60-90s\n", ami)
}

func cmdTeardown() {
	cfg, drv := loadDriver()
	if !confirm("remove all pier groundwork (role, instance profile, security group, baked AMI) from the account?", false) {
		return
	}
	if err := drv.Teardown(context.Background()); err != nil {
		fatal(err)
	}
	if cfg.AWS.BakedAMI != "" {
		cfg.AWS.BakedAMI = ""
		cfg.Save()
	}
	fmt.Println("groundwork removed — the account is clean")
}

func confirm(prompt string, def bool) bool {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	fmt.Printf("%s [%s] ", prompt, hint)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "" {
		return def
	}
	return line == "y" || line == "yes"
}

// --- TUI --------------------------------------------------------------------------

func cmdTUI() {
	_, drv := loadDriver()
	action, err := tui.Run(tui.Options{
		// Async: the TUI opens instantly and the quota fills in when it lands.
		FetchQuota: func() string {
			q, err := drv.Headroom(context.Background())
			if err != nil {
				return ""
			}
			return q.Detail
		},
		Fetch: func() ([]driver.Session, error) {
			sessions, err := drv.List(context.Background())
			if err != nil {
				return nil, err
			}
			enrich(drv, sessions)
			return sessions, nil
		},
		Destroy: func(s driver.Session) error {
			return drv.Destroy(context.Background(), s.ID)
		},
		Pin: func(s driver.Session) error {
			return pin(drv, s)
		},
	})
	if err != nil {
		fatal(err)
	}
	switch action.Kind {
	case tui.ActionAttach:
		resumeIfParked(drv, action.Session)
		attach(drv, action.Session.ID)
	case tui.ActionNew:
		cmdNew([]string{action.Branch})
	}
}
