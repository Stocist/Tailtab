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

const testToken = "TESTTOKEN-not-a-real-secret"

var testAuthHeader = "Basic " + base64.StdEncoding.EncodeToString([]byte(User+":"+testToken))

func proxyClient(p *Server) *http.Client {
	u, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", p.Port()))
	if err != nil {
		panic(err)
	}
	u.User = url.UserPassword(User, testToken)
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}, Timeout: 10 * time.Second}
}

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

	// Exercise payload integrity when CONNECT parsing buffers tunnel bytes.
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
	if _, err := br.ReadString('\n'); err != nil {
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

// hostCase is shared by the browser and host routing-policy tests.
type hostCase struct {
	Host   string   `json:"host"`
	Suffix string   `json:"suffix"`
	Proxy  bool     `json:"proxy"`
	Why    string   `json:"why"`
	Routes []string `json:"routes"`
}

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

// Browser and host routing decisions must remain identical.
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

// Custom domains remain blocked until the node reports a trusted suffix.
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

	if code := socks5Connect(t, p.Port(), "host.my-tailnet.example.com", 80); code != 0 {
		t.Errorf("SOCKS5 CONNECT to the custom domain: reply code %d, want 0", code)
	}
	if code := socks5Connect(t, p.Port(), "other.example.com", 80); code == 0 {
		t.Error("SOCKS5 CONNECT to a domain outside the tailnet succeeded")
	}
}

func TestNonTailnetDestinationsAreRefused(t *testing.T) {
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

const (
	socksNoAuth       = byte(0)
	socksPassword     = byte(2)
	socksNoAcceptable = byte(0xff)
)

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

func socks5Request(t *testing.T, conn net.Conn, host string, port uint16) byte {
	t.Helper()
	req := []byte{5, 1, 0, 3, byte(len(host))}
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

// Loopback access must not grant another process the profile's tailnet identity.
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

	if got := connectStatus(t, p.Port(), testAuthHeader); strings.HasPrefix(got, "HTTP/1.1 407") {
		t.Errorf("CONNECT with the right credential: %q, want past the 407", got)
	}
	if !dialed {
		t.Error("an authenticated CONNECT never reached the tailnet dialer")
	}
}

// Proxy credentials must not cross the tailnet trust boundary.
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

	if _, method := socks5Greet(t, p.Port(), socksNoAuth); method != socksNoAcceptable {
		t.Errorf("the server accepted method %d for a no-auth client, want 0xff", method)
	}
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

func TestCredentialsAreComparedInConstantTime(t *testing.T) {
	src, err := os.ReadFile("proxy.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "subtle.ConstantTimeCompare") {
		t.Error("the credential comparison is not subtle.ConstantTimeCompare")
	}
	// A username short circuit would create a timing oracle.
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

// Control-provided suffixes widen routing and require strict validation.
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

	for i := 0; i < 5; i++ {
		p.SetMagicDNSSuffix("com")
	}
	if n := strings.Count(logged.String(), "ignoring the MagicDNS suffix"); n != 1 {
		t.Errorf("the refusal was logged %d times, want 1:\n%s", n, logged.String())
	}
	p.SetMagicDNSSuffix("net")
	if n := strings.Count(logged.String(), "ignoring the MagicDNS suffix"); n != 2 {
		t.Errorf("a second, different bad suffix was logged %d times in total, want 2", n)
	}
	p.SetMagicDNSSuffix(".my-tailnet.example.com.")
	if got := p.MagicDNSSuffix(); got != "my-tailnet.example.com" {
		t.Errorf("MagicDNSSuffix() = %q, want the trimmed good suffix", got)
	}
}

// Unauthenticated clients must not hold sockets indefinitely.
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
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	if _, err := io.ReadFull(conn, make([]byte, 1)); err == nil {
		t.Fatal("the stalled handshake was answered rather than dropped")
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Errorf("the stalled handshake was still open after %v", waited)
	}
}

// Handshake deadlines must not cut off authenticated tunnels.
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
	// Consume the remaining IPv4 bound address and port.
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

// Exit mode allows public traffic but not the exit node's private network.
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

// Exit mode must widen only public routing.
func TestTheTwoModesDifferOnlyWhereIntended(t *testing.T) {
	for _, c := range loadExitCases(t) {
		exitErr := allowExitHost(c.Host, prefixes(t, c.Routes))
		tailnetErr := allowTailnetHost(c.Host, "", prefixes(t, c.Routes))
		if tailnetErr == nil && exitErr != nil {
			t.Errorf("%q is allowed in the tailnet rule but refused in exit mode (%s)", c.Host, c.Why)
		}
	}
	for _, h := range []string{"127.0.0.1", "10.0.0.5", "192.168.1.1", "169.254.1.1", "fe80::1", "fd00::1", "localhost"} {
		if err := allowExitHost(h, nil); err == nil {
			t.Errorf("allowExitHost(%q) = nil; exit mode must not reach a LAN", h)
		}
	}
}

// Routing mode changes must apply atomically to the next request.
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

	// Selected but inactive must fail closed rather than dial locally.
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

	p.SetExitActive(false)
	if code := get("http://github.com/"); code != http.StatusForbidden {
		t.Errorf("github.com after the exit node went away: status %d, want 403", code)
	}
	if code := get("http://wiki/"); code != 200 {
		t.Errorf("a tailnet name after the exit node went away: status %d, want 200", code)
	}
}

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

// Exit mode must not weaken authentication.
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
