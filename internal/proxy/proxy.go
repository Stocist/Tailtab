// Package proxy serves a combined SOCKS5 and HTTP proxy on loopback whose
// connections are dialed through the tailnet.
//
// The CONNECT handler below is adapted from github.com/tailscale/ts-browser-ext,
// which is:
//
//	Copyright (c) Tailscale Inc & AUTHORS
//	SPDX-License-Identifier: BSD-3-Clause
package proxy

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"time"

	"tailscale.com/net/proxymux"
	"tailscale.com/net/socks5"
	"tailscale.com/tsnet"
	"tailscale.com/types/logger"
)

// DialFunc dials one connection out through the tailnet.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// ErrNotTailnet is returned for a destination the tailnet does not serve.
var ErrNotTailnet = errors.New("not a tailnet destination")

// User is the username half of the proxy credential. It is fixed; the password
// is the per-process token below.
const User = "tailtab"

// NewToken returns a fresh proxy password: 32 bytes from crypto/rand, in
// base64url. One is generated per host process and given to the extension in
// every status event, so only the browser profile this host serves can use the
// listener. Without it any local process could borrow the profile's tailnet
// identity simply by connecting to the port.
//
// It is a secret: it must never be logged, and it must never be shown in the
// popup.
func NewToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating the proxy token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// Tailscale's address ranges: CGNAT for IPv4, and its ULA prefix for IPv6.
var (
	tailscaleV4 = netip.MustParsePrefix("100.64.0.0/10")
	tailscaleV6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// privateRanges are the addresses that must never be dialled through an exit
// node: they would resolve on the exit node's own LAN rather than the user's,
// which is both a surprise and a way to reach a stranger's network. Loopback
// and link-local are refused for the same reason a proxy never dials them.
//
// Tailscale's own ranges are not here: they are allowed in both modes, and are
// checked before this list.
var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("255.255.255.255/32"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

// isPrivate reports whether addr is in a range that must stay off the exit
// node.
func isPrivate(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, p := range privateRanges {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// allowExitHost is the destination rule while an exit node is carrying this
// profile's traffic. The whole point of an exit node is that everything goes
// through it, so the rule is the inverse of the tailnet one: anything public is
// allowed, and only what would be dialled on somebody's LAN is refused.
//
// extension/rules.js has the same rule for the browser side, and both flip on
// the same status field, or one side sends traffic the other refuses (G14).
func allowExitHost(host string) error {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if h == "" {
		return fmt.Errorf("%w: no host", ErrNotTailnet)
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		addr = addr.Unmap()
		if tailscaleV4.Contains(addr) || tailscaleV6.Contains(addr) {
			return nil
		}
		if isPrivate(addr) {
			return fmt.Errorf("%w: %s is a private address, which an exit node must not dial", ErrNotTailnet, h)
		}
		return nil
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return fmt.Errorf("%w: %s is loopback", ErrNotTailnet, h)
	}
	// A single label is still a MagicDNS short name; everything else is a
	// public name, which is exactly what the exit node is for.
	if numericHost.MatchString(h) {
		return fmt.Errorf("%w: %s is a numeric address", ErrNotTailnet, h)
	}
	return nil
}

// allowTailnetHost reports whether host is something the tailnet can serve.
//
// suffix is this node's own MagicDNS suffix, which is not under .ts.net when the
// tailnet uses a custom domain. It is empty until the node reports one, and
// until then only the .ts.net and single-label rules apply.
//
// Without this check the listener is a general-purpose open forward proxy, not
// a tailnet proxy: UserDial falls back to the system resolver and then to a
// plain dial for anything MagicDNS does not know, so any local process could
// use it to reach the whole internet. The extension only ever sends tailnet
// traffic here, so refusing the rest costs nothing.
//
// These are the rules in extension/rules.js, in Go. Nothing keeps the two in
// step automatically: they are held together by testdata/tailnet-hosts.json,
// a shared table of decisions that both this function and rules.js are tested
// against, so a change to one without the other fails a test.
func allowTailnetHost(host, suffix string) error {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if h == "" {
		return fmt.Errorf("%w: no host", ErrNotTailnet)
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		addr = addr.Unmap()
		if tailscaleV4.Contains(addr) || tailscaleV6.Contains(addr) {
			return nil
		}
		return fmt.Errorf("%w: %s is outside %s and %s", ErrNotTailnet, h, tailscaleV4, tailscaleV6)
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return fmt.Errorf("%w: %s is loopback", ErrNotTailnet, h)
	}
	if strings.HasSuffix(h, ".ts.net") {
		return nil
	}
	if d := strings.Trim(suffix, "."); validMagicDNSSuffix(d) {
		// On a label boundary only: with a suffix of "tail4d5e6f.example",
		// "evil-tail4d5e6f.example" is somebody else's name.
		if h == d || strings.HasSuffix(h, "."+d) {
			return nil
		}
	}
	if strings.Contains(h, ".") {
		return fmt.Errorf("%w: %s is not a MagicDNS name", ErrNotTailnet, h)
	}
	// A single label is a MagicDNS short name, e.g. "wiki" — unless it is a
	// bare number, which is only ever an obfuscated IPv4 address such as
	// 2130706433 or 0x7f000001 for 127.0.0.1.
	if numericHost.MatchString(h) {
		return fmt.Errorf("%w: %s is a numeric address", ErrNotTailnet, h)
	}
	return nil
}

// numericHost matches the decimal and hexadecimal forms of a bare IPv4 address.
var numericHost = regexp.MustCompile(`^([0-9]+|0x[0-9a-f]+)$`)

// magicDNSSuffixRE matches a lowercase DNS name of at least two labels, each
// one to 63 characters of a-z, 0-9 and "-", not starting or ending with "-".
var magicDNSSuffixRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// validMagicDNSSuffix reports whether s is worth trusting as this tailnet's
// MagicDNS suffix.
//
// The suffix arrives from the coordination server, and it widens the split
// tunnel wherever it is used. An unchecked one turns the tunnel inside out: a
// suffix of "com" would send every .com host through the tailnet, in the guard
// here and in the browser's PAC alike. A control server willing to say that is
// the precondition — a self-hosted one, or a tailnet a user was talked into
// joining — which makes this cheap to refuse and expensive to allow.
//
// So: lowercase, at least two labels, nothing longer than a DNS name can be,
// and not "ts.net" itself, which is the public parent of every tailnet rather
// than any one tailnet's own domain (and is already covered by the .ts.net
// rule).
func validMagicDNSSuffix(s string) bool {
	if s == "" || len(s) > 253 || s == "ts.net" {
		return false
	}
	return magicDNSSuffixRE.MatchString(s)
}

// hostOnly strips a port from a host:port, tolerating a bare host.
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// guard refuses non-tailnet destinations before they reach dial. It wraps every
// dialer the proxy uses, so the SOCKS5 path is covered too — SOCKS has no
// handler in front of it to check first.
func (s *Server) guard(dial DialFunc) DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if err := s.allow(hostOnly(addr)); err != nil {
			return nil, err
		}
		return dial(ctx, network, addr)
	}
}

// dialTailnet is the single place this program dials the tailnet.
//
// It goes through the system dialer's UserDial rather than tsnet.Server.Dial:
// Server.Dial calls Start() and awaitRunning() first, so a request made before
// login would block instead of failing, and the two are otherwise the same code
// path (research/tsnet.md §5). Swap this one function to ts.Dial if that turns
// out to be wrong.
//
// The address is passed through as a hostname. MagicDNS resolution happens
// inside UserDial, for both short names ("wiki") and FQDNs; resolving here
// would bypass the tailnet resolver entirely.
func dialTailnet(ts *tsnet.Server, allow func(string) error) DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		// Also checked in guard, which wraps this. Repeated here because this
		// is the function that reaches the network: it has to be safe alone.
		// It is the same rule either way, exit mode included.
		if err := allow(hostOnly(addr)); err != nil {
			return nil, err
		}
		sys := ts.Sys()
		if sys == nil {
			return nil, errors.New("tailtab: the node is not running")
		}
		d, ok := sys.Dialer.GetOK()
		if !ok {
			return nil, errors.New("tailtab: the node has no dialer yet")
		}
		return d.UserDial(ctx, network, addr)
	}
}

// Server is the loopback proxy. Both protocols share one port: SOCKS5 and HTTP
// are told apart from the first byte of each connection.
type Server struct {
	dial  DialFunc
	ln    net.Listener
	http  *http.Server
	token string

	mu sync.Mutex
	// suffix is the node's MagicDNS suffix, pushed in from the status stream.
	// It is read on every dial, from whichever goroutine is serving, and
	// written from the message loop, so it lives under the mutex.
	suffix string
	// badSuffix is the last suffix that was refused, kept only so the refusal
	// is logged once rather than on every status refresh.
	badSuffix string
	// exitActive is whether an exit node is selected AND online. Only then
	// does the guard widen: a selected-but-offline exit node leaves the
	// tailnet rule in place, so a public destination is refused instead of
	// being dialled straight out of this machine (G15).
	exitActive bool
}

// SetExitActive tells the proxy whether an exit node is currently carrying this
// profile's traffic. The host calls it from the status path, beside
// SetMagicDNSSuffix.
func (s *Server) SetExitActive(active bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exitActive = active
}

// ExitActive reports the last value SetExitActive was given.
func (s *Server) ExitActive() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitActive
}

// allow applies whichever destination rule is in force.
func (s *Server) allow(host string) error {
	if s.ExitActive() {
		return allowExitHost(host)
	}
	return allowTailnetHost(host, s.MagicDNSSuffix())
}

// SetMagicDNSSuffix tells the proxy the node's own MagicDNS suffix, so a
// tailnet with a custom domain is served rather than refused. The host calls it
// on every status refresh; a tailnet rename therefore reaches the guard.
func (s *Server) SetMagicDNSSuffix(suffix string) {
	if s == nil {
		return
	}
	clean := strings.Trim(suffix, ".")
	if clean != "" && !validMagicDNSSuffix(clean) {
		s.mu.Lock()
		firstTime := s.badSuffix != clean
		// Refused, and the previous one is dropped with it: the rules fall back
		// to .ts.net and single labels, which is where they started.
		s.suffix, s.badSuffix = "", clean
		s.mu.Unlock()
		if firstTime {
			log.Printf("ignoring the MagicDNS suffix %q: it is not a tailnet domain", clean)
		}
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.suffix, s.badSuffix = clean, ""
}

// MagicDNSSuffix returns the suffix last pushed in, or "" if none is known.
func (s *Server) MagicDNSSuffix() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.suffix
}

// Start binds a proxy for ts on 127.0.0.1 and serves it in the background. The
// proxy comes up with the node, before login, so the extension can point the
// browser at a stable port once; connections simply fail until the node is
// running.
func Start(ts *tsnet.Server, token string) (*Server, error) {
	s := &Server{token: token}
	if err := s.serve(dialTailnet(ts, s.allow)); err != nil {
		return nil, err
	}
	return s, nil
}

// start builds a server around an arbitrary dialer. Tests use it; Start is the
// only caller in the program.
func start(dial DialFunc, token string) (*Server, error) {
	s := &Server{token: token}
	if err := s.serve(dial); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) serve(dial DialFunc) error {
	// A server with no credential would be the open loopback proxy this whole
	// mechanism exists to close, so it is a programming error rather than a
	// mode.
	if s.token == "" {
		return errors.New("tailtab: the proxy needs a token")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("binding the loopback proxy: %w", err)
	}
	s.dial, s.ln = s.guard(dial), ln

	socksLn, httpLn := proxymux.SplitSOCKSAndHTTP(ln)
	s.http = &http.Server{
		Handler: s.handler(),
		// A local process must not be able to pin connections open by opening
		// them and going quiet.
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	go func() {
		if err := s.http.Serve(httpLn); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			log.Printf("HTTP proxy stopped: %v", err)
		}
	}()
	go func() {
		// Zen reaches the listener over SOCKS5 and authenticates in-protocol
		// (RFC 1929). Setting these makes the server offer only password
		// authentication, so a client that asks for "no auth" is refused
		// outright. Edge cannot do this at all — Chromium has never
		// implemented SOCKS5 auth (research/browser.md §1.3) — which is why it
		// uses the HTTP side and a 407 instead.
		//
		// The comparison inside socks5.Server is a plain !=, not a
		// constant-time one. That is upstream's, not ours; the HTTP side below
		// uses subtle.ConstantTimeCompare.
		ss := &socks5.Server{
			Logf:     logger.WithPrefix(log.Printf, "socks5: "),
			Dialer:   s.dial,
			Username: User,
			Password: s.token,
		}
		if err := ss.Serve(handshakeDeadlineListener{socksLn}); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("SOCKS5 proxy stopped: %v", err)
		}
	}()
	return nil
}

// socksHandshakeTimeout is how long a client has to get from connecting to the
// end of the SOCKS5 handshake. It is the counterpart of the HTTP server's
// ReadHeaderTimeout above: a local process must not be able to hold a socket
// and a goroutine open by connecting and going quiet, and on this path it has
// not authenticated yet. A variable so tests need not wait 30 seconds.
var socksHandshakeTimeout = 30 * time.Second

// handshakeDeadlineListener puts a deadline on every connection it accepts.
// socks5.Server sets none of its own, and proxymux clears the deadline it used
// to read the first byte, so without this there is nothing to close a stalled
// handshake.
type handshakeDeadlineListener struct{ net.Listener }

func (l handshakeDeadlineListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(socksHandshakeTimeout))
	return &socksConn{Conn: c}, nil
}

// socksConn drops that deadline once the handshake is over, so a tunnel that
// has authenticated is not cut off mid-transfer.
type socksConn struct {
	net.Conn
	// relaying is written once, by the server goroutine, before any relay
	// goroutine exists.
	relaying bool
}

// Write clears the deadline when the server sends the reply that ends the
// handshake. Of the three replies in a SOCKS5 exchange that is the only one
// four bytes or longer that starts with the version byte: the chosen method
// and the authentication result are two bytes each (RFC 1928 §3 and §5,
// RFC 1929 §2). A failure reply has the same shape, and closes straight after.
func (c *socksConn) Write(b []byte) (int, error) {
	if !c.relaying && len(b) >= 4 && b[0] == 5 {
		c.relaying = true
		_ = c.Conn.SetDeadline(time.Time{})
	}
	return c.Conn.Write(b)
}

// Port returns the bound TCP port.
func (s *Server) Port() int {
	if s == nil || s.ln == nil {
		return 0
	}
	if a, ok := s.ln.Addr().(*net.TCPAddr); ok {
		return a.Port
	}
	return 0
}

// Close stops the proxy.
func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	if s.http != nil {
		_ = s.http.Close()
	}
	return s.ln.Close()
}

// hopByHop are the headers that belong to a single transport hop and must not
// be forwarded. httputil.ReverseProxy removes the standard set itself; this
// covers Proxy-Connection, which is not standard but is what browsers send.
var hopByHop = []string{"Proxy-Connection", "Proxy-Authenticate", "Proxy-Authorization"}

func (s *Server) handler() http.Handler {
	rp := &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			// ReverseProxy adds X-Forwarded-For by default, which would tell
			// every tailnet service the loopback address of the proxy. The
			// browser did not send it, so neither do we.
			r.Header["X-Forwarded-For"] = nil
		},
		Transport: &http.Transport{DialContext: s.dial},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("proxying %s: %v", r.Host, err)
			if errors.Is(err, ErrNotTailnet) {
				http.Error(w, "tailtab: "+err.Error(), http.StatusForbidden)
				return
			}
			http.Error(w, "tailtab: "+err.Error(), http.StatusBadGateway)
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Before anything else, including the tailnet rules: an unauthorised
		// caller learns nothing about this tailnet, not even which names it
		// would have served.
		if !s.authorized(r.Header.Get("Proxy-Authorization")) {
			w.Header().Set("Proxy-Authenticate", `Basic realm="tailtab"`)
			w.Header().Set("Connection", "close")
			http.Error(w, "tailtab: proxy authentication required", http.StatusProxyAuthRequired)
			return
		}
		// This is also what strips Proxy-Authorization from a plain request
		// before it is forwarded: the credential is for this hop only, and no
		// tailnet service has any business seeing it.
		for _, h := range hopByHop {
			r.Header.Del(h)
		}
		if r.Method == http.MethodConnect {
			s.serveConnect(w, r)
			return
		}
		// A proxied request carries an absolute URI. A path-only request means
		// something is talking to us as if we were an origin server.
		if strings.HasPrefix(r.RequestURI, "/") || r.RequestURI == "*" {
			http.Error(w, "tailtab: this is a proxy; use an absolute URL or CONNECT", http.StatusBadRequest)
			return
		}
		if err := s.allow(r.URL.Hostname()); err != nil {
			http.Error(w, "tailtab: "+err.Error(), http.StatusForbidden)
			return
		}
		rp.ServeHTTP(w, r)
	})
}

// authorized reports whether a Proxy-Authorization header carries this
// process's credential. Both halves are compared in constant time, and both
// comparisons always run: a short circuit on the username would leak, by
// timing, whether a guess had the username right.
func (s *Server) authorized(header string) bool {
	user, pass, ok := basicProxyAuth(header)
	if !ok {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(User))
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.token))
	return userOK&passOK == 1
}

// basicProxyAuth pulls the username and password out of a Basic
// Proxy-Authorization header. net/http parses Authorization but not this one.
func basicProxyAuth(header string) (user, pass string, ok bool) {
	const prefix = "basic "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header[len(prefix):]))
	if err != nil {
		return "", "", false
	}
	user, pass, ok = strings.Cut(string(raw), ":")
	return user, pass, ok
}

// serveConnect tunnels a CONNECT request to the tailnet.
func (s *Server) serveConnect(w http.ResponseWriter, r *http.Request) {
	if err := s.allow(hostOnly(r.RequestURI)); err != nil {
		http.Error(w, "tailtab: "+err.Error(), http.StatusForbidden)
		return
	}
	dst, err := s.dial(r.Context(), "tcp", r.RequestURI)
	if err != nil {
		log.Printf("CONNECT %s: %v", r.RequestURI, err)
		if errors.Is(err, ErrNotTailnet) {
			http.Error(w, "tailtab: "+err.Error(), http.StatusForbidden)
			return
		}
		http.Error(w, "tailtab: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer dst.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "tailtab: connection cannot be hijacked", http.StatusInternalServerError)
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		http.Error(w, "tailtab: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\n\r\n"); err != nil {
		return
	}

	// The bufio.Reader may already hold bytes the client sent after the
	// CONNECT line; read through it so they are not dropped. With nothing
	// buffered, read the socket directly and let the bufio pair be collected.
	var client io.Reader = buf
	if buf.Reader.Buffered() == 0 {
		client = conn
	}

	done := make(chan struct{}, 2)
	go func() { io.Copy(conn, dst); done <- struct{}{} }()
	go func() { io.Copy(dst, client); done <- struct{}{} }()
	<-done
}
