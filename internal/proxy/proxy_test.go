package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testToken is the proxy credential these tests use. The real one is 32 random
// bytes per host process; the value does not matter here, only that every
// request has to carry it.
const testToken = "TESTTOKEN-not-a-real-secret"

// testAuthHeader is what a client sends: Basic base64("tailtab:<token>").
var testAuthHeader = "Basic " + base64.StdEncoding.EncodeToString([]byte(User+":"+testToken))

// proxyClient returns an HTTP client that proxies through p with the
// credential. Go puts the userinfo from the proxy URL into Proxy-Authorization
// on both plain requests and CONNECT.
func proxyClient(p *Server) *http.Client {
	u, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", p.Port()))
	if err != nil {
		panic(err)
	}
	u.User = url.UserPassword(User, testToken)
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}, Timeout: 10 * time.Second}
}

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
	p, err := start(d.dial, testToken)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.Close()
	if p.Port() == 0 {
		t.Fatal("Port() returned 0 after a successful bind")
	}

	c := proxyClient(p)
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
	p, err := start(d.dial, testToken)
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
	if _, err := io.WriteString(conn, "CONNECT wiki.tail4d5e6f.ts.net:443 HTTP/1.1\r\nHost: wiki.tail4d5e6f.ts.net:443\r\n"+
		"Proxy-Authorization: "+testAuthHeader+"\r\n\r\nping"); err != nil {
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
	p, err := start(fail, testToken)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.Close()

	c := proxyClient(p)
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
	}, testToken)
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
	io.WriteString(conn, "GET /admin HTTP/1.1\r\nHost: 127.0.0.1\r\n"+
		"Proxy-Authorization: "+testAuthHeader+"\r\n\r\n")
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	if !strings.HasPrefix(line, "HTTP/1.1 400") {
		t.Errorf("response %q, want 400", strings.TrimSpace(line))
	}
}

// hostCase is one row of testdata/tailnet-hosts.json, the decision table this
// guard shares with extension/rules.js.
type hostCase struct {
	Host   string `json:"host"`
	Suffix string `json:"suffix"`
	Proxy  bool   `json:"proxy"`
	Why    string `json:"why"`
	// Routes is the subnet routes the node knows at the time, as CIDRs.
	Routes []string `json:"routes"`
}

// prefixes parses a table row's routes the way the host does before they
// reach the guard (parseRoutes in cmd/tailtab): a malformed one is dropped, so
// the table can say that a bad route widens nothing.
func prefixes(t *testing.T, cidrs []string) []netip.Prefix {
	t.Helper()
	var out []netip.Prefix
	for _, c := range cidrs {
		if p, err := netip.ParsePrefix(c); err == nil {
			out = append(out, p.Masked())
		}
	}
	return out
}

func loadHostCases(t *testing.T) []hostCase {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "tailnet-hosts.json"))
	if err != nil {
		t.Fatalf("reading the shared host table: %v", err)
	}
	var table struct {
		Cases []hostCase `json:"cases"`
	}
	if err := json.Unmarshal(b, &table); err != nil {
		t.Fatalf("parsing the shared host table: %v", err)
	}
	if len(table.Cases) < 55 {
		t.Fatalf("the shared host table has only %d cases; it should not have shrunk", len(table.Cases))
	}
	return table.Cases
}

// TestAllowTailnetHost drives the guard from the same table extension/rules.js
// is tested against. The two implementations are held together by this file:
// a host the extension proxies but the guard refuses is a 403 the user sees.
func TestAllowTailnetHost(t *testing.T) {
	for _, c := range loadHostCases(t) {
		err := allowTailnetHost(c.Host, c.Suffix, prefixes(t, c.Routes))
		if c.Proxy && err != nil {
			t.Errorf("allowTailnetHost(%q, %q) = %v, want nil (%s)", c.Host, c.Suffix, err, c.Why)
		}
		if !c.Proxy {
			if err == nil {
				t.Errorf("allowTailnetHost(%q, %q) = nil, want a refusal (%s)", c.Host, c.Suffix, c.Why)
			} else if !errors.Is(err, ErrNotTailnet) {
				t.Errorf("allowTailnetHost(%q, %q) = %v, which does not wrap ErrNotTailnet", c.Host, c.Suffix, err)
			}
		}
	}
}

// TestCustomMagicDNSSuffixIsRefusedUntilKnown is R1: a tailnet on a custom
// domain is proxied by the extension, so the guard has to serve it — but only
// once the node has actually reported that suffix, never on a name the browser
// happened to send.
func TestCustomMagicDNSSuffixIsRefusedUntilKnown(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "reached")
	}))
	defer backend.Close()

	d := &recordingDialer{target: strings.TrimPrefix(backend.URL, "http://")}
	p, err := start(d.dial, testToken)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.Close()

	c := proxyClient(p)

	if p.MagicDNSSuffix() != "" {
		t.Fatalf("a fresh proxy already knows the suffix %q", p.MagicDNSSuffix())
	}
	resp, err := c.Get("http://host.my-tailnet.example.com/")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("before the suffix is known: status %d, want 403", resp.StatusCode)
	}

	p.SetMagicDNSSuffix("my-tailnet.example.com")
	resp, err = c.Get("http://host.my-tailnet.example.com/")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "reached" {
		t.Errorf("after the suffix is known: status %d body %q, want 200 \"reached\"", resp.StatusCode, body)
	}

	// The SOCKS path reads the same suffix, through the dialer guard.
	if code := socks5Connect(t, p.Port(), "host.my-tailnet.example.com", 80); code != 0 {
		t.Errorf("SOCKS5 CONNECT to the custom domain: reply code %d, want 0", code)
	}
	if code := socks5Connect(t, p.Port(), "other.example.com", 80); code == 0 {
		t.Error("SOCKS5 CONNECT to a domain outside the tailnet succeeded")
	}
}

func TestNonTailnetDestinationsAreRefused(t *testing.T) {
	// The dialer must never be reached: this listener is not an open forward
	// proxy, whatever UserDial would have been willing to do.
	dialed := false
	p, err := start(func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, fmt.Errorf("should not be reached")
	}, testToken)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.Close()

	c := proxyClient(p)

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
	io.WriteString(conn, "CONNECT github.com:443 HTTP/1.1\r\nHost: github.com:443\r\n"+
		"Proxy-Authorization: "+testAuthHeader+"\r\n\r\n")
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

// SOCKS5 method numbers, from RFC 1928.
const (
	socksNoAuth       = byte(0)
	socksPassword     = byte(2)
	socksNoAcceptable = byte(0xff)
)

// socks5Greet opens a connection to the proxy and offers the given
// authentication methods. It returns the connection and the method the server
// chose; 0xff means it accepted none of them.
func socks5Greet(t *testing.T, proxyPort int, methods ...byte) (net.Conn, byte) {
	t.Helper()
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	greeting := append([]byte{5, byte(len(methods))}, methods...)
	if _, err := conn.Write(greeting); err != nil {
		t.Fatalf("SOCKS greeting: %v", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("reading the SOCKS greeting reply: %v", err)
	}
	if reply[0] != 5 {
		t.Fatalf("SOCKS greeting reply = %v, want a version-5 reply", reply)
	}
	return conn, reply[1]
}

// socks5Auth runs the RFC 1929 username/password exchange and returns the
// server's status byte; 0 is success. A server that hangs up instead of
// answering is a refusal too.
func socks5Auth(t *testing.T, conn net.Conn, user, pass string) byte {
	t.Helper()
	msg := []byte{1, byte(len(user))}
	msg = append(msg, user...)
	msg = append(msg, byte(len(pass)))
	msg = append(msg, pass...)
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("SOCKS authentication: %v", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return 1
	}
	return reply[1]
}

// socks5Request sends a CONNECT for host:port and returns the reply code. 0 is
// success; anything else is a refusal.
func socks5Request(t *testing.T, conn net.Conn, host string, port uint16) byte {
	t.Helper()
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

// socks5Connect performs an authenticated SOCKS5 CONNECT and returns the reply
// code. 0 is success; anything else is a refusal.
func socks5Connect(t *testing.T, proxyPort int, host string, port uint16) byte {
	t.Helper()
	conn, method := socks5Greet(t, proxyPort, socksPassword)
	if method != socksPassword {
		t.Fatalf("the server chose method %d, want password authentication (%d)", method, socksPassword)
	}
	if status := socks5Auth(t, conn, User, testToken); status != 0 {
		t.Fatalf("SOCKS authentication with the right credential failed with status %d", status)
	}
	return socks5Request(t, conn, host, port)
}

func TestSOCKSRefusesNonTailnetDestinations(t *testing.T) {
	// SOCKS has no handler in front of it, so the guard has to live on the
	// dialer itself.
	dialed := make(chan string, 4)
	p, err := start(func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed <- addr
		return nil, fmt.Errorf("no node")
	}, testToken)
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
	p, err := start(d.dial, testToken)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.Close()

	c := proxyClient(p)
	resp, err := c.Get("http://wiki/")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	resp.Body.Close()
	if xff := <-got; xff != "" {
		t.Errorf("the tailnet service saw X-Forwarded-For: %q, want no header", xff)
	}
}

// --------------------------------------------------------------- H2, auth

// connectStatus opens a raw connection, sends a CONNECT with the given
// Proxy-Authorization header value ("" for none), and returns the status line.
func connectStatus(t *testing.T, proxyPort int, auth string) string {
	t.Helper()
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	req := "CONNECT wiki:443 HTTP/1.1\r\nHost: wiki:443\r\n"
	if auth != "" {
		req += "Proxy-Authorization: " + auth + "\r\n"
	}
	if _, err := io.WriteString(conn, req+"\r\n"); err != nil {
		t.Fatalf("writing CONNECT: %v", err)
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading the CONNECT response: %v", err)
	}
	// Drain the headers so the 407's Proxy-Authenticate can be read by the
	// caller through the same reader.
	hdrs := map[string]string{}
	for {
		h, err := br.ReadString('\n')
		if err != nil || strings.TrimSpace(h) == "" {
			break
		}
		if k, v, ok := strings.Cut(h, ":"); ok {
			hdrs[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	if strings.HasPrefix(line, "HTTP/1.1 407") {
		if got := hdrs["proxy-authenticate"]; got != `Basic realm="tailtab"` {
			t.Errorf("407 carried Proxy-Authenticate %q, want Basic realm=\"tailtab\"", got)
		}
	}
	return strings.TrimSpace(line)
}

// Any local process can open the loopback port. Without a credential it can
// also borrow this browser profile's tailnet identity, which is what these
// tests close.
func TestHTTPRequiresTheToken(t *testing.T) {
	dialed := false
	p, err := start(func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, fmt.Errorf("should not be reached")
	}, testToken)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.Close()

	wrong := "Basic " + base64.StdEncoding.EncodeToString([]byte(User+":wrong-token"))
	wrongUser := "Basic " + base64.StdEncoding.EncodeToString([]byte("someone:"+testToken))
	for _, tc := range []struct{ name, auth string }{
		{"no credential", ""},
		{"wrong token", wrong},
		{"wrong username", wrongUser},
		{"not base64", "Basic %%%%"},
		{"another scheme", "Bearer " + testToken},
		{"the token alone", testToken},
	} {
		if got := connectStatus(t, p.Port(), tc.auth); !strings.HasPrefix(got, "HTTP/1.1 407") {
			t.Errorf("CONNECT with %s: %q, want 407", tc.name, got)
		}
	}
	// Plain HTTP, the other half of the proxy.
	plain := &http.Client{
		Transport: &http.Transport{Proxy: func(*http.Request) (*url.URL, error) {
			return url.Parse(fmt.Sprintf("http://127.0.0.1:%d", p.Port()))
		}},
		Timeout: 10 * time.Second,
	}
	resp, err := plain.Get("http://wiki/")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Errorf("plain GET with no credential: status %d, want 407", resp.StatusCode)
	}
	if got := resp.Header.Get("Proxy-Authenticate"); got != `Basic realm="tailtab"` {
		t.Errorf("Proxy-Authenticate = %q", got)
	}
	if dialed {
		t.Fatal("an unauthenticated request reached the tailnet dialer")
	}

	// The same credential that works over SOCKS gets through here, which is
	// what proves the refusals above were the credential check and not a
	// broken handler.
	if got := connectStatus(t, p.Port(), testAuthHeader); strings.HasPrefix(got, "HTTP/1.1 407") {
		t.Errorf("CONNECT with the right credential: %q, want past the 407", got)
	}
	if !dialed {
		t.Error("an authenticated CONNECT never reached the tailnet dialer")
	}
}

// The credential is for this hop. A tailnet service must never see it.
func TestProxyAuthorizationIsNotForwarded(t *testing.T) {
	got := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Proxy-Authorization")
	}))
	defer backend.Close()

	d := &recordingDialer{target: strings.TrimPrefix(backend.URL, "http://")}
	p, err := start(d.dial, testToken)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.Close()

	resp, err := proxyClient(p).Get("http://wiki/")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200 with the right credential", resp.StatusCode)
	}
	if h := <-got; h != "" {
		t.Errorf("the tailnet service was sent Proxy-Authorization: %q", h)
	}
}

func TestSOCKSRequiresTheToken(t *testing.T) {
	dialed := make(chan string, 4)
	p, err := start(func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed <- addr
		return nil, fmt.Errorf("no node")
	}, testToken)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.Close()

	// A client offering only "no authentication" is refused outright.
	if _, method := socks5Greet(t, p.Port(), socksNoAuth); method != socksNoAcceptable {
		t.Errorf("the server accepted method %d for a no-auth client, want 0xff", method)
	}
	// Offering both, the server must still pick password authentication.
	conn, method := socks5Greet(t, p.Port(), socksNoAuth, socksPassword)
	if method != socksPassword {
		t.Fatalf("the server chose method %d, want password authentication", method)
	}
	if status := socks5Auth(t, conn, User, "wrong-token"); status == 0 {
		t.Error("SOCKS authentication with the wrong token succeeded")
	}
	conn, method = socks5Greet(t, p.Port(), socksPassword)
	if method != socksPassword {
		t.Fatalf("the server chose method %d, want password authentication", method)
	}
	if status := socks5Auth(t, conn, "someone", testToken); status == 0 {
		t.Error("SOCKS authentication with the wrong username succeeded")
	}
	select {
	case addr := <-dialed:
		t.Errorf("an unauthenticated SOCKS client reached the dialer with %q", addr)
	default:
	}

	// The right credential gets through to the guarded dialer, and no further.
	if code := socks5Connect(t, p.Port(), "wiki", 80); code == 0 {
		t.Error("SOCKS5 CONNECT reported success with no node running")
	}
	select {
	case addr := <-dialed:
		if addr != "wiki:80" {
			t.Errorf("dialer was asked for %q, want wiki:80", addr)
		}
	case <-time.After(5 * time.Second):
		t.Error("an authenticated SOCKS client never reached the dialer")
	}
}

// A guess must not be able to learn how much of the credential it got right
// from how long the answer took.
func TestCredentialsAreComparedInConstantTime(t *testing.T) {
	src, err := os.ReadFile("proxy.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "subtle.ConstantTimeCompare") {
		t.Error("the credential comparison is not subtle.ConstantTimeCompare")
	}
	// Both comparisons must run: a short circuit on the username is a timing
	// oracle for the username.
	if strings.Contains(string(src), "userOK && passOK") {
		t.Error("the two comparisons are short-circuited, which leaks the username by timing")
	}
}

func TestAServerWithoutATokenDoesNotStart(t *testing.T) {
	p, err := start(func(context.Context, string, string) (net.Conn, error) {
		return nil, nil
	}, "")
	if err == nil {
		p.Close()
		t.Fatal("a proxy with no token started; that is the open listener this closes")
	}
}

func TestNewTokenIsLongAndRandom(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 16; i++ {
		tok, err := NewToken()
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		if len(tok) < 40 {
			t.Errorf("token %q is %d characters; 32 random bytes is 43 in base64url", tok, len(tok))
		}
		if strings.ContainsAny(tok, "+/=") {
			t.Errorf("token %q is not base64url", tok)
		}
		if seen[tok] {
			t.Fatalf("NewToken returned %q twice", tok)
		}
		seen[tok] = true
	}
}

// ------------------------------------------- FIX 2, the suffix from control

// The suffix widens the split tunnel wherever it is used, and it comes from
// whichever coordination server the node is talking to.
func TestValidMagicDNSSuffix(t *testing.T) {
	good := []string{
		"tail4d5e6f.ts.net",
		"my-tailnet.example.com",
		"a.b",
		"tail4d5e6f.example",
		strings.Repeat("a", 63) + ".example.com",
	}
	for _, s := range good {
		if !validMagicDNSSuffix(s) {
			t.Errorf("validMagicDNSSuffix(%q) = false, want true", s)
		}
	}

	bad := []struct{ suffix, why string }{
		{"", "empty"},
		{"com", "a bare TLD would send every .com host through the tailnet"},
		{"example", "a single label"},
		{"ts.net", "the public parent of every tailnet, not one tailnet's domain"},
		{"My-Tailnet.Example.Com", "not lowercase"},
		{"bad_domain.example", "an underscore is not a DNS label"},
		{"-bad.example", "a label may not start with a dash"},
		{"bad-.example", "a label may not end with a dash"},
		{"a..b", "an empty label"},
		{"exa mple.com", "a space"},
		{"héllo.example", "not ASCII"},
		{strings.Repeat("a", 64) + ".example.com", "a label longer than 63"},
		{strings.Repeat("a.", 130) + "example", "longer than 253"},
	}
	for _, c := range bad {
		if validMagicDNSSuffix(c.suffix) {
			t.Errorf("validMagicDNSSuffix(%q) = true, want false (%s)", c.suffix, c.why)
		}
	}
}

func TestARefusedSuffixLeavesTheRuleAlone(t *testing.T) {
	var logged strings.Builder
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)

	p, err := start(func(context.Context, string, string) (net.Conn, error) {
		return nil, fmt.Errorf("no node")
	}, testToken)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.Close()

	p.SetMagicDNSSuffix("my-tailnet.example.com")
	if p.MagicDNSSuffix() != "my-tailnet.example.com" {
		t.Fatalf("a good suffix was not kept: %q", p.MagicDNSSuffix())
	}

	// Control now says something that would route the internet through the
	// tailnet. The suffix is dropped, taking the good one with it, and the
	// .ts.net and single-label rules are what is left.
	p.SetMagicDNSSuffix("com")
	if got := p.MagicDNSSuffix(); got != "" {
		t.Errorf("MagicDNSSuffix() = %q after a refused suffix, want empty", got)
	}
	if err := allowTailnetHost("github.com", p.MagicDNSSuffix(), nil); err == nil {
		t.Error("github.com is allowed after a suffix of com")
	}
	if err := allowTailnetHost("wiki.tail4d5e6f.ts.net", p.MagicDNSSuffix(), nil); err != nil {
		t.Errorf("a .ts.net name is refused after a bad suffix: %v", err)
	}

	// Once, not on every status refresh.
	for i := 0; i < 5; i++ {
		p.SetMagicDNSSuffix("com")
	}
	if n := strings.Count(logged.String(), "ignoring the MagicDNS suffix"); n != 1 {
		t.Errorf("the refusal was logged %d times, want 1:\n%s", n, logged.String())
	}
	// A different bad suffix is worth saying again.
	p.SetMagicDNSSuffix("net")
	if n := strings.Count(logged.String(), "ignoring the MagicDNS suffix"); n != 2 {
		t.Errorf("a second, different bad suffix was logged %d times in total, want 2", n)
	}
	// And a good one afterwards is accepted.
	p.SetMagicDNSSuffix(".my-tailnet.example.com.")
	if got := p.MagicDNSSuffix(); got != "my-tailnet.example.com" {
		t.Errorf("MagicDNSSuffix() = %q, want the trimmed good suffix", got)
	}
}

// ------------------------------------------ NIT 2, the handshake deadline

// A client that connects and goes quiet has not authenticated yet, and must not
// be able to hold a socket and a goroutine open indefinitely.
func TestAStalledSOCKSHandshakeIsClosed(t *testing.T) {
	old := socksHandshakeTimeout
	socksHandshakeTimeout = 250 * time.Millisecond
	defer func() { socksHandshakeTimeout = old }()

	p, err := start(func(context.Context, string, string) (net.Conn, error) {
		return nil, fmt.Errorf("no node")
	}, testToken)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.Close()

	conn, method := socks5Greet(t, p.Port(), socksPassword)
	if method != socksPassword {
		t.Fatalf("the server chose method %d, want password authentication", method)
	}
	// Never send the credential. The server should give up on its own.
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	if _, err := io.ReadFull(conn, make([]byte, 1)); err == nil {
		t.Fatal("the stalled handshake was answered rather than dropped")
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Errorf("the stalled handshake was still open after %v", waited)
	}
}

// The deadline is for the handshake, not the tunnel: a transfer that has
// authenticated must not be cut off part-way through.
func TestAnAuthenticatedTunnelOutlivesTheHandshakeDeadline(t *testing.T) {
	old := socksHandshakeTimeout
	socksHandshakeTimeout = 250 * time.Millisecond
	defer func() { socksHandshakeTimeout = old }()

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
	p, err := start(d.dial, testToken)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.Close()

	conn, method := socks5Greet(t, p.Port(), socksPassword)
	if method != socksPassword {
		t.Fatalf("the server chose method %d, want password authentication", method)
	}
	if status := socks5Auth(t, conn, User, testToken); status != 0 {
		t.Fatalf("authentication failed with status %d", status)
	}
	if code := socks5Request(t, conn, "wiki", 80); code != 0 {
		t.Fatalf("CONNECT reply code %d, want 0", code)
	}
	// The bound-address part of the reply, which socks5Request leaves unread:
	// one address type byte was consumed with the reply, so read the IPv4
	// address and port that follow.
	if _, err := io.ReadFull(conn, make([]byte, 6)); err != nil {
		t.Fatalf("reading the bound address: %v", err)
	}

	conn.SetDeadline(time.Now().Add(10 * time.Second))
	time.Sleep(3 * socksHandshakeTimeout)
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("writing after the handshake deadline would have fired: %v", err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("reading the echo after the handshake deadline would have fired: %v", err)
	}
	if string(got) != "ping" {
		t.Errorf("the tunnel echoed %q, want %q", got, "ping")
	}
}

// ------------------------------------------------------- X2, the exit mode

func loadExitCases(t *testing.T) []hostCase {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "exit-mode-hosts.json"))
	if err != nil {
		t.Fatalf("reading the exit-mode table: %v", err)
	}
	var table struct {
		Cases []hostCase `json:"cases"`
	}
	if err := json.Unmarshal(b, &table); err != nil {
		t.Fatalf("parsing the exit-mode table: %v", err)
	}
	if len(table.Cases) < 30 {
		t.Fatalf("the exit-mode table has only %d cases; it should not have shrunk", len(table.Cases))
	}
	return table.Cases
}

// With an exit node carrying the traffic the rule inverts: the public internet
// is the point, and what is refused is what would be dialled on the exit node's
// LAN rather than the user's.
func TestAllowExitHost(t *testing.T) {
	for _, c := range loadExitCases(t) {
		err := allowExitHost(c.Host, prefixes(t, c.Routes))
		if c.Proxy && err != nil {
			t.Errorf("allowExitHost(%q) = %v, want nil (%s)", c.Host, err, c.Why)
		}
		if !c.Proxy {
			if err == nil {
				t.Errorf("allowExitHost(%q) = nil, want a refusal (%s)", c.Host, c.Why)
			} else if !errors.Is(err, ErrNotTailnet) {
				t.Errorf("allowExitHost(%q) = %v, which does not wrap ErrNotTailnet", c.Host, err)
			}
		}
	}
}

// The two rules must differ in exactly the way the design says: exit mode is
// wider for public destinations and no wider for anything local.
func TestTheTwoModesDifferOnlyWhereIntended(t *testing.T) {
	for _, c := range loadExitCases(t) {
		exitErr := allowExitHost(c.Host, prefixes(t, c.Routes))
		tailnetErr := allowTailnetHost(c.Host, "", prefixes(t, c.Routes))
		if tailnetErr == nil && exitErr != nil {
			t.Errorf("%q is allowed in the tailnet rule but refused in exit mode (%s)", c.Host, c.Why)
		}
	}
	// And nothing private becomes reachable by turning exit mode on.
	for _, h := range []string{"127.0.0.1", "10.0.0.5", "192.168.1.1", "169.254.1.1", "fe80::1", "fd00::1", "localhost"} {
		if err := allowExitHost(h, nil); err == nil {
			t.Errorf("allowExitHost(%q) = nil; exit mode must not reach a LAN", h)
		}
	}
}

// The mode follows the status while the proxy is running: no restart, and the
// switch has to take effect on the very next request.
func TestTheGuardFollowsTheExitMode(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "reached")
	}))
	defer backend.Close()

	d := &recordingDialer{target: strings.TrimPrefix(backend.URL, "http://")}
	p, err := start(d.dial, testToken)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.Close()
	c := proxyClient(p)

	get := func(url string) int {
		t.Helper()
		resp, err := c.Get(url)
		if err != nil {
			t.Fatalf("GET %s through the proxy: %v", url, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// Selected but not active is the phase-1 rule: a public host is refused,
	// rather than dialled straight out of this machine while the browser
	// believes it is behind the exit node.
	if p.ExitActive() {
		t.Fatal("a fresh proxy is already in exit mode")
	}
	if code := get("http://github.com/"); code != http.StatusForbidden {
		t.Errorf("github.com with no exit node: status %d, want 403", code)
	}

	p.SetExitActive(true)
	if code := get("http://github.com/"); code != 200 {
		t.Errorf("github.com in exit mode: status %d, want 200", code)
	}
	if code := get("http://wiki/"); code != 200 {
		t.Errorf("a tailnet name in exit mode: status %d, want 200", code)
	}
	if code := get("http://192.168.1.1/"); code != http.StatusForbidden {
		t.Errorf("a LAN address in exit mode: status %d, want 403", code)
	}

	// And back again, without a restart.
	p.SetExitActive(false)
	if code := get("http://github.com/"); code != http.StatusForbidden {
		t.Errorf("github.com after the exit node went away: status %d, want 403", code)
	}
	if code := get("http://wiki/"); code != 200 {
		t.Errorf("a tailnet name after the exit node went away: status %d, want 200", code)
	}
}

// The SOCKS path reads the same mode, through the dialer guard.
func TestSOCKSFollowsTheExitMode(t *testing.T) {
	dialed := make(chan string, 4)
	p, err := start(func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed <- addr
		return nil, fmt.Errorf("no node")
	}, testToken)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.Close()

	if code := socks5Connect(t, p.Port(), "github.com", 443); code == 0 {
		t.Error("SOCKS5 reached github.com with no exit node")
	}
	select {
	case addr := <-dialed:
		t.Errorf("the dialer was asked for %q with no exit node", addr)
	default:
	}

	p.SetExitActive(true)
	if code := socks5Connect(t, p.Port(), "github.com", 443); code == 0 {
		t.Error("SOCKS5 CONNECT reported success with no node running")
	}
	select {
	case addr := <-dialed:
		if addr != "github.com:443" {
			t.Errorf("the dialer was asked for %q, want github.com:443", addr)
		}
	case <-time.After(5 * time.Second):
		t.Error("a public destination never reached the dialer in exit mode")
	}
}

// Authentication is unchanged by the mode: exit mode is about where traffic may
// go, not about who may send it.
func TestExitModeStillNeedsTheToken(t *testing.T) {
	p, err := start(func(context.Context, string, string) (net.Conn, error) {
		t.Error("an unauthenticated request reached the dialer in exit mode")
		return nil, nil
	}, testToken)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.Close()
	p.SetExitActive(true)

	if got := connectStatus(t, p.Port(), ""); !strings.HasPrefix(got, "HTTP/1.1 407") {
		t.Errorf("CONNECT with no credential in exit mode: %q, want 407", got)
	}
	if _, method := socks5Greet(t, p.Port(), socksNoAuth); method != socksNoAcceptable {
		t.Errorf("SOCKS5 accepted method %d in exit mode, want 0xff", method)
	}
}
