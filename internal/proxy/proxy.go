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

// Tailscale's address ranges: CGNAT for IPv4, and its ULA prefix for IPv6.
var (
	tailscaleV4 = netip.MustParsePrefix("100.64.0.0/10")
	tailscaleV6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// allowTailnetHost reports whether host is something the tailnet can serve.
//
// Without this check the listener is a general-purpose open forward proxy, not
// a tailnet proxy: UserDial falls back to the system resolver and then to a
// plain dial for anything MagicDNS does not know, so any local process could
// use it to reach the whole internet. The extension only ever sends tailnet
// traffic here, so refusing the rest costs nothing.
//
// These are the rules in extension/rules.js, in Go. The two are kept in step by
// hand; each is tested on both sides.
func allowTailnetHost(host string) error {
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
func guard(dial DialFunc) DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if err := allowTailnetHost(hostOnly(addr)); err != nil {
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
func dialTailnet(ts *tsnet.Server) DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		// Also checked in guard, which wraps this. Repeated here because this
		// is the function that reaches the network: it has to be safe alone.
		if err := allowTailnetHost(hostOnly(addr)); err != nil {
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
	dial DialFunc
	ln   net.Listener
	http *http.Server
}

// Start binds a proxy for ts on 127.0.0.1 and serves it in the background. The
// proxy comes up with the node, before login, so the extension can point the
// browser at a stable port once; connections simply fail until the node is
// running.
func Start(ts *tsnet.Server) (*Server, error) {
	return start(dialTailnet(ts))
}

func start(dial DialFunc) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("binding the loopback proxy: %w", err)
	}
	s := &Server{dial: guard(dial), ln: ln}

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
		// No authentication: the listener is on loopback with a random port,
		// and Chromium cannot do SOCKS5 auth at all (research/browser.md §1.3).
		ss := &socks5.Server{
			Logf:   logger.WithPrefix(log.Printf, "socks5: "),
			Dialer: s.dial,
		}
		if err := ss.Serve(socksLn); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("SOCKS5 proxy stopped: %v", err)
		}
	}()
	return s, nil
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
		if err := allowTailnetHost(r.URL.Hostname()); err != nil {
			http.Error(w, "tailtab: "+err.Error(), http.StatusForbidden)
			return
		}
		rp.ServeHTTP(w, r)
	})
}

// serveConnect tunnels a CONNECT request to the tailnet.
func (s *Server) serveConnect(w http.ResponseWriter, r *http.Request) {
	if err := allowTailnetHost(hostOnly(r.RequestURI)); err != nil {
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
