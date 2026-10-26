package helperproto

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFrameRoundTrip(t *testing.T) {
	cfg := VMConfig{Key: Key{VMID: "vm-a", Generation: 2}, CPUs: 2, MemoryBytes: 512 << 20, Kernel: "/k/vmlinux",
		CommandLine: "console=hvc0", RootDisk: "/d/root.img", Network: NetworkNone, VsockPort: 1024}
	f, err := NewFrame(ProtocolVersion, 7, TypeCreate, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteFrame(&buf, f); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	if raw[0] != 'S' || raw[1] != 'H' || raw[2] != 1 || raw[3] != 0 || int(binary.BigEndian.Uint32(raw[4:])) != len(raw)-HeaderSize {
		t.Fatalf("header % x", raw[:HeaderSize])
	}
	got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	var back VMConfig
	if got.ID != 7 || got.Type != TypeCreate || got.Decode(&back) != nil || back != cfg {
		t.Fatalf("round trip %+v %+v", got, back)
	}
	if _, err := ReadFrame(&buf); err != io.EOF {
		t.Fatalf("at a frame boundary: %v, want io.EOF", err)
	}
}

func TestFramingRejectsBadInput(t *testing.T) {
	good := func() []byte {
		var b bytes.Buffer
		f, _ := NewFrame(1, 1, TypeState, nil)
		WriteFrame(&b, f)
		return b.Bytes()
	}
	cases := map[string]struct {
		raw  []byte
		code Code
		err  error
	}{
		"bad magic":   {append([]byte("SW"), good()[2:]...), CodeMalformed, nil},
		"flags":       {func() []byte { b := good(); b[3] = 1; return b }(), CodeMalformed, nil},
		"too large":   {func() []byte { b := good(); binary.BigEndian.PutUint32(b[4:], MaxFrameSize+1); return b }(), CodeTooLarge, nil},
		"truncated":   {good()[:len(good())-3], "", io.ErrUnexpectedEOF},
		"half header": {good()[:5], "", io.ErrUnexpectedEOF},
		"not json":    {append([]byte{'S', 'H', 1, 0, 0, 0, 0, 3}, "{x}"...), CodeMalformed, nil},
		"no type":     {append([]byte{'S', 'H', 1, 0, 0, 0, 0, 7}, `{"v":1}`...), CodeMalformed, nil},
	}
	for name, c := range cases {
		_, err := ReadFrame(bytes.NewReader(c.raw))
		if c.err != nil && !errors.Is(err, c.err) || c.code != "" && !IsCode(err, c.code) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Oversize bodies are refused on encode too, and nothing is written.
	var b bytes.Buffer
	f, _ := NewFrame(1, 1, TypeCreate, VMConfig{CommandLine: strings.Repeat("x", MaxFrameSize)})
	if err := WriteFrame(&b, f); !IsCode(err, CodeTooLarge) || b.Len() != 0 {
		t.Fatalf("oversize encode: %v, %d bytes written", err, b.Len())
	}
}

// TestGarbageNeverPanics feeds random bytes and mutated frames to the
// reader and decoders.
func TestGarbageNeverPanics(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	var seed bytes.Buffer
	f, _ := NewFrame(1, 9, TypeCreate, VMConfig{Key: Key{VMID: "a", Generation: 1}})
	WriteFrame(&seed, f)
	for i := range 2000 {
		b := bytes.Clone(seed.Bytes())
		if i%2 == 0 {
			b = make([]byte, rng.IntN(64))
			for j := range b {
				b[j] = byte(rng.IntN(256))
			}
		} else {
			for range 1 + rng.IntN(6) {
				b[rng.IntN(len(b))] ^= byte(1 + rng.IntN(255))
			}
		}
		if fr, err := ReadFrame(bytes.NewReader(b)); err == nil {
			var c VMConfig
			fr.Decode(&c)
			var h HelloResult
			fr.Decode(&h)
		}
	}
}

func TestNegotiate(t *testing.T) {
	if v, err := Negotiate(1, 9); err != nil || v != ProtocolVersion {
		t.Fatalf("1..9: %d %v", v, err)
	}
	for _, r := range [][2]uint16{{2, 9}, {0, 0}, {3, 1}} {
		if _, err := Negotiate(r[0], r[1]); !IsCode(err, CodeVersion) {
			t.Errorf("%v: %v", r, err)
		}
	}
}

func TestValidation(t *testing.T) {
	ok := VMConfig{Key: Key{VMID: "vm-a", Generation: 1}, CPUs: 1, MemoryBytes: 256 << 20, Kernel: "/k", RootDisk: "/d", Network: NetworkNone, VsockPort: 1024}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*VMConfig){
		"relative disk":  func(c *VMConfig) { c.RootDisk = "root.img" },
		"unclean kernel": func(c *VMConfig) { c.Kernel = "/k/../etc/passwd" },
		"no kernel":      func(c *VMConfig) { c.Kernel = "" },
		"odd memory":     func(c *VMConfig) { c.MemoryBytes = 256<<20 + 1 },
		"no cpus":        func(c *VMConfig) { c.CPUs = 0 },
		"no vsock":       func(c *VMConfig) { c.VsockPort = 0 },
		"slash in vm id": func(c *VMConfig) { c.Key.VMID = "../x" },
		"generation 0":   func(c *VMConfig) { c.Key.Generation = 0 },
	} {
		c := ok
		mut(&c)
		if err := c.Validate(); !IsCode(err, CodeInvalidArgument) {
			t.Errorf("%s: %v", name, err)
		}
	}
	nat := ok
	nat.Network = "nat"
	if err := nat.Validate(); !IsCode(err, CodeUnsupported) {
		t.Errorf("nat network: %v", err)
	}
	if err := (&Stop{GraceMillis: -1}).Validate(); err == nil {
		t.Error("negative grace accepted")
	}
	if err := (&StateResult{State: "flying"}).Validate(); err == nil {
		t.Error("unknown state accepted")
	}
}

// TestClientChecksIncarnation: a helper answering as another incarnation
// (a recycled socket or PID) is refused before any request.
func TestClientChecksIncarnation(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "c.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			f, err := ReadFrame(c)
			if err == nil {
				Reply(c, 1, f, HelloResult{Version: 1, Incarnation: "other", Key: Key{VMID: "vm", Generation: 1}}, nil)
			}
			c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Open(ctx, sock, "test", "mine"); !IsCode(err, CodeUnauthorized) {
		t.Fatalf("wrong incarnation: %v", err)
	}
	c, err := Open(ctx, sock, "test", "")
	if err != nil || c.Hello.Incarnation != "other" {
		t.Fatalf("no expectation: %v", err)
	}
	c.Close()
}
