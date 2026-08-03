package awsec2

// direct.go: the default fast path. Every ssh connection dials sshd on the
// instance's public IPv4 instead of riding the SSM tunnel, turning tunnel
// throughput (~100KB/s-1MB/s) into plain line rate and typing echo into raw
// RTT. pier keeps exactly one inbound rule per caller in the pier security
// group — TCP 22 from the caller's current public IP as a /32, recognized
// again by its description — and the SSM path stays as the automatic
// fallback wherever direct can't work (no public IP, a network that blocks
// outbound 22). aws.direct = false forces the tunnel for every connection.
// `pier teardown` deletes the group, rules included.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Probe results are trusted only briefly so long-lived processes (the
// proxy) heal within a minute after a park/resume hands the VM a new
// public IP or the laptop changes networks.
const (
	directTTL = time.Minute
	// "connection refused" means the VM is mid-boot: the SG is open but
	// sshd isn't up yet. Retry almost immediately, so a fresh create goes
	// direct the moment sshd listens instead of waiting out a full TTL.
	directBootTTL = 3 * time.Second
)

type directProbe struct {
	ip    string // "" = use the SSM tunnel
	until time.Time
}

// directIP returns the instance's public address when the direct path is
// enabled, allowed and answering — or "" for the SSM tunnel.
func (d *Driver) directIP(ctx context.Context, id string) string {
	if !d.Direct {
		return ""
	}
	d.dmu.Lock()
	p, ok := d.dprobe[id]
	d.dmu.Unlock()
	if ok && time.Now().Before(p.until) {
		return p.ip
	}
	ip, ttl := d.probeDirect(ctx, id)
	d.dmu.Lock()
	if d.dprobe == nil {
		d.dprobe = map[string]directProbe{}
	}
	d.dprobe[id] = directProbe{ip: ip, until: time.Now().Add(ttl)}
	d.dmu.Unlock()
	return ip
}

func (d *Driver) probeDirect(ctx context.Context, id string) (string, time.Duration) {
	out, err := d.aws(ctx, "ec2", "describe-instances", "--instance-ids", id,
		"--query", "Reservations[0].Instances[0].[PublicIpAddress,SecurityGroups[0].GroupId]",
		"--output", "text")
	if err != nil {
		return "", directTTL
	}
	f := strings.Fields(out)
	if len(f) != 2 || f[0] == "None" || f[1] == "None" {
		// No public address (yet): still pending, parked, or a private
		// subnet. Cheap to re-check.
		return "", directBootTTL
	}
	ip, sg := f[0], f[1]
	if err := d.ensureDirectRule(ctx, sg); err != nil {
		d.directNotice(id, "direct connect: "+err.Error()+" — using the ssm tunnel")
		return "", directTTL
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort(ip, "22"), 2*time.Second)
	if err != nil {
		if strings.Contains(err.Error(), "refused") {
			return "", directBootTTL
		}
		d.directNotice(id, "direct connect: "+ip+":22 does not answer from this network — using the ssm tunnel")
		return "", directTTL
	}
	c.Close()
	return ip, directTTL
}

const directRuleDesc = "pier direct"

// ensureDirectRule reconciles this caller's one inbound rule: TCP 22 from
// the current public IP /32. The caller's stale rules (old addresses) are
// revoked; other callers' rules are left alone. Two machines behind
// different networks sharing one IAM identity will steal the rule from each
// other, healing on the loser's next probe — messy but self-correcting,
// with the tunnel covering the gap.
func (d *Driver) ensureDirectRule(ctx context.Context, sg string) error {
	ip, err := d.callerPublicIP(ctx)
	if err != nil {
		return err
	}
	cidr := ip + "/32"
	d.dmu.Lock()
	done := d.ensured == cidr
	d.dmu.Unlock()
	if done {
		return nil
	}
	desc := directRuleDesc + " " + d.shortUser(ctx)
	out, err := d.aws(ctx, "ec2", "describe-security-group-rules",
		"--filters", "Name=group-id,Values="+sg,
		"--query", "SecurityGroupRules[?IsEgress==`false`].[SecurityGroupRuleId,CidrIpv4,Description]",
		"--output", "json")
	if err != nil {
		return err
	}
	var rules [][]string
	if err := json.Unmarshal([]byte(out), &rules); err != nil {
		return err
	}
	have := false
	for _, r := range rules {
		if len(r) != 3 || r[2] != desc {
			continue
		}
		if r[1] == cidr {
			have = true
			continue
		}
		_, _ = d.aws(ctx, "ec2", "revoke-security-group-ingress",
			"--group-id", sg, "--security-group-rule-ids", r[0])
	}
	if !have {
		// A concurrent pier process may have just added it: Duplicate is fine.
		if _, err := d.aws(ctx, "ec2", "authorize-security-group-ingress",
			"--group-id", sg, "--ip-permissions",
			fmt.Sprintf("IpProtocol=tcp,FromPort=22,ToPort=22,IpRanges=[{CidrIp=%s,Description=%s}]",
				cidr, desc)); err != nil && !strings.Contains(err.Error(), "Duplicate") {
			return err
		}
	}
	d.dmu.Lock()
	d.ensured = cidr
	d.dmu.Unlock()
	return nil
}

// callerPublicIP asks checkip.amazonaws.com — over IPv4, so the answer is
// the address the security group will see when ssh dials the instance's
// IPv4.
func (d *Driver) callerPublicIP(ctx context.Context) (string, error) {
	d.dmu.Lock()
	if d.myIP != "" && time.Now().Before(d.myIPUntil) {
		ip := d.myIP
		d.dmu.Unlock()
		return ip, nil
	}
	d.dmu.Unlock()

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp4", addr)
			},
		},
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "https://checkip.amazonaws.com", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("public IP lookup: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", fmt.Errorf("public IP lookup: %w", err)
	}
	s := strings.TrimSpace(string(b))
	if ip := net.ParseIP(s); ip == nil || ip.To4() == nil {
		return "", fmt.Errorf("public IP lookup: implausible answer %q", s)
	}
	d.dmu.Lock()
	d.myIP, d.myIPUntil = s, time.Now().Add(directTTL)
	d.dmu.Unlock()
	return s, nil
}

// shortUser is the caller's IAM name (the ARN's last segment), sanitized to
// characters SG rule descriptions accept — it keys this caller's rule.
func (d *Driver) shortUser(ctx context.Context) string {
	arn, err := d.user(ctx)
	if err != nil {
		return "unknown"
	}
	return sanitizeRuleName(arn[strings.LastIndex(arn, "/")+1:])
}

func sanitizeRuleName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-', r == '@':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// directNotice prints a fallback reason once per session per process —
// enough to explain a suddenly-slow connection without repeating it on
// every retry.
func (d *Driver) directNotice(id, msg string) {
	d.dmu.Lock()
	seen := d.noticed[id]
	if !seen {
		if d.noticed == nil {
			d.noticed = map[string]bool{}
		}
		d.noticed[id] = true
	}
	d.dmu.Unlock()
	if !seen {
		fmt.Fprintln(os.Stderr, "pier: "+msg)
	}
}
