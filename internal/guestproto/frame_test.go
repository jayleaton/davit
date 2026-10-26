package guestproto

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
)

func header(n uint32) []byte {
	h := []byte{'S', 'W', 1, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(h[4:], n)
	return h
}

func rawFrame(payload string) []byte { return append(header(uint32(len(payload))), payload...) }

func mustEncode(t testing.TB, id uint64, m Message) []byte {
	t.Helper()
	f, err := Encode(id, m)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := WriteFrame(&b, f); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// countingReader records how many bytes were consumed.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestReadFrameEdgeCases(t *testing.T) {
	valid := mustEncode(t, 1, Ping{})
	tests := []struct {
		name string
		in   []byte
		want error
	}{
		{"empty stream is clean EOF", nil, io.EOF},
		{"partial header", valid[:5], io.ErrUnexpectedEOF},
		{"header only", valid[:HeaderSize], io.ErrUnexpectedEOF},
		{"truncated payload", valid[:len(valid)-1], io.ErrUnexpectedEOF},
		{"zero length", header(0), ErrMalformedFrame},
		{"one byte", rawFrame("{"), ErrMalformedFrame},
		{"bad magic", append([]byte{'X', 'W'}, valid[2:]...), ErrMalformedFrame},
		{"nonzero flags", append([]byte{'S', 'W', 1, 1}, valid[4:]...), ErrMalformedFrame},
		{"garbage json", rawFrame("{not json"), ErrMalformedFrame},
		{"json array", rawFrame("[1,2,3]"), ErrMalformedFrame},
		{"missing type", rawFrame(`{"v":1,"id":1}`), ErrMalformedFrame},
		{"zero version", rawFrame(`{"v":0,"type":"ping"}`), ErrMalformedFrame},
		{"oversize claimed", header(MaxFrameSize + 1), ErrFrameTooLarge},
		{"huge claimed", header(0xFFFFFFFF), ErrFrameTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := ReadFrame(bytes.NewReader(tt.in))
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v (frame %+v)", err, tt.want, f)
			}
		})
	}
}

func TestReadFrameOversizeDoesNotReadPayload(t *testing.T) {
	// A 4 GiB claim followed by an endless stream: ReadFrame must stop after
	// the header and must not allocate the claimed size.
	cr := &countingReader{r: io.MultiReader(bytes.NewReader(header(0xFFFFFFFF)), zeroReader{})}
	allocs := testing.AllocsPerRun(10, func() {
		cr.r = io.MultiReader(bytes.NewReader(header(0xFFFFFFFF)), zeroReader{})
		cr.n = 0
		if _, err := ReadFrame(cr); !errors.Is(err, ErrFrameTooLarge) {
			t.Fatalf("err = %v", err)
		}
	})
	if cr.n != HeaderSize {
		t.Fatalf("read %d bytes, want %d", cr.n, HeaderSize)
	}
	if allocs > 20 {
		t.Fatalf("too many allocations: %v", allocs)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestCleanEOFAtBoundary(t *testing.T) {
	in := append(mustEncode(t, 1, Ping{}), mustEncode(t, 2, Pong{})...)
	r := bytes.NewReader(in)
	for i := 0; i < 2; i++ {
		if _, err := ReadFrame(r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ReadFrame(r); err != io.EOF {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

func TestWriteFrameRejects(t *testing.T) {
	var b bytes.Buffer
	big := &Frame{V: 1, Type: TypeStdout, Body: json.RawMessage(`"` + strings.Repeat("a", MaxFrameSize) + `"`)}
	if err := WriteFrame(&b, big); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversize: err = %v", err)
	}
	if err := WriteFrame(&b, &Frame{Type: TypePing}); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("zero v: err = %v", err)
	}
	if err := WriteFrame(&b, &Frame{V: 1}); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("no type: err = %v", err)
	}
	if b.Len() != 0 {
		t.Fatalf("wrote %d bytes on error", b.Len())
	}
	// Largest permitted chunk fits in a frame.
	f, err := EncodeStream(1, 1, Stdout{Data: bytes.Repeat([]byte{0xff}, MaxChunkSize)})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&b, f); err != nil {
		t.Fatalf("max chunk: %v", err)
	}
}

func TestUnknownFieldsIgnoredAndUnknownTypeUnsupported(t *testing.T) {
	f, err := ReadFrame(bytes.NewReader(rawFrame(
		`{"v":1,"id":9,"type":"exec","future":true,"body":{"argv":["ls"],"newField":{"x":1}}}`)))
	if err != nil {
		t.Fatal(err)
	}
	e, err := Decode[Exec](f)
	if err != nil || len(e.Argv) != 1 {
		t.Fatalf("decode: %+v %v", e, err)
	}

	f, err = ReadFrame(bytes.NewReader(rawFrame(`{"v":1,"id":10,"type":"teleport","body":{}}`)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = DecodeMessage(f)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want unsupported", err)
	}
	reply := UnsupportedFrame(f)
	if reply.ID != 10 || reply.Type != TypeError || reply.Error.Code != CodeUnsupported {
		t.Fatalf("reply = %+v", reply)
	}
	// The error frame itself round-trips and surfaces as a typed error.
	var b bytes.Buffer
	if err := WriteFrame(&b, reply); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrame(&b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode[Pong](got); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("decode error frame: %v", err)
	}
}

func TestDecodeTypeMismatch(t *testing.T) {
	f, _ := Encode(1, Ping{})
	if _, err := Decode[Pong](f); !errors.Is(err, ErrUnexpectedType) {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeValidates(t *testing.T) {
	f, _ := Encode(1, Exec{})
	if _, err := Decode[Exec](f); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Decode err = %v", err)
	}
	if _, err := DecodeMessage(f); !errors.Is(err, ErrInvalid) {
		t.Fatalf("DecodeMessage err = %v", err)
	}
}

func TestErrorFrameRedactsUntypedErrors(t *testing.T) {
	f := ErrorFrame(3, errors.New("open /var/lib/secret: denied"))
	if f.Error.Code != CodeInternal || strings.Contains(f.Error.Message, "/var") {
		t.Fatalf("not redacted: %+v", f.Error)
	}
}

func TestConnConcurrentWriters(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	w, r := NewConn(a), NewConn(b)

	const writers, perWriter = 8, 50
	payload := bytes.Repeat([]byte("x"), 4096) // larger than pipe chunks
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(stream uint64) {
			defer wg.Done()
			for seq := uint64(1); seq <= perWriter; seq++ {
				f, err := EncodeStream(stream, seq, Stdout{Data: payload})
				if err != nil {
					t.Error(err)
					return
				}
				if err := w.WriteFrame(f); err != nil {
					t.Error(err)
					return
				}
			}
		}(uint64(i + 1))
	}

	var seqs SeqTracker
	for n := 0; n < writers*perWriter; n++ {
		f, err := r.ReadFrame()
		if err != nil {
			t.Fatalf("frame %d: %v", n, err)
		}
		dup, err := seqs.Accept(f)
		if err != nil || dup {
			t.Fatalf("frame %d: dup=%v err=%v", n, dup, err)
		}
		c, err := Decode[Stdout](f)
		if err != nil || !bytes.Equal(c.Data, payload) {
			t.Fatalf("frame %d corrupted: %v", n, err)
		}
	}
	wg.Wait()
}

func TestConnSendStampsNegotiatedVersion(t *testing.T) {
	var buf bytes.Buffer
	c := NewConn(&buf)
	c.SetVersion(7)
	if err := c.Send(1, Ping{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Send(2, HelloAck{}); err != nil {
		t.Fatal(err)
	}
	f1, _ := c.ReadFrame()
	f2, _ := c.ReadFrame()
	if f1.V != 7 || f2.V != MinProtocolVersion {
		t.Fatalf("versions = %d, %d", f1.V, f2.V)
	}
}

func TestSeqTracker(t *testing.T) {
	var s SeqTracker
	fr := func(stream, seq uint64) *Frame { return &Frame{V: 1, Type: TypeStdout, Stream: stream, Seq: seq} }
	if dup, err := s.Accept(fr(1, 1)); dup || err != nil {
		t.Fatal(dup, err)
	}
	if dup, err := s.Accept(fr(1, 1)); !dup || err != nil {
		t.Fatal("replay not reported as dup", err)
	}
	if _, err := s.Accept(fr(1, 3)); !errors.Is(err, ErrSequenceGap) {
		t.Fatalf("gap err = %v", err)
	}
	s.Resume(2, 10)
	if dup, err := s.Accept(fr(2, 11)); dup || err != nil {
		t.Fatal(dup, err)
	}
	if _, err := s.Accept(fr(0, 1)); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("non-stream err = %v", err)
	}
}

func FuzzReadFrame(f *testing.F) {
	f.Add(mustEncode(f, 1, Ping{}))
	f.Add(mustEncode(f, 2, Exec{Argv: []string{"sh", "-lc", "echo hi"}, Env: []string{"A=1"}}))
	f.Add(rawFrame(`{"v":1,"type":"stdout","stream":1,"seq":1,"body":{"data":"aGk="}}`))
	f.Add(header(0))
	f.Add(header(0xFFFFFFFF))
	f.Add(rawFrame("{garbage"))
	f.Add([]byte("SW"))
	f.Fuzz(func(t *testing.T, in []byte) {
		fr, err := ReadFrame(bytes.NewReader(in))
		if err != nil {
			return
		}
		if fr.V == 0 || fr.Type == "" {
			t.Fatalf("accepted invalid envelope %+v", fr)
		}
		_, _ = DecodeMessage(fr) // must not panic
		var b bytes.Buffer
		if err := WriteFrame(&b, fr); err != nil && !errors.Is(err, ErrFrameTooLarge) {
			t.Fatalf("re-encode: %v", err)
		}
	})
}
