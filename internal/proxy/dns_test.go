package proxy

import (
	"encoding/binary"
	"net"
	"testing"
)

func query(name string, qtype uint16) []byte {
	q := []byte{0xAB, 0xCD, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0} // RD set
	for _, label := range splitLabels(name) {
		q = append(q, byte(len(label)))
		q = append(q, label...)
	}
	q = append(q, 0)
	q = binary.BigEndian.AppendUint16(q, qtype)
	q = binary.BigEndian.AppendUint16(q, 1) // IN
	return q
}

func splitLabels(name string) []string {
	var out []string
	for len(name) > 0 {
		i := 0
		for i < len(name) && name[i] != '.' {
			i++
		}
		out = append(out, name[:i])
		if i < len(name) {
			i++
		}
		name = name[i:]
	}
	return out
}

func TestDNSAnswer(t *testing.T) {
	ip := net.IPv4(127, 94, 0, 101)
	lookup := func(name string) (net.IP, bool) {
		if name == "payments-retry" {
			return ip, true
		}
		return nil, false
	}

	// Known name, A query → one A record with the slot IP.
	resp := dnsAnswer(query("Payments-Retry.pier", 1), lookup) // mixed case on purpose
	if resp == nil {
		t.Fatal("no response for known A query")
	}
	if resp[0] != 0xAB || resp[1] != 0xCD {
		t.Error("id not echoed")
	}
	if resp[2]&0x80 == 0 {
		t.Error("QR not set")
	}
	if rcode := resp[3] & 0x0F; rcode != 0 {
		t.Errorf("rcode = %d, want NOERROR", rcode)
	}
	if an := binary.BigEndian.Uint16(resp[6:8]); an != 1 {
		t.Fatalf("answer count = %d, want 1", an)
	}
	if got := net.IP(resp[len(resp)-4:]); !got.Equal(ip) {
		t.Errorf("answer ip = %v, want %v", got, ip)
	}

	// Known name, AAAA query → NOERROR with zero answers (never NXDOMAIN:
	// that would negative-cache the name and kill the A lookup too).
	resp = dnsAnswer(query("payments-retry.pier", 28), lookup)
	if resp == nil {
		t.Fatal("no response for AAAA query")
	}
	if rcode := resp[3] & 0x0F; rcode != 0 {
		t.Errorf("AAAA rcode = %d, want NOERROR", rcode)
	}
	if an := binary.BigEndian.Uint16(resp[6:8]); an != 0 {
		t.Errorf("AAAA answer count = %d, want 0", an)
	}

	// Unknown session → NXDOMAIN.
	resp = dnsAnswer(query("nope.pier", 1), lookup)
	if rcode := resp[3] & 0x0F; rcode != 3 {
		t.Errorf("unknown rcode = %d, want NXDOMAIN", rcode)
	}

	// Bare "pier" and truncated garbage don't crash or answer.
	if resp = dnsAnswer(query("pier", 1), lookup); resp[3]&0x0F != 3 {
		t.Error("bare domain should be NXDOMAIN")
	}
	if dnsAnswer([]byte{1, 2, 3}, lookup) != nil {
		t.Error("garbage should be dropped")
	}
}

func TestHostname(t *testing.T) {
	cases := map[string]string{
		"payments-retry": "payments-retry",
		"feat/API_v2":    "feat-api-v2",
		"--x--":          "x",
		"":               "session",
	}
	for in, want := range cases {
		if got := hostname(in); got != want {
			t.Errorf("hostname(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSlots(t *testing.T) {
	s := &slots{byID: map[string]int{}}
	a, _ := s.acquire("i-a")
	b, _ := s.acquire("i-b")
	if a.Equal(b) {
		t.Fatal("two sessions share an IP")
	}
	if again, _ := s.acquire("i-a"); !again.Equal(a) {
		t.Error("re-acquire should be stable")
	}
	s.release("i-a")
	c, _ := s.acquire("i-c")
	if !c.Equal(a) {
		t.Error("released slot should be reused (lowest free)")
	}
}
