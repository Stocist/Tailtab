package node

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"tailscale.com/health"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsconst"
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

// unhealthy builds a health snapshot with the given warnable codes and texts.
func unhealthy(pairs ...string) *health.State {
	hs := &health.State{Warnings: map[health.WarnableCode]health.UnhealthyState{}}
	for i := 0; i+1 < len(pairs); i += 2 {
		code := health.WarnableCode(pairs[i])
		hs.Warnings[code] = health.UnhealthyState{WarnableCode: code, Text: pairs[i+1]}
	}
	return hs
}

func TestHealthWarningsReachTheStatus(t *testing.T) {
	n, _, _ := newTestNode(t)
	ctx := context.Background()

	n.apply(ctx, state(ipn.NeedsLogin))
	n.apply(ctx, ipn.Notify{Health: unhealthy(
		tsconst.HealthWarnableLoginState, "You are logged out. The last login error was: register request: all connection attempts failed",
		"not-in-map-poll", "Cannot reach the coordination server",
	)})

	st := n.Status()
	// Sorted by warnable code: login-state before not-in-map-poll.
	want := []string{
		"You are logged out. The last login error was: register request: all connection attempts failed",
		"Cannot reach the coordination server",
	}
	if !slices.Equal(st.Warnings, want) {
		t.Errorf("Warnings = %q, want %q", st.Warnings, want)
	}
	// This is the whole point: a node that is logged out because it cannot
	// reach control must not look like one merely waiting for the user.
	if !strings.Contains(st.Error, "all connection attempts failed") {
		t.Errorf("Error = %q, want the login-state warnable's text", st.Error)
	}
}

func TestHealthChangesCountAsAChange(t *testing.T) {
	n, _, seen := newTestNode(t)
	ctx := context.Background()

	n.apply(ctx, state(ipn.NeedsLogin))
	pushes := len(*seen)

	n.apply(ctx, ipn.Notify{Health: unhealthy("not-in-map-poll", "Cannot reach the coordination server")})
	if len(*seen) != pushes+1 {
		t.Fatalf("a new warning pushed %d events, want 1", len(*seen)-pushes)
	}
	// The same warning again changes nothing and must be suppressed.
	n.apply(ctx, ipn.Notify{Health: unhealthy("not-in-map-poll", "Cannot reach the coordination server")})
	if len(*seen) != pushes+1 {
		t.Errorf("an unchanged warning pushed another event")
	}
	// Recovering clears it, which is a change.
	n.apply(ctx, ipn.Notify{Health: &health.State{}})
	if len(*seen) != pushes+2 {
		t.Fatalf("clearing the warnings pushed %d events, want 1", len(*seen)-pushes-1)
	}
	if got := n.Status().Warnings; len(got) != 0 {
		t.Errorf("Warnings = %q after recovery, want none", got)
	}
}

func TestLoginWarningIsReappliedAndCleared(t *testing.T) {
	n, _, _ := newTestNode(t)
	ctx := context.Background()

	n.apply(ctx, ipn.Notify{Health: unhealthy(tsconst.HealthWarnableLoginState, "You are logged out.")})
	n.apply(ctx, state(ipn.NeedsLogin))
	if got := n.Status().Error; got != "You are logged out." {
		t.Errorf("Error = %q, want the login warning applied when the state arrives after it", got)
	}
	// Health recovers: the stale explanation must go with it.
	n.apply(ctx, ipn.Notify{Health: &health.State{}})
	n.apply(ctx, state(ipn.Starting))
	n.apply(ctx, state(ipn.NeedsLogin))
	if got := n.Status().Error; got != "" {
		t.Errorf("Error = %q after the warning cleared, want empty", got)
	}
}

func TestAnAuthURLIsNeverClearedByAStatusRefresh(t *testing.T) {
	// applyIPNStatus runs on every refresh. A snapshot that happens to carry no
	// AuthURL must not wipe the one the popup is showing.
	st := Status{State: "NeedsLogin", AuthURL: "https://login.tailscale.com/a/live"}
	applyIPNStatus(&st, &ipnstate.Status{BackendState: "NeedsLogin"})
	if st.AuthURL != "https://login.tailscale.com/a/live" {
		t.Errorf("AuthURL = %q, want the live URL kept", st.AuthURL)
	}
	// A snapshot that carries one while logged out may set it.
	empty := Status{State: "NeedsLogin"}
	applyIPNStatus(&empty, &ipnstate.Status{BackendState: "NeedsLogin", AuthURL: "https://login.tailscale.com/a/fresh"})
	if empty.AuthURL != "https://login.tailscale.com/a/fresh" {
		t.Errorf("AuthURL = %q, want the snapshot's URL", empty.AuthURL)
	}
	// Once Running the URL is spent and must not come back.
	running := Status{State: "Running"}
	applyIPNStatus(&running, &ipnstate.Status{BackendState: "Running", AuthURL: "https://login.tailscale.com/a/spent"})
	if running.AuthURL != "" {
		t.Errorf("AuthURL = %q while Running, want empty", running.AuthURL)
	}
}

func TestHealthDoesNotClearAnAuthURL(t *testing.T) {
	n, _, _ := newTestNode(t)
	ctx := context.Background()

	n.apply(ctx, state(ipn.NeedsLogin))
	url := "https://login.tailscale.com/a/deadbeef"
	n.apply(ctx, ipn.Notify{BrowseToURL: &url})
	n.apply(ctx, ipn.Notify{Health: unhealthy("not-in-map-poll", "Cannot reach the coordination server")})
	if got := n.Status().AuthURL; got != url {
		t.Errorf("AuthURL = %q after a health notification, want it untouched", got)
	}
}
