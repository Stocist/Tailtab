package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingDialer stands in for the tailnet dialer: it records the address it
// was asked for, which is how these tests prove the hostname reaches the dialer
// unresolved, then connects to a real local server.
type recordingDialer struct {
	target string
	mu     sync.Mutex
	asked  []string
}

func (d *recordingDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.asked = append(d.asked, addr)
	d.mu.Unlock()
	return (&net.Dialer{}).DialContext(ctx, network, d.target)
}

func (d *recordingDialer) addresses() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.asked...)
}

func TestPlainHTTPIsProxiedByHostname(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "host=%s path=%s proxyconn=%q", r.Host, r.URL.Path, r.Header.Get("Proxy-Connection"))
	}))
	defer backend.Close()

	d := &recordingDialer{target: strings.TrimPrefix(backend.URL, "http://")}
	p, err := start(d.dial)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.Close()
	if p.Port() == 0 {
		t.Fatal("Port() returned 0 after a successful bind")
	}

	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", p.Port()))
	c := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
		Timeout:   10 * time.Second,
	}
	// A single-label MagicDNS name, which is exactly what must not be resolved
	// before it reaches the dialer.
	resp, err := c.Get("http://wiki/hello")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, body %q", resp.StatusCode, body)
	}
	if got, want := string(body), `host=wiki path=/hello proxyconn=""`; got != want {
		t.Errorf("backend saw %q, want %q", got, want)
	}
	if asked := d.addresses(); len(asked) != 1 || asked[0] != "wiki:80" {
		t.Errorf("dialer was asked for %v, want [wiki:80]", asked)
	}
}

func TestConnectTunnels(t *testing.T) {
	// A raw TCP echo server stands in for an HTTPS origin on the tailnet.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()

	d := &recordingDialer{target: ln.Addr().String()}
	p, err := start(d.dial)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.Close()

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port()))
	if err != nil {
		t.Fatalf("dialing the proxy: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	// Send the payload immediately after the CONNECT request so it lands in the
	// server's bufio.Reader: the handler has to flush those buffered bytes into
	// the tunnel or they are lost.
	if _, err := io.WriteString(conn, "CONNECT wiki.tail4d5e6f.ts.net:443 HTTP/1.1\r\nHost: wiki.tail4d5e6f.ts.net:443\r\n\r\nping"); err != nil {
		t.Fatalf("writing CONNECT: %v", err)
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading the CONNECT response: %v", err)
	}
	if !strings.HasPrefix(line, "HTTP/1.1 200") {
		t.Fatalf("CONNECT response %q, want 200", strings.TrimSpace(line))
	}
	if _, err := br.ReadString('\n'); err != nil { // the blank line
		t.Fatalf("reading the header terminator: %v", err)
	}
	echoed := make([]byte, 4)
	if _, err := io.ReadFull(br, echoed); err != nil {
		t.Fatalf("reading the echoed payload: %v", err)
	}
	if string(echoed) != "ping" {
		t.Errorf("tunnel echoed %q, want %q; buffered bytes were dropped", echoed, "ping")
	}
	if asked := d.addresses(); len(asked) != 1 || asked[0] != "wiki.tail4d5e6f.ts.net:443" {
		t.Errorf("dialer was asked for %v, want [wiki.tail4d5e6f.ts.net:443]", asked)
	}
}

func TestDialFailureIsReported(t *testing.T) {
	fail := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, fmt.Errorf("the node is not running")
	}
	p, err := start(fail)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.Close()

	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", p.Port()))
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 10 * time.Second}
	resp, err := c.Get("http://wiki/")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status %d, want 502 when the tailnet dial fails", resp.StatusCode)
	}
}

func TestOriginStyleRequestRejected(t *testing.T) {
	p, err := start(func(context.Context, string, string) (net.Conn, error) {
		t.Error("a path-only request should never reach the dialer")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.Close()

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(conn, "GET /admin HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	if !strings.HasPrefix(line, "HTTP/1.1 400") {
		t.Errorf("response %q, want 400", strings.TrimSpace(line))
	}
}
