package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"

	"github.com/Stocist/tailtab/internal/nm"
	"github.com/Stocist/tailtab/internal/node"
	"github.com/Stocist/tailtab/internal/proxy"
)

// nodeBackend adapts a tsnet node and its loopback proxy to the host's backend
// interface.
type nodeBackend struct {
	h     *host
	node  *node.Node
	proxy *proxy.Server
}

func (b *nodeBackend) Init(profileID, browser string) error {
	if err := b.node.Start(profileID, browser); err != nil {
		return err
	}
	// The proxy comes up with the node, before login, so the extension can wire
	// the browser to a stable port once. Requests fail until the node runs.
	p, err := proxy.Start(b.node.TSNet())
	if err != nil {
		return err
	}
	b.proxy = p
	b.h.mu.Lock()
	b.h.proxyPort = p.Port()
	b.h.mu.Unlock()
	log.Printf("proxy listening on 127.0.0.1:%d", p.Port())
	return nil
}

func (b *nodeBackend) Status() *nm.Event {
	st := b.node.Status()
	ev := nm.StatusEvent()
	ev.State = st.State
	ev.AuthURL = st.AuthURL
	ev.Tailnet = st.Tailnet
	// The proxy's guard has to know this node's own MagicDNS suffix, which is
	// not under .ts.net when the tailnet uses a custom domain (R1). Status
	// carries the suffix, not the tailnet's display name: node.applyIPNStatus
	// reads ipnstate.Status.CurrentTailnet.MagicDNSSuffix and falls back to the
	// top-level MagicDNSSuffix. Pushing it here means a rename reaches the
	// guard as soon as it reaches the popup.
	b.proxy.SetMagicDNSSuffix(st.Tailnet)
	ev.Hostname = st.Hostname
	ev.SelfIP = st.SelfIP
	ev.Error = st.Error
	ev.Warnings = st.Warnings
	return ev
}

func (b *nodeBackend) SetWantRunning(up bool) error { return b.node.SetWantRunning(up) }
func (b *nodeBackend) Logout() error                { return b.node.Logout() }
func (b *nodeBackend) Close() error {
	err := b.proxy.Close()
	if nerr := b.node.Close(); err == nil {
		err = nerr
	}
	return err
}

// backend is the node half of the host, behind an interface so the message
// loop can be exercised without a tailnet.
type backend interface {
	// Init starts the node for one browser profile. It is called at most once.
	Init(profileID, browser string) error
	// Status returns the current state as a status event.
	Status() *nm.Event
	// SetWantRunning connects (true) or disconnects (false) the node.
	SetWantRunning(up bool) error
	// Logout drops the node's credentials.
	Logout() error
	// Close shuts the node down.
	Close() error
}

// host owns the native-messaging conversation with one browser profile.
type host struct {
	codec *nm.Codec

	mu        sync.Mutex
	be        backend
	initTried bool  // an init command has been accepted
	initDone  bool  // that init has been processed, either way
	initOK    bool  // that init succeeded, so the node exists
	fatal     error // set when the host cannot go on, e.g. the node would not start
	proxyPort int
}

// runHost runs the native-messaging loop until stdin closes, then exits.
func runHost() {
	h := &host{codec: nm.NewCodec(os.Stdin, os.Stdout)}
	h.be = &nodeBackend{h: h, node: node.New(func(node.Status) { h.pushStatus() })}

	err := h.loop()
	h.close()
	if err != nil && !errors.Is(err, io.EOF) {
		log.Printf("message loop ended: %v", err)
		os.Exit(1)
	}
	log.Printf("browser closed the port; exiting")
}

// loop reads and handles messages until the stream ends. Only a framing or I/O
// error stops it: a bad command or a backend failure is reported as an error
// event and the loop continues.
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
	case nm.CmdLogout:
		return h.withBackend(backend.Logout)
	case "":
		return errors.New("message has no cmd field")
	default:
		return fmt.Errorf("unknown command %q", req.Cmd)
	}
}

// withBackend runs f against the started backend, then pushes a status event so
// the extension sees the result either way.
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
		// A second init on one process would mean two nodes sharing a state
		// directory. The extension opens a fresh process per connection.
		h.sendStatus()
		return errors.New("already initialised; a host process serves one profile")
	}
	// Validate before anything can reach a filesystem path.
	if !nm.ValidProfileID(req.ProfileID) {
		h.mu.Unlock()
		return fmt.Errorf("profileID %q is not a lowercase UUID", req.ProfileID)
	}
	if req.Browser != "zen" && req.Browser != "edge" {
		h.mu.Unlock()
		return fmt.Errorf("browser %q is not %q or %q", req.Browser, "zen", "edge")
	}
	be := h.be
	h.initTried = true
	h.mu.Unlock()

	if be == nil {
		return errors.New("no backend configured")
	}
	// Whatever happens below, init has now been processed: the first status
	// event the extension sees is this one, and pushes from the node's bus
	// goroutine stop being suppressed (N4).
	defer func() {
		h.mu.Lock()
		h.initDone = true
		h.mu.Unlock()
		h.sendStatus()
	}()
	if err := be.Init(req.ProfileID, req.Browser); err != nil {
		// Without a node this process has nothing to offer. Report it and let
		// the loop exit non-zero; the extension reconnects with backoff.
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

// pushStatus is the node's onChange callback. It runs on the IPN bus
// goroutine, which starts inside Init and can therefore fire while init is
// still being handled. Until init has been processed the push is dropped: the
// extension's first status event has to be the reply to its init, not a
// spurious {"state":"NoState","proxyPort":0} racing it (N4).
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
