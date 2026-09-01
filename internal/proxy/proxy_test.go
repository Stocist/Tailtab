package proxy

import (
	"bufio"
	"context"
	"errors"
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

func TestAllowTailnetHost(t *testing.T) {
	allowed := []string{
		"wiki",                     // MagicDNS short name
		"server",                    //
		"wiki.tail4d5e6f.ts.net",   // MagicDNS FQDN
		"host.tail1a2b3c.ts.net",    // another tailnet
		"WIKI.TAIL4D5E6F.TS.NET",   // case
		"wiki.tail4d5e6f.ts.net.",  // trailing dot
		"100.64.0.1",                // CGNAT, first
		"100.101.102.103",           //
		"100.127.255.255",           // CGNAT, last
		"fd7a:115c:a1e0::1",         // Tailscale ULA
		"[fd7a:115c:a1e0:ab12::1]",  // bracketed
		"::ffff:100.64.0.1",         // v4-mapped CGNAT
	}
	for _, h := range allowed {
		if err := allowTailnetHost(h); err != nil {
			t.Errorf("allowTailnetHost(%q) = %v, want nil", h, err)
		}
	}

	refused := []string{
		"",
		"github.com",
		"www.google.com",
		"8.8.8.8",
		"localhost",
		"dev.localhost",
		"127.0.0.1",
		"::1",
		"192.168.1.1",
		"100.63.255.255",                    // just below CGNAT
		"100.128.0.1",                       // just above CGNAT
		"fd00::1",                           // a different ULA
		"evil-ts.net",                       // not a subdomain of ts.net
		"ts.net.attacker.com",               // suffix in the middle
		"wiki.tail4d5e6f.ts.net.evil.com",  // suffix in the middle
		"2130706433",                        // 127.0.0.1 as an integer
		"0x7f000001",                        // 127.0.0.1 as hexadecimal
	}
	for _, h := range refused {
		err := allowTailnetHost(h)
		if err == nil {
			t.Errorf("allowTailnetHost(%q) = nil, want a refusal", h)
			continue
		}
		if !errors.Is(err, ErrNotTailnet) {
			t.Errorf("allowTailnetHost(%q) = %v, which does not wrap ErrNotTailnet", h, err)
		}
	}
}

func TestNonTailnetDestinationsAreRefused(t *testing.T) {
	// The dialer must never be reached: this listener is not an open forward
	// proxy, whatever UserDial would have been willing to do.
	dialed := false
	p, err := start(func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, fmt.Errorf("should not be reached")
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.Close()

	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", p.Port()))
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 10 * time.Second}

	resp, err := c.Get("http://github.com/")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("plain GET to github.com: status %d, want 403", resp.StatusCode)
	}

	// CONNECT, the path a browser uses for https.
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(conn, "CONNECT github.com:443 HTTP/1.1\r\nHost: github.com:443\r\n\r\n")
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the CONNECT response: %v", err)
	}
	if !strings.HasPrefix(line, "HTTP/1.1 403") {
		t.Errorf("CONNECT to github.com: %q, want 403", strings.TrimSpace(line))
	}
	if dialed {
		t.Error("a non-tailnet destination reached the dialer")
	}
}

// socks5Connect performs a no-auth SOCKS5 CONNECT and returns the reply code.
// 0 is success; anything else is a refusal.
func socks5Connect(t *testing.T, proxyPort int, host string, port uint16) byte {
	t.Helper()
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := conn.Write([]byte{5, 1, 0}); err != nil { // version, one method, "no auth"
		t.Fatalf("SOCKS greeting: %v", err)
	}
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(conn, greeting); err != nil {
		t.Fatalf("reading the SOCKS greeting reply: %v", err)
	}
	if greeting[0] != 5 || greeting[1] != 0 {
		t.Fatalf("SOCKS greeting reply = %v, want [5 0]", greeting)
	}

	req := []byte{5, 1, 0, 3, byte(len(host))} // CONNECT, domain name
	req = append(req, host...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := conn.Write(req); err != nil {
		t.Fatalf("SOCKS request: %v", err)
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("reading the SOCKS reply: %v", err)
	}
	return reply[1]
}

func TestSOCKSRefusesNonTailnetDestinations(t *testing.T) {
	// SOCKS has no handler in front of it, so the guard has to live on the
	// dialer itself.
	dialed := make(chan string, 4)
	p, err := start(func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed <- addr
		return nil, fmt.Errorf("no node")
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.Close()

	if code := socks5Connect(t, p.Port(), "github.com", 443); code == 0 {
		t.Error("SOCKS5 CONNECT to github.com succeeded; the listener is an open forward proxy")
	}
	select {
	case addr := <-dialed:
		t.Errorf("SOCKS5 reached the dialer with %q", addr)
	default:
	}

	// A tailnet name gets through the guard and fails at the dialer instead,
	// which proves the refusal above was the guard and not a broken SOCKS path.
	if code := socks5Connect(t, p.Port(), "wiki", 80); code == 0 {
		t.Error("SOCKS5 CONNECT reported success with no node running")
	}
	select {
	case addr := <-dialed:
		if addr != "wiki:80" {
			t.Errorf("dialer was asked for %q, want wiki:80", addr)
		}
	case <-time.After(5 * time.Second):
		t.Error("a tailnet name never reached the dialer")
	}
}

func TestXForwardedForIsNotAdded(t *testing.T) {
	got := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("X-Forwarded-For")
	}))
	defer backend.Close()

	d := &recordingDialer{target: strings.TrimPrefix(backend.URL, "http://")}
	p, err := start(d.dial)
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
	resp.Body.Close()
	if xff := <-got; xff != "" {
		t.Errorf("the tailnet service saw X-Forwarded-For: %q, want no header", xff)
	}
}
