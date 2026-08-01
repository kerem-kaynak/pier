package awsec2

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kerem-kaynak/pier/internal/driver"
)

func TestSanitize(t *testing.T) {
	for in, want := range map[string]string{
		"fix-auth":          "fix-auth",
		"feat/big_Refactor": "feat-big-refactor",
		"//weird//":         "weird",
		"":                  "session",
	} {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDurConf(t *testing.T) {
	if got := durConf(0); got != "never" {
		t.Errorf("durConf(0) = %q, want never", got)
	}
	if got := durConf(30 * time.Minute); got != "30m0s" {
		t.Errorf("durConf(30m) = %q, want 30m0s", got)
	}
}

func TestClaudeSeed(t *testing.T) {
	home := t.TempDir()
	src, wd := "/Users/x/repo", "/home/agent/work/myrepo"
	if got := claudeSeed(home, src, wd); got != nil {
		t.Errorf("no local .claude.json should seed nothing, got %s", got)
	}
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"hasCompletedOnboarding":false,"theme":"dark"}`)
	if got := claudeSeed(home, src, wd); got != nil {
		t.Errorf("incomplete onboarding should seed nothing, got %s", got)
	}
	write(`{"hasCompletedOnboarding":true,"theme":"dark",
		"projects":{
			"/Users/x/repo":{"lastCost":1,"mcpServers":{
				"linear":{"type":"http","url":"https://mcp.linear.app/mcp"},
				"mactool":{"type":"stdio","command":"/opt/homebrew/bin/x"}}},
			"/Users/x/elsewhere":{"mcpServers":{"stray":{"type":"http","url":"https://s"}}}},
		"oauthAccount":{"x":1},
		"mcpServers":{
			"portable":{"type":"stdio","command":"npx","args":["-y","x"],"env":{"API_KEY":"k"}},
			"macapp":{"type":"stdio","command":"/Applications/X.app/Contents/MacOS/x"}}}`)
	got := string(claudeSeed(home, src, wd))
	for _, want := range []string{`"hasCompletedOnboarding":true`, `"theme":"dark"`,
		`"portable"`, `"API_KEY":"k"`, wd, `"hasTrustDialogAccepted":true`,
		`"linear"`, `"https://mcp.linear.app/mcp"`} { // project-scoped MCPs follow the repo
		if !strings.Contains(got, want) {
			t.Errorf("seed missing %s: %s", want, got)
		}
	}
	for _, bad := range []string{"macapp", "mactool", "stray", "oauthAccount", "/Users/x"} {
		if strings.Contains(got, bad) {
			t.Errorf("seed must not carry %s: %s", bad, got)
		}
	}
}

func TestOauthRemotes(t *testing.T) {
	home := t.TempDir()
	if got := OAuthRemotes(home, "/Users/x/repo"); got != nil {
		t.Errorf("no config should list nothing, got %v", got)
	}
	cfg := `{"mcpServers":{
			"keyed":{"type":"http","url":"https://api","headers":{"Authorization":"Bearer k"}},
			"tool":{"type":"stdio","command":"npx","env":{"KEY":"v"}}},
		"projects":{"/Users/x/repo":{"mcpServers":{
			"notion":{"type":"http","url":"https://mcp.notion.com/mcp"},
			"lin":{"type":"sse","url":"https://mcp.linear.app/sse"}}}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	got := OAuthRemotes(home, "/Users/x/repo")
	if want := []string{"lin", "notion"}; !slices.Equal(got, want) {
		t.Errorf("oauthRemotes = %v, want %v (header-auth'd and stdio servers excluded)", got, want)
	}
}

func TestFetchURL(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:org/repo.git":     "https://github.com/org/repo",
		"ssh://git@github.com/org/repo":   "https://github.com/org/repo",
		"https://github.com/org/repo.git": "https://github.com/org/repo.git",
		"https://gitlab.com/org/repo":     "",
		"git@bitbucket.org:org/repo.git":  "",
	} {
		if got := fetchURL(in); got != want {
			t.Errorf("fetchURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderBootstrapModes(t *testing.T) {
	spec := driver.CreateSpec{Name: "x", Repo: "/tmp/myrepo", Branch: "feat"}

	origin := renderBootstrap(spec, "origin", "abc123", "https://github.com/o/r")
	for _, want := range []string{
		"git fetch -q --no-tags origin abc123",
		"git remote add origin 'https://github.com/o/r'",
		"git reset -q --hard abc123",
		`[projects."/home/agent/work/myrepo"]`, // codex pre-trust
	} {
		if !strings.Contains(origin, want) {
			t.Errorf("origin-mode bootstrap missing %q", want)
		}
	}
	if strings.Contains(origin, "origin) git fetch -q /tmp/pier.bundle") {
		t.Error("origin mode must not fetch a bundle")
	}

	full := renderBootstrap(spec, "full", "abc123", "")
	if !strings.Contains(full, "git fetch -q /tmp/pier.bundle refs/pier/export") {
		t.Error("full-mode bootstrap must fetch the bundle")
	}
}

func TestRenderUserDataGuards(t *testing.T) {
	spec := driver.CreateSpec{Name: "feat/x", IdleTimeout: 30 * time.Minute}
	ud := renderUserData(spec, "ssh-ed25519 AAAA pier")
	if !strings.HasPrefix(ud, "#cloud-config\n") {
		t.Fatal("user-data must start with #cloud-config")
	}
	if !strings.Contains(ud, "hostname: feat-x") {
		t.Error("hostname not sanitized")
	}
	if !strings.Contains(ud, "idle_timeout=30m0s") {
		t.Error("supervisor conf not rendered")
	}
	// Every install step must be guarded so the same user-data is a fast
	// no-op on a baked AMI. Guards must test packages the stock image LACKS:
	// Ubuntu 24.04 ships tmux/git/jq/curl, so guarding the apt line on tmux
	// silently skipped it everywhere (the docker-less-sessions bug).
	for _, guard := range []string{"command -v docker", "command -v make", "command -v node",
		"docker compose version", "docker buildx version",
		"command -v gh", "command -v claude", "command -v codex", "grep -q 'pier/env'"} {
		if !strings.Contains(ud, guard) {
			t.Errorf("user-data missing idempotency guard %q", guard)
		}
	}
}

// A session gets the working tree as it sits: every untracked file plus the
// ignored .env* at any depth (monorepos keep them per-app). Wholly-ignored
// dirs (node_modules), other ignored files, and tracked files (they arrive
// with the fetch) stay out.
func TestRepoLooseFiles(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write(".gitignore", ".env\n.env.local\nnode_modules/\n*.log\n")
	write(".env", "ROOT=1\n")                    // ignored env — travels
	write("apps/api/.env", "API=1\n")            // ignored env, nested — travels
	write("apps/api/.env.local", "LOCAL=1\n")    // ignored env, nested — travels
	write("apps/api/main.go", "package main\n")  // untracked — travels
	write("notes.md", "wip\n")                   // untracked, root — travels
	write("apps/api/.env.example", "X=\n")       // tracked below — arrives with the fetch
	write("node_modules/dep/.env", "FIXTURE=\n") // inside wholly-ignored dir
	write("debug.log", "x\n")                    // ignored non-env — stays home
	git("add", ".gitignore", "apps/api/.env.example")

	got := repoLooseFiles(root)
	want := []string{".env", "apps/api/.env", "apps/api/.env.local", "apps/api/main.go", "notes.md"}
	if !slices.Equal(got, want) {
		t.Errorf("repoLooseFiles = %v, want %v", got, want)
	}
}
