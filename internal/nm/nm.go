// Package nm implements the browser native-messaging wire protocol: a 4-byte
// little-endian length prefix followed by a JSON body, in both directions.
//
// Portions of the framing logic in this file are adapted from
// github.com/tailscale/ts-browser-ext, which is:
//
//	Copyright (c) Tailscale Inc & AUTHORS
//	SPDX-License-Identifier: BSD-3-Clause
package nm

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sync"
)

// MaxMsgSize is Chromium's 1 MiB native-messaging limit.
const MaxMsgSize = 1 << 20

// Command is an extension -> host command name.
type Command string

const (
	CmdInit   Command = "init"
	CmdStatus Command = "status"
	CmdUp     Command = "up"
	CmdDown   Command = "down"
	CmdLogout Command = "logout"
	// CmdExitNode selects an exit node by stable ID, or clears it for an empty ID.
	CmdExitNode Command = "exitnode"
	// CmdSwitch activates a login profile by ID.
	CmdSwitch Command = "switch"
	// CmdAddAccount starts another login profile.
	CmdAddAccount Command = "addaccount"
)

// Request is an untrusted message from the browser extension.
type Request struct {
	Cmd Command `json:"cmd"`

	// ProfileID must pass ValidProfileID before becoming a path component.
	ProfileID string `json:"profileID,omitempty"`

	// Browser is validated before becoming part of a node hostname.
	Browser string `json:"browser,omitempty"`

	// ControlURL must pass ValidControlURL before entering node preferences.
	ControlURL string `json:"controlURL,omitempty"`

	// ID must match an exit node or account reported by the node.
	ID string `json:"id,omitempty"`
}

// Account is a Tailscale login profile held by the node.
type Account struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	Picture     string `json:"picture,omitempty"`
	Tailnet     string `json:"tailnet,omitempty"`
	Active      bool   `json:"active"`
}

// Peer is a machine on the current tailnet.
type Peer struct {
	Name    string `json:"name"`
	DNSName string `json:"dnsName,omitempty"`
	IP      string `json:"ip,omitempty"`
	Online  bool   `json:"online"`
	OS      string `json:"os,omitempty"`
}

// ExitNode is an advertised exit node.
type ExitNode struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	DNSName string `json:"dnsName,omitempty"`
	Online  bool   `json:"online"`
	OS      string `json:"os,omitempty"`
}

// Event is a status or error message sent to the browser extension.
type Event struct {
	Event string `json:"event"`

	// State passes ipn.State through verbatim so new states reach the UI.
	State     string `json:"state,omitempty"`
	AuthURL   string `json:"authURL,omitempty"`
	Tailnet   string `json:"tailnet,omitempty"`
	Hostname  string `json:"hostname,omitempty"`
	SelfIP    string `json:"selfIP,omitempty"`
	ProxyPort int    `json:"proxyPort,omitempty"`
	// ProxyToken is a per-process secret; never log, display, or persist it.
	ProxyToken string `json:"proxyToken,omitempty"`
	Error      string `json:"error,omitempty"`

	ExitNodes []ExitNode `json:"exitNodes,omitempty"`
	// ExitNode is the selected stable ID; empty means none.
	ExitNode string `json:"exitNode,omitempty"`
	// ExitNodeActive gates routing; selected but inactive must fail closed.
	ExitNodeActive bool      `json:"exitNodeActive,omitempty"`
	Warnings       []string  `json:"warnings,omitempty"`
	Accounts       []Account `json:"accounts,omitempty"`
	Peers          []Peer    `json:"peers,omitempty"`
	// SubnetRoutes widens routing only to peer-advertised CIDRs.
	SubnetRoutes []string `json:"subnetRoutes,omitempty"`
	ControlURL   string   `json:"controlURL,omitempty"`
}

// ValidControlURL validates an extension-provided URL before it enters node
// preferences.
func ValidControlURL(s string) error {
	if s == "" {
		return nil
	}
	if len(s) > 512 {
		return errors.New("control URL is too long")
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("control URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("control URL %q must start with http:// or https://", s)
	}
	if u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("control URL %q has no host", s)
	}
	if u.User != nil {
		return fmt.Errorf("control URL %q must not carry credentials", s)
	}
	if u.Fragment != "" || u.RawQuery != "" {
		return fmt.Errorf("control URL %q must not have a query or fragment", s)
	}
	return nil
}

// StatusEvent returns an empty status event.
func StatusEvent() *Event { return &Event{Event: "status"} }

// ErrorEvent returns an error event carrying err's text.
func ErrorEvent(err error) *Event {
	return &Event{Event: "error", Error: err.Error()}
}

var profileIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidProfileID reports whether id is a lowercase UUID safe for one path
// component.
func ValidProfileID(id string) bool { return profileIDRE.MatchString(id) }

// Codec reads Requests and writes Events. Writes are serialized so bus pushes
// cannot interleave with command replies; reads are not concurrent-safe.
type Codec struct {
	br *bufio.Reader
	w  io.Writer

	wmu sync.Mutex // guards w and wlen

	rlen [4]byte
	wlen [4]byte
}

// NewCodec returns a Codec reading from r and writing to w.
func NewCodec(r io.Reader, w io.Writer) *Codec {
	return &Codec{br: bufio.NewReaderSize(r, 4096), w: w}
}

// Read returns the next Request. Invalid JSON preserves framing; framing and
// I/O errors are terminal.
func (c *Codec) Read() (*Request, error) {
	if _, err := io.ReadFull(c.br, c.rlen[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(c.rlen[:])
	if n > MaxMsgSize {
		return nil, fmt.Errorf("nm: incoming message of %d bytes exceeds the %d byte limit", n, MaxMsgSize)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.br, buf); err != nil {
		return nil, fmt.Errorf("nm: short read of %d byte message: %w", n, err)
	}
	req := new(Request)
	if err := json.Unmarshal(buf, req); err != nil {
		return nil, &BadJSONError{Err: err}
	}
	return req, nil
}

// BadJSONError reports invalid JSON whose framing remains synchronized.
type BadJSONError struct{ Err error }

func (e *BadJSONError) Error() string { return "nm: invalid JSON message body: " + e.Err.Error() }
func (e *BadJSONError) Unwrap() error { return e.Err }

// Write frames and writes ev.
func (c *Codec) Write(ev *Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("nm: encoding event: %w", err)
	}
	if len(b) > MaxMsgSize {
		return fmt.Errorf("nm: outgoing message of %d bytes exceeds the %d byte limit", len(b), MaxMsgSize)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	binary.LittleEndian.PutUint32(c.wlen[:], uint32(len(b)))
	if _, err := c.w.Write(c.wlen[:]); err != nil {
		return err
	}
	_, err = c.w.Write(b)
	return err
}
