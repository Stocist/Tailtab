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
	"strings"
	"sync"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
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
}

// Node wraps a tsnet.Server for a single browser profile.
type Node struct {
	// onChange is called on every state change, off the IPN bus goroutine.
	onChange func(Status)

	cancel context.CancelFunc

	mu      sync.Mutex
	ts      *tsnet.Server
	lc      *local.Client
	started bool
	st      Status
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
	n.mu.Unlock()

	// NotifyInitialStatus is what carries the tailnet name and self IP:
	// Notify.NetMap is Windows-only as of v1.102.3 and is always nil here
	// (research/tsnet.md §3), so nothing in this package may read it.
	w, err := lc.WatchIPNBus(ctx, ipn.NotifyInitialState|ipn.NotifyInitialStatus|ipn.NotifyRateLimit)
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

	n.update(func(st *Status) {
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
		}
		if notify.ErrMessage != nil {
			st.Error = *notify.ErrMessage
		}
		wantLogin = st.State == ipn.NeedsLogin.String() && st.AuthURL == ""
	})

	// A state change can also change the tailnet name or the self IP (they are
	// only known once the node is up), so re-read the authoritative status.
	if notify.State != nil {
		n.refresh(ctx)
	}

	// NeedsLogin with no URL in hand means nobody has asked control for one:
	// this is the case on a first run and again after a logout.
	if wantLogin {
		if err := n.startLoginInteractive(ctx); err != nil {
			log.Printf("requesting a login URL: %v", err)
		}
	}
}

// refresh re-reads the node status from the local API.
func (n *Node) refresh(ctx context.Context) {
	n.mu.Lock()
	lc := n.lc
	n.mu.Unlock()
	if lc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s, err := lc.StatusWithoutPeers(ctx)
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
		if self.HostName != "" {
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

// update mutates the status under the lock and notifies the extension.
func (n *Node) update(f func(*Status)) {
	n.mu.Lock()
	f(&n.st)
	st := n.st
	n.mu.Unlock()
	n.onChange(st)
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
		return n.startLoginInteractive(ctx)
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
	n.update(func(st *Status) { st.AuthURL = "" })
	if err := lc.Logout(ctx); err != nil {
		return fmt.Errorf("logging out: %w", err)
	}
	return nil
}

// startLoginInteractive asks control for an auth URL. The URL arrives
// asynchronously on the bus as BrowseToURL.
func (n *Node) startLoginInteractive(ctx context.Context) error {
	lc, err := n.client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := lc.StartLoginInteractive(ctx); err != nil {
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
