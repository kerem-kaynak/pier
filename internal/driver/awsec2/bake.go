package awsec2

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kerem-kaynak/pier/internal/driver"
)

// Bake launches a throwaway instance with the exact session user-data, lets
// cloud-init finish the harness install, and images the result. Because every
// install step in the user-data is guarded, sessions launched from the baked
// AMI skip straight past it — cold create drops to boot + push time (~60-90s).
func (d *Driver) Bake(ctx context.Context) (string, error) {
	arch, err := d.archOf(ctx, d.InstanceType)
	if err != nil {
		return "", err
	}
	// Always bake from the stock Ubuntu AMI (not a previous bake) so images
	// don't accrete layers.
	base := d.BakedAMI
	d.BakedAMI = ""
	ami, err := d.resolveAMI(ctx, arch)
	d.BakedAMI = base
	if err != nil {
		return "", err
	}

	me, err := d.user(ctx)
	if err != nil {
		return "", err
	}
	spec := driver.CreateSpec{Name: "bake", Repo: "bake", Branch: "bake"} // timeouts 0 = never park mid-bake
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
	if err := os.WriteFile(udPath, []byte(renderUserData(spec, pub)), 0o600); err != nil {
		return "", err
	}

	id, err := d.launch(ctx, spec, me, ami, udPath)
	if err != nil {
		return "", err
	}
	// The bake instance must never outlive this call, success or failure.
	defer d.aws(context.WithoutCancel(ctx), "ec2", "terminate-instances", "--instance-ids", id)
	os.Rename(d.keyPath("bake"), d.keyPath(id))
	os.Rename(d.keyPath("bake")+".pub", d.keyPath(id)+".pub")
	defer os.Remove(d.keyPath(id))
	defer os.Remove(d.keyPath(id) + ".pub")
	fmt.Println("  bake instance", id, "launched — installing harnesses (a few minutes)")

	if err := d.waitSSH(ctx, id, 240*time.Second); err != nil {
		return "", err
	}
	if _, err := d.sshRun(ctx, id, "sudo cloud-init status --wait >/dev/null && command -v claude && command -v codex && command -v gh"); err != nil {
		return "", fmt.Errorf("harness install did not complete: %w", err)
	}
	// Per-instance state must not leak into the image.
	if _, err := d.sshRun(ctx, id, "rm -f ~/.ssh/authorized_keys && sudo rm -rf /etc/pier"); err != nil {
		return "", err
	}

	fmt.Println("  imaging...")
	if _, err := d.aws(ctx, "ec2", "stop-instances", "--instance-ids", id); err != nil {
		return "", err
	}
	if _, err := d.aws(ctx, "ec2", "wait", "instance-stopped", "--instance-ids", id); err != nil {
		return "", err
	}
	name := "pier-base-" + time.Now().Format("20060102-1504")
	img, err := d.aws(ctx, "ec2", "create-image", "--instance-id", id, "--name", name,
		"--description", "pier session base (harnesses preinstalled)",
		"--tag-specifications",
		"ResourceType=image,Tags=[{Key=pier:managed,Value=1}]",
		"ResourceType=snapshot,Tags=[{Key=pier:managed,Value=1}]",
		"--query", "ImageId", "--output", "text")
	if err != nil {
		return "", err
	}
	if _, err := d.aws(ctx, "ec2", "wait", "image-available", "--image-ids", img); err != nil {
		return "", err
	}

	if d.BakedAMI != "" && d.BakedAMI != img {
		d.deregisterImage(ctx, d.BakedAMI)
	}
	return img, nil
}
