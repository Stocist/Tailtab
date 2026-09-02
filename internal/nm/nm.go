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
	"fmt"
	"io"
	"regexp"
	"sync"
)

// MaxMsgSize is the largest message we will read or write, in bytes.
// Chromium's own limit for host->browser messages is 1 MiB.
const MaxMsgSize = 1 << 20

// Command is an extension -> host command name.
type Command string

const (
	CmdInit   Command = "init"
	CmdStatus Command = "status"
	CmdUp     Command = "up"
	CmdDown   Command = "down"
	CmdLogout Command = "logout"
)

// Request is a message from the browser extension. Nothing in it is trusted.
type Request struct {
	Cmd Command `json:"cmd"`

	// ProfileID identifies the browser profile. It becomes a filesystem path
	// component, so it must be a lowercase UUID; see ValidProfileID.
	ProfileID string `json:"profileID,omitempty"`

	// Browser is "zen" or "edge". It is only used to build a node hostname.
	Browser string `json:"browser,omitempty"`
}

// Event is a message from the host to the browser extension. Exactly one of
// the two event names is used: "status" or "error".
type Event struct {
	Event string `json:"event"`

	// State is an ipn.State string, passed through verbatim:
	// NoState, NeedsMachineAuth, NeedsLogin, Starting, Running, Stopped.
	State     string `json:"state,omitempty"`
	AuthURL   string `json:"authURL,omitempty"`
	Tailnet   string `json:"tailnet,omitempty"`
	Hostname  string `json:"hostname,omitempty"`
	SelfIP    string `json:"selfIP,omitempty"`
	ProxyPort int    `json:"proxyPort,omitempty"`
	// ProxyToken is the password half of the proxy credential, regenerated
	//per process start. It is a secret shared with this one extension: never
	// log an event wholesale, never show it in the popup, never write it to
	// storage.local.
	ProxyToken string `json:"proxyToken,omitempty"`
	Error      string `json:"error,omitempty"`
	// Warnings is the backend's unhealthy warnables, as text for the popup.
	Warnings []string `json:"warnings,omitempty"`
}

// StatusEvent returns an empty status event, ready to be filled in.
func StatusEvent() *Event { return &Event{Event: "status"} }

// ErrorEvent returns an error event carrying err's text.
func ErrorEvent(err error) *Event {
	return &Event{Event: "error", Error: err.Error()}
}

var profileIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidProfileID reports whether id is a lowercase-hex UUID and therefore safe
// to use as a single filesystem path component. Anything else is rejected
// before it can reach a path join.
func ValidProfileID(id string) bool { return profileIDRE.MatchString(id) }

// Codec reads Requests from r and writes Events to w. Writes are serialized by
// an internal mutex so a status push from the IPN bus cannot interleave with a
// command reply. Reads are not safe for concurrent use.
type Codec struct {
	br *bufio.Reader
	w  io.Writer

	wmu sync.Mutex // guards w and wlen

	rlen [4]byte // owned by Read
	wlen [4]byte // guarded by wmu
}

// NewCodec returns a Codec reading from r and writing to w.
func NewCodec(r io.Reader, w io.Writer) *Codec {
	return &Codec{br: bufio.NewReaderSize(r, 4096), w: w}
}

// Read returns the next Request. A malformed JSON body is reported as an error
// but leaves the stream in sync, so the caller may keep reading; a framing or
// I/O error is terminal.
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

// BadJSONError reports a message that was framed correctly but did not contain
// valid JSON. The stream is still in sync, so the read loop can continue.
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
