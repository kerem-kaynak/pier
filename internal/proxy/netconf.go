package proxy

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/kerem-kaynak/pier/internal/ui"
)

// ensureNet puts the two macOS prerequisites in place, with one sudo prompt
// and full disclosure of what runs:
//
//   - /etc/resolver/pier — tells macOS to send *.pier lookups to our
//     resolver (the same split-DNS mechanism VPNs and Tailscale use). The
//     port directive points it at our unprivileged high port. Written once,
//     survives everything; rewritten only if the content is stale.
//   - loopback aliases for 127.94.0.x — macOS only answers 127.0.0.1 out of
//     the box; each session's IP (and the resolver's) must be added to lo0.
//     Inert /32s, gone at reboot, re-added by the next run.
//
// Everything else the proxy does is unprivileged and dies with the process.
func ensureNet(out io.Writer) error {
	var steps []string
	content := "nameserver " + dnsAddr + "\nport " + dnsPort + "\ntimeout 1\n"
	if b, err := os.ReadFile("/etc/resolver/" + domain); err != nil || string(b) != content {
		esc := strings.ReplaceAll(content, "\n", `\n`)
		steps = append(steps, "mkdir -p /etc/resolver && printf '"+esc+"' > /etc/resolver/"+domain)
	}
	have := lo0Aliases()
	var missing []string
	for _, ip := range proxyIPs() {
		if !have[ip] {
			missing = append(missing, ip)
			steps = append(steps, "ifconfig lo0 alias "+ip+" 255.255.255.255")
		}
	}
	if len(steps) == 0 {
		return nil
	}
	fmt.Fprintln(out, ui.Bold.Render("network setup")+ui.Dim.Render(" — sudo runs:"))
	if len(steps) > len(missing) {
		fmt.Fprintln(out, ui.Dim.Render("  "+steps[0]))
	}
	if len(missing) > 0 {
		fmt.Fprintf(out, ui.Dim.Render("  ifconfig lo0 alias %s 255.255.255.255   (%d loopback IPs)\n"), missing[0], len(missing))
	}
	cmd := exec.Command("sudo", "/bin/sh", "-ec", strings.Join(steps, "\n"))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("network setup: %w", err)
	}
	return nil
}

// lo0Aliases reads which of our IPs are already on the loopback interface.
func lo0Aliases() map[string]bool {
	have := map[string]bool{}
	out, err := exec.Command("ifconfig", "lo0").Output()
	if err != nil {
		return have
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "inet" {
			have[f[1]] = true
		}
	}
	return have
}

// proxyIPs is every loopback IP the proxy may bind: the resolver's plus each
// session slot's public IP (pier's sniffing relay) and its shadow (where the
// ssh forwards bind).
func proxyIPs() []string {
	ips := []string{dnsAddr}
	for i := range slotCount {
		ips = append(ips, slotIP(i).String(), shadowOf(slotIP(i)).String())
	}
	return ips
}
