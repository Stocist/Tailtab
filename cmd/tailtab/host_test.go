package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Stocist/tailtab/internal/nm"
)

type fakeBackend struct {
	mu        sync.Mutex
	profileID string
	browser   string
	initErr   error
	// duringInit simulates an IPN bus callback before Init returns.
	duringInit func()
	// duringUp simulates a bus callback after initialization.
	duringUp        func()
	wantUp          []bool
	loggedOut       bool
	state           string
	exitNodes       []string
	exitErr         error
	switched        []string
	added           int
	controlURL      string
	addedControlURL string
}

func (f *fakeBackend) Init(profileID, browser, controlURL string) error {
	f.mu.Lock()
	f.controlURL = controlURL
	f.mu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.profileID, f.browser = profileID, browser
	during := f.duringInit
	if during != nil {
		f.mu.Unlock()
		during()
		f.mu.Lock()
	}
	if f.initErr != nil {
		return f.initErr
	}
	f.state = "NeedsLogin"
	return nil
}

func (f *fakeBackend) Status() *nm.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	ev := nm.StatusEvent()
	ev.State = f.state
	return ev
}

func (f *fakeBackend) SetWantRunning(up bool) error {
	f.mu.Lock()
	f.wantUp = append(f.wantUp, up)
	during := f.duringUp
	f.mu.Unlock()
	if up && during != nil {
		during()
	}
	return nil
}

func (f *fakeBackend) SetExitNode(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exitErr != nil {
		return f.exitErr
	}
	f.exitNodes = append(f.exitNodes, id)
	return nil
}

func (f *fakeBackend) Logout() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loggedOut = true
	return nil
}

func (f *fakeBackend) SwitchAccount(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.switched = append(f.switched, id)
	return nil
}

func (f *fakeBackend) AddAccount(controlURL string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added++
	f.addedControlURL = controlURL
	return nil
}

func (f *fakeBackend) Close() error { return nil }

func runLoop(t *testing.T, be backend, msgs ...string) []nm.Event {
	t.Helper()
	return runLoopWith(t, func(*host) backend { return be }, msgs...)
}

func runLoopWith(t *testing.T, mk func(*host) backend, msgs ...string) []nm.Event {
	t.Helper()
	var in bytes.Buffer
	for _, m := range msgs {
		var hdr [4]byte
		binary.LittleEndian.PutUint32(hdr[:], uint32(len(m)))
		in.Write(hdr[:])
		in.WriteString(m)
	}
	var out bytes.Buffer
	h := &host{codec: nm.NewCodec(&in, &out)}
	h.be = mk(h)
	if err := h.loop(); !errors.Is(err, io.EOF) && h.fatal == nil {
		t.Fatalf("loop ended with %v, want io.EOF", err)
	}

	var events []nm.Event
	b := out.Bytes()
	for len(b) > 0 {
		if len(b) < 4 {
			t.Fatalf("trailing %d bytes are not a complete length prefix", len(b))
		}
		n := int(binary.LittleEndian.Uint32(b[:4]))
		if len(b) < 4+n {
			t.Fatalf("length prefix says %d bytes but only %d remain", n, len(b)-4)
		}
		var ev nm.Event
		if err := json.Unmarshal(b[4:4+n], &ev); err != nil {
			t.Fatalf("event %d is not valid JSON: %v", len(events), err)
		}
		events = append(events, ev)
		b = b[4+n:]
	}
	return events
}

const goodID = "0f8fad5b-d9cb-469f-a165-70867728950e"

func TestLoopHappyPath(t *testing.T) {
	be := &fakeBackend{}
	events := runLoop(t, be,
		`{"cmd":"init","profileID":"`+goodID+`","browser":"zen"}`,
		`{"cmd":"up"}`,
		`{"cmd":"status"}`,
		`{"cmd":"down"}`,
		`{"cmd":"logout"}`,
	)
	if len(events) != 5 {
		t.Fatalf("got %d events, want 5: %+v", len(events), events)
	}
	for i, ev := range events {
		if ev.Event != "status" {
			t.Errorf("event %d is %q, want status: %+v", i, ev.Event, ev)
		}
	}
	if be.profileID != goodID || be.browser != "zen" {
		t.Errorf("init passed profileID=%q browser=%q", be.profileID, be.browser)
	}
	if want := []bool{true, false}; len(be.wantUp) != 2 || be.wantUp[0] != want[0] || be.wantUp[1] != want[1] {
		t.Errorf("SetWantRunning calls = %v, want %v", be.wantUp, want)
	}
	if !be.loggedOut {
		t.Error("logout was not forwarded to the backend")
	}
}

func TestLoopSurvivesBadInput(t *testing.T) {
	be := &fakeBackend{}
	events := runLoop(t, be,
		`{"cmd":`,
		`{"cmd":"nonsense"}`,
		`{}`,
		`{"cmd":"init","profileID":"../x","browser":"zen"}`,
		`{"cmd":"init","profileID":"`+goodID+`"}`,
		`{"cmd":"init","profileID":"`+goodID+`","browser":"Safari Mobile"}`,
		`{"cmd":"up"}`,
		`{"cmd":"status"}`,
	)
	if len(events) != 8 {
		t.Fatalf("got %d events, want 8: %+v", len(events), events)
	}
	for i, ev := range events[:7] {
		if ev.Event != "error" || ev.Error == "" {
			t.Errorf("event %d = %+v, want a non-empty error event", i, ev)
		}
	}
	if last := events[7]; last.Event != "status" {
		t.Errorf("final event = %+v, want status", last)
	}
	if be.profileID != "" {
		t.Errorf("a rejected profileID (%q) still reached the backend", be.profileID)
	}
}

func TestLoopRejectsSecondInit(t *testing.T) {
	be := &fakeBackend{}
	events := runLoop(t, be,
		`{"cmd":"init","profileID":"`+goodID+`","browser":"zen"}`,
		`{"cmd":"init","profileID":"`+goodID+`","browser":"edge"}`,
	)
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3 (status, status, error): %+v", len(events), events)
	}
	if events[2].Event != "error" || !strings.Contains(events[2].Error, "already initialised") {
		t.Errorf("second init produced %+v, want an already-initialised error", events[2])
	}
	if be.browser != "zen" {
		t.Errorf("the second init overwrote the browser: %q", be.browser)
	}
}

func TestLoopReportsInitFailure(t *testing.T) {
	be := &fakeBackend{initErr: errors.New("state dir is not writable")}
	events := runLoop(t, be, `{"cmd":"init","profileID":"`+goodID+`","browser":"edge"}`)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (status, error): %+v", len(events), events)
	}
	if events[1].Event != "error" || !strings.Contains(events[1].Error, "not writable") {
		t.Errorf("got %+v, want the backend error surfaced", events[1])
	}
}

func TestInitFailureIsFatal(t *testing.T) {
	be := &fakeBackend{initErr: errors.New("bind: address already in use")}
	var in bytes.Buffer
	for _, m := range []string{
		`{"cmd":"init","profileID":"` + goodID + `","browser":"edge"}`,
		`{"cmd":"status"}`,
	} {
		var hdr [4]byte
		binary.LittleEndian.PutUint32(hdr[:], uint32(len(m)))
		in.Write(hdr[:])
		in.WriteString(m)
	}
	h := &host{codec: nm.NewCodec(&in, &bytes.Buffer{}), be: be}
	err := h.loop()
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("loop returned %v, want the init failure", err)
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Errorf("loop returned %v, want the bind error", err)
	}
}

// IPN bus callbacks can race Init; none may precede the init reply.
func TestNoStatusEventEscapesBeforeInit(t *testing.T) {
	var be *fakeBackend
	events := runLoopWith(t, func(h *host) backend {
		be = &fakeBackend{duringInit: func() {
			h.pushStatus()
			h.pushStatus()
		}}
		return be
	}, `{"cmd":"init","profileID":"`+goodID+`","browser":"zen"}`)

	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 (the reply to init): %+v", len(events), events)
	}
	if events[0].Event != "status" || events[0].State != "NeedsLogin" {
		t.Errorf("first event = %+v, want the initialised status", events[0])
	}
	for i, ev := range events {
		if ev.State == "NoState" {
			t.Errorf("event %d is the spurious pre-init status: %+v", i, ev)
		}
	}
}

func TestStatusIsPushedOnceInitIsThrough(t *testing.T) {
	events := runLoopWith(t, func(h *host) backend {
		return &fakeBackend{duringUp: func() { h.pushStatus() }}
	},
		`{"cmd":"init","profileID":"`+goodID+`","browser":"zen"}`,
		`{"cmd":"up"}`,
	)
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(events), events)
	}
	for i, ev := range events {
		if ev.Event != "status" || ev.State != "NeedsLogin" {
			t.Errorf("event %d = %+v, want a status event from the backend", i, ev)
		}
	}
}

func TestExitNodeCommand(t *testing.T) {
	be := &fakeBackend{}
	events := runLoop(t, be,
		`{"cmd":"init","profileID":"`+goodID+`","browser":"edge"}`,
		`{"cmd":"exitnode","id":"nodeid-server"}`,
		`{"cmd":"exitnode"}`,
	)
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(events), events)
	}
	for i, ev := range events {
		if ev.Event != "status" {
			t.Errorf("event %d = %+v, want a status event", i, ev)
		}
	}
	if want := []string{"nodeid-server", ""}; !slices.Equal(be.exitNodes, want) {
		t.Errorf("SetExitNode calls = %q, want %q", be.exitNodes, want)
	}
}

func TestExitNodeCommandBeforeInitIsRefused(t *testing.T) {
	be := &fakeBackend{}
	events := runLoop(t, be, `{"cmd":"exitnode","id":"nodeid-server"}`)
	if len(events) != 1 || events[0].Event != "error" {
		t.Fatalf("got %+v, want one error event", events)
	}
	if len(be.exitNodes) != 0 {
		t.Errorf("SetExitNode was called before init: %q", be.exitNodes)
	}
}

func TestExitNodeRefusalIsReported(t *testing.T) {
	be := &fakeBackend{exitErr: errors.New(`"nope" is not an exit node this tailnet offers`)}
	events := runLoop(t, be,
		`{"cmd":"init","profileID":"`+goodID+`","browser":"edge"}`,
		`{"cmd":"exitnode","id":"nope"}`,
	)
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3 (status, status, error): %+v", len(events), events)
	}
	if events[2].Event != "error" || !strings.Contains(events[2].Error, "not an exit node") {
		t.Errorf("got %+v, want the refusal surfaced", events[2])
	}
}
