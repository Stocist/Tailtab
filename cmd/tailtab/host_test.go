package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/Stocist/tailtab/internal/nm"
)

// fakeBackend records calls and never touches the network or the filesystem.
type fakeBackend struct {
	mu        sync.Mutex
	profileID string
	browser   string
	initErr   error
	wantUp    []bool
	loggedOut bool
	state     string
}

func (f *fakeBackend) Init(profileID, browser string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.profileID, f.browser = profileID, browser
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
	defer f.mu.Unlock()
	f.wantUp = append(f.wantUp, up)
	return nil
}

func (f *fakeBackend) Logout() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loggedOut = true
	return nil
}

func (f *fakeBackend) Close() error { return nil }

// runLoop feeds framed messages through a host and returns the events written.
func runLoop(t *testing.T, be backend, msgs ...string) []nm.Event {
	t.Helper()
	var in bytes.Buffer
	for _, m := range msgs {
		var hdr [4]byte
		binary.LittleEndian.PutUint32(hdr[:], uint32(len(m)))
		in.Write(hdr[:])
		in.WriteString(m)
	}
	var out bytes.Buffer
	h := &host{codec: nm.NewCodec(&in, &out), be: be}
	if err := h.loop(); !errors.Is(err, io.EOF) {
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
	// Every one of these is rejected, and the loop must still be alive to
	// serve the trailing status command.
	be := &fakeBackend{}
	events := runLoop(t, be,
		`{"cmd":`,                                            // unframed JSON
		`{"cmd":"nonsense"}`,                                 // unknown command
		`{}`,                                                 // no cmd
		`{"cmd":"init","profileID":"../x","browser":"zen"}`,  // path traversal
		`{"cmd":"init","profileID":"`+goodID+`"}`,            // no browser
		`{"cmd":"init","profileID":"`+goodID+`","browser":"safari"}`, // unknown browser
		`{"cmd":"up"}`,                                       // before init
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
