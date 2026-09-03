// Package node runs one tsnet node for one browser profile and reports its
// state as it changes.
package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"os"
	"path/filepath"
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
	// ID is the stable node ID. It is what identifies an exit node across
	// restarts and address changes; never an IP or a hostname (G17).
	ID string
	// Name is the short hostname, e.g. "server", for the picker.
	Name string
	// DNSName is the full MagicDNS name, without the trailing dot.
	DNSName string
	// Online is whether the peer is reachable through the control plane. An
	// offline exit node is still listed, and still selectable, but browsing
	// through it is blocked rather than sent out some other way.
	Online bool
	OS     string
}

// Account is one Tailscale login profile held by this node. Several accounts
// live in one node's state; switching between them is a LocalAPI call, and
// each keeps its own node key on its own tailnet.
type Account struct {
	ID string
	// Name is the login name, e.g. "user@github"; DisplayName is the
	// human one the identity provider gave, e.g. "Alice"; Picture is the
	// avatar URL, if any.
	Name        string
	DisplayName string
	Picture     string
	Tailnet     string
	Active      bool
}

// Peer is one machine on the current tailnet, for the popup's search.
type Peer struct {
	Name    string
	DNSName string
	IP      string
	Online  bool
	OS      string
}

// Status is a snapshot of the node, shaped for the extension.
type Status struct {
	// State is an ipn.State string, passed through verbatim rather than
	// mapped, so a state this build does not know about still reaches the UI.
	State string
	// AuthURL is the login URL, set only while the node needs one.
	AuthURL string
	// Tailnet is the MagicDNS suffix, e.g. "tail4d5e6f.ts.net". The split
	// tunnel rules key off it, so it must be the suffix and not a display name.
	Tailnet  string
	Hostname string
	SelfIP   string
	Error    string
	// ExitNodes is every peer offering to be an exit node, sorted by name.
	ExitNodes []ExitNode
	// ExitNode is the stable ID of the selected exit node, or "" for none. It
	// comes from the prefs, which stay authoritative even when the peer has
	// left the netmap entirely.
	ExitNode string
	// ExitNodeActive is whether that exit node is present and online, which is
	// the only state in which traffic actually leaves through it. Selected but
	// not active means browsing is blocked, not rerouted (G15).
	ExitNodeActive bool
	// Warnings is the text of every unhealthy warnable the backend reports,
	// sorted by warnable code. This is where a node that cannot reach the
	// control plane says so: without it a blocked node looks identical to one
	// simply waiting for a login.
	Warnings []string
	// Accounts is every login profile in this node's state, sorted by name,
	// with the active one marked. Empty until a first login has completed.
	Accounts []Account
	// Peers is every machine on the current tailnet, sorted by name.
	Peers []Peer
	// SubnetRoutes is every subnet a peer currently routes for this tailnet
	// (its approved primary routes), as CIDRs, sorted. Addresses inside them
	// reach the tailnet through that peer, so the browser's rules and the
	// host's guard both treat them as tailnet destinations.
	SubnetRoutes []string
	// ControlURL is the coordination server the active account talks to,
	// from the prefs. Tailscale's own is reported too, so the popup can tell
	// a custom one apart.
	ControlURL string
}

// equal reports whether two snapshots are the same. Status holds a slice, so it
// cannot be compared with ==, and the change suppression in update depends on
// this being right: a missed difference is a status the extension never sees.
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
	// onChange is called on every state change, off the IPN bus goroutine.
	onChange func(Status)

	cancel context.CancelFunc

	mu sync.Mutex
	ts *tsnet.Server
	lc *local.Client
	// loginRequested records that control has already been asked for an auth
	// URL for the current NeedsLogin episode. Without it every notification in
	// that window starts another login session, each with its own URL, of
	// which the popup would only ever show the last.
	loginRequested bool
	// loginWarning is the text of the login-state warnable, kept so it can be
	// reapplied whenever the node is in NeedsLogin.
	loginWarning string
	// startLogin is lc.StartLoginInteractive once the node is up. It is a
	// field so tests can drive the login path without a control server.
	startLogin func(context.Context) error
	// readStatus is lc.Status once the node is up, for the same reason:
	// refresh has to be observable in a test with no local API. It is the
	// with-peers call, because the exit-node list is built from the peers.
	readStatus func(context.Context) (*ipnstate.Status, error)
	// editPrefs is lc.EditPrefs, a field for the same reason.
	editPrefs func(context.Context, *ipn.MaskedPrefs) (*ipn.Prefs, error)
	// readProfiles, switchProfile and newProfile are lc.ProfileStatus,
	// lc.SwitchProfile and lc.SwitchToEmptyProfile: the account switcher.
	readProfiles  func(context.Context) (ipn.LoginProfile, []ipn.LoginProfile, error)
	switchProfile func(context.Context, ipn.ProfileID) error
	newProfile    func(context.Context) error
	// hostname is the control-plane name this node was started with. tsnet
	// applies it once, at Start; a logout resets the prefs to their defaults,
	// so it has to be put back before the next login or the node registers
	// under the OS hostname (seen live: "Laptop" on the auth page instead of
	// "laptop-tailtab-edge").
	hostname string
	// dir is the node's state directory; tailtab keeps two small files of
	// its own there, "control-url" and "exit-node" (see Start).
	dir string
	// controlURL is the coordination server this browser profile's accounts
	// use. It is chosen at the first login and pinned in dir/control-url,
	// because tsnet replaces the active profile's prefs on every Start: the
	// same URL has to be passed every time or a Headscale account would be
	// repointed at Tailscale. Empty means Tailscale's.
	controlURL string
	// notes are warnings of tailtab's own (as opposed to Tailscale's health
	// warnables), appended to Status.Warnings.
	notes []string
	// exitRestored records which accounts have had their remembered exit
	// node put back this process, so it happens once per account per start.
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

// Files tailtab keeps in the node's state directory, beside tsnet's.
const (
	controlURLFile = "control-url"
	exitNodeFile   = "exit-node" // + "." + account ID
)

func exitNodeFileFor(account string) string { return exitNodeFile + "." + account }

func activeAccountID(accounts []Account) string {
	for _, a := range accounts {
		if a.Active {
			return a.ID
		}
	}
	return ""
}

func readStateFile(dir, name string) string {
	if dir == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeStateFile(dir, name, value string) error {
	if dir == "" {
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

// defaultPin is what the pin file holds for Tailscale's own server, so that a
// profile is pinned from its very first start whether or not a setting was
// given. Without an explicit pin a profile that joined Tailscale could be
// repointed by a setting made later.
const defaultPin = "default"

// hasAccounts reports whether tsnet's state in dir holds any login profile.
// It looks for the profile manager's key in tailscaled.state, which is a JSON
// object; before any login the key is absent.
func hasAccounts(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "tailscaled.state"))
	if err != nil {
		return false
	}
	return strings.Contains(string(b), `"_profiles"`)
}

// pinControlURL decides which coordination server this profile uses. The
// first start pins it, to the setting if there is one and to Tailscale's
// otherwise; later starts use the pin regardless of the setting, so an account
// can never be repointed. The one exception is a profile that has never
// logged in: it may still change its mind. It returns the effective URL ("" for
// Tailscale's) and a note for the popup when the setting and the pin disagree.
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

// StateDir returns the tsnet state directory for a profile. Each profile gets
// its own: tsnet does not lock its state directory, and two servers sharing one
// corrupt it silently (research/tsnet.md §7.1, tailscale#8287). The caller must
// have validated profileID as a UUID first.
func StateDir(profileID string) (string, error) {
	base, err := os.UserConfigDir() // ~/Library/Application Support on macOS
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

// sanitiseHostname turns a machine name into the DNS-safe label the node is
// named after.
func sanitiseHostname(h string) string {
	// The first label only: a machine on a tailnet reports its MagicDNS name
	// ("laptop.tail1a2b3c.ts.net") as its hostname, which would put the
	// tailnet's ID into this node's name on every tailnet it joins, and rename
	// it depending on whether the system client is up.
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

	// The coordination server is per browser profile and pinned on first
	// use: tsnet's Start replaces the active account's prefs wholesale, so
	// whatever URL is passed here becomes every account's. A setting that
	// differs from the pin is reported, not applied.
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
		Ephemeral:  false, // an ephemeral node needs an ephemeral auth key and is reaped
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

	// tsnet sets its own prefs at start and nothing else; accepting subnet
	// routes is ours to add, and it has to be on before the netmap arrives.
	// A failure here is not just logged: without RouteAll no subnet route
	// works for the whole session, and the popup should say so.
	if err := n.acceptRoutes(context.Background()); err != nil {
		log.Printf("accepting subnet routes: %v", err)
		n.mu.Lock()
		n.notes = append(n.notes, "Subnet routes are off: "+err.Error())
		n.mu.Unlock()
	}

	// NotifyInitialStatus is what carries the tailnet name and self IP:
	// Notify.NetMap is Windows-only as of v1.102.3 and is always nil here
	// (research/tsnet.md §3), so nothing in this package may read it.
	//
	// NotifyInitialHealthState gets the current health with the first message,
	// so a popup opened later sees warnings that were raised before it
	// connected. Runtime health changes need no opt-in bit: the backend sends
	// ipn.Notify{Health: ...} on every change (ipn/ipnlocal/local.go:1237).
	//
	// NotifyRateLimit is deliberately absent. At v1.102.3 it cannot be
	// combined with NotifyInitialStatus: ipn.ValidateNotifyWatchOpt rejects
	// the pair (ipn/backend.go, NotifyRateLimitIncompatibleBits) and the
	// LocalAPI answers 400 Bad Request. Losing the rate limit only means more
	// notifications, and update below drops the ones that change nothing.
	// NotifyInitialPrefs carries the current prefs with the first message and
	// the backend sends ipn.Notify{Prefs: ...} on every change after that, so
	// the selected exit node is known without polling GetPrefs. It is the
	// authoritative source for the selection: ipnstate.Status only reports an
	// exit node that is still in the netmap.
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
	// tailtab's own notes ride on every snapshot, not just the ones carrying a
	// health change, so they survive an account switch (which clears the
	// warnings) and a popup opened late.
	if len(n.notes) > 0 {
		st.Warnings = append(slices.Clone(st.Warnings), n.notes...)
	}
	return st
}

// watch consumes the IPN bus until the context is cancelled or the bus fails.
func (n *Node) watch(ctx context.Context, w *local.IPNBusWatcher) {
	defer w.Close()
	for {
		notify, err := w.Next()
		if err != nil {
			if ctx.Err() != nil {
				return // shutting down
			}
			log.Printf("IPN bus ended: %v", err)
			n.update(func(st *Status) { st.Error = "lost contact with the node: " + err.Error() })
			return
		}
		n.apply(ctx, notify)
	}
}

// apply folds one notification into the status and pushes it to the extension.
func (n *Node) apply(ctx context.Context, notify ipn.Notify) {
	var wantLogin bool

	changed := n.update(func(st *Status) {
		if s := notify.InitialStatus; s != nil {
			applyIPNStatus(st, s)
		}
		if notify.State != nil {
			st.State = notify.State.String()
			if *notify.State == ipn.Running {
				// The login URL is spent once we are up; leaving it set would
				// make the popup keep offering a stale "Log in" button.
				st.AuthURL = ""
			}
		}
		if notify.BrowseToURL != nil {
			st.AuthURL = *notify.BrowseToURL
			n.loginRequested = false // the request was answered
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
			// A node that is logged out because it could not reach control
			// looks exactly like one waiting for the user, unless the reason
			// is carried through. This is that reason.
			st.Error = n.loginWarning
		}
		if st.State != ipn.NeedsLogin.String() {
			n.loginRequested = false // a new episode may need a new URL
		}
		wantLogin = st.State == ipn.NeedsLogin.String() && st.AuthURL == "" && !n.loginRequested
	})

	// A state change can also change the tailnet name or the self IP (they are
	// only known once the node is up), so re-read the authoritative status.
	//
	// SelfChange is the same event without a state transition: the node's own
	// tailcfg.Node changed, which is how a tailnet rename, a new MagicDNS
	// suffix or a fresh address arrives once the node is already Running. The
	// netmap itself is nil here (G2), so the only way to see what changed is
	// to re-read the status (N3).
	// A prefs change is the third: choosing an exit node changes nothing in the
	// state machine, but it decides whether the exit node is active and so
	// whether the browser may leave the tailnet at all.
	if notify.State != nil || notify.SelfChange != nil || notify.Prefs != nil {
		n.refresh(ctx)
	}

	// NeedsLogin with no URL in hand means nobody has asked control for one:
	// this is the case on a first run and again after a logout. Notifications
	// that changed nothing are ignored, so a quiet stream of prefs and health
	// updates cannot turn into a stream of login sessions.
	if changed && wantLogin {
		if err := n.requestLogin(ctx); err != nil {
			log.Printf("requesting a login URL: %v", err)
		}
	}
}

// refresh re-reads the node status from the local API.
func (n *Node) refresh(ctx context.Context) {
	n.mu.Lock()
	read := n.readStatus
	n.mu.Unlock()
	if read == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s, err := read(ctx)
	if err != nil {
		log.Printf("reading node status: %v", err)
		return
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
	n.update(func(st *Status) {
		applyIPNStatus(st, s)
		if haveAccounts {
			st.Accounts = accounts
		}
		// The exit node remembered for this account, once: only if nothing is
		// selected now and the tailnet still offers it, so a node from another
		// account's tailnet can never be pushed into these prefs.
		if st.ExitNode == "" && st.State == ipn.Running.String() {
			if account := activeAccountID(st.Accounts); account != "" && !n.exitRestored[account] {
				if id := readStateFile(n.dir, exitNodeFileFor(account)); id != "" {
					// Only once it is actually on offer: a refresh before the
					// netmap arrives must not use up the one restore.
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
}

// accountsFrom shapes the login profiles for the popup. A profile only exists
// once a login has completed (PROFILES.md §2.1), so a node that has never
// logged in reports none.
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

// applyPeers lists the tailnet's machines from a status snapshot.
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

// applyIPNStatus copies the fields the extension needs out of an ipnstate
// snapshot. It never reads a netmap.
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
		// DNSName is the node's own MagicDNS name and is what the tailnet
		// calls this browser profile; HostName is the machine's OS hostname,
		// which is all that exists before login.
		// Before login the status only knows the OS hostname, which is not
		// this node's name; keep the name tailtab started with until the
		// tailnet has assigned one.
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

// applySubnetRoutes collects the subnets peers route for this tailnet. Only
// primary routes count: those are approved in the admin console and currently
// served. Default routes belong to exit nodes and are handled as exit mode, not
// as subnets.
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
				continue // the tailnet's own addresses are already covered
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

// reservedRanges are address ranges no subnet route may touch: this machine,
// link-local (including cloud metadata endpoints), multicast and the
// unspecified address. A route that overlaps any of them is dropped, so an
// approved-but-hostile route cannot make the proxy dial the user's own machine
// (an SSRF onto local services). extension/rules.js applies the same test.
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

// applyExitNodes copies the exit-node offers and the state of the selected one
// out of a status snapshot.
//
// The selection itself is not taken from here: ipnstate only reports an exit
// node that is still in the netmap, so a node that has been removed would read
// as "none selected" while the prefs still point at it. The prefs, which arrive
// on the bus, are the authority for that.
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
		nodes = append(nodes, ExitNode{
			ID:      string(p.ID),
			Name:    name,
			DNSName: dns,
			Online:  p.Online,
			OS:      p.OS,
		})
	}
	// s.Peer is a map, so without a sort the list would arrive in a different
	// order every refresh and every one would look like a change.
	slices.SortFunc(nodes, func(a, b ExitNode) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	st.ExitNodes = nodes
	// Present in the netmap and online: the one state in which traffic really
	// leaves through it.
	st.ExitNodeActive = s.ExitNodeStatus != nil && s.ExitNodeStatus.Online
}

// healthWarnings flattens a health snapshot into the text shown to the user,
// sorted by warnable code so an unchanged set of warnings compares equal. It
// also returns the login-state warnable's text, which is the one that explains
// why a login is failing.
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

// update mutates the status under the lock and notifies the extension, but
// only when something actually changed: the bus is unrate-limited, so most
// notifications leave the extension-visible status untouched. It reports
// whether the status changed.
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
		// A user pressing Connect is always allowed to ask again, even if the
		// bus already asked for this episode.
		return n.requestLogin(ctx)
	}
	return nil
}

// SetExitNode selects an exit node by stable ID, or clears the selection when
// id is empty. An id that is not one of the offers reported in the last status
// is refused: the browser is about to route all its traffic through whatever
// this names, so a typo or a stale id must not become "no exit node" silently.
//
// ExitNodeAllowLANAccess is deliberately not set. The browser never reaches the
// exit node's LAN: the proxy's guard refuses private address space in exit mode
// and the browser's own rules send it direct.
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
	// Remembered by tailtab, per account, because tsnet's Start resets the
	// prefs; the next refresh that finds the node on offer puts it back.
	n.mu.Lock()
	dir := n.dir
	account := activeAccountID(n.st.Accounts)
	n.mu.Unlock()
	if account != "" {
		if err := writeStateFile(dir, exitNodeFileFor(account), id); err != nil {
			log.Printf("remembering the exit node: %v", err)
		}
	}
	// The prefs notification that follows carries the new selection, and the
	// refresh it triggers says whether the node is actually usable.
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

// SwitchAccount makes another login profile the active one. The switch resets
// the prefs (PROFILES.md §3), so the node's own are put back straight after;
// with WantRunning restored a logged-in profile comes up on its own, and a
// profile that needs a login goes through the usual NeedsLogin path.
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

// clearAccountState drops everything in a status that belongs to the account
// being left: its tailnet, its node identity, its peers and exit nodes, its
// login URL and login error. Whatever the new profile has arrives from the bus.
func (n *Node) clearAccountState(st *Status) {
	st.AuthURL = ""
	st.Error = ""
	st.Tailnet = ""
	// Our own name, not the OS hostname: the tailnet's name for the node
	// arrives with the new account's status (F6).
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
	// One coordination server per browser profile (see the controlURL
	// field). A profile with no accounts yet may still choose; afterwards a
	// different server needs a different browser profile.
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
	// The new profile's prefs are the defaults, so the profile's coordination
	// server has to be set on it before the login is requested.
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
		n.loginRequested = false // the next NeedsLogin needs a fresh URL
	})
	if err := lc.Logout(ctx); err != nil {
		return fmt.Errorf("logging out: %w", err)
	}
	return nil
}

// requestLogin asks control for an auth URL, at most once per NeedsLogin
// episode from the bus. The URL arrives asynchronously as BrowseToURL.
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
	// A logout reset the prefs, and tsnet only sets ours at Start. Put the
	// hostname and WantRunning back first, so the login that follows registers
	// this node under its own name and comes up running.
	if err := n.reapplyPrefs(ctx); err != nil {
		n.mu.Lock()
		n.loginRequested = false
		n.mu.Unlock()
		return err
	}
	if err := login(ctx); err != nil {
		n.mu.Lock()
		n.loginRequested = false // it did not take; allow another attempt
		n.mu.Unlock()
		return fmt.Errorf("starting interactive login: %w", err)
	}
	return nil
}

// acceptRoutes turns on RouteAll, so subnets advertised by peers are routed.
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

// reapplyPrefs restores the prefs tsnet set at Start and a logout wiped: the
// hostname and WantRunning. It is what keeps a re-login from registering the
// node under the machine's own name.
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
		// RouteAll accepts the subnet routes peers advertise; without it the
		// node never learns the route and a LAN address behind a subnet
		// router is unreachable however the browser routes it.
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
