// Package node runs one tsnet node for one browser profile and reports its
// state as it changes.
package node

import (
	"context"
	"errors"
	"fmt"
	"log"
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
	"tailscale.com/tsconst"
	"tailscale.com/tsnet"
)

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
	// Warnings is the text of every unhealthy warnable the backend reports,
	// sorted by warnable code. This is where a node that cannot reach the
	// control plane says so: without it a blocked node looks identical to one
	// simply waiting for a login.
	Warnings []string
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
		slices.Equal(s.Warnings, o.Warnings)
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
	// readStatus is lc.StatusWithoutPeers once the node is up, for the same
	// reason: refresh has to be observable in a test with no local API.
	readStatus func(context.Context) (*ipnstate.Status, error)
	started    bool
	st         Status
}

// New returns a Node that calls onChange whenever its status changes.
func New(onChange func(Status)) *Node {
	if onChange == nil {
		onChange = func(Status) {}
	}
	return &Node{onChange: onChange, st: Status{State: ipn.NoState.String()}}
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
	h = strings.TrimSuffix(h, ".local")
	h = strings.TrimSuffix(h, ".lan")
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
	return name + "-tailtab-" + browser
}

// Start brings up the node for profileID and begins watching the IPN bus. It
// does not wait for login: the caller learns about login through the status
// callback, which carries the auth URL.
func (n *Node) Start(profileID, browser string) error {
	dir, err := StateDir(profileID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating the state directory: %w", err)
	}

	logf := func(format string, args ...any) { log.Printf("tsnet: "+format, args...) }
	ts := &tsnet.Server{
		Dir:       dir,
		Hostname:  HostnameFor(browser),
		Logf:      logf,
		UserLogf:  logf,
		Ephemeral: false, // an ephemeral node needs an ephemeral auth key and is reaped
	}

	n.mu.Lock()
	if n.started {
		n.mu.Unlock()
		return errors.New("node is already started")
	}
	n.ts = ts
	n.started = true
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
	n.readStatus = lc.StatusWithoutPeers
	n.mu.Unlock()

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
	w, err := lc.WatchIPNBus(ctx, ipn.NotifyInitialState|ipn.NotifyInitialStatus|ipn.NotifyInitialHealthState)
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
	return n.st
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
	if notify.State != nil || notify.SelfChange != nil {
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
	n.update(func(st *Status) { applyIPNStatus(st, s) })
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
		if name, _, ok := strings.Cut(strings.TrimSuffix(self.DNSName, "."), "."); ok && name != "" {
			st.Hostname = name
		} else if self.HostName != "" {
			st.Hostname = self.HostName
		}
		if len(self.TailscaleIPs) > 0 {
			st.SelfIP = self.TailscaleIPs[0].String()
		}
	}
	if len(s.TailscaleIPs) > 0 && st.SelfIP == "" {
		st.SelfIP = s.TailscaleIPs[0].String()
	}
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
	if err := login(ctx); err != nil {
		n.mu.Lock()
		n.loginRequested = false // it did not take; allow another attempt
		n.mu.Unlock()
		return fmt.Errorf("starting interactive login: %w", err)
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
