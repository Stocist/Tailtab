package nm

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	// Write an Event, then read the same bytes back as a Request: the framing
	// is identical in both directions, so one buffer exercises both halves.
	var buf bytes.Buffer
	out := NewCodec(nil, &buf)
	if err := out.Write(&Event{Event: "status", State: "Running", Tailnet: "tail4d5e6f.ts.net", ProxyPort: 51234}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if got := binary.LittleEndian.Uint32(buf.Bytes()[:4]); int(got) != buf.Len()-4 {
		t.Errorf("length prefix = %d, want %d", got, buf.Len()-4)
	}
	var back Event
	if err := json.Unmarshal(buf.Bytes()[4:], &back); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if back.State != "Running" || back.Tailnet != "tail4d5e6f.ts.net" || back.ProxyPort != 51234 {
		t.Errorf("event round-tripped as %+v", back)
	}

	in := NewCodec(bytes.NewReader(frame(t, `{"cmd":"init","profileID":"0f8fad5b-d9cb-469f-a165-70867728950e","browser":"zen"}`)), nil)
	req, err := in.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if req.Cmd != CmdInit || req.Browser != "zen" || req.ProfileID != "0f8fad5b-d9cb-469f-a165-70867728950e" {
		t.Errorf("request round-tripped as %+v", req)
	}
}

func TestReadSequence(t *testing.T) {
	// Several messages back to back must all decode from one stream.
	var in bytes.Buffer
	in.Write(frame(t, `{"cmd":"status"}`))
	in.Write(frame(t, `{"cmd":"up"}`))
	in.Write(frame(t, `{"cmd":"logout"}`))
	c := NewCodec(&in, nil)
	for _, want := range []Command{CmdStatus, CmdUp, CmdLogout} {
		req, err := c.Read()
		if err != nil {
			t.Fatalf("Read %q: %v", want, err)
		}
		if req.Cmd != want {
			t.Errorf("got cmd %q, want %q", req.Cmd, want)
		}
	}
	if _, err := c.Read(); !errors.Is(err, io.EOF) {
		t.Errorf("after the last message, got err %v, want io.EOF", err)
	}
}

func TestReadRejectsOversize(t *testing.T) {
	var in bytes.Buffer
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], MaxMsgSize+1)
	in.Write(hdr[:])
	if _, err := NewCodec(&in, nil).Read(); err == nil {
		t.Fatal("Read accepted a message declaring MaxMsgSize+1 bytes")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWriteRejectsOversize(t *testing.T) {
	var buf bytes.Buffer
	err := NewCodec(nil, &buf).Write(&Event{Event: "error", Error: strings.Repeat("x", MaxMsgSize+1)})
	if err == nil {
		t.Fatal("Write accepted an oversize event")
	}
	if buf.Len() != 0 {
		t.Errorf("Write emitted %d bytes for a rejected event; the stream must stay clean", buf.Len())
	}
}

func TestReadBadJSONIsRecoverable(t *testing.T) {
	// A framed-but-invalid body must not desync the stream: the next message
	// still reads. This is what keeps the host alive on a malformed command.
	var in bytes.Buffer
	in.Write(frame(t, `{"cmd":`))
	in.Write(frame(t, `{"cmd":"status"}`))
	c := NewCodec(&in, nil)

	_, err := c.Read()
	var bad *BadJSONError
	if !errors.As(err, &bad) {
		t.Fatalf("got err %v, want *BadJSONError", err)
	}
	req, err := c.Read()
	if err != nil {
		t.Fatalf("stream desynced after bad JSON: %v", err)
	}
	if req.Cmd != CmdStatus {
		t.Errorf("got cmd %q after bad JSON, want status", req.Cmd)
	}
}

func TestReadTruncatedBody(t *testing.T) {
	var in bytes.Buffer
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], 64)
	in.Write(hdr[:])
	in.WriteString(`{"cmd":"status"}`) // fewer than 64 bytes
	if _, err := NewCodec(&in, nil).Read(); err == nil {
		t.Fatal("Read accepted a truncated body")
	}
}

func TestValidProfileID(t *testing.T) {
	good := []string{
		"0f8fad5b-d9cb-469f-a165-70867728950e",
		"00000000-0000-0000-0000-000000000000",
		"ffffffff-ffff-ffff-ffff-ffffffffffff",
	}
	for _, id := range good {
		if !ValidProfileID(id) {
			t.Errorf("ValidProfileID(%q) = false, want true", id)
		}
	}
	bad := []string{
		"",
		"0F8FAD5B-D9CB-469F-A165-70867728950E",   // uppercase
		"0f8fad5b-d9cb-469f-a165-70867728950",    // too short
		"0f8fad5bd9cb469fa16570867728950e",       // no hyphens
		"../../../etc/passwd",                    // traversal
		"0f8fad5b-d9cb-469f-a165-70867728950e/x", // trailing path
		"0f8fad5b-d9cb-469f-a165-70867728950e\n",
		"0f8fad5b-d9cb-469g-a165-70867728950e", // non-hex digit
	}
	for _, id := range bad {
		if ValidProfileID(id) {
			t.Errorf("ValidProfileID(%q) = true, want false", id)
		}
	}
}

// frame wraps body in the 4-byte little-endian length prefix.
func frame(t *testing.T, body string) []byte {
	t.Helper()
	var b bytes.Buffer
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(body)))
	b.Write(hdr[:])
	b.WriteString(body)
	return b.Bytes()
}

func TestValidControlURL(t *testing.T) {
	good := []string{"", "https://headscale.example.com", "http://10.0.0.5:8080", "https://controlplane.tailscale.com", "https://hs.example.com/base"}
	for _, u := range good {
		if err := ValidControlURL(u); err != nil {
			t.Errorf("ValidControlURL(%q) = %v, want nil", u, err)
		}
	}
	bad := []string{"headscale.example.com", "ftp://x.example", "https://", "https://user:pw@hs.example.com", "https://hs.example.com/?x=1", "https://hs.example.com/#f", "https://" + strings.Repeat("a", 600) + ".example"}
	for _, u := range bad {
		if err := ValidControlURL(u); err == nil {
			t.Errorf("ValidControlURL(%q) = nil, want an error", u)
		}
	}
}
