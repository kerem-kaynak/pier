package awsec2

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kerem-kaynak/pier/internal/driver"
)

const (
	trustPolicy  = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	ssmPolicyARN = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
)

// SetupOnce creates the tiny per-account groundwork: an instance role whose
// only permission is SSM, its instance profile, and an egress-only security
// group. Idempotent — a second dev on the same account finds it all in Existed.
func (d *Driver) SetupOnce(ctx context.Context) (driver.SetupReport, error) {
	var rep driver.SetupReport
	note := func(created bool, what string) {
		if created {
			rep.Created = append(rep.Created, what)
		} else {
			rep.Existed = append(rep.Existed, what)
		}
	}

	if _, err := d.aws(ctx, "iam", "get-role", "--role-name", RoleName); err != nil {
		if _, err := d.aws(ctx, "iam", "create-role", "--role-name", RoleName,
			"--assume-role-policy-document", trustPolicy,
			"--description", "pier session instances: SSM access only",
			"--tags", "Key=pier:managed,Value=1"); err != nil {
			return rep, err
		}
		note(true, "IAM role "+RoleName)
	} else {
		note(false, "IAM role "+RoleName)
	}
	if _, err := d.aws(ctx, "iam", "attach-role-policy",
		"--role-name", RoleName, "--policy-arn", ssmPolicyARN); err != nil {
		return rep, err
	}

	if _, err := d.aws(ctx, "iam", "get-instance-profile", "--instance-profile-name", ProfileName); err != nil {
		if _, err := d.aws(ctx, "iam", "create-instance-profile",
			"--instance-profile-name", ProfileName,
			"--tags", "Key=pier:managed,Value=1"); err != nil {
			return rep, err
		}
		note(true, "instance profile "+ProfileName)
	} else {
		note(false, "instance profile "+ProfileName)
	}
	// Profiles hold at most one role; "LimitExceeded" here means already added.
	if _, err := d.aws(ctx, "iam", "add-role-to-instance-profile",
		"--instance-profile-name", ProfileName, "--role-name", RoleName); err != nil &&
		!strings.Contains(err.Error(), "LimitExceeded") {
		return rep, err
	}

	vpc, err := d.vpcID(ctx)
	if err != nil {
		return rep, err
	}
	sg, err := d.securityGroupID(ctx)
	if err != nil {
		return rep, err
	}
	if sg == "" {
		if _, err := d.aws(ctx, "ec2", "create-security-group",
			"--group-name", SecurityGroup,
			"--description", "pier sessions: egress only, zero inbound",
			"--vpc-id", vpc,
			"--tag-specifications", "ResourceType=security-group,Tags=[{Key=pier:managed,Value=1}]"); err != nil {
			return rep, err
		}
		note(true, "security group "+SecurityGroup+" (zero inbound)")
	} else {
		note(false, "security group "+SecurityGroup)
	}
	return rep, nil
}

// Teardown removes the groundwork. Refuses while sessions exist.
func (d *Driver) Teardown(ctx context.Context) error {
	sessions, err := d.List(ctx)
	if err != nil {
		return err
	}
	if len(sessions) > 0 {
		return fmt.Errorf("%d session(s) still exist — `pier rm` them first", len(sessions))
	}

	// ENIs of just-terminated instances release slowly; retry the SG delete.
	if sg, _ := d.securityGroupID(ctx); sg != "" {
		var derr error
		for range 20 {
			if _, derr = d.aws(ctx, "ec2", "delete-security-group", "--group-id", sg); derr == nil {
				break
			}
			time.Sleep(6 * time.Second)
		}
		if derr != nil {
			return derr
		}
	}

	ignoreMissing := func(err error) error {
		if err == nil || strings.Contains(err.Error(), "NoSuchEntity") {
			return nil
		}
		return err
	}
	if _, err := d.aws(ctx, "iam", "remove-role-from-instance-profile",
		"--instance-profile-name", ProfileName, "--role-name", RoleName); ignoreMissing(err) != nil {
		return err
	}
	if _, err := d.aws(ctx, "iam", "delete-instance-profile",
		"--instance-profile-name", ProfileName); ignoreMissing(err) != nil {
		return err
	}
	if _, err := d.aws(ctx, "iam", "detach-role-policy",
		"--role-name", RoleName, "--policy-arn", ssmPolicyARN); ignoreMissing(err) != nil {
		return err
	}
	if _, err := d.aws(ctx, "iam", "delete-role", "--role-name", RoleName); ignoreMissing(err) != nil {
		return err
	}

	if d.BakedAMI != "" {
		d.deregisterImage(ctx, d.BakedAMI)
	}

	os.RemoveAll(filepath.Join(d.StateDir, "keys"))
	os.Remove(filepath.Join(d.StateDir, "known_hosts"))
	return nil
}

func (d *Driver) Doctor(ctx context.Context) []driver.Check {
	var checks []driver.Check
	tool := func(name, bin, hint string) {
		_, err := exec.LookPath(bin)
		c := driver.Check{Name: name, OK: err == nil}
		if err != nil {
			c.Detail = hint
		}
		checks = append(checks, c)
	}
	tool("aws CLI", "aws", "brew install awscli")
	tool("session-manager-plugin", "session-manager-plugin", "brew install --cask session-manager-plugin")
	tool("ssh + ssh-keygen", "ssh-keygen", "install OpenSSH")

	// Optional but load-bearing for GitHub repos: without any of these,
	// private repos ship the slow full bundle and sessions can't push.
	gh := driver.Check{Name: "github credential", OK: true}
	switch {
	case GitHubToken() != "":
		gh.Detail = "token found — private-repo fast fetch + push from sessions"
	case exec.Command("ssh-add", "-l").Run() == nil:
		gh.Detail = "ssh agent only — fast fetch relays it; pushes work while attached (`gh auth login` for detached pushes)"
	default:
		gh.OK = false
		gh.Detail = "none found (optional) — private repos use the slow bundle path and sessions can't push; fix: `gh auth login`"
	}
	checks = append(checks, gh)

	me, err := d.user(ctx)
	if err != nil {
		checks = append(checks, driver.Check{Name: "AWS credentials", Detail: err.Error()})
		return checks // everything below needs credentials
	}
	checks = append(checks, driver.Check{Name: "AWS credentials", OK: true, Detail: me})

	if _, err := d.vpcID(ctx); err != nil {
		checks = append(checks, driver.Check{Name: "VPC", Detail: err.Error()})
	} else {
		checks = append(checks, driver.Check{Name: "VPC", OK: true})
	}

	_, roleErr := d.aws(ctx, "iam", "get-role", "--role-name", RoleName)
	_, profErr := d.aws(ctx, "iam", "get-instance-profile", "--instance-profile-name", ProfileName)
	sg, _ := d.securityGroupID(ctx)
	ok := roleErr == nil && profErr == nil && sg != ""
	c := driver.Check{Name: "groundwork (role, profile, SG)", OK: ok}
	if !ok {
		c.Detail = "run `pier setup`"
	}
	checks = append(checks, c)

	if d.SupervisorBin != nil {
		_, err := d.SupervisorBin("arm64")
		c := driver.Check{Name: "embedded supervisor", OK: err == nil}
		if err != nil {
			c.Detail = err.Error()
		}
		checks = append(checks, c)
	}

	if q, err := d.Headroom(ctx); err == nil {
		checks = append(checks, driver.Check{
			Name: "vCPU quota", OK: q.Limit-q.Used >= 2, Detail: q.Detail,
		})
	} else {
		checks = append(checks, driver.Check{Name: "vCPU quota", OK: true, Detail: "unknown (service-quotas denied)"})
	}
	return checks
}

// Headroom: on-demand standard-family vCPU quota vs. what is running now
// (all running instances, not just pier's — the quota is account-wide).
func (d *Driver) Headroom(ctx context.Context) (driver.Quota, error) {
	limitS, err := d.aws(ctx, "service-quotas", "get-service-quota",
		"--service-code", "ec2", "--quota-code", "L-1216C47A",
		"--query", "Quota.Value", "--output", "text")
	if err != nil {
		return driver.Quota{}, err
	}
	limit, _ := strconv.ParseFloat(limitS, 64)

	out, err := d.aws(ctx, "ec2", "describe-instances",
		"--filters", "Name=instance-state-name,Values=pending,running",
		"--query", "Reservations[].Instances[].CpuOptions.[CoreCount,ThreadsPerCore]",
		"--output", "text")
	if err != nil {
		return driver.Quota{}, err
	}
	used := 0
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			cores, _ := strconv.Atoi(f[0])
			threads, _ := strconv.Atoi(f[1])
			used += cores * threads
		}
	}
	q := driver.Quota{Used: used, Limit: int(limit)}
	q.Detail = fmt.Sprintf("%d/%d vCPUs in use", q.Used, q.Limit)
	return q, nil
}

func (d *Driver) vpcID(ctx context.Context) (string, error) {
	if d.Subnet != "" {
		return d.aws(ctx, "ec2", "describe-subnets", "--subnet-ids", d.Subnet,
			"--query", "Subnets[0].VpcId", "--output", "text")
	}
	out, err := d.aws(ctx, "ec2", "describe-vpcs",
		"--filters", "Name=is-default,Values=true",
		"--query", "Vpcs[0].VpcId", "--output", "text")
	if err != nil {
		return "", err
	}
	if out == "" || out == "None" {
		return "", fmt.Errorf("no default VPC in %s — set aws.subnet in %s", d.Region, "config")
	}
	return out, nil
}

func (d *Driver) securityGroupID(ctx context.Context) (string, error) {
	vpc, err := d.vpcID(ctx)
	if err != nil {
		return "", err
	}
	out, err := d.aws(ctx, "ec2", "describe-security-groups",
		"--filters", "Name=group-name,Values="+SecurityGroup, "Name=vpc-id,Values="+vpc,
		"--query", "SecurityGroups[0].GroupId", "--output", "text")
	if err != nil {
		return "", err
	}
	if out == "None" {
		return "", nil
	}
	return out, nil
}

func (d *Driver) deregisterImage(ctx context.Context, ami string) {
	snaps, _ := d.aws(ctx, "ec2", "describe-images", "--image-ids", ami,
		"--query", "Images[0].BlockDeviceMappings[].Ebs.SnapshotId", "--output", "text")
	_, _ = d.aws(ctx, "ec2", "deregister-image", "--image-id", ami)
	for _, s := range strings.Fields(snaps) {
		if s != "None" {
			_, _ = d.aws(ctx, "ec2", "delete-snapshot", "--snapshot-id", s)
		}
	}
}
