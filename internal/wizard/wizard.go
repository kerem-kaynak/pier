// Package wizard implements `pier setup`: detect → ask (plain prompts, ≤6
// questions) → apply groundwork → doctor → write config → offer bake.
// Everything detected becomes a prefilled default, so a second dev on a
// prepared account just presses enter a few times.
package wizard

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kerem-kaynak/pier/internal/config"
	"github.com/kerem-kaynak/pier/internal/driver"
	"github.com/kerem-kaynak/pier/internal/ui"
)

// adminDoc is for devs without IAM rights: the exact groundwork their admin
// must create (equivalent to what SetupOnce does).
const adminDoc = `# pier groundwork — run with an account that can manage IAM + EC2.
# Everything pier creates is tagged pier:managed=1. Removal: pier teardown.

aws iam create-role --role-name pier-session \
  --assume-role-policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}' \
  --description "pier session instances: SSM access only" --tags Key=pier:managed,Value=1
aws iam attach-role-policy --role-name pier-session \
  --policy-arn arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore
aws iam create-instance-profile --instance-profile-name pier-session --tags Key=pier:managed,Value=1
aws iam add-role-to-instance-profile --instance-profile-name pier-session --role-name pier-session
aws ec2 create-security-group --group-name pier-egress-only \
  --description "pier sessions: egress only, zero inbound" --vpc-id <default-vpc-id> \
  --tag-specifications 'ResourceType=security-group,Tags=[{Key=pier:managed,Value=1}]'

# Devs then need permissions for: ec2 run/start/stop/terminate/describe*,
# ssm start-session + get-parameter, sts get-caller-identity,
# iam:PassRole on pier-session, and for direct connect (the default)
# ec2 describe-security-group-rules + authorize/revoke-security-group-ingress
# (pier keeps one TCP-22 rule per caller, scoped to their current public
# IP /32; aws.direct = false forces the SSM tunnel and needs none of it).
`

func Run(newDriver func(config.Config) (driver.Driver, error), printAdminOnly bool) error {
	if printAdminOnly {
		fmt.Print(adminDoc)
		return nil
	}
	in := bufio.NewReader(os.Stdin)
	ctx := context.Background()

	// 1. detect
	fmt.Println("\n " + ui.Title.Render("⚓ pier setup") +
		ui.Dim.Render("  sessions run on your own AWS account; nothing leaves it"))
	fmt.Println()
	for bin, hint := range map[string]string{
		"aws":                    "brew install awscli",
		"session-manager-plugin": "brew install --cask session-manager-plugin",
		"ssh-keygen":             "install OpenSSH",
		"git":                    "install git",
	} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s not found — %s, then re-run `pier setup`", bin, hint)
		}
	}

	cfg := config.Default()
	if existing, err := config.Load(); err == nil {
		cfg = existing // re-running keeps previous answers as defaults
	}

	// 2. ask
	if profiles, err := exec.Command("aws", "configure", "list-profiles").Output(); err == nil {
		if p := strings.Fields(string(profiles)); len(p) > 0 {
			fmt.Println(ui.Dim.Render("  aws profiles: " + strings.Join(p, ", ")))
		}
	}
	cfg.AWS.Profile = ask(in, "AWS profile", or(cfg.AWS.Profile, "default"))
	arn, err := checkIdentity(in, cfg.AWS.Profile)
	if err != nil {
		return err
	}
	fmt.Println("  "+ui.Mark(true), "authenticated as", ui.Bold.Render(arn))
	if cfg.AWS.Region == "" {
		out, _ := exec.Command("aws", "configure", "get", "region", "--profile", cfg.AWS.Profile).Output()
		cfg.AWS.Region = strings.TrimSpace(string(out))
	}
	cfg.AWS.Region = ask(in, "region", or(cfg.AWS.Region, "eu-central-1"))
	cfg.AWS.InstanceType = ask(in, "instance type", cfg.AWS.InstanceType)
	cfg.IdleTimeout = ask(in, "self-park after idling for (e.g. 30m, never)", cfg.IdleTimeout)

	if len(cfg.Secrets.Manifest) == 0 {
		cfg.Secrets.Manifest = detectManifest()
	}
	if len(cfg.Secrets.Manifest) > 0 {
		fmt.Println("  found agent config to copy into sessions:")
		for _, m := range cfg.Secrets.Manifest {
			fmt.Println(ui.Dim.Render("    ~/" + m))
		}
		if !yes(in, "copy these into every session?", true) {
			cfg.Secrets.Manifest = nil
		}
	}

	if cfg.Secrets.ClaudeOAuthToken == "" {
		if p := claudeSelfContained(); p != "" && slices.Contains(cfg.Secrets.Manifest, ".claude/settings.json") {
			fmt.Printf("  %s claude auth: %s in ~/.claude/settings.json %s\n",
				ui.Mark(true), p, ui.Dim.Render("— travels with the manifest, no token needed"))
		} else {
			fmt.Println(ui.Dim.Render("  Claude subscription auth lives in the macOS Keychain and can't be copied."))
			fmt.Println(ui.Dim.Render("  Run `claude setup-token` in another terminal to mint a session token."))
			if tok := ask(in, "paste token (enter to skip)", ""); tok != "" {
				cfg.Secrets.ClaudeOAuthToken = tok
			}
		}
	}

	// 3. apply
	drv, err := newDriver(cfg)
	if err != nil {
		return err
	}
	fmt.Println("\n " + ui.Accent.Render("creating groundwork") +
		ui.Dim.Render("  IAM role + instance profile + egress-only security group"))
	rep, err := drv.SetupOnce(ctx)
	if err != nil {
		// Only blame IAM rights when it actually is a permissions error.
		if low := strings.ToLower(err.Error()); strings.Contains(low, "accessdenied") || strings.Contains(low, "not authorized") {
			return fmt.Errorf("%w\n\nno IAM rights? `pier setup --print-admin` prints the commands for your admin", err)
		}
		return err
	}
	for _, c := range rep.Created {
		fmt.Println("  " + ui.OK.Render("+") + " created " + c)
	}
	for _, e := range rep.Existed {
		fmt.Println(ui.Dim.Render("  = found " + e))
	}

	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Println(ui.Dim.Render("  wrote " + config.Path()))

	// 4. doctor
	fmt.Println("\n " + ui.Accent.Render("checks"))
	for _, c := range drv.Doctor(ctx) {
		line := "  " + ui.Mark(c.OK) + " " + c.Name
		if c.Detail != "" {
			line += ui.Dim.Render(" — " + c.Detail)
		}
		fmt.Println(line)
	}

	// 5. offer bake — images are repo-specific, so only when the wizard runs
	// inside a repo; otherwise point at `pier bake` from one.
	fmt.Println()
	if repo := gitToplevel(); repo == "" {
		fmt.Println(ui.Dim.Render("  (images bake per repo: cd <repo> && pier bake — ~5 min once, cuts creates to ~60-90s)"))
	} else if name := filepath.Base(repo); yes(in, "bake the session image for "+name+" now? (~5 min once; cuts creates to ~60-90s)", true) {
		ami, err := drv.Bake(ctx, driver.BakeSpec{RepoName: name, HookPath: driver.BakeHook(repo)})
		if err != nil {
			return err
		}
		cfg.AWS.BakedAMIs = map[string]string{name: ami}
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Println("  "+ui.Mark(true), "baked", ami, "for", name)
	} else {
		fmt.Println(ui.Dim.Render("  (you can run `pier bake` in any repo, anytime)"))
	}

	fmt.Println("\n " + ui.OK.Render("done") + " — try: " +
		ui.Accent.Render("cd <some-repo> && pier my-branch"))
	return nil
}

// checkIdentity fails fast on dead credentials — right after the profile
// question, not six prompts later at groundwork. It also shows which
// account/ARN is about to be touched. Expired SSO gets the exact login
// command and an offer to run it inline.
// gitToplevel is the enclosing repo's root, "" when the wizard runs outside
// any repo (bake needs one — images are repo-specific).
func gitToplevel() string {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func checkIdentity(in *bufio.Reader, profile string) (string, error) {
	sts := func() (string, error) {
		out, err := exec.Command("aws", "sts", "get-caller-identity",
			"--query", "Arn", "--output", "text", "--profile", profile).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("%s", strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out)), nil
	}
	arn, err := sts()
	if err == nil {
		return arn, nil
	}
	if low := strings.ToLower(err.Error()); strings.Contains(low, "sso") && (strings.Contains(low, "expired") || strings.Contains(low, "refresh")) {
		fmt.Printf("  %s the SSO session for profile %q has expired\n", ui.Mark(false), profile)
		if yes(in, "run `aws sso login --profile "+profile+"` now?", true) {
			login := exec.Command("aws", "sso", "login", "--profile", profile)
			login.Stdin, login.Stdout, login.Stderr = os.Stdin, os.Stdout, os.Stderr
			if login.Run() == nil {
				return sts()
			}
		}
		return "", fmt.Errorf("profile %q needs `aws sso login --profile %s`, then re-run `pier setup`", profile, profile)
	}
	return "", fmt.Errorf("credentials for profile %q aren't working: %w", profile, err)
}

// claudeSelfContained reports whether ~/.claude/settings.json's env block
// alone authenticates claude on a fresh VM (Foundry/API-key setups — the
// file travels with the manifest). Bedrock/Vertex/Entra need cloud creds
// that deliberately never enter sessions, so those still get the token
// prompt.
func claudeSelfContained() string {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".claude/settings.json"))
	if err != nil {
		return ""
	}
	var s struct {
		Env map[string]string `json:"env"`
	}
	if json.Unmarshal(b, &s) != nil {
		return ""
	}
	switch {
	case s.Env["CLAUDE_CODE_USE_FOUNDRY"] == "1" && s.Env["ANTHROPIC_FOUNDRY_API_KEY"] != "":
		return "foundry deployment + api key"
	case s.Env["ANTHROPIC_API_KEY"] != "":
		return "anthropic api key"
	case s.Env["ANTHROPIC_AUTH_TOKEN"] != "":
		return "auth token"
	}
	return ""
}

// detectManifest proposes $HOME-relative agent config worth carrying into
// sessions (only entries that exist).
func detectManifest() []string {
	home, _ := os.UserHomeDir()
	candidates := []string{
		".claude/CLAUDE.md",
		".claude/settings.json",
		".claude/agents",
		".claude/commands",
		".claude/skills",
		".claude/plugins",
		".claude/.credentials.json", // Linux; macOS uses the token flow
		".codex/config.toml",
		".codex/auth.json",
		".codex/AGENTS.md",
		".codex/prompts",
		".codex/skills",
		// NOT .codex/plugins: a machine-specific runtime cache (hundreds of
		// MB); config.toml declares the plugins and codex re-materializes.
		".tmux.conf", // absent → bootstrap seeds mouse mode + deep history
	}
	var found []string
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(home, c)); err == nil {
			found = append(found, c)
		}
	}
	return found
}

func ask(in *bufio.Reader, prompt, def string) string {
	if def != "" {
		fmt.Printf("  %s %s: ", prompt, ui.Dim.Render("["+def+"]"))
	} else {
		fmt.Printf("  %s: ", prompt)
	}
	line, _ := in.ReadString('\n')
	if line = strings.TrimSpace(line); line != "" {
		return line
	}
	return def
}

func yes(in *bufio.Reader, prompt string, def bool) bool {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	line := strings.ToLower(ask(in, prompt+" ["+hint+"]", ""))
	if line == "" {
		return def
	}
	return line == "y" || line == "yes"
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
