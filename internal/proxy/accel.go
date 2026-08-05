package proxy

// The accelerator: dev servers ship apps as thousands of tiny module files,
// which is free on localhost and unusable over a WAN — each file costs a
// round trip and browsers only run 6 at a time on an http origin (36s for
// what loads in 2s locally). The relay therefore answers HTTP itself: it
// caches what the dev server marks cacheable (immutable dep chunks, ETagged
// source modules) and reads import statements in every JS/HTML response to
// fetch the rest of the graph over pooled backend connections before the
// browser asks. Source files are still revalidated against the VM — a
// reload turns into one burst of parallel conditional requests, not a
// serial crawl — so nothing is served stale. Everything protocol-shaped
// (websockets, POSTs, streaming) passes straight through untouched.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// A revalidated entry answers for its ETag this long without another
	// upstream trip, so one reload's burst of conditional requests shares a
	// single validation per file instead of racing 1500 of its own.
	revalWindow   = 3 * time.Second
	maxEntry      = 8 << 20   // bigger responses stream through uncached
	maxStore      = 256 << 20 // cache full → wipe; dev caches self-heal
	prefetchLanes = 64        // parallel backend fetches per port
)

// entry is one cached GET response. body, etag and header are immutable
// after creation; checked is only touched under the accel's lock.
type entry struct {
	body      []byte
	header    http.Header
	etag      string
	immutable bool
	checked   time.Time
}

// respond synthesizes the client-facing response: a 304 when the client
// already holds this exact ETag, the full body otherwise.
func (e *entry) respond(req *http.Request) *http.Response {
	h := e.header.Clone()
	status := http.StatusOK
	body := io.NopCloser(bytes.NewReader(e.body))
	length := int64(len(e.body))
	if e.etag != "" && strings.Contains(req.Header.Get("If-None-Match"), e.etag) {
		status, body, length = http.StatusNotModified, http.NoBody, 0
	} else {
		h.Set("Content-Length", strconv.FormatInt(length, 10))
	}
	return &http.Response{
		StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: h, Body: body, ContentLength: length, Request: req,
	}
}

// accel owns one mirrored port's HTTP path: an http.Server fed sniffed
// connections by the relays (.pier and localhost share one accel, so they
// share one cache), a reverse proxy whose RoundTripper is the cache, and
// the prefetcher. Non-HTTP connections never reach it.
type accel struct {
	tr  *http.Transport // pooled connections to the session port
	srv *http.Server
	ln  *chanListener
	sem chan struct{} // prefetch lanes

	mu       sync.Mutex
	store    map[string]*entry
	bytes    int
	inflight map[string]bool
}

func newAccel(backend string) *accel {
	a := &accel{
		tr: &http.Transport{
			// Every URL host dials the session port: the relay is the origin.
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp", backend)
			},
			MaxIdleConns:        prefetchLanes * 2,
			MaxIdleConnsPerHost: prefetchLanes,
			IdleConnTimeout:     90 * time.Second,
		},
		ln:       newChanListener(),
		sem:      make(chan struct{}, prefetchLanes),
		store:    map[string]*entry{},
		inflight: map[string]bool{},
	}
	silent := log.New(io.Discard, "", 0)
	a.srv = &http.Server{
		Handler: &httputil.ReverseProxy{
			Director: func(r *http.Request) {
				r.URL.Scheme = "http"
				if r.URL.Host = r.Host; r.URL.Host == "" {
					r.URL.Host = backend
				}
			},
			Transport:     a,
			FlushInterval: -1, // dev servers stream; never buffer
			ErrorLog:      silent,
		},
		ErrorLog: silent,
	}
	go func() { _ = a.srv.Serve(a.ln) }()
	return a
}

func (a *accel) push(c net.Conn) { a.ln.push(c) }

func (a *accel) Close() {
	a.ln.Close()
	_ = a.srv.Close()
	a.tr.CloseIdleConnections()
}

// RoundTrip is the cache. Only plain GETs participate — upgrades, ranges and
// authenticated requests ride the pooled transport untouched.
func (a *accel) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet || req.Header.Get("Upgrade") != "" ||
		req.Header.Get("Range") != "" || req.Header.Get("Authorization") != "" {
		return a.tr.RoundTrip(req)
	}
	key := req.URL.RequestURI()
	a.mu.Lock()
	e := a.store[key]
	fresh := e != nil && (e.immutable || time.Since(e.checked) < revalWindow)
	a.mu.Unlock()
	if fresh {
		return e.respond(req), nil
	}

	up := req.Clone(req.Context())
	up.Header.Del("If-Modified-Since")
	if e != nil && e.etag != "" {
		// Revalidate our copy; the client's view falls out of respond().
		up.Header.Set("If-None-Match", e.etag)
	}
	resp, err := a.tr.RoundTrip(up)
	if err != nil {
		return nil, err
	}
	if e != nil && resp.StatusCode == http.StatusNotModified {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		a.mu.Lock()
		e.checked = time.Now()
		a.mu.Unlock()
		go a.scan(*req.URL, req.Host, e) // a reload revalidates the whole graph in parallel
		return e.respond(req), nil
	}
	if resp.StatusCode != http.StatusOK || !cacheable(resp.Header) {
		return resp, nil
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxEntry+1))
	if err != nil {
		resp.Body.Close()
		return nil, err
	}
	if len(buf) > maxEntry {
		resp.Body = splice{io.MultiReader(bytes.NewReader(buf), resp.Body), resp.Body}
		return resp, nil
	}
	resp.Body.Close()
	ne := &entry{
		body: buf, header: keepHeaders(resp.Header),
		etag: resp.Header.Get("Etag"), immutable: strings.Contains(strings.ToLower(resp.Header.Get("Cache-Control")), "immutable"),
		checked: time.Now(),
	}
	a.put(key, ne)
	go a.scan(*req.URL, req.Host, ne)
	return ne.respond(req), nil
}

// cacheable: the dev server must opt the response in (immutable, or an ETag
// on a static type). Dynamic API responses — JSON, no ETag, cookie-varying —
// never qualify, so the cache can key on URL alone.
func cacheable(h http.Header) bool {
	cc := strings.ToLower(h.Get("Cache-Control"))
	if strings.Contains(cc, "no-store") || strings.Contains(cc, "private") {
		return false
	}
	// Vary on Origin or Accept-Encoding is fine: dev traffic is same-origin
	// (no Origin header, no CORS headers in play — and keepHeaders drops
	// them from entries anyway). Vite sends "Vary: Origin" on every module,
	// so rejecting it would disable the accelerator for the main case.
	for v := range strings.SplitSeq(h.Get("Vary"), ",") {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "", "origin", "accept-encoding":
		default:
			return false
		}
	}
	if strings.Contains(cc, "immutable") {
		return true
	}
	if h.Get("Etag") == "" {
		return false
	}
	ct := h.Get("Content-Type")
	for _, p := range []string{"text/javascript", "application/javascript", "text/css",
		"text/html", "application/wasm", "image/", "font/", "video/", "audio/"} {
		if strings.HasPrefix(ct, p) {
			return true
		}
	}
	return false
}

func keepHeaders(h http.Header) http.Header {
	kept := http.Header{}
	for _, k := range []string{"Content-Type", "Cache-Control", "Etag", "Content-Encoding", "Last-Modified"} {
		if v := h.Get(k); v != "" {
			kept.Set(k, v)
		}
	}
	return kept
}

func (a *accel) put(key string, e *entry) {
	a.mu.Lock()
	if old := a.store[key]; old != nil {
		a.bytes -= len(old.body)
	}
	a.bytes += len(e.body)
	if a.bytes > maxStore {
		a.store = map[string]*entry{}
		a.bytes = len(e.body)
	}
	a.store[key] = e
	a.mu.Unlock()
}

// --- prefetch ----------------------------------------------------------------

var (
	jsFrom  = regexp.MustCompile(`\bfrom\s*["']([^"'\n]+)["']`)
	jsImp   = regexp.MustCompile(`\bimport\s*\(?\s*["']([^"'\n]+)["']`)
	htmlRef = regexp.MustCompile(`(?:src|href)=["']([^"'\s]+)["']`)
)

// refs pulls same-origin references out of a JS or HTML body: import/export
// specifiers, script src, stylesheet href. Dev servers rewrite everything to
// rooted paths, so bare package names and absolute URLs are skipped. A false
// positive costs one 404 in the background; a miss just isn't prefetched.
func refs(contentType string, body []byte) []string {
	var raw []string
	switch {
	case strings.Contains(contentType, "javascript"):
		for _, re := range []*regexp.Regexp{jsFrom, jsImp} {
			for _, m := range re.FindAllSubmatch(body, -1) {
				raw = append(raw, string(m[1]))
			}
		}
	case strings.Contains(contentType, "html"):
		for _, m := range htmlRef.FindAllSubmatch(body, -1) {
			raw = append(raw, string(m[1]))
		}
	}
	var out []string
	for _, r := range raw {
		if strings.HasPrefix(r, "/") || strings.HasPrefix(r, "./") || strings.HasPrefix(r, "../") {
			out = append(out, r)
		}
	}
	return out
}

// scan walks an entry's references and warms each one. Prefetches run back
// through RoundTrip, so they store, revalidate and scan recursively — the
// whole import graph resolves at prefetchLanes wide while the browser is
// still parsing the first file.
func (a *accel) scan(base url.URL, host string, e *entry) {
	seen := map[string]bool{}
	for _, ref := range refs(e.header.Get("Content-Type"), e.body) {
		u, err := url.Parse(ref)
		if err != nil || u.Host != "" || u.Scheme != "" {
			continue
		}
		t := base.ResolveReference(u)
		if key := t.RequestURI(); !seen[key] {
			seen[key] = true
			a.prefetch(t, host)
		}
	}
}

func (a *accel) prefetch(t *url.URL, host string) {
	key := t.RequestURI()
	a.mu.Lock()
	e := a.store[key]
	if (e != nil && (e.immutable || time.Since(e.checked) < revalWindow)) || a.inflight[key] {
		a.mu.Unlock()
		return
	}
	a.inflight[key] = true
	a.mu.Unlock()
	go func() {
		a.sem <- struct{}{}
		defer func() {
			<-a.sem
			a.mu.Lock()
			delete(a.inflight, key)
			a.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		u := *t
		u.Scheme, u.Host = "http", host
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return
		}
		req.Host = host
		resp, err := a.RoundTrip(req)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
}

// --- plumbing ----------------------------------------------------------------

// splice hands back the buffered prefix of an oversized body plus the
// unread rest of the upstream stream.
type splice struct {
	io.Reader
	io.Closer
}

// chanListener feeds relay-sniffed connections to the accel's http.Server.
type chanListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func newChanListener() *chanListener {
	return &chanListener{ch: make(chan net.Conn), done: make(chan struct{})}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func (l *chanListener) push(c net.Conn) {
	select {
	case l.ch <- c:
	case <-l.done:
		c.Close()
	}
}

// --- http sniffing -----------------------------------------------------------

var httpVerbs = []string{"GET ", "HEAD ", "POST ", "PUT ", "DELETE ", "OPTIONS ", "PATCH ", "TRACE "}

// sniffHTTP peeks just enough bytes to recognize an HTTP request line.
// Almost every connection decides on its first byte; peeking more only
// happens while the bytes are still an ambiguous verb prefix (max 8).
func sniffHTTP(br *bufio.Reader) bool {
	n := 1
	for {
		b, err := br.Peek(n)
		if err != nil {
			return false
		}
		yes, more := looksHTTP(b)
		if yes || !more {
			return yes
		}
		n = len(b) + 1
	}
}

func looksHTTP(b []byte) (yes, needMore bool) {
	for _, v := range httpVerbs {
		if len(b) >= len(v) {
			if string(b[:len(v)]) == v {
				return true, false
			}
		} else if strings.HasPrefix(v, string(b)) {
			needMore = true
		}
	}
	return false, needMore
}
