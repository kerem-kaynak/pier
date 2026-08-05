package proxy

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLooksHTTP(t *testing.T) {
	for in, want := range map[string]bool{
		"GET / HTTP/1.1": true,
		"POST /api":      true,
		"OPTIONS ":       true,
		"ping":           false, // no verb prefixes 'p': decided on byte one
		"\x00\x00\x00":   false, // postgres SSLRequest
		"SSH-2.0-":       false,
	} {
		yes, _ := looksHTTP([]byte(in))
		if yes != want {
			t.Errorf("looksHTTP(%q) = %v, want %v", in, yes, want)
		}
	}
	// "GE" could still become "GET " — the sniffer must keep reading, not decide.
	if yes, more := looksHTTP([]byte("GE")); yes || !more {
		t.Errorf("looksHTTP(GE) = %v,%v, want false,true", yes, more)
	}
}

func TestRefs(t *testing.T) {
	js := []byte(`import React from "/node_modules/.vite/deps/react.js?v=abc123";
import "/src/index.css";
import { x } from "./util.ts";
export { y } from "../lib/y.ts";
const lazy = import("/src/lazy.tsx");
import bare from "react";
import remote from "https://cdn.example.com/x.js";`)
	got := refs("text/javascript", js)
	want := []string{"/node_modules/.vite/deps/react.js?v=abc123", "./util.ts",
		"../lib/y.ts", "/src/index.css", "/src/lazy.tsx"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("js refs = %v, want %v", got, want)
	}

	html := []byte(`<script type="module" src="/src/main.tsx"></script>
<link rel="stylesheet" href="/style.css"><a href="https://example.com">x</a>`)
	got = refs("text/html", html)
	want = []string{"/src/main.tsx", "/style.css"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("html refs = %v, want %v", got, want)
	}

	if r := refs("application/json", []byte(`{"from": "/x"}`)); r != nil {
		t.Errorf("json must not be scanned, got %v", r)
	}
}

func TestLhostTable(t *testing.T) {
	lh := &lhostTable{owners: map[int]lhostOwner{}}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	ln, msg := lh.claim(port, "i-a", "alpha")
	if ln == nil {
		t.Fatalf("first claim failed: %s", msg)
	}
	defer ln.Close()

	if ln2, msg := lh.claim(port, "i-b", "beta"); ln2 != nil || !strings.Contains(msg, "alpha") {
		t.Errorf("second claim must lose to alpha, got ln=%v msg=%q", ln2, msg)
	}
	lh.release(port, "i-b") // loser's release must not evict the owner
	if _, ok := lh.owners[port]; !ok {
		t.Error("release by non-owner evicted the owner")
	}
	lh.release(port, "i-a")
	ln.Close()
	if ln3, msg := lh.claim(port, "i-b", "beta"); ln3 == nil {
		t.Errorf("claim after release failed: %s", msg)
	} else {
		ln3.Close()
		lh.release(port, "i-b")
	}
}

// devServer is a counting stand-in for a vite-style backend: an ETagged
// module that imports an immutable dep chunk.
type devServer struct {
	mu   sync.Mutex
	hits map[string]int
	inm  map[string]string // last If-None-Match seen per path
}

func (d *devServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.hits[r.URL.Path]++
		d.inm[r.URL.Path] = r.Header.Get("If-None-Match")
		d.mu.Unlock()
		switch r.URL.Path {
		case "/app.js":
			w.Header().Set("Content-Type", "text/javascript")
			w.Header().Set("Etag", `W/"v1"`)
			w.Header().Set("Cache-Control", "no-cache")
			if strings.Contains(r.Header.Get("If-None-Match"), `W/"v1"`) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			io.WriteString(w, `import "/dep.js?v=abc";`)
		case "/dep.js":
			w.Header().Set("Content-Type", "text/javascript")
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			io.WriteString(w, "export {}")
		default:
			http.NotFound(w, r)
		}
	})
}

func (d *devServer) count(path string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.hits[path]
}

// The accelerator contract end to end through a real relay listener: cache
// immutable deps forever, prefetch imports before the browser asks, share
// one upstream validation per burst, and revalidate with the dev server's
// own ETags once the burst window passes.
func TestAccelCachesPrefetchesRevalidates(t *testing.T) {
	dev := &devServer{hits: map[string]int{}, inm: map[string]string{}}
	backend := httptest.NewServer(dev.handler())
	defer backend.Close()
	backendAddr := backend.Listener.Addr().String()

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
	ac := newAccel(backendAddr)
	defer ac.Close()
	go serveRelay(front, backendAddr, tcfg, ac)
	base := "http://" + front.Addr().String()

	get := func(path, inm string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		if inm != "" {
			req.Header.Set("If-None-Match", inm)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp
	}

	if resp := get("/app.js", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("app.js: %d", resp.StatusCode)
	}
	// The import scanner must fetch the dep before any client asks for it.
	deadline := time.Now().Add(5 * time.Second)
	for dev.count("/dep.js") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("dep.js was never prefetched")
		}
		time.Sleep(10 * time.Millisecond)
	}

	get("/dep.js?v=abc", "")
	get("/dep.js?v=abc", "")
	if n := dev.count("/dep.js"); n != 1 {
		t.Errorf("immutable dep fetched %d times upstream, want exactly the prefetch", n)
	}
	appHits := dev.count("/app.js")
	get("/app.js", "") // within the burst window: answered locally
	if n := dev.count("/app.js"); n != appHits {
		t.Errorf("fresh entry still went upstream (%d -> %d hits)", appHits, n)
	}

	// Age the entry past the burst window: the accel must revalidate with
	// its stored ETag and serve the cached body on the 304.
	ac.mu.Lock()
	ac.store["/app.js"].checked = time.Now().Add(-time.Minute)
	ac.mu.Unlock()
	if resp := get("/app.js", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("revalidated fetch: %d, want 200 from cache", resp.StatusCode)
	}
	dev.mu.Lock()
	inm := dev.inm["/app.js"]
	dev.mu.Unlock()
	if !strings.Contains(inm, `W/"v1"`) {
		t.Errorf("revalidation sent If-None-Match %q, want the stored ETag", inm)
	}

	// A client holding the same ETag gets a synthesized 304, no upstream trip.
	appHits = dev.count("/app.js")
	if resp := get("/app.js", `W/"v1"`); resp.StatusCode != http.StatusNotModified {
		t.Errorf("client with matching ETag got %d, want 304", resp.StatusCode)
	}
	if n := dev.count("/app.js"); n != appHits {
		t.Errorf("client 304 went upstream (%d -> %d hits)", appHits, n)
	}

	// Raw TCP through the same relay must still pipe (not parse as HTTP).
	raw, err := net.Dial("tcp", front.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Write([]byte("nonsense\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 9)
	raw.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(raw, buf); err != nil || !strings.HasPrefix(string(buf), "HTTP/1.1 ") {
		// The echo here is the httptest server rejecting garbage — reaching
		// it at all proves the relay piped raw bytes instead of eating them.
		t.Errorf("raw pipe: read %q err %v", buf, err)
	}
}
