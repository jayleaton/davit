package guestproto

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

func TestNegotiateVersion(t *testing.T) {
	tests := []struct {
		host, guest VersionRange
		want        uint16
		err         error
	}{
		{VersionRange{1, 1}, VersionRange{1, 1}, 1, nil},
		{VersionRange{1, 3}, VersionRange{2, 5}, 3, nil},
		{VersionRange{2, 5}, VersionRange{1, 3}, 3, nil},
		{VersionRange{1, 2}, VersionRange{2, 2}, 2, nil},
		{VersionRange{1, 2}, VersionRange{3, 4}, 0, ErrVersionMismatch},
		{VersionRange{4, 5}, VersionRange{1, 3}, 0, ErrVersionMismatch},
		{VersionRange{0, 1}, VersionRange{1, 1}, 0, ErrInvalid},
		{VersionRange{1, 1}, VersionRange{2, 1}, 0, ErrInvalid},
	}
	for _, tt := range tests {
		got, err := NegotiateVersion(tt.host, tt.guest)
		if got != tt.want || !errors.Is(err, tt.err) || (tt.err == nil) != (err == nil) {
			t.Errorf("NegotiateVersion(%v,%v) = %d,%v want %d,%v", tt.host, tt.guest, got, err, tt.want, tt.err)
		}
	}
}

func TestIntersectFeatures(t *testing.T) {
	got := IntersectFeatures([]string{"tty", "files", "pty", "files"}, []string{"pty", "files", "x"})
	if !reflect.DeepEqual(got, []string{"files", "pty"}) {
		t.Fatalf("got %v", got)
	}
	if got := IntersectFeatures(nil, []string{"a"}); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

// guestAnswer plays the guest side of the handshake with secret.
func guestAnswer(t *testing.T, secret []byte, h Hello) HelloAck {
	t.Helper()
	v, err := NegotiateVersion(h.Versions, SupportedVersions)
	if err != nil {
		t.Fatal(err)
	}
	ack := HelloAck{Version: v, Features: IntersectFeatures(h.Features, []string{"files", "pty"}),
		AgentVersion: "test", GuestNonce: NewChallenge()}
	ack.Proof, err = ComputeProof(secret, TranscriptFor(h, ack))
	if err != nil {
		t.Fatal(err)
	}
	return ack
}

func TestHandshakeProof(t *testing.T) {
	secret := bytes.Repeat([]byte{0x5a}, 32)
	hello := Hello{Versions: SupportedVersions, Features: []string{"pty", "files", "tty"},
		VMID: "vm_one", Generation: 7, Challenge: NewChallenge()}
	if err := hello.Validate(); err != nil {
		t.Fatal(err)
	}

	// Run the exchange over a Conn to cover the encoded path.
	var buf bytes.Buffer
	c := NewConn(&buf)
	if err := c.Send(1, hello); err != nil {
		t.Fatal(err)
	}
	f, _ := c.ReadFrame()
	gotHello, err := Decode[Hello](f)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Send(1, guestAnswer(t, secret, gotHello)); err != nil {
		t.Fatal(err)
	}
	f, _ = c.ReadFrame()
	ack, err := Decode[HelloAck](f)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyHelloAck(secret, hello, ack); err != nil {
		t.Fatalf("valid proof rejected: %v", err)
	}

	mutate := func(fn func(*Hello)) Hello {
		h := hello
		fn(&h)
		return h
	}
	fails := []struct {
		name   string
		secret []byte
		hello  Hello
		ack    HelloAck
		want   error
	}{
		{"wrong secret", bytes.Repeat([]byte{1}, 32), hello, ack, ErrUnauthenticated},
		{"wrong generation", secret, mutate(func(h *Hello) { h.Generation = 6 }), ack, ErrUnauthenticated},
		{"wrong vmId", secret, mutate(func(h *Hello) { h.VMID = "vm_two" }), ack, ErrUnauthenticated},
		{"other challenge", secret, mutate(func(h *Hello) { h.Challenge = NewChallenge() }), ack, ErrUnauthenticated},
		{"tampered nonce", secret, hello, func() HelloAck { a := ack; a.GuestNonce = NewChallenge(); return a }(), ErrUnauthenticated},
		{"dropped feature", secret, hello, func() HelloAck { a := ack; a.Features = []string{"pty"}; return a }(), ErrUnauthenticated},
		{"unoffered feature", secret, hello, func() HelloAck { a := ack; a.Features = []string{"evil"}; return a }(), ErrInvalid},
		{"version out of range", secret, hello, func() HelloAck { a := ack; a.Version = 9; return a }(), ErrVersionMismatch},
		{"short secret", []byte("short"), hello, ack, ErrInvalid},
	}
	for _, tt := range fails {
		t.Run(tt.name, func(t *testing.T) {
			if err := VerifyHelloAck(tt.secret, tt.hello, tt.ack); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestTranscriptFeatureOrderInsensitive(t *testing.T) {
	secret := bytes.Repeat([]byte{3}, 32)
	tr := Transcript{HostVersions: SupportedVersions, Version: 1, Features: []string{"b", "a"},
		VMID: "vm", Generation: 1, Challenge: NewChallenge(), GuestNonce: NewChallenge()}
	p, err := ComputeProof(secret, tr)
	if err != nil {
		t.Fatal(err)
	}
	tr.Features = []string{"a", "b"}
	if err := VerifyProof(secret, tr, p); err != nil {
		t.Fatal(err)
	}
	// Length-prefixing keeps ("ab","c") and ("a","bc") distinct.
	tr.Features = []string{"ab", "c"}
	p2, _ := ComputeProof(secret, tr)
	tr.Features = []string{"a", "bc"}
	if VerifyProof(secret, tr, p2) == nil {
		t.Fatal("ambiguous transcript encoding")
	}
}

func TestNewChallengeRandom(t *testing.T) {
	a, b := NewChallenge(), NewChallenge()
	if len(a) != NonceSize || bytes.Equal(a, b) {
		t.Fatal("challenge not random or wrong size")
	}
}

// TestProofLabelFrozen: agents baked into existing images prove with this
// label; changing it (a product rename) locks every such guest out.
func TestProofLabelFrozen(t *testing.T) {
	if proofLabel != "slipway/guestproto/hello-proof/v1" {
		t.Fatalf("proofLabel changed to %q: it is wire protocol shared with baked guest agents", proofLabel)
	}
}
