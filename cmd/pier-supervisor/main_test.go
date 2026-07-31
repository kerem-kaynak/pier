package main

import (
	"reflect"
	"testing"
)

func TestParsePSIAvg60(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want float64
	}{
		{"typical", "some avg10=0.00 avg60=12.34 avg300=5.00 total=123456\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n", 12.34},
		{"quiet", "some avg10=0.00 avg60=0.00 avg300=0.00 total=0\n", 0},
		{"empty", "", 0},
		{"garbage", "not a psi file\n", 0},
	}
	for _, c := range cases {
		if got := parsePSIAvg60(c.in); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseSS(t *testing.T) {
	// Captured shape of `ss -Htnap` on Ubuntu 24.04: the resolver stub and
	// sshd's own listener are plumbing; node/docker-proxy are the session's.
	// The two ESTAB sshd rows are one :22 transport (not a tunnel) and one
	// loopback dial serving a forwarded port (a tunnel); the node row is the
	// app's side of that same dial (not sshd's, so not counted twice).
	in := `LISTEN 0      4096   127.0.0.53%lo:53        0.0.0.0:*     users:(("systemd-resolve",pid=339,fd=14))
LISTEN 0      128          0.0.0.0:22             0.0.0.0:*     users:(("sshd",pid=708,fd=3))
LISTEN 0      511                *:3000                 *:*     users:(("node",pid=1234,fd=18))
LISTEN 0      511             [::]:3000              [::]:*     users:(("node",pid=1234,fd=19))
LISTEN 0      4096         0.0.0.0:5432           0.0.0.0:*     users:(("docker-proxy",pid=990,fd=4))
ESTAB  0      0          127.0.0.1:22         127.0.0.1:54321   users:(("sshd",pid=740,fd=4))
ESTAB  0      0          127.0.0.1:47110      127.0.0.1:3000    users:(("sshd",pid=812,fd=11))
ESTAB  0      0          127.0.0.1:3000       127.0.0.1:47110   users:(("node",pid=1234,fd=21))
`
	listening, tunnels := parseSS(in)
	if want := []int{3000, 5432}; !reflect.DeepEqual(listening, want) {
		t.Errorf("listening: got %v, want %v", listening, want)
	}
	if tunnels != 1 {
		t.Errorf("tunnels: got %d, want 1", tunnels)
	}

	listening, tunnels = parseSS("")
	if listening == nil || len(listening) != 0 || tunnels != 0 {
		t.Errorf("empty: got %v/%d, want []/0 (non-nil — the beacon field must serialize as [])", listening, tunnels)
	}
}
