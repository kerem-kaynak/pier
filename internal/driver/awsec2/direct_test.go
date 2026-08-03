package awsec2

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The transport choice is the whole point of direct connect: with a live
// probe result the opts must dial the public IP (keyed by instance id in
// known_hosts) and drop both the ProxyCommand and the tunnel-only -C; with
// direct off — or the probe negative — they must ride SSM.
func TestSSHOptsTransports(t *testing.T) {
	d := &Driver{StateDir: t.TempDir()}
	opts := strings.Join(d.sshOpts(context.Background(), "i-0abc"), " ")
	if !strings.Contains(opts, "ProxyCommand=aws ssm start-session") || !strings.Contains(opts, "-C") {
		t.Errorf("direct off must use the SSM tunnel with compression, got %q", opts)
	}

	d = &Driver{
		StateDir: t.TempDir(),
		Direct:   true,
		dprobe: map[string]directProbe{
			"i-0abc": {ip: "203.0.113.7", until: time.Now().Add(time.Minute)},
		},
	}
	opts = strings.Join(d.sshOpts(context.Background(), "i-0abc"), " ")
	if !strings.Contains(opts, "HostName=203.0.113.7") || !strings.Contains(opts, "HostKeyAlias=i-0abc") {
		t.Errorf("direct on must dial the public IP keyed by instance id, got %q", opts)
	}
	if strings.Contains(opts, "ProxyCommand") || strings.Contains(opts, "-C") {
		t.Errorf("direct path must not carry SSM options, got %q", opts)
	}

	// A negative probe (cached "") means the tunnel, without re-probing.
	d.dprobe["i-0abc"] = directProbe{ip: "", until: time.Now().Add(time.Minute)}
	opts = strings.Join(d.sshOpts(context.Background(), "i-0abc"), " ")
	if !strings.Contains(opts, "ProxyCommand") {
		t.Errorf("negative probe must fall back to the SSM tunnel, got %q", opts)
	}
}

func TestSanitizeRuleName(t *testing.T) {
	for in, want := range map[string]string{
		"kerem":          "kerem",
		"dev role/x y":   "dev-role-x-y",
		"a.b_c-d@e":      "a.b_c-d@e",
		"":               "unknown",
		"héllo":          "h-llo",
		"user+tag=extra": "user-tag-extra",
	} {
		if got := sanitizeRuleName(in); got != want {
			t.Errorf("sanitizeRuleName(%q) = %q, want %q", in, got, want)
		}
	}
}
