package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/netip"
	"os"
	"regexp"
	"sync"

	"github.com/Stocist/tailtab/internal/nm"
	"github.com/Stocist/tailtab/internal/node"
	"github.com/Stocist/tailtab/internal/proxy"
)

type nodeBackend struct {
	h     *host
	node  *node.Node
	proxy *proxy.Server
}

func (b *nodeBackend) Init(profileID, browser, controlURL string) error {
	if err := b.node.Start(profileID, browser, controlURL); err != nil {
		return err
	}
	// Bind before login so the browser can use one stable port.
	p, err := proxy.Start(b.node.TSNet(), b.h.token())
	if err != nil {
		return err
	}
	b.h.mu.Lock()
	b.proxy = p
	b.h.proxyPort = p.Port()
	b.h.mu.Unlock()
	// The bus may report routing state before the proxy is assigned.
	st := b.node.Status()
	p.SetMagicDNSSuffix(st.Tailnet)
	p.SetExitActive(st.ExitNodeActive)
	log.Printf("proxy listening on 127.0.0.1:%d", p.Port())
	return nil
}

// statusChanged updates guard state before notifying the extension. Keeping
// this out of Status avoids mutating routing as a read side effect.
func (b *nodeBackend) statusChanged(st node.Status) {
	b.h.mu.Lock()
	p := b.proxy
	b.h.mu.Unlock()
	// SetMagicDNSSuffix validates the control-provided suffix; nil is safe here.
	p.SetMagicDNSSuffix(st.Tailnet)
	// Widen only for an active exit node; selected but offline fails closed.
	p.SetExitActive(st.ExitNodeActive)
	p.SetSubnetRoutes(parseRoutes(st.SubnetRoutes))
	b.h.pushStatus()
}

// Browser names cross into a control-plane DNS label and must stay bounded.
var browserNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,15}$`)

// Invalid routes must not widen the proxy guard.
func parseRoutes(cidrs []string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		if p, err := netip.ParsePrefix(c); err == nil {
			out = append(out, p.Masked())
		}
	}
	return out
}

func (b *nodeBackend) Status() *nm.Event {
	st := b.node.Status()
	ev := nm.StatusEvent()
	ev.State = st.State
	ev.AuthURL = st.AuthURL
	ev.Tailnet = st.Tailnet
	ev.Hostname = st.Hostname
	ev.SelfIP = st.SelfIP
	ev.Error = st.Error
	ev.Warnings = st.Warnings
	ev.ExitNode = st.ExitNode
	ev.ExitNodeActive = st.ExitNodeActive
	for _, n := range st.ExitNodes {
		ev.ExitNodes = append(ev.ExitNodes, nm.ExitNode{
			ID:      n.ID,
			Name:    n.Name,
			DNSName: n.DNSName,
			Online:  n.Online,
			OS:      n.OS,
			Country: n.Country,
			City:    n.City,
		})
	}
	for _, a := range st.Accounts {
		ev.Accounts = append(ev.Accounts, nm.Account{ID: a.ID, Name: a.Name, DisplayName: a.DisplayName, Picture: a.Picture, Tailnet: a.Tailnet, Active: a.Active})
	}
	for _, p := range st.Peers {
		ev.Peers = append(ev.Peers, nm.Peer{Name: p.Name, DNSName: p.DNSName, IP: p.IP, Online: p.Online, OS: p.OS})
	}
	ev.SubnetRoutes = st.SubnetRoutes
	ev.ControlURL = st.ControlURL
	return ev
}

func (b *nodeBackend) SetWantRunning(up bool) error  { return b.node.SetWantRunning(up) }
func (b *nodeBackend) SetExitNode(id string) error   { return b.node.SetExitNode(id) }
func (b *nodeBackend) Logout() error                 { return b.node.Logout() }
func (b *nodeBackend) SwitchAccount(id string) error { return b.node.SwitchAccount(id) }
func (b *nodeBackend) AddAccount(controlURL string) error {
	return b.node.AddAccount(controlURL)
}
func (b *nodeBackend) Close() error {
	err := b.proxy.Close()
	if nerr := b.node.Close(); err == nil {
		err = nerr
	}
	return err
}

// backend isolates the message loop from tsnet for tests.
type backend interface {
	Init(profileID, browser, controlURL string) error
	Status() *nm.Event
	SetWantRunning(up bool) error
	SetExitNode(id string) error
	Logout() error
	SwitchAccount(id string) error
	AddAccount(controlURL string) error
	Close() error
}

type host struct {
	codec *nm.Codec

	mu        sync.Mutex
	be        backend
	initTried bool
	initDone  bool
	initOK    bool
	fatal     error
	proxyPort int
	// proxyToken is sent only in status events and must never be logged.
	proxyToken string
}

func (h *host) token() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.proxyToken
}

func runHost() {
	// Generate the credential before listening so only this extension session
	// can borrow the profile's tailnet identity.
	token, err := proxy.NewToken()
	if err != nil {
		log.Printf("%v", err)
		os.Exit(1)
	}
	h := &host{codec: nm.NewCodec(os.Stdin, os.Stdout), proxyToken: token}
	be := &nodeBackend{h: h}
	be.node = node.New(be.statusChanged)
	h.be = be

	err = h.loop()
	h.close()
	if err != nil && !errors.Is(err, io.EOF) {
		log.Printf("message loop ended: %v", err)
		os.Exit(1)
	}
	log.Printf("browser closed the port; exiting")
}

// loop keeps processing command errors; framing and I/O errors are terminal.
func (h *host) loop() error {
	for {
		req, err := h.codec.Read()
		if err != nil {
			var bad *nm.BadJSONError
			if errors.As(err, &bad) {
				log.Printf("%v", bad)
				h.sendError(bad)
				continue
			}
			return err
		}
		if err := h.handle(req); err != nil {
			log.Printf("command %q: %v", req.Cmd, err)
			h.sendError(err)
		}
		h.mu.Lock()
		fatal := h.fatal
		h.mu.Unlock()
		if fatal != nil {
			return fatal
		}
	}
}

func (h *host) handle(req *nm.Request) error {
	switch req.Cmd {
	case nm.CmdInit:
		return h.handleInit(req)
	case nm.CmdStatus:
		h.sendStatus()
		return nil
	case nm.CmdUp:
		return h.withBackend(func(be backend) error { return be.SetWantRunning(true) })
	case nm.CmdDown:
		return h.withBackend(func(be backend) error { return be.SetWantRunning(false) })
	case nm.CmdExitNode:
		id := req.ID
		return h.withBackend(func(be backend) error { return be.SetExitNode(id) })
	case nm.CmdLogout:
		return h.withBackend(backend.Logout)
	case nm.CmdSwitch:
		id := req.ID
		return h.withBackend(func(be backend) error { return be.SwitchAccount(id) })
	case nm.CmdAddAccount:
		if err := nm.ValidControlURL(req.ControlURL); err != nil {
			return err
		}
		controlURL := req.ControlURL
		return h.withBackend(func(be backend) error { return be.AddAccount(controlURL) })
	case "":
		return errors.New("message has no cmd field")
	default:
		return fmt.Errorf("unknown command %q", req.Cmd)
	}
}

// withBackend pushes the resulting status even when the backend call fails.
func (h *host) withBackend(f func(backend) error) error {
	h.mu.Lock()
	be, ok := h.be, h.initOK
	h.mu.Unlock()
	if !ok || be == nil {
		return errors.New("node is not started; send init first")
	}
	defer h.sendStatus()
	return f(be)
}

func (h *host) handleInit(req *nm.Request) error {
	h.mu.Lock()
	if h.initTried {
		h.mu.Unlock()
		// A second init could make two nodes share and corrupt one state directory.
		h.sendStatus()
		return errors.New("already initialised; a host process serves one profile")
	}
	// Validate browser input before deriving filesystem or DNS names.
	if !nm.ValidProfileID(req.ProfileID) {
		h.mu.Unlock()
		return fmt.Errorf("profileID %q is not a lowercase UUID", req.ProfileID)
	}
	if !browserNameRE.MatchString(req.Browser) {
		h.mu.Unlock()
		return fmt.Errorf("browser %q is not a short lowercase name", req.Browser)
	}
	if err := nm.ValidControlURL(req.ControlURL); err != nil {
		h.mu.Unlock()
		return err
	}
	be := h.be
	h.initTried = true
	h.mu.Unlock()

	if be == nil {
		return errors.New("no backend configured")
	}
	// Mark init done before its reply so later bus pushes cannot race ahead.
	defer func() {
		h.mu.Lock()
		h.initDone = true
		h.mu.Unlock()
		h.sendStatus()
	}()
	if err := be.Init(req.ProfileID, req.Browser, req.ControlURL); err != nil {
		// A host without a node must fail rather than remain unusable.
		h.mu.Lock()
		h.fatal = err
		h.mu.Unlock()
		return err
	}
	h.mu.Lock()
	h.initOK = true
	h.mu.Unlock()
	return nil
}

// pushStatus drops bus callbacks that race Init, keeping the init reply first.
func (h *host) pushStatus() {
	h.mu.Lock()
	ready := h.initDone
	h.mu.Unlock()
	if !ready {
		return
	}
	h.sendStatus()
}

func (h *host) sendStatus() {
	h.mu.Lock()
	be, ok := h.be, h.initOK
	h.mu.Unlock()

	ev := nm.StatusEvent()
	if ok && be != nil {
		ev = be.Status()
	} else {
		ev.State = "NoState"
	}
	h.mu.Lock()
	ev.ProxyPort = h.proxyPort
	ev.ProxyToken = h.proxyToken
	h.mu.Unlock()
	if err := h.codec.Write(ev); err != nil {
		log.Printf("writing status event: %v", err)
	}
}

func (h *host) sendError(err error) {
	if werr := h.codec.Write(nm.ErrorEvent(err)); werr != nil {
		log.Printf("writing error event: %v", werr)
	}
}

func (h *host) close() {
	h.mu.Lock()
	be, ok := h.be, h.initTried
	h.mu.Unlock()
	if ok && be != nil {
		if err := be.Close(); err != nil {
			log.Printf("shutting down node: %v", err)
		}
	}
}
