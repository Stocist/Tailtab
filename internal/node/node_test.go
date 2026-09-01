package node

import (
	"context"
	"strings"
	"sync"
	"testing"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
)

// newTestNode returns a node with the login call stubbed, so the login path can
// be driven without a control server, and a counter of login requests.
func newTestNode(t *testing.T) (*Node, *int, *[]Status) {
	t.Helper()
	var mu sync.Mutex
	logins := 0
	var seen []Status
	n := New(func(st Status) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, st)
	})
	n.startLogin = func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		logins++
		return nil
	}
	return n, &logins, &seen
}

func state(s ipn.State) ipn.Notify { return ipn.Notify{State: &s} }

func TestLoginIsRequestedOncePerEpisode(t *testing.T) {
	n, logins, _ := newTestNode(t)
	ctx := context.Background()

	n.apply(ctx, state(ipn.NeedsLogin))
	if *logins != 1 {
		t.Fatalf("NeedsLogin asked for %d login URLs, want 1", *logins)
	}

	// The bus is not rate limited, so the same state arrives again and again
	// alongside prefs and health notifications. None of them changes the
	// status, and none may start another login session.
	for i := 0; i < 5; i++ {
		n.apply(ctx, state(ipn.NeedsLogin))
		n.apply(ctx, ipn.Notify{})
	}
	if *logins != 1 {
		t.Errorf("repeated notifications asked for %d login URLs, want 1", *logins)
	}

	url := "https://login.tailscale.com/a/deadbeef"
	n.apply(ctx, ipn.Notify{BrowseToURL: &url})
	if got := n.Status().AuthURL; got != url {
		t.Errorf("AuthURL = %q, want %q", got, url)
	}
	n.apply(ctx, state(ipn.NeedsLogin))
	if *logins != 1 {
		t.Errorf("after the URL arrived, %d login URLs were requested, want 1", *logins)
	}
}

func TestLoginIsRequestedAgainForANewEpisode(t *testing.T) {
	n, logins, _ := newTestNode(t)
	ctx := context.Background()

	n.apply(ctx, state(ipn.NeedsLogin))
	url := "https://login.tailscale.com/a/first"
	n.apply(ctx, ipn.Notify{BrowseToURL: &url})

	// Logging in clears the URL; a later logout puts the node back to
	// NeedsLogin, and that episode needs a URL of its own.
	n.apply(ctx, state(ipn.Running))
	if got := n.Status().AuthURL; got != "" {
		t.Errorf("AuthURL = %q after Running, want it cleared", got)
	}
	n.apply(ctx, state(ipn.NeedsLogin))
	if *logins != 2 {
		t.Errorf("the second episode asked for %d login URLs in total, want 2", *logins)
	}
}

func TestFailedLoginRequestIsRetried(t *testing.T) {
	n, _, _ := newTestNode(t)
	calls := 0
	n.startLogin = func(context.Context) error {
		calls++
		if calls == 1 {
			return context.DeadlineExceeded
		}
		return nil
	}
	ctx := context.Background()

	n.apply(ctx, state(ipn.NeedsLogin))
	// The first attempt failed, so the guard must not be latched: the next
	// notification that changes something tries again.
	n.apply(ctx, state(ipn.Starting))
	n.apply(ctx, state(ipn.NeedsLogin))
	if calls != 2 {
		t.Errorf("login was attempted %d times, want 2", calls)
	}
}

func TestStatusIsPushedOnlyWhenItChanges(t *testing.T) {
	n, _, seen := newTestNode(t)
	ctx := context.Background()

	n.apply(ctx, state(ipn.NeedsLogin))
	n.apply(ctx, state(ipn.NeedsLogin))
	n.apply(ctx, ipn.Notify{})
	if len(*seen) != 1 {
		t.Errorf("pushed %d status events, want 1", len(*seen))
	}
	if (*seen)[0].State != "NeedsLogin" {
		t.Errorf("state = %q, want NeedsLogin", (*seen)[0].State)
	}
}

func TestApplyIPNStatusUsesTheMagicDNSName(t *testing.T) {
	var st Status
	applyIPNStatus(&st, &ipnstate.Status{
		BackendState:   ipn.Running.String(),
		MagicDNSSuffix: "legacy.ts.net",
		CurrentTailnet: &ipnstate.TailnetStatus{Name: "display name", MagicDNSSuffix: "tail4d5e6f.ts.net"},
		Self: &ipnstate.PeerStatus{
			HostName: "Laptop.local",
			DNSName:  "laptop-tailtab-zen.tail4d5e6f.ts.net.",
		},
	})
	// The suffix, not the display name: the split-tunnel rules match on it.
	if st.Tailnet != "tail4d5e6f.ts.net" {
		t.Errorf("Tailnet = %q, want the current tailnet's MagicDNS suffix", st.Tailnet)
	}
	// The node's own name, not the machine's OS hostname.
	if st.Hostname != "laptop-tailtab-zen" {
		t.Errorf("Hostname = %q, want laptop-tailtab-zen", st.Hostname)
	}
	if st.State != "Running" {
		t.Errorf("State = %q, want Running", st.State)
	}
}

func TestHostnameForIsSanitised(t *testing.T) {
	got := HostnameFor("zen")
	if !strings.HasSuffix(got, "-tailtab-zen") {
		t.Errorf("HostnameFor(zen) = %q, want a -tailtab-zen suffix", got)
	}
	for _, r := range got {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' {
			t.Errorf("HostnameFor(zen) = %q contains %q, which is not safe for a machine name", got, r)
		}
	}
}

func TestStateDirIsPerProfile(t *testing.T) {
	a, err := StateDir("0f8fad5b-d9cb-469f-a165-70867728950e")
	if err != nil {
		t.Fatal(err)
	}
	b, err := StateDir("11111111-2222-4333-8444-555555555555")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two profiles resolved to one state directory; tsnet does not lock it")
	}
	if !strings.HasSuffix(a, "tailtab/0f8fad5b-d9cb-469f-a165-70867728950e") {
		t.Errorf("StateDir = %q, want it under tailtab/<profile>", a)
	}
}
