package awsec2

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kerem-kaynak/pier/internal/driver"
	"github.com/kerem-kaynak/pier/internal/ui"
)

// Bake launches a throwaway instance with the exact session user-data, lets
// cloud-init finish the harness install, runs the repo's .pier-bake.sh (if
// any), and images the result. Because every install step in the user-data is
// guarded, sessions launched from the baked AMI skip straight past it — cold
// create drops to boot + push time (~60-90s). Images are repo-specific: the
// hook is where a repo's toolchains (pnpm, python, ...) get baked in.
func (d *Driver) Bake(ctx context.Context, spec driver.BakeSpec) (string, error) {
	arch, err := d.archOf(ctx, d.InstanceType)
	if err != nil {
		return "", err
	}
	// Always bake from the stock Ubuntu AMI (not a previous bake) so images
	// don't accrete layers.
	ami, err := d.resolveAMI(ctx, arch, "")
	if err != nil {
		return "", err
	}

	me, err := d.user(ctx)
	if err != nil {
		return "", err
	}
	cspec := driver.CreateSpec{Name: "bake", Repo: "bake", Branch: "bake"} // timeouts 0 = never park mid-bake
	pub, err := d.newKeypair("bake")
	if err != nil {
		return "", err
	}
	defer os.Remove(d.keyPath("bake"))
	defer os.Remove(d.keyPath("bake") + ".pub")

	work, err := os.MkdirTemp("", "pier-bake-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	udPath := filepath.Join(work, "user-data.yaml")
	if err := os.WriteFile(udPath, []byte(renderUserData(cspec, pub)), 0o600); err != nil {
		return "", err
	}

	id, err := d.launch(ctx, cspec, me, ami, udPath)
	if err != nil {
		return "", err
	}
	// The bake instance must never outlive this call, success or failure.
	defer d.aws(context.WithoutCancel(ctx), "ec2", "terminate-instances", "--instance-ids", id)
	os.Rename(d.keyPath("bake"), d.keyPath(id))
	os.Rename(d.keyPath("bake")+".pub", d.keyPath(id)+".pub")
	defer os.Remove(d.keyPath(id))
	defer os.Remove(d.keyPath(id) + ".pub")
	fmt.Println(ui.Step("bake instance " + id + " launched — installing harnesses (a few minutes)"))

	if err := d.waitSSH(ctx, id, 240*time.Second); err != nil {
		return "", err
	}
	// On failure, say WHICH harness is missing and show the install log tail
	// — "did not complete" alone sends people digging through a VM that this
	// function is about to terminate.
	verify := `sudo cloud-init status --wait >/dev/null
miss=""
for c in claude codex gh; do command -v "$c" >/dev/null || miss="$miss $c"; done
[ -z "$miss" ] && exit 0
echo "not installed:$miss — cloud-init log tail:"
sudo tail -n 25 /var/log/cloud-init-output.log
exit 1`
	if _, err := d.sshRun(ctx, id, verify); err != nil {
		return "", fmt.Errorf("harness install did not complete: %w", err)
	}
	if spec.HookPath != "" {
		fmt.Println(ui.Step("running .pier-bake.sh (output follows)"))
		if err := d.scpTo(ctx, id, spec.HookPath, "/tmp/pier-bake.sh"); err != nil {
			return "", err
		}
		if err := d.sshStream(ctx, id, "bash /tmp/pier-bake.sh && rm -f /tmp/pier-bake.sh"); err != nil {
			return "", fmt.Errorf(".pier-bake.sh failed — nothing baked: %w", err)
		}
	}
	// Per-instance state must not leak into the image.
	if _, err := d.sshRun(ctx, id, "rm -f ~/.ssh/authorized_keys && sudo rm -rf /etc/pier"); err != nil {
		return "", err
	}

	fmt.Println(ui.Step("imaging — stop, snapshot, register (a few minutes)"))
	if _, err := d.aws(ctx, "ec2", "stop-instances", "--instance-ids", id); err != nil {
		return "", err
	}
	if _, err := d.aws(ctx, "ec2", "wait", "instance-stopped", "--instance-ids", id); err != nil {
		return "", err
	}
	repo := sanitize(spec.RepoName)
	name := "pier-" + repo + "-" + time.Now().Format("20060102-1504")
	tags := "Tags=[{Key=pier:managed,Value=1},{Key=pier:repo,Value=" + repo + "}]"
	img, err := d.aws(ctx, "ec2", "create-image", "--instance-id", id, "--name", name,
		"--description", "pier session base for "+repo+" (harnesses + bake hook preinstalled)",
		"--tag-specifications",
		"ResourceType=image,"+tags,
		"ResourceType=snapshot,"+tags,
		"--query", "ImageId", "--output", "text")
	if err != nil {
		return "", err
	}
	if _, err := d.aws(ctx, "ec2", "wait", "image-available", "--image-ids", img); err != nil {
		return "", err
	}

	for _, old := range spec.Replaces {
		if old != "" && old != img {
			d.deregisterImage(ctx, old)
		}
	}
	return img, nil
}
