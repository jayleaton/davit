package guestproto

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"
)

// FuzzDecodeMessage drives every registered message type with arbitrary
// bodies (FuzzReadFrame rarely guesses a valid type name): decoding never
// panics, and a body it accepts survives a round trip, re-encoded and
// decoded again, still valid.
func FuzzDecodeMessage(f *testing.F) {
	types := make([]MsgType, 0, len(registry))
	for t := range registry {
		types = append(types, t)
	}
	slices.Sort(types)
	for i := range types {
		for _, b := range []string{`{}`, `null`, `[]`, `{"argv":["sh"],"env":["A=1"]}`, `{"path":"/etc/../x","data":"AA=="}`,
			`{"processId":"","signal":"KILL"}`, `{"handle":"` + string(bytes.Repeat([]byte("h"), 300)) + `"}`, `{"port":65536}`} {
			f.Add(uint8(i), []byte(b))
		}
	}
	f.Fuzz(func(t *testing.T, ti uint8, body []byte) {
		typ := types[int(ti)%len(types)]
		m, err := DecodeMessage(&Frame{V: framingVersion, Type: typ, Body: json.RawMessage(body)})
		if err != nil {
			return
		}
		fr, err := Encode(1, m)
		if err != nil {
			t.Fatalf("%s: accepted %q but cannot re-encode: %v", typ, body, err)
		}
		var b bytes.Buffer
		if err := WriteFrame(&b, fr); err != nil {
			return // over the frame limit once re-encoded
		}
		back, err := ReadFrame(&b)
		if err != nil {
			t.Fatalf("%s: re-read: %v", typ, err)
		}
		if _, err := DecodeMessage(back); err != nil {
			t.Fatalf("%s: accepted %q, rejected after a round trip: %v", typ, body, err)
		}
	})
}

// FuzzVerifyHelloAck: any Hello/HelloAck pair (the guest's answer is
// untrusted) is validated without panicking, and an ack is accepted only
// with the proof computed from the shared secret.
func FuzzVerifyHelloAck(f *testing.F) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	h := Hello{Versions: VersionRange{Min: 1, Max: 1}, Features: []string{"jobs"}, VMID: "vm_1", Generation: 1, Challenge: NewChallenge()}
	a := HelloAck{Version: 1, Features: []string{"jobs"}, AgentVersion: "t", GuestNonce: NewChallenge()}
	a.Proof, _ = ComputeProof(secret, TranscriptFor(h, a))
	hb, _ := json.Marshal(h)
	ab, _ := json.Marshal(a)
	f.Add(hb, ab) // a valid pair
	f.Add([]byte(`{}`), []byte(`{}`))
	f.Fuzz(func(t *testing.T, hb, ab []byte) {
		var h Hello
		var a HelloAck
		if json.Unmarshal(hb, &h) != nil || json.Unmarshal(ab, &a) != nil {
			return
		}
		_ = h.Validate()
		_ = a.Validate()
		if err := VerifyHelloAck(secret, h, a); err == nil {
			want, perr := ComputeProof(secret, TranscriptFor(h, a))
			if perr != nil || !bytes.Equal(want, a.Proof) {
				t.Fatalf("ack accepted without the right proof: %+v", a)
			}
		}
	})
}
