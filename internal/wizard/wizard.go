// Package wizard implements `pier setup`: detect → ask (plain prompts, ≤6
// questions) → apply groundwork → doctor → write config → offer bake.
// Everything detected becomes a prefilled default, so a second dev on a
// prepared account just presses enter a few times.
package wizard

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kerem-kaynak/pier/internal/config"
	"github.com/kerem-kaynak/pier/internal/driver"
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
# iam:PassRole on pier-session.
`

func Run(newDriver func(config.Config) (driver.Driver, error), printAdminOnly bool) error {
	if printAdminOnly {
		fmt.Print(adminDoc)
		return nil
	}
	in := bufio.NewReader(os.Stdin)
	ctx := context.Background()

	// 1. detect
	fmt.Println("pier setup — sessions run on your own AWS account; nothing leaves it.")
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
			fmt.Println("  aws profiles:", strings.Join(p, ", "))
		}
	}
	cfg.AWS.Profile = ask(in, "AWS profile", or(cfg.AWS.Profile, "default"))
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
			fmt.Println("    ~/" + m)
		}
		if !yes(in, "copy these into every session?", true) {
			cfg.Secrets.Manifest = nil
		}
	}

	if cfg.Secrets.ClaudeOAuthToken == "" {
		fmt.Println("  Claude subscription auth lives in the macOS Keychain and can't be copied.")
		fmt.Println("  Run `claude setup-token` in another terminal to mint a session token.")
		if tok := ask(in, "paste token (enter to skip)", ""); tok != "" {
			cfg.Secrets.ClaudeOAuthToken = tok
		}
	}

	// 3. apply
	drv, err := newDriver(cfg)
	if err != nil {
		return err
	}
	fmt.Println("\ncreating groundwork (IAM role + instance profile + egress-only security group)...")
	rep, err := drv.SetupOnce(ctx)
	if err != nil {
		return fmt.Errorf("%w\n\nno IAM rights? `pier setup --print-admin` prints the commands for your admin", err)
	}
	for _, c := range rep.Created {
		fmt.Println("  + created", c)
	}
	for _, e := range rep.Existed {
		fmt.Println("  = found", e)
	}

	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Println("  wrote", config.Path())

	// 4. doctor
	fmt.Println("\nchecks:")
	for _, c := range drv.Doctor(ctx) {
		mark := "✓"
		if !c.OK {
			mark = "✗"
		}
		if c.Detail != "" {
			fmt.Printf("  %s %s — %s\n", mark, c.Name, c.Detail)
		} else {
			fmt.Printf("  %s %s\n", mark, c.Name)
		}
	}

	// 5. offer bake
	fmt.Println()
	if yes(in, "bake the session image now? (~5 min once; cuts creates to ~60-90s)", true) {
		ami, err := drv.Bake(ctx)
		if err != nil {
			return err
		}
		cfg.AWS.BakedAMI = ami
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Println("  baked", ami)
	} else {
		fmt.Println("  (you can run `pier bake` anytime)")
	}

	fmt.Println("\ndone — try: cd <some-repo> && pier my-branch")
	return nil
}

// detectManifest proposes $HOME-relative agent config worth carrying into
// sessions (only entries that exist).
func detectManifest() []string {
	home, _ := os.UserHomeDir()
	candidates := []string{
		".claude/CLAUDE.md",
		".claude/settings.json",
		".claude/agents",
		".claude/.credentials.json", // Linux; macOS uses the token flow
		".codex/config.toml",
		".codex/auth.json",
		".codex/AGENTS.md",
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
		fmt.Printf("  %s [%s]: ", prompt, def)
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
