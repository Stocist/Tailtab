package node

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"

	"tailscale.com/health"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/tsconst"
	"tailscale.com/types/key"
	"tailscale.com/types/views"
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

// N3 (REVIEW.md). A tailnet rename, or a netmap landing after the node is
// already Running, changes this node's own tailcfg.Node without a state
// transition. Nothing else re-reads the status, so the popup — and the proxy's
// guard, which is handed the suffix from here — would keep the old values for
// the life of the process.
func TestSelfChangeRefreshesTheStatus(t *testing.T) {
	n, _, seen := newTestNode(t)
	ctx := context.Background()

	reads := 0
	n.readStatus = func(context.Context) (*ipnstate.Status, error) {
		reads++
		return &ipnstate.Status{
			BackendState:   ipn.Running.String(),
			CurrentTailnet: &ipnstate.TailnetStatus{Name: "display name", MagicDNSSuffix: "my-tailnet.example.com"},
			Self: &ipnstate.PeerStatus{
				DNSName:      "laptop-tailtab-zen.my-tailnet.example.com.",
				TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.9")},
			},
		}, nil
	}

	n.apply(ctx, ipn.Notify{SelfChange: &tailcfg.Node{StableID: "nodeid-1"}})
	if reads != 1 {
		t.Fatalf("SelfChange caused %d status reads, want 1", reads)
	}
	st := n.Status()
	if st.Tailnet != "my-tailnet.example.com" {
		t.Errorf("Tailnet = %q, want the suffix from the refreshed status", st.Tailnet)
	}
	if st.SelfIP != "100.64.0.9" {
		t.Errorf("SelfIP = %q, want 100.64.0.9", st.SelfIP)
	}
	if st.Hostname != "laptop-tailtab-zen" {
		t.Errorf("Hostname = %q, want laptop-tailtab-zen", st.Hostname)
	}
	// It has to reach the extension, not just the cached status.
	if len(*seen) == 0 {
		t.Fatal("the refreshed status was never pushed to the extension")
	}
	if last := (*seen)[len(*seen)-1]; last.Tailnet != "my-tailnet.example.com" {
		t.Errorf("the extension was pushed Tailnet %q", last.Tailnet)
	}

	// A notification that changes nothing must not push again.
	pushes := len(*seen)
	n.apply(ctx, ipn.Notify{SelfChange: &tailcfg.Node{StableID: "nodeid-1"}})
	if reads != 2 {
		t.Errorf("the second SelfChange caused %d status reads in total, want 2", reads)
	}
	if len(*seen) != pushes {
		t.Errorf("an unchanged status was pushed to the extension again")
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

// ---------------------------------------------------------------- exit nodes

func exitPeer(id, host string, online, offer bool) *ipnstate.PeerStatus {
	return &ipnstate.PeerStatus{
		ID:             tailcfg.StableNodeID(id),
		HostName:       host,
		DNSName:        host + ".tail1a2b3c.ts.net.",
		OS:             "linux",
		Online:         online,
		ExitNodeOption: offer,
	}
}

func TestExitNodeOffersReachTheStatus(t *testing.T) {
	var st Status
	applyIPNStatus(&st, &ipnstate.Status{
		BackendState: ipn.Running.String(),
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{
			key.NewNode().Public(): exitPeer("nodeid-server", "server", true, true),
			key.NewNode().Public(): exitPeer("nodeid-laptop", "laptop", true, false),
			key.NewNode().Public(): exitPeer("nodeid-attic", "attic", false, true),
		},
	})

	// Only the peers that offer, sorted by name so a map's iteration order
	// cannot make every refresh look like a change.
	want := []ExitNode{
		{ID: "nodeid-attic", Name: "attic", DNSName: "attic.tail1a2b3c.ts.net", Online: false, OS: "linux"},
		{ID: "nodeid-server", Name: "server", DNSName: "server.tail1a2b3c.ts.net", Online: true, OS: "linux"},
	}
	if !slices.Equal(st.ExitNodes, want) {
		t.Errorf("ExitNodes = %+v, want %+v", st.ExitNodes, want)
	}
	if st.ExitNodeActive {
		t.Error("ExitNodeActive is true with nothing selected")
	}
}

func TestExitNodeIsActiveOnlyWhenOnline(t *testing.T) {
	base := func(exit *ipnstate.ExitNodeStatus) *ipnstate.Status {
		return &ipnstate.Status{
			BackendState:   ipn.Running.String(),
			ExitNodeStatus: exit,
			Peer: map[key.NodePublic]*ipnstate.PeerStatus{
				key.NewNode().Public(): exitPeer("nodeid-server", "server", true, true),
			},
		}
	}
	var st Status
	applyIPNStatus(&st, base(&ipnstate.ExitNodeStatus{ID: "nodeid-server", Online: true}))
	if !st.ExitNodeActive {
		t.Error("ExitNodeActive is false for an online exit node in use")
	}
	applyIPNStatus(&st, base(&ipnstate.ExitNodeStatus{ID: "nodeid-server", Online: false}))
	if st.ExitNodeActive {
		t.Error("ExitNodeActive is true for an offline exit node; browsing must be blocked, not rerouted")
	}
	// Selected but gone from the netmap: ipnstate reports no exit node at all.
	applyIPNStatus(&st, base(nil))
	if st.ExitNodeActive {
		t.Error("ExitNodeActive is true with no exit node in the netmap")
	}
}

// The selection comes from the prefs, not from the status: a node that has left
// the netmap is still selected, and reading it from the status would make it
// look as though nothing was.
func TestExitNodeSelectionComesFromThePrefs(t *testing.T) {
	n, _, seen := newTestNode(t)
	ctx := context.Background()
	reads := 0
	n.readStatus = func(context.Context) (*ipnstate.Status, error) {
		reads++
		return &ipnstate.Status{
			BackendState: ipn.Running.String(),
			Peer: map[key.NodePublic]*ipnstate.PeerStatus{
				key.NewNode().Public(): exitPeer("nodeid-server", "server", true, true),
			},
		}, nil
	}

	prefs := (&ipn.Prefs{ExitNodeID: "nodeid-gone"}).View()
	n.apply(ctx, ipn.Notify{Prefs: &prefs})
	if got := n.Status().ExitNode; got != "nodeid-gone" {
		t.Errorf("ExitNode = %q, want the id from the prefs", got)
	}
	if n.Status().ExitNodeActive {
		t.Error("ExitNodeActive is true for a node that is not in the netmap")
	}
	// A prefs change has to refresh: whether that node is usable is only
	// visible in the status.
	if reads != 1 {
		t.Errorf("a prefs notification caused %d status reads, want 1", reads)
	}
	if len(*seen) == 0 {
		t.Fatal("the new selection never reached the extension")
	}

	none := (&ipn.Prefs{}).View()
	n.apply(ctx, ipn.Notify{Prefs: &none})
	if got := n.Status().ExitNode; got != "" {
		t.Errorf("ExitNode = %q after clearing the selection, want empty", got)
	}
}

func TestSetExitNodeRefusesAnUnknownID(t *testing.T) {
	n, _, _ := newTestNode(t)
	var edited []string
	n.editPrefs = func(_ context.Context, mp *ipn.MaskedPrefs) (*ipn.Prefs, error) {
		if !mp.ExitNodeIDSet {
			t.Error("EditPrefs was called without ExitNodeIDSet, so nothing would change")
		}
		edited = append(edited, string(mp.Prefs.ExitNodeID))
		return &ipn.Prefs{}, nil
	}
	n.st.ExitNodes = []ExitNode{{ID: "nodeid-server", Name: "server", Online: true}}

	if err := n.SetExitNode("nodeid-nope"); err == nil {
		t.Error("an unknown exit node id was accepted")
	}
	if len(edited) != 0 {
		t.Errorf("an unknown id reached EditPrefs: %q", edited)
	}
	if err := n.SetExitNode("nodeid-server"); err != nil {
		t.Errorf("a known exit node was refused: %v", err)
	}
	// Clearing needs no offer to match.
	if err := n.SetExitNode(""); err != nil {
		t.Errorf("clearing the exit node failed: %v", err)
	}
	if want := []string{"nodeid-server", ""}; !slices.Equal(edited, want) {
		t.Errorf("EditPrefs saw %q, want %q", edited, want)
	}
}

func TestExitNodeChangesCountAsAChange(t *testing.T) {
	a := Status{State: "Running", ExitNodes: []ExitNode{{ID: "n1", Name: "server", Online: true}}}
	b := a
	b.ExitNodes = []ExitNode{{ID: "n1", Name: "server", Online: false}}
	if a.equal(b) {
		t.Error("an exit node going offline compares equal, so the popup would never hear about it")
	}
	c := a
	c.ExitNode = "n1"
	if a.equal(c) {
		t.Error("selecting an exit node compares equal")
	}
	d := a
	d.ExitNodeActive = true
	if a.equal(d) {
		t.Error("an exit node becoming active compares equal")
	}
}

// Seen live: after Log out, the auth page offered to connect "Laptop" — the
// OS hostname — because lc.Logout resets the prefs and tsnet only applies the
// node's own at Start. The prefs go back before every login request.
func TestLoginRestoresTheHostnameAfterALogout(t *testing.T) {
	n, logins, _ := newTestNode(t)
	n.hostname = "mac-tailtab-edge"
	var order []string
	n.editPrefs = func(_ context.Context, mp *ipn.MaskedPrefs) (*ipn.Prefs, error) {
		order = append(order, "prefs")
		if !mp.HostnameSet || mp.Prefs.Hostname != "mac-tailtab-edge" {
			t.Errorf("hostname not restored: %+v", mp)
		}
		if !mp.WantRunningSet || !mp.Prefs.WantRunning {
			t.Errorf("WantRunning not restored: %+v", mp)
		}
		return &mp.Prefs, nil
	}
	login := n.startLogin
	n.startLogin = func(ctx context.Context) error {
		order = append(order, "login")
		return login(ctx)
	}
	if err := n.requestLogin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(order, ","), "prefs,login"; got != want {
		t.Fatalf("call order %q, want %q", got, want)
	}
	if *logins != 1 {
		t.Fatalf("logins = %d, want 1", *logins)
	}
}

func TestLoginWithoutAClientStillWorksInTests(t *testing.T) {
	// No editPrefs and no hostname, as the older tests set things up: the
	// login path must not need them.
	n, logins, _ := newTestNode(t)
	if err := n.requestLogin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *logins != 1 {
		t.Fatalf("logins = %d, want 1", *logins)
	}
}

func TestAccountsComeFromTheLoginProfiles(t *testing.T) {
	current := ipn.LoginProfile{ID: "p1", Name: "stocist@github", NetworkProfile: ipn.NetworkProfile{MagicDNSName: "tail1a2b3c.ts.net."},
		UserProfile: tailcfg.UserProfile{LoginName: "stocist@github", DisplayName: "Alice", ProfilePicURL: "https://example.com/a.png"}}
	all := []ipn.LoginProfile{
		{ID: "p2", Name: "bob@example.com", NetworkProfile: ipn.NetworkProfile{MagicDNSName: "tail4d5e6f.ts.net"}},
		current,
	}
	got := accountsFrom(current, all)
	want := []Account{
		{ID: "p2", Name: "bob@example.com", Tailnet: "tail4d5e6f.ts.net"},
		{ID: "p1", Name: "stocist@github", DisplayName: "Alice", Picture: "https://example.com/a.png", Tailnet: "tail1a2b3c.ts.net", Active: true},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("accounts = %+v, want %+v", got, want)
	}
	// A node that has never logged in: no profiles, and an empty current one
	// must not be marked active by an empty-ID match.
	if got := accountsFrom(ipn.LoginProfile{}, nil); len(got) != 0 {
		t.Fatalf("accounts before any login = %+v, want none", got)
	}
}

// PROFILES.md §3: a switch wipes the prefs and tsnet only sets its own at
// Start, so the node's hostname and WantRunning go back right after.
func TestSwitchAccountValidatesAndRestoresPrefs(t *testing.T) {
	n, _, _ := newTestNode(t)
	n.hostname = "mac-tailtab-edge"
	n.st.Accounts = []Account{{ID: "p1", Name: "a", Active: true}, {ID: "p2", Name: "b"}}
	var order []string
	n.switchProfile = func(_ context.Context, id ipn.ProfileID) error {
		order = append(order, "switch:"+string(id))
		return nil
	}
	n.editPrefs = func(_ context.Context, mp *ipn.MaskedPrefs) (*ipn.Prefs, error) {
		if mp.HostnameSet && mp.Prefs.Hostname == "mac-tailtab-edge" && mp.WantRunningSet && mp.Prefs.WantRunning {
			order = append(order, "prefs")
		} else {
			order = append(order, "prefs:wrong")
		}
		return &mp.Prefs, nil
	}
	if err := n.SwitchAccount("nope"); err == nil {
		t.Fatal("an unknown profile id was accepted")
	}
	if err := n.SwitchAccount(""); err == nil {
		t.Fatal("an empty profile id was accepted")
	}
	if len(order) != 0 {
		t.Fatalf("a refused switch still called the backend: %v", order)
	}
	if err := n.SwitchAccount("p2"); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(order, ","), "switch:p2,prefs"; got != want {
		t.Fatalf("call order %q, want %q", got, want)
	}
}

func TestAddAccountStartsAFreshProfileAndRestoresPrefs(t *testing.T) {
	n, _, _ := newTestNode(t)
	n.hostname = "mac-tailtab-edge"
	var order []string
	n.newProfile = func(context.Context) error {
		order = append(order, "new")
		return nil
	}
	n.editPrefs = func(_ context.Context, mp *ipn.MaskedPrefs) (*ipn.Prefs, error) {
		order = append(order, "prefs")
		return &mp.Prefs, nil
	}
	n.st.AuthURL = "https://login.tailscale.com/a/stale"
	n.st.Tailnet = "tail1a2b3c.ts.net"
	n.st.Hostname = "mac-tailtab-edge"
	n.st.Peers = []Peer{{Name: "server"}}
	if err := n.AddAccount(""); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(order, ","), "new,prefs"; got != want {
		t.Fatalf("call order %q, want %q", got, want)
	}
	st := n.Status()
	if st.AuthURL != "" || st.Tailnet != "" || st.Hostname != "" || len(st.Peers) != 0 {
		t.Fatalf("the old account's state survived into the new profile: %+v", st)
	}
}

func TestPeersComeFromTheStatus(t *testing.T) {
	st := &Status{}
	s := &ipnstate.Status{Peer: map[key.NodePublic]*ipnstate.PeerStatus{
		key.NewNode().Public(): {HostName: "PC", DNSName: "pc.tail1a2b3c.ts.net.", Online: false, OS: "windows", TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.80.1.94")}},
		key.NewNode().Public(): {HostName: "server", DNSName: "server.tail1a2b3c.ts.net.", Online: true, OS: "linux", TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.80.1.7")}},
	}}
	applyPeers(st, s)
	want := []Peer{
		{Name: "pc", DNSName: "pc.tail1a2b3c.ts.net", IP: "100.80.1.94", OS: "windows"},
		{Name: "server", DNSName: "server.tail1a2b3c.ts.net", IP: "100.80.1.7", Online: true, OS: "linux"},
	}
	if !slices.Equal(st.Peers, want) {
		t.Fatalf("peers = %+v, want %+v", st.Peers, want)
	}
}

func TestSubnetRoutesComeFromPrimaryRoutes(t *testing.T) {
	mk := func(cidrs ...string) *views.Slice[netip.Prefix] {
		var ps []netip.Prefix
		for _, c := range cidrs {
			ps = append(ps, netip.MustParsePrefix(c))
		}
		v := views.SliceOf(ps)
		return &v
	}
	st := &Status{}
	s := &ipnstate.Status{Peer: map[key.NodePublic]*ipnstate.PeerStatus{
		key.NewNode().Public(): {HostName: "router", PrimaryRoutes: mk("192.168.1.0/24", "10.0.0.0/8")},
		key.NewNode().Public(): {HostName: "exit", PrimaryRoutes: mk("0.0.0.0/0", "::/0")},
		key.NewNode().Public(): {HostName: "dup", PrimaryRoutes: mk("192.168.1.7/24", "fd00:1:2::/64", "100.64.0.0/10")},
		key.NewNode().Public(): {HostName: "plain"},
	}}
	applySubnetRoutes(st, s)
	want := []string{"10.0.0.0/8", "192.168.1.0/24", "fd00:1:2::/64"}
	if !slices.Equal(st.SubnetRoutes, want) {
		t.Fatalf("routes = %v, want %v (default routes, tailnet ranges and duplicates dropped, masked and sorted)", st.SubnetRoutes, want)
	}
}

func TestLoginRestoresRouteAcceptance(t *testing.T) {
	n, _, _ := newTestNode(t)
	n.hostname = "mac-tailtab-edge"
	var got *ipn.MaskedPrefs
	n.editPrefs = func(_ context.Context, mp *ipn.MaskedPrefs) (*ipn.Prefs, error) { got = mp; return &mp.Prefs, nil }
	if err := n.requestLogin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got == nil || !got.RouteAllSet || !got.Prefs.RouteAll {
		t.Fatalf("RouteAll not restored with the other prefs: %+v", got)
	}
}

func TestAddAccountSetsTheControlServerBeforeLogin(t *testing.T) {
	n, _, _ := newTestNode(t)
	n.hostname = "mac-tailtab-edge"
	var order []string
	n.newProfile = func(context.Context) error { order = append(order, "new"); return nil }
	n.editPrefs = func(_ context.Context, mp *ipn.MaskedPrefs) (*ipn.Prefs, error) {
		if mp.ControlURLSet {
			order = append(order, "control:"+mp.Prefs.ControlURL)
		} else {
			order = append(order, "prefs")
		}
		return &mp.Prefs, nil
	}
	if err := n.AddAccount("https://headscale.example.com"); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(order, ","), "new,control:https://headscale.example.com,prefs"; got != want {
		t.Fatalf("call order %q, want %q", got, want)
	}
	order = nil
	if err := n.AddAccount(""); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(order, ","), "new,prefs"; got != want {
		t.Fatalf("without a control URL: %q, want %q", got, want)
	}
}
