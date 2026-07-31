package proxy

import (
	"encoding/binary"
	"io"
	"net"
	"strings"
)

// serveDNS answers A queries for *.pier on a UDP socket. macOS routes only
// .pier queries here (via /etc/resolver/pier), so this needs exactly one
// trick: name → the session's loopback IP. Hand-rolled on purpose — a full
// DNS library for one A record is the kind of dependency pier doesn't take.
func serveDNS(addr string, lookup func(name string) (net.IP, bool)) (io.Closer, error) {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, err
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, raddr, err := pc.ReadFrom(buf)
			if err != nil {
				return // closed
			}
			if resp := dnsAnswer(buf[:n], lookup); resp != nil {
				_, _ = pc.WriteTo(resp, raddr)
			}
		}
	}()
	return pc, nil
}

// dnsAnswer builds the reply for one query packet (pure; tested).
//
// Behavior matters more than completeness here:
//   - known name + A query  → one A record, TTL 10s (sessions come and go)
//   - known name + AAAA/HTTPS/anything else → NOERROR with zero answers, so
//     clients fall back to A. Never NXDOMAIN: that's per-name, and a cached
//     NXDOMAIN from an AAAA lookup would kill the A lookup too.
//   - unknown name → NXDOMAIN
func dnsAnswer(q []byte, lookup func(string) (net.IP, bool)) []byte {
	if len(q) < 12 || q[2]&0x80 != 0 { // too short, or already a response
		return nil
	}
	if binary.BigEndian.Uint16(q[4:6]) == 0 { // no question
		return nil
	}
	i := 12
	var labels []string
	for {
		if i >= len(q) {
			return nil
		}
		l := int(q[i])
		if l == 0 {
			i++
			break
		}
		if l >= 0xC0 || i+1+l > len(q) { // compression can't appear in our questions
			return nil
		}
		labels = append(labels, strings.ToLower(string(q[i+1:i+1+l])))
		i += 1 + l
	}
	if i+4 > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[i : i+2])
	qclass := binary.BigEndian.Uint16(q[i+2 : i+4])
	qEnd := i + 4

	name := strings.Join(labels, ".")
	var ip net.IP
	known := false
	if sess, ok := strings.CutSuffix(name, "."+domain); ok {
		ip, known = lookup(sess)
	}
	answer := known && qtype == 1 && qclass == 1 && ip.To4() != nil

	rcode := byte(0)
	if !known {
		rcode = 3 // NXDOMAIN
	}
	an := uint16(0)
	if answer {
		an = 1
	}
	resp := make([]byte, 0, qEnd+16)
	resp = append(resp, q[0], q[1])            // id
	resp = append(resp, 0x84|q[2]&0x79, rcode) // QR+AA, opcode+RD copied
	resp = binary.BigEndian.AppendUint16(resp, 1)
	resp = binary.BigEndian.AppendUint16(resp, an)
	resp = binary.BigEndian.AppendUint16(resp, 0)
	resp = binary.BigEndian.AppendUint16(resp, 0)
	resp = append(resp, q[12:qEnd]...) // question echoed
	if answer {
		resp = append(resp, 0xC0, 0x0C)                // name = pointer to the question
		resp = binary.BigEndian.AppendUint16(resp, 1)  // A
		resp = binary.BigEndian.AppendUint16(resp, 1)  // IN
		resp = binary.BigEndian.AppendUint32(resp, 10) // TTL
		resp = binary.BigEndian.AppendUint16(resp, 4)
		resp = append(resp, ip.To4()...)
	}
	return resp
}
