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
	"strings"

	"tailscale.com/net/proxymux"
	"tailscale.com/net/socks5"
	"tailscale.com/tsnet"
	"tailscale.com/types/logger"
)

// DialFunc dials one connection out through the tailnet.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

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
	s := &Server{dial: dial, ln: ln}

	socksLn, httpLn := proxymux.SplitSOCKSAndHTTP(ln)
	s.http = &http.Server{Handler: s.handler()}

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
		Director:  func(*http.Request) {}, // the request is already absolute
		Transport: &http.Transport{DialContext: s.dial},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("proxying %s: %v", r.Host, err)
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
		rp.ServeHTTP(w, r)
	})
}

// serveConnect tunnels a CONNECT request to the tailnet.
func (s *Server) serveConnect(w http.ResponseWriter, r *http.Request) {
	dst, err := s.dial(r.Context(), "tcp", r.RequestURI)
	if err != nil {
		log.Printf("CONNECT %s: %v", r.RequestURI, err)
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
