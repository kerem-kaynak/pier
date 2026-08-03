package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
)

func TestCAAndLeafMinting(t *testing.T) {
	dir := t.TempDir()
	m, err := loadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := loadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.ca.SerialNumber.Cmp(m2.ca.SerialNumber) != 0 {
		t.Error("second load must reuse the CA, not mint a new one")
	}

	c, err := m.getCertificate(&tls.ClientHelloInfo{ServerName: "test.pier"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Leaf.CheckSignatureFrom(m.ca); err != nil {
		t.Errorf("leaf not signed by the CA: %v", err)
	}
	if len(c.Leaf.DNSNames) != 1 || c.Leaf.DNSNames[0] != "test.pier" {
		t.Errorf("leaf SAN = %v, want [test.pier]", c.Leaf.DNSNames)
	}
	if len(c.Leaf.ExtKeyUsage) != 1 || c.Leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("leaf must be a server cert, got EKU %v", c.Leaf.ExtKeyUsage)
	}
	if c2, _ := m.getCertificate(&tls.ClientHelloInfo{ServerName: "test.pier"}); c2 != c {
		t.Error("same hostname must hit the cert cache")
	}
	if _, err := m.getCertificate(&tls.ClientHelloInfo{}); err == nil {
		t.Error("no SNI must fail, not serve a bogus cert")
	}
}

// The relay must pass plain TCP through untouched and TLS-terminate
// handshakes on the same port — that's the whole sniffing contract.
func TestRelaySniffsTLSAndPlain(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() { // echo server standing in for the ssh forward
		for {
			c, err := backend.Accept()
			if err != nil {
				return
			}
			go func() {
				buf := make([]byte, 64)
				for {
					n, err := c.Read(buf)
					if err != nil {
						c.Close()
						return
					}
					c.Write(buf[:n])
				}
			}()
		}
	}()

	m, err := loadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tcfg := &tls.Config{GetCertificate: m.getCertificate, NextProtos: []string{"http/1.1"}}
	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer front.Close()
	go serveRelay(front, backend.Addr().String(), tcfg)

	echo := func(c net.Conn) {
		t.Helper()
		defer c.Close()
		if _, err := c.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 4)
		if _, err := c.Read(buf); err != nil || string(buf) != "ping" {
			t.Fatalf("echo through relay: got %q err %v", buf, err)
		}
	}

	plain, err := net.Dial("tcp", front.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	echo(plain)

	pool := x509.NewCertPool()
	pool.AddCert(m.ca)
	secure, err := tls.Dial("tcp", front.Addr().String(), &tls.Config{ServerName: "test.pier", RootCAs: pool})
	if err != nil {
		t.Fatalf("tls through relay: %v", err)
	}
	echo(secure)
}
