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
	"slices"
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

// User is the fixed username paired with the per-process proxy token.
const User = "tailtab"

// NewToken returns a 32-byte random proxy password. It limits the loopback
// listener to one extension session and must never be logged or displayed.
func NewToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating the proxy token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

var (
	tailscaleV4 = netip.MustParsePrefix("100.64.0.0/10")
	tailscaleV6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// Exit nodes must not reach their own private, loopback, or link-local networks.
var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

func isPrivate(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, p := range privateRanges {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// Exit mode allows public destinations but refuses networks local to either
// endpoint. The browser rule must switch on the same status field.
func allowExitHost(host string, routes []netip.Prefix) error {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if h == "" {
		return fmt.Errorf("%w: no host", ErrNotTailnet)
	}
	if addr, ok := hostAddr(h); ok {
		if tailscaleV4.Contains(addr) || tailscaleV6.Contains(addr) {
			return nil
		}
		// Check local-only ranges before routes to prevent SSRF.
		if isLocalOnly(addr) {
			return fmt.Errorf("%w: %s is local to this machine", ErrNotTailnet, h)
		}
		// Peer-routed subnets stay on their subnet router in exit mode.
		if inRoutes(addr, routes) {
			return nil
		}
		if isPrivate(addr) {
			return fmt.Errorf("%w: %s is a private address, which an exit node must not dial", ErrNotTailnet, h)
		}
		return nil
	}
	if strings.ContainsAny(h, ":%") {
		return fmt.Errorf("%w: %s is neither an address nor a name", ErrNotTailnet, h)
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return fmt.Errorf("%w: %s is loopback", ErrNotTailnet, h)
	}
	// Refuse numeric forms netip rejects, such as IPv4 with leading zeros.
	if numericHost.MatchString(h) || dottedNumeric.MatchString(h) {
		return fmt.Errorf("%w: %s is a numeric address", ErrNotTailnet, h)
	}
	return nil
}

var dottedNumeric = regexp.MustCompile(`^[0-9.]+$`)

// hostAddr parses h without a zone: Prefix.Contains never matches a zoned
// address, so "::1%lo0" would pass every range check.
func hostAddr(h string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(h)
	if err != nil || addr.Zone() != "" {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

// allowTailnetHost prevents UserDial's system fallback from turning the listener
// into an open forward proxy. The browser and host enforce the same decisions
// from testdata/tailnet-hosts.json.
func allowTailnetHost(host, suffix string, routes []netip.Prefix) error {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if h == "" {
		return fmt.Errorf("%w: no host", ErrNotTailnet)
	}
	if addr, ok := hostAddr(h); ok {
		if tailscaleV4.Contains(addr) || tailscaleV6.Contains(addr) {
			return nil
		}
		// Check local-only ranges before routes to prevent SSRF.
		if isLocalOnly(addr) {
			return fmt.Errorf("%w: %s is local to this machine", ErrNotTailnet, h)
		}
		if inRoutes(addr, routes) {
			return nil
		}
		return fmt.Errorf("%w: %s is outside %s, %s and the tailnet's subnet routes", ErrNotTailnet, h, tailscaleV4, tailscaleV6)
	}
	if strings.ContainsAny(h, ":%") {
		return fmt.Errorf("%w: %s is neither an address nor a name", ErrNotTailnet, h)
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return fmt.Errorf("%w: %s is loopback", ErrNotTailnet, h)
	}
	if strings.HasSuffix(h, ".ts.net") {
		return nil
	}
	if d := strings.Trim(suffix, "."); validMagicDNSSuffix(d) {
		// Match on label boundaries to avoid suffix-confusion attacks.
		if h == d || strings.HasSuffix(h, "."+d) {
			return nil
		}
	}
	if strings.Contains(h, ".") {
		return fmt.Errorf("%w: %s is not a MagicDNS name", ErrNotTailnet, h)
	}
	// Bare decimal and hexadecimal values can conceal IPv4 addresses.
	if numericHost.MatchString(h) {
		return fmt.Errorf("%w: %s is a numeric address", ErrNotTailnet, h)
	}
	return nil
}

// localOnlyRanges stay blocked even when a peer advertises an overlapping route.
var localOnlyRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

func isLocalOnly(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, p := range localOnlyRanges {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// inRoutes repeats route validation at the final network boundary.
func inRoutes(addr netip.Addr, routes []netip.Prefix) bool {
	for _, r := range routes {
		if !usableRoute(r) {
			continue
		}
		if r.Contains(addr) {
			return true
		}
	}
	return false
}

func usableRoute(r netip.Prefix) bool {
	if !r.IsValid() {
		return false
	}
	floor := 8
	if r.Addr().Unmap().Is6() && !r.Addr().Is4In6() {
		floor = 16
	}
	if r.Bits() < floor {
		return false
	}
	r = r.Masked()
	for _, p := range localOnlyRanges {
		if r.Overlaps(p) {
			return false
		}
	}
	return true
}

var numericHost = regexp.MustCompile(`^([0-9]+|0x[0-9a-f]+)$`)

var magicDNSSuffixRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// Coordination servers provide this suffix, which widens split-tunnel routing.
// Require a full lowercase DNS name and reject public parents such as ts.net.
func validMagicDNSSuffix(s string) bool {
	if s == "" || len(s) > 253 || s == "ts.net" {
		return false
	}
	return magicDNSSuffixRE.MatchString(s)
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// guard enforces destination policy at every dial, including SOCKS5.
func (s *Server) guard(dial DialFunc) DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if err := s.allow(hostOnly(addr)); err != nil {
			return nil, err
		}
		return dial(ctx, network, addr)
	}
}

// UserDial fails before login instead of blocking in Server.Dial. Hostnames stay
// unresolved until UserDial so MagicDNS remains authoritative.
func dialTailnet(ts *tsnet.Server, allow func(string) error) DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		// Recheck policy at the function that reaches the network.
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
	// Routing state is written by the message loop and read by serving goroutines.
	suffix    string
	badSuffix string
	// Widen only for a selected, online exit node; all other states fail closed.
	exitActive bool
	routes     []netip.Prefix
}

// SetSubnetRoutes replaces the routed subnets the guard allows.
func (s *Server) SetSubnetRoutes(routes []netip.Prefix) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes = slices.Clone(routes)
}

// SubnetRoutes returns the routes last pushed in.
func (s *Server) SubnetRoutes() []netip.Prefix {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.routes)
}

// SetExitActive sets whether an exit node is currently carrying traffic.
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

func (s *Server) allow(host string) error {
	routes := s.SubnetRoutes()
	if s.ExitActive() {
		return allowExitHost(host, routes)
	}
	return allowTailnetHost(host, s.MagicDNSSuffix(), routes)
}

// SetMagicDNSSuffix updates the trusted custom-domain routing boundary.
func (s *Server) SetMagicDNSSuffix(suffix string) {
	if s == nil {
		return
	}
	clean := strings.Trim(suffix, ".")
	if clean != "" && !validMagicDNSSuffix(clean) {
		s.mu.Lock()
		firstTime := s.badSuffix != clean
		// Drop the previous suffix too, returning to the fail-closed base rules.
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

// start is the dialer injection seam for tests.
func start(dial DialFunc, token string) (*Server, error) {
	s := &Server{token: token}
	if err := s.serve(dial); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) serve(dial DialFunc) error {
	// Refuse an unauthenticated open loopback proxy.
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
		// Bound unauthenticated idle connections from local processes.
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	go func() {
		if err := s.http.Serve(httpLn); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			log.Printf("HTTP proxy stopped: %v", err)
		}
	}()
	go func() {
		// Zen supports RFC 1929 credentials; Chromium does not and uses HTTP 407.
		// The upstream SOCKS comparison is not constant-time; HTTP auth below is.
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

// A variable keeps the unauthenticated handshake timeout testable.
var socksHandshakeTimeout = 30 * time.Second

// proxymux clears its deadline and socks5 sets none, so apply one after accept.
type handshakeDeadlineListener struct{ net.Listener }

func (l handshakeDeadlineListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(socksHandshakeTimeout))
	return &socksConn{Conn: c}, nil
}

// socksConn clears the handshake deadline before relaying authenticated traffic.
type socksConn struct {
	net.Conn
	// relaying is written before relay goroutines start.
	relaying bool
}

// Write identifies the final SOCKS reply by its version and length, then clears
// the deadline before relay goroutines start.
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
			// Do not disclose the proxy's loopback address to tailnet services.
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
		// Authenticate before routing checks to avoid exposing tailnet names.
		if !s.authorized(r.Header.Get("Proxy-Authorization")) {
			w.Header().Set("Proxy-Authenticate", `Basic realm="tailtab"`)
			w.Header().Set("Connection", "close")
			http.Error(w, "tailtab: proxy authentication required\n\nThis request reached the tailtab proxy without the extension's credential, which usually means the browser is holding proxy settings from an earlier tailtab host. Reopen the tailtab popup and reload the page.", http.StatusProxyAuthRequired)
			return
		}
		// Never forward the hop-specific proxy credential.
		for _, h := range hopByHop {
			r.Header.Del(h)
		}
		if r.Method == http.MethodConnect {
			s.serveConnect(w, r)
			return
		}
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

// authorized always compares both credential halves in constant time.
func (s *Server) authorized(header string) bool {
	user, pass, ok := basicProxyAuth(header)
	if !ok {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(User))
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.token))
	return userOK&passOK == 1
}

// net/http does not parse Proxy-Authorization.
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

	// Preserve payload bytes buffered while parsing CONNECT.
	var client io.Reader = buf
	if buf.Reader.Buffered() == 0 {
		client = conn
	}

	done := make(chan struct{}, 2)
	go func() { io.Copy(conn, dst); done <- struct{}{} }()
	go func() { io.Copy(dst, client); done <- struct{}{} }()
	<-done
}
