package proxy

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// https support: browsers only grant a secure context (crypto.subtle, and
// with it auth0-spa-js and friends) to localhost or to https origins, so
// http://<session>.pier white-screens any app that needs one. The proxy
// therefore owns each mirrored port itself: it sniffs the first byte of
// every connection, terminates TLS when it sees a handshake, and pipes
// plain connections through untouched — http, https and raw TCP (psql)
// all keep working on the same port.
//
// Certificates come from a local CA created once under the config dir
// (the mkcert model). Leaf certs are minted in memory per hostname; the
// user trusts the CA once with a printed one-liner. pier never modifies
// the keychain itself.

const (
	caCertFile = "proxy-ca.pem"
	caKeyFile  = "proxy-ca.key"
)

// minter signs per-hostname leaf certs from the local CA on first use.
type minter struct {
	ca  *x509.Certificate
	key *ecdsa.PrivateKey

	mu    sync.Mutex
	cache map[string]*tls.Certificate
}

// loadOrCreateCA returns the CA from dir, creating it on first run.
func loadOrCreateCA(dir string) (*minter, error) {
	certPath := filepath.Join(dir, caCertFile)
	keyPath := filepath.Join(dir, caKeyFile)
	if m, err := loadCA(certPath, keyPath); err == nil {
		return m, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "pier proxy CA", Organization: []string{"pier"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, err
	}
	return loadCA(certPath, keyPath)
}

func loadCA(certPath, keyPath string) (*minter, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, errors.New("bad PEM in proxy CA files")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	return &minter{ca: cert, key: key, cache: map[string]*tls.Certificate{}}, nil
}

// getCertificate is the tls.Config callback: one leaf per hostname, minted
// on first use and re-minted near expiry (Apple rejects long-lived leafs,
// so they get 90 days — plenty for certs that live in one proxy run).
func (m *minter) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name := hello.ServerName
	if name == "" {
		return nil, errors.New("no server name — use the <session>." + domain + " hostname")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.cache[name]; ok && time.Now().Before(c.Leaf.NotAfter.Add(-24*time.Hour)) {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(0, 0, 90),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{name},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, m.ca, &key.PublicKey, m.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	m.cache[name] = c
	return c, nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		panic(err) // crypto/rand failing is not a recoverable state
	}
	return n
}

// caTrusted reports whether the keychain trusts the CA (best-effort hint).
func caTrusted(certPath string) bool {
	return exec.Command("security", "verify-cert", "-c", certPath).Run() == nil
}

// trustHint is the one-time command that makes browsers accept the CA.
func trustHint(certPath string) string {
	return "security add-trusted-cert -r trustRoot -k ~/Library/Keychains/login.keychain-db " + certPath
}

// --- the sniffing relay ------------------------------------------------------

// serveRelay accepts on ln until it closes, piping each connection to dial
// (the shadow IP where ssh's forward listens) and TLS-terminating the ones
// that arrive as handshakes.
func serveRelay(ln net.Listener, dial string, tcfg *tls.Config) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go pipe(c, dial, tcfg)
	}
}

func pipe(c net.Conn, dial string, tcfg *tls.Config) {
	defer c.Close()
	br := bufio.NewReader(c)
	first, err := br.Peek(1)
	if err != nil {
		return
	}
	var client net.Conn = bufConn{c, br}
	if first[0] == 0x16 { // a TLS handshake record; anything else pipes raw
		client = tls.Server(client, tcfg)
	}
	backend, err := net.DialTimeout("tcp", dial, 10*time.Second)
	if err != nil {
		return
	}
	go func() {
		defer c.Close()
		defer backend.Close()
		_, _ = io.Copy(backend, client)
	}()
	defer backend.Close()
	_, _ = io.Copy(client, backend)
}

// bufConn re-attaches the sniffed byte to the connection.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// shadowOf maps a session's public loopback IP to its shadow, where the ssh
// forwards bind so the relay can own the public ip:port.
func shadowOf(ip net.IP) net.IP {
	v4 := ip.To4()
	return net.IPv4(v4[0], v4[1], 1, v4[3])
}

func caPath(stateDir string) string { return filepath.Join(stateDir, caCertFile) }
