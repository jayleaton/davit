package guestclient

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"davit/internal/guestproto"
	"davit/internal/runtime"
)

// forgingGuest answers Hello with a proof built from the given transcript
// tweak and secret, as a compromised or misrouted guest would.
func forgingGuest(t *testing.T, conn net.Conn, secret []byte, tweak func(*guestproto.Hello)) {
	t.Helper()
	pc := guestproto.NewConn(conn)
	f, err := pc.ReadFrame()
	if err != nil {
		return
	}
	h, err := guestproto.Decode[guestproto.Hello](f)
	if err != nil {
		t.Error(err)
		return
	}
	tweak(&h)
	ack := guestproto.HelloAck{Version: 1, AgentVersion: "evil", GuestNonce: guestproto.NewChallenge()}
	ack.Proof, _ = guestproto.ComputeProof(secret, guestproto.TranscriptFor(h, ack))
	_ = pc.Send(f.ID, ack)
}

func TestHandshakeRejectsForgedProofs(t *testing.T) {
	good := bytes.Repeat([]byte{7}, 32)
	cases := []struct {
		name   string
		secret []byte
		tweak  func(*guestproto.Hello)
	}{
		{"wrong secret", bytes.Repeat([]byte{8}, 32), func(*guestproto.Hello) {}},
		{"claims another vm", good, func(h *guestproto.Hello) { h.VMID = "vm_other" }},
		{"claims older generation", good, func(h *guestproto.Hello) { h.Generation-- }},
		{"replays another challenge", good, func(h *guestproto.Hello) { h.Challenge = guestproto.NewChallenge() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, guest := net.Pipe()
			defer host.Close()
			go forgingGuest(t, guest, tc.secret, tc.tweak)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := Handshake(ctx, host, Session{VMID: "vm_a", Generation: 5, Secrets: [][]byte{good}})
			if !errors.Is(err, guestproto.ErrUnauthenticated) {
				t.Fatalf("err = %v, want unauthenticated", err)
			}
		})
	}
}

func TestDirSecretsDurableAndScoped(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "secrets")
	s := DirSecrets{Dir: dir}
	k1 := runtime.Key{VMID: "vm_a", Generation: 1}
	a, b := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	if err := s.Save(ctx, k1, [][]byte{a, b}); err != nil {
		t.Fatal(err)
	}
	if got, _ := (DirSecrets{Dir: dir}).Load(ctx, k1); len(got) != 2 || !bytes.Equal(got[0], a) || !bytes.Equal(got[1], b) {
		t.Fatalf("load = %x", got)
	}
	if fi, err := os.Stat(filepath.Join(dir, "vm_a.1")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("secret file mode: %v %v", fi, err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", fi.Mode())
	}
	// A new generation replaces the old one; other VMs are untouched.
	other := runtime.Key{VMID: "vm_ab", Generation: 1}
	if err := s.Save(ctx, other, [][]byte{a}); err != nil {
		t.Fatal(err)
	}
	k2 := runtime.Key{VMID: "vm_a", Generation: 2}
	if err := s.Save(ctx, k2, [][]byte{b}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Load(ctx, k1); len(got) != 0 {
		t.Fatal("old generation secret kept")
	}
	if got, _ := s.Load(ctx, other); len(got) != 1 {
		t.Fatal("another VM's secret was dropped")
	}
	if err := s.Save(ctx, k2, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Load(ctx, k2); len(got) != 0 {
		t.Fatal("forget did not remove the secret")
	}
	if err := s.Save(ctx, runtime.Key{VMID: "../x", Generation: 1}, [][]byte{a}); err == nil {
		t.Fatal("path-like VM ID accepted")
	}
}

func TestHostnameFor(t *testing.T) {
	for in, want := range map[string]string{
		"vm_0123abc": "vm-0123abc", "My Box!": "my-box", "__": "vm", "a--b": "a-b",
		"x" + string(bytes.Repeat([]byte("y"), 80)): "x" + string(bytes.Repeat([]byte("y"), 62)),
	} {
		if got := HostnameFor(in); got != want {
			t.Errorf("HostnameFor(%q) = %q, want %q", in, got, want)
		}
	}
}
