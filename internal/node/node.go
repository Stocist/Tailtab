// Package node runs one tsnet node for one browser profile and reports its
// state as it changes.
package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/health"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/tsconst"
	"tailscale.com/tsnet"
)

// ExitNode is a peer that offers to route this node's internet traffic.
type ExitNode struct {
	// ID is stable across restarts and address changes.
	ID      string
	Name    string
	DNSName string
	// Online gates exit routing; offline selections fail closed.
	Online bool
	OS     string
	// Country and City come from the control plane for location-aware exit
	// nodes such as Mullvad's, whose hostnames are only short codes.
	Country string
	City    string
}

// Account is a Tailscale login profile held by the node.
type Account struct {
	ID          string
	Name        string
	DisplayName string
	Picture     string
	Tailnet     string
	Active      bool
}

// Peer is a machine on the current tailnet.
type Peer struct {
	Name    string
	DNSName string
	IP      string
	Online  bool
	OS      string
}

// Status is a snapshot of the node, shaped for the extension.
type Status struct {
	// State passes ipn.State through so new states still reach the UI.
	State   string
	AuthURL string
	// Tailnet is the MagicDNS suffix used by split-tunnel rules.
	Tailnet  string
	Hostname string
	SelfIP   string
	Error    string
	// ExitNodes is sorted to keep snapshots stable.
	ExitNodes []ExitNode
	// ExitNode comes from preferences, which retain selections absent from netmap.
	ExitNode string
	// ExitNodeActive gates routing; selected but inactive fails closed.
	ExitNodeActive bool
	// Warnings are sorted by code so unchanged snapshots compare equal.
	Warnings []string
	Accounts []Account
	Peers    []Peer
	// SubnetRoutes contains sorted, approved peer routes used by both guards.
	SubnetRoutes []string
	// ControlURL comes from the active account's pinned preferences.
	ControlURL string
}

// equal gates status notifications; omitted fields would suppress real changes.
func (s Status) equal(o Status) bool {
	return s.State == o.State &&
		s.AuthURL == o.AuthURL &&
		s.Tailnet == o.Tailnet &&
		s.Hostname == o.Hostname &&
		s.SelfIP == o.SelfIP &&
		s.Error == o.Error &&
		s.ExitNode == o.ExitNode &&
		s.ExitNodeActive == o.ExitNodeActive &&
		slices.Equal(s.ExitNodes, o.ExitNodes) &&
		slices.Equal(s.Warnings, o.Warnings) &&
		slices.Equal(s.Accounts, o.Accounts) &&
		slices.Equal(s.Peers, o.Peers) &&
		slices.Equal(s.SubnetRoutes, o.SubnetRoutes) &&
		s.ControlURL == o.ControlURL
}

// Node wraps a tsnet.Server for a single browser profile.
type Node struct {
	// onChange runs outside the mutex and off the IPN bus goroutine.
	onChange func(Status)

	cancel context.CancelFunc

	mu sync.Mutex
	ts *tsnet.Server
	lc *local.Client
	// loginRequested prevents duplicate sessions during one NeedsLogin episode.
	loginRequested bool
	// loginWarning survives health and state notifications arriving out of order.
	loginWarning string
	// loginRefused keeps the refusal on the status until the episode ends.
	loginRefused bool
	// LocalAPI operations are fields so tests need no control server.
	startLogin    func(context.Context) error
	readStatus    func(context.Context) (*ipnstate.Status, error)
	editPrefs     func(context.Context, *ipn.MaskedPrefs) (*ipn.Prefs, error)
	readProfiles  func(context.Context) (ipn.LoginProfile, []ipn.LoginProfile, error)
	switchProfile func(context.Context, ipn.ProfileID) error
	newProfile    func(context.Context) error
	// Logout resets this hostname preference, so reapply it before login.
	hostname string
	dir      string
	// controlURL is pinned per browser profile to prevent account repointing.
	controlURL string
	notes      []string
	// exitRestored limits restoration to once per account and process.
	exitRestored map[string]bool
	started      bool
	st           Status
}

// New returns a Node that calls onChange whenever its status changes.
func New(onChange func(Status)) *Node {
	if onChange == nil {
		onChange = func(Status) {}
	}
	return &Node{onChange: onChange, st: Status{State: ipn.NoState.String()}}
}

const (
	controlURLFile = "control-url"
	exitNodeFile   = "exit-node"
)

var accountIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// exitNodeFileFor returns "" for an id that is not safe as a file name.
func exitNodeFileFor(account string) string {
	if !accountIDPattern.MatchString(account) {
		return ""
	}
	return exitNodeFile + "." + account
}

func activeAccountID(accounts []Account) string {
	for _, a := range accounts {
		if a.Active {
			return a.ID
		}
	}
	return ""
}

func readStateFile(dir, name string) string {
	if dir == "" || name == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeStateFile(dir, name, value string) error {
	if dir == "" || name == "" {
		return nil
	}
	p := filepath.Join(dir, name)
	if value == "" {
		err := os.Remove(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return os.WriteFile(p, []byte(value+"\n"), 0o600)
}

// defaultPin explicitly pins Tailscale's server before the first login.
const defaultPin = "default"

// hasAccounts detects whether changing an existing control-server pin is safe.
func hasAccounts(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "tailscaled.state"))
	if err != nil {
		return false
	}
	return strings.Contains(string(b), `"_profiles"`)
}

// pinControlURL prevents existing accounts from moving between coordination
// servers. A profile may change its pin only before its first login.
func pinControlURL(dir, setting string) (effective, note string, err error) {
	pinned := readStateFile(dir, controlURLFile)
	want := setting
	if want == "" {
		want = defaultPin
	}
	if pinned != "" && pinned != want && hasAccounts(dir) {
		shown := pinned
		if shown == defaultPin {
			shown = "Tailscale's coordination server"
		}
		if setting != "" {
			note = "This profile's accounts use " + shown + "; the coordination server in Settings applies only to a browser profile that has not logged in yet."
		}
		if pinned == defaultPin {
			return "", note, nil
		}
		return pinned, note, nil
	}
	if pinned != want {
		if err := writeStateFile(dir, controlURLFile, want); err != nil {
			return "", "", fmt.Errorf("remembering the coordination server: %w", err)
		}
	}
	if want == defaultPin {
		return "", "", nil
	}
	return want, "", nil
}

// StateDir returns a profile's tsnet state directory. tsnet does not lock state
// directories, so sharing one can silently corrupt state. profileID must be a
// validated UUID.
func StateDir(profileID string) (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locating the user config directory: %w", err)
	}
	return filepath.Join(base, "tailtab", profileID), nil
}

// HostnameFor returns the control-plane hostname for a browser profile, e.g.
// "laptop-tailtab-zen".
func HostnameFor(browser string) string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		h = "mac"
	}
	return sanitiseHostname(h) + "-tailtab-" + browser
}

func sanitiseHostname(h string) string {
	// Ignore MagicDNS suffixes so system-client state cannot rename this node.
	h, _, _ = strings.Cut(h, ".")
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '.' || r == ' ' || r == '_':
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		name = "mac"
	}
	return name
}

// Start brings up the node for profileID and begins watching the IPN bus. It
// does not wait for login: the caller learns about login through the status
// callback, which carries the auth URL.
func (n *Node) Start(profileID, browser, controlURL string) error {
	dir, err := StateDir(profileID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating the state directory: %w", err)
	}

	// A different setting is reported, not allowed to repoint existing accounts.
	effective, note, err := pinControlURL(dir, controlURL)
	if err != nil {
		return err
	}

	logf := func(format string, args ...any) { log.Printf("tsnet: "+format, args...) }
	ts := &tsnet.Server{
		Dir:        dir,
		Hostname:   HostnameFor(browser),
		Logf:       logf,
		UserLogf:   logf,
		Ephemeral:  false, // Ephemeral nodes require ephemeral auth keys and are reaped.
		ControlURL: effective,
	}

	n.mu.Lock()
	if n.started {
		n.mu.Unlock()
		return errors.New("node is already started")
	}
	n.ts = ts
	n.started = true
	n.hostname = ts.Hostname
	n.dir = dir
	n.controlURL = effective
	if note != "" {
		n.notes = append(n.notes, note)
	}
	n.st.Hostname = ts.Hostname
	n.mu.Unlock()

	if err := ts.Start(); err != nil {
		return fmt.Errorf("starting tsnet: %w", err)
	}
	lc, err := ts.LocalClient()
	if err != nil {
		return fmt.Errorf("getting the local client: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	n.mu.Lock()
	n.lc = lc
	n.cancel = cancel
	n.startLogin = lc.StartLoginInteractive
	n.readStatus = lc.Status
	n.editPrefs = lc.EditPrefs
	n.readProfiles = lc.ProfileStatus
	n.switchProfile = lc.SwitchProfile
	n.newProfile = lc.SwitchToEmptyProfile
	n.mu.Unlock()

	// Enable subnet routes before the netmap arrives and surface failure to the UI.
	if err := n.acceptRoutes(context.Background()); err != nil {
		log.Printf("accepting subnet routes: %v", err)
		n.mu.Lock()
		n.notes = append(n.notes, "Subnet routes are off: "+err.Error())
		n.mu.Unlock()
	}

	// InitialStatus replaces platform-specific NetMap notifications. Initial
	// health and prefs prevent missing pre-watch state; prefs remain authoritative
	// when a selected exit node leaves the netmap. NotifyRateLimit is omitted
	// because Tailscale rejects it when combined with NotifyInitialStatus.
	w, err := lc.WatchIPNBus(ctx, ipn.NotifyInitialState|ipn.NotifyInitialStatus|ipn.NotifyInitialHealthState|ipn.NotifyInitialPrefs)
	if err != nil {
		cancel()
		return fmt.Errorf("watching the IPN bus: %w", err)
	}
	go n.watch(ctx, w)
	return nil
}

// TSNet returns the underlying server, for the proxy's dialer. It is nil until
// Start has been called.
func (n *Node) TSNet() *tsnet.Server {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ts
}

// Status returns the last known status.
func (n *Node) Status() Status {
	n.mu.Lock()
	defer n.mu.Unlock()
	st := n.st
	// Include local notes on every snapshot so switches cannot discard them.
	if len(n.notes) > 0 {
		st.Warnings = append(slices.Clone(st.Warnings), n.notes...)
	}
	return st
}

func (n *Node) watch(ctx context.Context, w *local.IPNBusWatcher) {
	defer w.Close()
	for {
		notify, err := w.Next()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("IPN bus ended: %v", err)
			n.update(func(st *Status) { st.Error = "lost contact with the node: " + err.Error() })
			return
		}
		n.apply(ctx, notify)
	}
}

func (n *Node) apply(ctx context.Context, notify ipn.Notify) {
	changed := n.update(func(st *Status) {
		if s := notify.InitialStatus; s != nil {
			applyIPNStatus(st, s)
			n.vetAuthURL(st)
		}
		if notify.State != nil {
			st.State = notify.State.String()
			if *notify.State == ipn.Running {
				// A successful login invalidates its URL.
				st.AuthURL = ""
			}
		}
		if notify.BrowseToURL != nil {
			st.AuthURL = *notify.BrowseToURL
			// A refused link still answers the request; asking again would loop.
			if n.vetAuthURL(st) {
				n.loginRequested = false
			}
		}
		if notify.Prefs != nil && notify.Prefs.Valid() {
			st.ExitNode = string(notify.Prefs.ExitNodeID())
			st.ControlURL = notify.Prefs.ControlURL()
		}
		if notify.Health != nil {
			st.Warnings, n.loginWarning = healthWarnings(notify.Health)
		}
		if notify.ErrMessage != nil {
			st.Error = *notify.ErrMessage
		} else if st.State == ipn.NeedsLogin.String() {
			// Distinguish control failure from an ordinary logged-out state.
			st.Error = n.loginWarning
			if n.loginRefused {
				st.Error = refusedLoginError
			}
		}
		if st.State != ipn.NeedsLogin.String() {
			n.loginRequested = false
			n.loginRefused = false
		}
	})

	// These notifications can change routing fields without a state transition;
	// refresh from the authoritative LocalAPI status.
	if notify.State != nil || notify.SelfChange != nil || notify.Prefs != nil {
		if n.refresh(ctx) {
			changed = true
		}
	}

	// Checked after the refresh: a new profile's prefs arrive before its state,
	// so the refresh is what first reports NeedsLogin.
	if changed && n.wantLogin() {
		if err := n.requestLogin(ctx); err != nil {
			log.Printf("requesting a login URL: %v", err)
		}
	}
}

// wantLogin reports whether a logged-out node still needs a login URL.
func (n *Node) wantLogin() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.st.State == ipn.NeedsLogin.String() && n.st.AuthURL == "" && !n.loginRequested
}

// refresh reports whether the status changed.
func (n *Node) refresh(ctx context.Context) bool {
	n.mu.Lock()
	read := n.readStatus
	n.mu.Unlock()
	if read == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s, err := read(ctx)
	if err != nil {
		log.Printf("reading node status: %v", err)
		return false
	}
	n.mu.Lock()
	profiles := n.readProfiles
	n.mu.Unlock()
	var accounts []Account
	haveAccounts := false
	if profiles != nil {
		current, all, perr := profiles(ctx)
		if perr != nil {
			log.Printf("reading login profiles: %v", perr)
		} else {
			accounts = accountsFrom(current, all)
			haveAccounts = true
		}
	}
	var restore string
	changed := n.update(func(st *Status) {
		applyIPNStatus(st, s)
		n.vetAuthURL(st)
		if haveAccounts {
			st.Accounts = accounts
		}
		// Restore only an offer from this account's current tailnet.
		if st.ExitNode == "" && st.State == ipn.Running.String() {
			if account := activeAccountID(st.Accounts); account != "" && !n.exitRestored[account] {
				if id := readStateFile(n.dir, exitNodeFileFor(account)); id != "" {
					// Do not consume the one restore before the netmap arrives.
					if slices.ContainsFunc(st.ExitNodes, func(e ExitNode) bool { return e.ID == id }) {
						restore = id
						if n.exitRestored == nil {
							n.exitRestored = map[string]bool{}
						}
						n.exitRestored[account] = true
					}
				}
			}
		}
	})
	if restore != "" {
		if err := n.setExitNodePref(ctx, restore); err != nil {
			log.Printf("restoring the exit node %q: %v", restore, err)
		}
	}
	return changed
}

func accountsFrom(current ipn.LoginProfile, all []ipn.LoginProfile) []Account {
	accounts := make([]Account, 0, len(all))
	for _, p := range all {
		accounts = append(accounts, Account{
			ID:          string(p.ID),
			Name:        p.Name,
			DisplayName: p.UserProfile.DisplayName,
			Picture:     p.UserProfile.ProfilePicURL,
			Tailnet:     strings.TrimSuffix(p.NetworkProfile.MagicDNSName, "."),
			Active:      p.ID != "" && p.ID == current.ID,
		})
	}
	slices.SortFunc(accounts, func(a, b Account) int { return strings.Compare(a.Name, b.Name) })
	return accounts
}

func applyPeers(st *Status, s *ipnstate.Status) {
	peers := make([]Peer, 0, len(s.Peer))
	for _, p := range s.Peer {
		if p == nil {
			continue
		}
		dns := strings.TrimSuffix(p.DNSName, ".")
		name, _, _ := strings.Cut(dns, ".")
		if name == "" {
			name = p.HostName
		}
		peer := Peer{Name: name, DNSName: dns, Online: p.Online, OS: p.OS}
		if len(p.TailscaleIPs) > 0 {
			peer.IP = p.TailscaleIPs[0].String()
		}
		peers = append(peers, peer)
	}
	slices.SortFunc(peers, func(a, b Peer) int { return strings.Compare(a.Name, b.Name) })
	st.Peers = peers
}

// applyIPNStatus avoids platform-specific NetMap notifications.
func applyIPNStatus(st *Status, s *ipnstate.Status) {
	if s.BackendState != "" {
		st.State = s.BackendState
	}
	if t := s.CurrentTailnet; t != nil && t.MagicDNSSuffix != "" {
		st.Tailnet = t.MagicDNSSuffix
	} else if s.MagicDNSSuffix != "" {
		st.Tailnet = s.MagicDNSSuffix
	}
	if s.AuthURL != "" && s.BackendState != ipn.Running.String() {
		st.AuthURL = s.AuthURL
	}
	if self := s.Self; self != nil {
		// Keep tailtab's name until the tailnet assigns its MagicDNS name; the
		// pre-login HostName is the machine's OS hostname.
		if name, _, ok := strings.Cut(strings.TrimSuffix(self.DNSName, "."), "."); ok && name != "" {
			st.Hostname = name
		} else if st.Hostname == "" && self.HostName != "" {
			st.Hostname = self.HostName
		}
		if len(self.TailscaleIPs) > 0 {
			st.SelfIP = self.TailscaleIPs[0].String()
		}
	}
	if len(s.TailscaleIPs) > 0 && st.SelfIP == "" {
		st.SelfIP = s.TailscaleIPs[0].String()
	}
	applyExitNodes(st, s)
	applyPeers(st, s)
	applySubnetRoutes(st, s)
}

// applySubnetRoutes accepts only approved primary routes; default routes belong
// to exit mode.
func applySubnetRoutes(st *Status, s *ipnstate.Status) {
	seen := map[string]bool{}
	var routes []string
	for _, p := range s.Peer {
		if p == nil || p.PrimaryRoutes == nil {
			continue
		}
		for _, r := range p.PrimaryRoutes.AsSlice() {
			if !UsableRoute(r) {
				continue
			}
			r = r.Masked()
			if tailscaleCGNAT.Contains(r.Addr()) || tailscaleULA.Contains(r.Addr()) {
				continue
			}
			k := r.String()
			if !seen[k] {
				seen[k] = true
				routes = append(routes, k)
			}
		}
	}
	slices.Sort(routes)
	st.SubnetRoutes = routes
}

var (
	tailscaleCGNAT = netip.MustParsePrefix("100.64.0.0/10")
	tailscaleULA   = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// Reserved ranges are excluded before routes to prevent SSRF to local services.
var reservedRanges = []netip.Prefix{
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

// UsableRoute reports whether a subnet route is one tailtab will honour: no
// broader than /8 (IPv4) or /16 (IPv6), and not overlapping a reserved range.
// Default routes belong to exit nodes and are handled as exit mode instead.
func UsableRoute(r netip.Prefix) bool {
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
	for _, res := range reservedRanges {
		if r.Overlaps(res) {
			return false
		}
	}
	return true
}

// Preferences remain authoritative because status omits selected nodes that
// have left the netmap.
func applyExitNodes(st *Status, s *ipnstate.Status) {
	nodes := make([]ExitNode, 0, len(s.Peer))
	for _, p := range s.Peer {
		if p == nil || !p.ExitNodeOption {
			continue
		}
		name := p.HostName
		dns := strings.TrimSuffix(p.DNSName, ".")
		if name == "" {
			name, _, _ = strings.Cut(dns, ".")
		}
		if name == "" {
			name = string(p.ID)
		}
		node := ExitNode{
			ID:      string(p.ID),
			Name:    name,
			DNSName: dns,
			Online:  p.Online,
			OS:      p.OS,
		}
		if p.Location != nil {
			node.Country = p.Location.Country
			node.City = p.Location.City
		}
		nodes = append(nodes, node)
	}
	// Stabilize map iteration so unchanged snapshots compare equal. Nodes
	// without a location are the tailnet's own machines and sort first.
	slices.SortFunc(nodes, func(a, b ExitNode) int {
		if c := strings.Compare(a.Country, b.Country); c != 0 {
			return c
		}
		if c := strings.Compare(a.City, b.City); c != 0 {
			return c
		}
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	st.ExitNodes = nodes
	// Traffic leaves only through a present, online exit node.
	st.ExitNodeActive = s.ExitNodeStatus != nil && s.ExitNodeStatus.Online
}

// healthWarnings sorts output for stable comparisons and preserves the login
// failure reason separately.
func healthWarnings(hs *health.State) (warnings []string, loginWarning string) {
	codes := make([]string, 0, len(hs.Warnings))
	for code := range hs.Warnings {
		codes = append(codes, string(code))
	}
	slices.Sort(codes)
	for _, code := range codes {
		w := hs.Warnings[health.WarnableCode(code)]
		text := w.Text
		if text == "" {
			text = w.Title
		}
		if text == "" {
			continue
		}
		warnings = append(warnings, text)
		if code == tsconst.HealthWarnableLoginState {
			loginWarning = text
		}
	}
	return warnings, loginWarning
}

// update notifies outside the lock and suppresses unchanged bus updates.
func (n *Node) update(f func(*Status)) bool {
	n.mu.Lock()
	before := n.st
	f(&n.st)
	st := n.st
	n.mu.Unlock()
	if st.equal(before) {
		return false
	}
	n.onChange(st)
	return true
}

// SetWantRunning connects or disconnects the node. Connecting also asks control
// for a login URL if the node has no credentials yet.
func (n *Node) SetWantRunning(up bool) error {
	lc, err := n.client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := lc.EditPrefs(ctx, &ipn.MaskedPrefs{
		Prefs:          ipn.Prefs{WantRunning: up},
		WantRunningSet: true,
	}); err != nil {
		return fmt.Errorf("setting WantRunning=%v: %w", up, err)
	}
	if up && n.Status().State == ipn.NeedsLogin.String() {
		// An explicit Connect may retry within the current login episode.
		return n.requestLogin(ctx)
	}
	return nil
}

// SetExitNode selects a reported stable ID or clears the selection. Unknown IDs
// are refused rather than silently disabling exit routing.
//
// Exit-node LAN access stays disabled; private destinations fail closed.
func (n *Node) SetExitNode(id string) error {
	n.mu.Lock()
	edit := n.editPrefs
	known := slices.ContainsFunc(n.st.ExitNodes, func(e ExitNode) bool { return e.ID == id })
	n.mu.Unlock()
	if edit == nil {
		return errors.New("node is not started")
	}
	if id != "" && !known {
		return fmt.Errorf("%q is not an exit node this tailnet offers", id)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := n.setExitNodePref(ctx, id); err != nil {
		return err
	}
	// Persist per account because tsnet resets preferences at startup.
	n.mu.Lock()
	dir := n.dir
	account := activeAccountID(n.st.Accounts)
	n.mu.Unlock()
	if account != "" {
		if err := writeStateFile(dir, exitNodeFileFor(account), id); err != nil {
			log.Printf("remembering the exit node: %v", err)
		}
	}
	return nil
}

func (n *Node) setExitNodePref(ctx context.Context, id string) error {
	n.mu.Lock()
	edit := n.editPrefs
	n.mu.Unlock()
	if edit == nil {
		return errors.New("node is not started")
	}
	if _, err := edit(ctx, &ipn.MaskedPrefs{
		Prefs:         ipn.Prefs{ExitNodeID: tailcfg.StableNodeID(id)},
		ExitNodeIDSet: true,
	}); err != nil {
		return fmt.Errorf("selecting the exit node: %w", err)
	}
	return nil
}

// SwitchAccount activates a held profile and reapplies preferences reset by the
// switch.
func (n *Node) SwitchAccount(id string) error {
	n.mu.Lock()
	sw := n.switchProfile
	known := slices.ContainsFunc(n.st.Accounts, func(a Account) bool { return a.ID == id })
	n.mu.Unlock()
	if sw == nil {
		return errors.New("node is not started")
	}
	if id == "" || !known {
		return fmt.Errorf("%q is not an account this node holds", id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n.update(func(st *Status) {
		n.clearAccountState(st)
		n.loginRequested = false
	})
	if err := sw(ctx, ipn.ProfileID(id)); err != nil {
		return fmt.Errorf("switching account: %w", err)
	}
	return n.reapplyPrefs(ctx)
}

// clearAccountState prevents old-account data surviving until new bus state.
func (n *Node) clearAccountState(st *Status) {
	n.loginRefused = false
	st.AuthURL = ""
	st.Error = ""
	st.Tailnet = ""
	// Keep tailtab's name rather than leaking the OS hostname between accounts.
	st.Hostname = n.hostname
	st.SelfIP = ""
	st.Peers = nil
	st.ExitNodes = nil
	st.ExitNode = ""
	st.ExitNodeActive = false
	st.Warnings = nil
	st.SubnetRoutes = nil
	st.ControlURL = ""
}

// AddAccount starts a fresh login profile alongside the existing ones. The
// node lands in NeedsLogin, and the login URL follows from the bus as usual.
func (n *Node) AddAccount(controlURL string) error {
	n.mu.Lock()
	add := n.newProfile
	edit := n.editPrefs
	pinned := n.controlURL
	accounts := len(n.st.Accounts)
	dir := n.dir
	n.mu.Unlock()
	if add == nil {
		return errors.New("node is not started")
	}
	// Existing accounts pin the browser profile to one coordination server.
	if controlURL != "" && controlURL != pinned {
		if accounts > 0 {
			shown := pinned
			if shown == "" {
				shown = "Tailscale's coordination server"
			}
			return fmt.Errorf("this browser profile's accounts use %s; a different coordination server needs a new browser profile", shown)
		}
		if err := writeStateFile(dir, controlURLFile, controlURL); err != nil {
			return fmt.Errorf("remembering the coordination server: %w", err)
		}
		n.mu.Lock()
		n.controlURL = controlURL
		n.mu.Unlock()
		pinned = controlURL
	}
	controlURL = pinned
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n.update(func(st *Status) {
		n.clearAccountState(st)
		n.loginRequested = false
	})
	if err := add(ctx); err != nil {
		return fmt.Errorf("adding an account: %w", err)
	}
	// Set the pinned server before login can contact a control plane.
	if controlURL != "" && edit != nil {
		if _, err := edit(ctx, &ipn.MaskedPrefs{Prefs: ipn.Prefs{ControlURL: controlURL}, ControlURLSet: true}); err != nil {
			return fmt.Errorf("setting the control server for the new account: %w", err)
		}
	}
	return n.reapplyPrefs(ctx)
}

// Logout drops the node's credentials. The bus then reports NeedsLogin, which
// makes apply request a fresh login URL.
func (n *Node) Logout() error {
	lc, err := n.client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n.update(func(st *Status) {
		st.AuthURL = ""
		n.loginRequested = false
	})
	if err := lc.Logout(ctx); err != nil {
		return fmt.Errorf("logging out: %w", err)
	}
	return nil
}

const refusedLoginError = "The coordination server sent a login link for another site, which tailtab will not open."

// loginURLAllowed reports whether control may send the browser to u: https on
// the pinned server's host, or on Tailscale's login host for the default.
func loginURLAllowed(u, controlURL string) bool {
	p, err := url.Parse(u)
	if err != nil || p.Scheme != "https" || p.User != nil || p.Hostname() == "" {
		return false
	}
	host := strings.ToLower(p.Hostname())
	if controlURL == "" {
		return host == "login.tailscale.com" || host == "controlplane.tailscale.com"
	}
	c, err := url.Parse(controlURL)
	return err == nil && host == strings.ToLower(c.Hostname())
}

// vetAuthURL drops a login URL that is not on the coordination server and
// reports whether one was kept. Called with n.mu held.
func (n *Node) vetAuthURL(st *Status) bool {
	if st.AuthURL == "" {
		return false
	}
	if loginURLAllowed(st.AuthURL, n.controlURL) {
		n.loginRefused = false
		return true
	}
	log.Printf("refusing a login URL off the coordination server: %q", st.AuthURL)
	st.AuthURL = ""
	st.Error = refusedLoginError
	n.loginRefused = true
	return false
}

// requestLogin permits one in-flight request per NeedsLogin episode.
func (n *Node) requestLogin(ctx context.Context) error {
	n.mu.Lock()
	login := n.startLogin
	if login == nil {
		n.mu.Unlock()
		return errors.New("node is not started")
	}
	n.loginRequested = true
	n.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Reapply reset preferences before login registers the node.
	if err := n.reapplyPrefs(ctx); err != nil {
		n.mu.Lock()
		n.loginRequested = false
		n.mu.Unlock()
		return err
	}
	if err := login(ctx); err != nil {
		n.mu.Lock()
		n.loginRequested = false
		n.mu.Unlock()
		return fmt.Errorf("starting interactive login: %w", err)
	}
	return nil
}

func (n *Node) acceptRoutes(ctx context.Context) error {
	n.mu.Lock()
	edit := n.editPrefs
	n.mu.Unlock()
	if edit == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := edit(ctx, &ipn.MaskedPrefs{Prefs: ipn.Prefs{RouteAll: true}, RouteAllSet: true})
	return err
}

// reapplyPrefs restores values reset by logout or account switching.
func (n *Node) reapplyPrefs(ctx context.Context) error {
	n.mu.Lock()
	edit := n.editPrefs
	hostname := n.hostname
	controlURL := n.controlURL
	n.mu.Unlock()
	if edit == nil || hostname == "" {
		return nil
	}
	mp := &ipn.MaskedPrefs{
		Prefs:          ipn.Prefs{Hostname: hostname, WantRunning: true, RouteAll: true},
		HostnameSet:    true,
		WantRunningSet: true,
		RouteAllSet:    true,
	}
	if controlURL != "" {
		mp.Prefs.ControlURL = controlURL
		mp.ControlURLSet = true
	}
	if _, err := edit(ctx, mp); err != nil {
		return fmt.Errorf("restoring the node's prefs before login: %w", err)
	}
	return nil
}

func (n *Node) client() (*local.Client, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.lc == nil {
		return nil, errors.New("node is not started")
	}
	return n.lc, nil
}

// Close stops the node and the bus watcher.
func (n *Node) Close() error {
	n.mu.Lock()
	ts, cancel := n.ts, n.cancel
	n.ts, n.lc, n.cancel = nil, nil, nil
	n.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if ts == nil {
		return nil
	}
	return ts.Close()
}
