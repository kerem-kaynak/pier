package awsec2

import (
	"archive/tar"
	"io"
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

// Names are spliced into the VM bootstrap script, so validation is the only
// thing standing between a hostile branch name and shell on the instance.
func TestValidateNames(t *testing.T) {
	ok := func(branch string) driver.CreateSpec {
		return driver.CreateSpec{Name: branch, Branch: branch, Repo: "/tmp/myrepo"}
	}
	for _, branch := range []string{"fix-auth", "feature/login-v2", "v1.2.3", "UPPER_case.mix"} {
		if err := validateNames(ok(branch)); err != nil {
			t.Errorf("branch %q rejected: %v", branch, err)
		}
	}
	for _, branch := range []string{
		"", "a'b", `a"b`, "a b", "a\tb", "a$b", "a`b", `a\b`, // shell metacharacters
		"-flag", ".hidden", "/abs", // hostile leading char
		"a..b", "a//b", "a/", "a.", "a.lock", // git-invalid shapes
		strings.Repeat("x", 101),
	} {
		if err := validateNames(ok(branch)); err == nil {
			t.Errorf("branch %q accepted", branch)
		}
	}
	// The session name is validated on its own when it differs from the branch.
	bad := driver.CreateSpec{Name: "a b", Branch: "main", Repo: "/tmp/myrepo"}
	if err := validateNames(bad); err == nil {
		t.Error("session name \"a b\" accepted")
	}
	// The repo directory becomes a path in the setup script: metacharacters
	// and whitespace are out, but ordinary punctuation-free unicode is fine.
	for _, repo := range []string{"/tmp/My Project", "/tmp/shop's", "/tmp/a$b", "/tmp/a`b"} {
		if err := validateNames(driver.CreateSpec{Name: "x", Branch: "x", Repo: repo}); err == nil {
			t.Errorf("repo %q accepted", repo)
		}
	}
	if err := validateNames(driver.CreateSpec{Name: "x", Branch: "x", Repo: "/tmp/übung-2026"}); err != nil {
		t.Errorf("repo /tmp/übung-2026 rejected: %v", err)
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
		// Setup outcome must be written for the beacon and the log, and its
		// shell variables must survive rendering escaped — they belong to the
		// tmux window's bash, not to the bootstrap shell.
		`echo running > ~/.pier-setup.status`,
		`c=\${PIPESTATUS[0]}`,
		`pier setup: FAILED (exit \$c)`,
		// The failure rename must target its own pane: a bare rename-window
		// resolved to the attached client's current window and mislabeled
		// the user's shell as setup-failed.
		`tmux rename-window -t \$TMUX_PANE setup-failed`,
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
	// Container-root writes on bind mounts must land agent-owned (the Docker
	// Desktop ownership behavior repos are written against): daemon config,
	// the uid-0 mapping, and the restart that applies it on pre-remap bakes.
	for _, remap := range []string{`{"userns-remap": "agent"}`, "agent:1000:1",
		"docker info 2>/dev/null | grep -q userns || systemctl restart docker"} {
		if !strings.Contains(ud, remap) {
			t.Errorf("user-data missing userns-remap piece %q", remap)
		}
	}
}

// Nothing loose ships by default — but the create warns about env files it
// is NOT carrying (untracked or ignored, any depth), minus wholly-ignored
// dirs (node_modules fixtures), tracked ones (they arrive with the fetch),
// and whatever .pier-include already carries.
func TestEnvFilesNotCarried(t *testing.T) {
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
	write(".env", "ROOT=1\n")                    // ignored env — warned about
	write("apps/api/.env", "API=1\n")            // ignored env, nested — warned about
	write("apps/api/.env.local", "LOCAL=1\n")    // ignored env, nested — warned about
	write("apps/api/main.go", "package main\n")  // untracked non-env — not the hint's business
	write("apps/api/.env.example", "X=\n")       // tracked below — arrives with the fetch
	write("node_modules/dep/.env", "FIXTURE=\n") // inside wholly-ignored dir
	write("debug.log", "x\n")                    // ignored non-env
	git("add", ".gitignore", "apps/api/.env.example")

	if files := pierIncludeFiles(root); files != nil {
		t.Errorf("no .pier-include must mean nothing loose ships, got %v", files)
	}
	got := envFilesNotCarried(root, nil)
	want := []string{".env", "apps/api/.env", "apps/api/.env.local"}
	if !slices.Equal(got, want) {
		t.Errorf("envFilesNotCarried = %v, want %v", got, want)
	}
	got = envFilesNotCarried(root, []string{"apps/api/.env"})
	want = []string{".env", "apps/api/.env.local"}
	if !slices.Equal(got, want) {
		t.Errorf("envFilesNotCarried minus carried = %v, want %v", got, want)
	}
}

// Uncommitted work on tracked files — edits and staged adds — travels as one
// patch; untracked files don't (that's .pier-include's channel). A clean
// tree writes no patch at all.
func TestDirtyPatch(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write("a.txt", "one\n")
	git("add", "a.txt")
	git("-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "commit", "-qm", "init")

	dst := filepath.Join(t.TempDir(), "dirty.patch")
	if ok, err := dirtyPatch(root, dst); err != nil || ok {
		t.Fatalf("clean tree: ok=%v err=%v, want no patch", ok, err)
	}

	write("a.txt", "two\n") // modified tracked — in
	write("b.txt", "new\n") // staged new file — in
	git("add", "b.txt")
	write("c.txt", "loose\n") // untracked — out
	ok, err := dirtyPatch(root, dst)
	if err != nil || !ok {
		t.Fatalf("dirty tree: ok=%v err=%v, want a patch", ok, err)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a.txt", "b.txt"} {
		if !strings.Contains(string(b), f) {
			t.Errorf("dirty patch missing %s", f)
		}
	}
	if strings.Contains(string(b), "c.txt") {
		t.Error("untracked file leaked into the dirty patch")
	}
}

// .pier-include is the only loose-file channel: lines are paths or globs,
// matched against the disk with no git-status distinction (the fixture isn't
// even a git repo). Directory lines carry their whole subtree; escapes and
// comments are dropped.
func TestPierInclude(t *testing.T) {
	root := t.TempDir()
	write := func(rel string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".pier-include"),
		[]byte("# what travels\napps/*/.env*\nuploads/\nsecrets.txt\n../escape\n/etc/passwd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	write("apps/api/.env")
	write("apps/api/.env.local")
	write("apps/api/config.yaml") // not listed
	write("apps/web/.env")
	write("uploads/fixtures/a.bin") // via the directory line
	write("secrets.txt")
	write(".env") // root env NOT listed — the file is the whole selection

	got := pierIncludeFiles(root)
	want := []string{"apps/api/.env", "apps/api/.env.local", "apps/web/.env",
		"secrets.txt", "uploads/fixtures/a.bin"}
	if !slices.Equal(got, want) {
		t.Errorf("with .pier-include = %v, want %v", got, want)
	}
}

// AGE reads the created tag: launch time resets on every resume and the
// beacon's state changes are even noisier, so create time must ride a tag.
func TestTagListCarriesCreateTime(t *testing.T) {
	spec := driver.CreateSpec{Name: "x", Repo: "/tmp/myrepo", Branch: "main"}
	for _, tag := range tagList(spec, "me", "pier-x") {
		if tag["Key"] != TagCreated {
			continue
		}
		if _, err := time.Parse(time.RFC3339, tag["Value"]); err != nil {
			t.Fatalf("%s value %q is not RFC3339: %v", TagCreated, tag["Value"], err)
		}
		return
	}
	t.Fatalf("tagList missing %s", TagCreated)
}

// Carried repo files must land world-readable (exec kept for scripts): the
// VM's docker daemon is userns-remapped, so a 0600 .env that works under
// Docker Desktop bind-mounts unreadable to every container on the VM.
func TestFilesTarWidensCarriedModes(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".pier-include"), []byte(".env\nrun.sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("K=v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "run.sh"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "files.tar")
	if err := buildFilesTar(dst, nil, root, nil, ""); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	want := map[string]int64{"repo/.env": 0o644, "repo/run.sh": 0o755}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if mode, ok := want[h.Name]; ok {
			if h.Mode != mode {
				t.Errorf("%s carried as %o, want %o", h.Name, h.Mode, mode)
			}
			delete(want, h.Name)
		}
	}
	for name := range want {
		t.Errorf("%s missing from the files tar", name)
	}
}
