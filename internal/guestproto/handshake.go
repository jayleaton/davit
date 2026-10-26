package guestproto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"sort"
)

const (
	// NonceSize is the size of the host challenge and the guest nonce.
	NonceSize = 32
	// ProofSize is the size of an HMAC-SHA256 proof.
	ProofSize = sha256.Size
	// MinSecretSize/MaxSecretSize bound a per-generation agent secret.
	MinSecretSize = 32
	MaxSecretSize = 64

	// proofLabel is frozen wire protocol: agents baked into images built
	// before the slipway -> davit rename prove with it too.
	proofLabel = "slipway/guestproto/hello-proof/v1"
)

// VersionRange is an inclusive range of protocol versions.
type VersionRange struct {
	Min uint16 `json:"min"`
	Max uint16 `json:"max"`
}

// SupportedVersions is the range this package implements.
var SupportedVersions = VersionRange{Min: MinProtocolVersion, Max: ProtocolVersion}

// Validate checks 1 <= Min <= Max.
func (r VersionRange) Validate() error {
	if r.Min == 0 || r.Min > r.Max {
		return invalidf("bad version range [%d,%d]", r.Min, r.Max)
	}
	return nil
}

// NegotiateVersion returns the highest version in both ranges, or an
// ErrVersionMismatch-coded error when they do not overlap.
func NegotiateVersion(host, guest VersionRange) (uint16, error) {
	if err := host.Validate(); err != nil {
		return 0, err
	}
	if err := guest.Validate(); err != nil {
		return 0, err
	}
	hi := min(host.Max, guest.Max)
	if hi < max(host.Min, guest.Min) {
		return 0, Errorf(CodeVersionMismatch, "host supports [%d,%d], guest supports [%d,%d]",
			host.Min, host.Max, guest.Min, guest.Max)
	}
	return hi, nil
}

// IntersectFeatures returns the sorted, de-duplicated features present in
// both lists.
func IntersectFeatures(host, guest []string) []string {
	have := make(map[string]bool, len(guest))
	for _, g := range guest {
		have[g] = true
	}
	var out []string
	for _, h := range host {
		if have[h] {
			out = append(out, h)
			delete(have, h)
		}
	}
	sort.Strings(out)
	return out
}

// Hello opens every control session (host -> guest). VMID and Generation
// are the host's records; the guest learns them here and cannot choose them.
type Hello struct {
	Versions   VersionRange `json:"versions"`
	Features   []string     `json:"features,omitempty"`
	VMID       string       `json:"vmId"`
	Generation uint64       `json:"generation"`
	Challenge  []byte       `json:"challenge"`
	// BootstrapSecret is sent only when the guest cannot already hold this
	// generation's secret: first session after cold boot or golden/fork
	// restore. It is freshly generated per generation by the host and never
	// stored in an image. Its proof is then only as strong as the per-VM vsock
	// transport, which the host owns; later sessions in the same generation
	// (e.g. after daemon restart) omit it and prove possession.
	BootstrapSecret []byte `json:"bootstrapSecret,omitempty"`
}

// HelloAck answers Hello (guest -> host).
type HelloAck struct {
	Version      uint16   `json:"version"`
	Features     []string `json:"features,omitempty"`
	AgentVersion string   `json:"agentVersion"`
	GuestNonce   []byte   `json:"guestNonce"`
	Proof        []byte   `json:"proof"`
}

// Validate checks Hello's shape.
func (h Hello) Validate() error {
	if err := h.Versions.Validate(); err != nil {
		return err
	}
	if err := validateVMID(h.VMID); err != nil {
		return err
	}
	if h.Generation == 0 {
		return invalidf("generation must be > 0")
	}
	if len(h.Features) > MaxFeatures {
		return invalidf("too many features")
	}
	if len(h.Challenge) != NonceSize {
		return invalidf("challenge must be %d bytes", NonceSize)
	}
	if h.BootstrapSecret != nil {
		if err := validateSecret(h.BootstrapSecret); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks HelloAck's shape (not its proof; use VerifyHelloAck).
func (a HelloAck) Validate() error {
	if a.Version == 0 {
		return invalidf("version must be > 0")
	}
	if len(a.Features) > MaxFeatures {
		return invalidf("too many features")
	}
	if len(a.GuestNonce) != NonceSize {
		return invalidf("guestNonce must be %d bytes", NonceSize)
	}
	if len(a.Proof) != ProofSize {
		return invalidf("proof must be %d bytes", ProofSize)
	}
	return nil
}

// NewChallenge returns NonceSize random bytes (host challenge or guest
// nonce).
func NewChallenge() []byte {
	b := make([]byte, NonceSize)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return b
}

// Transcript is the canonical handshake transcript the proof covers. The
// host must fill VMID, Generation, Challenge and HostVersions from its own
// state, never from anything the guest reported.
type Transcript struct {
	HostVersions VersionRange
	Version      uint16
	Features     []string // negotiated; order-insensitive
	VMID         string
	Generation   uint64
	Challenge    []byte
	GuestNonce   []byte
}

// TranscriptFor builds the transcript for a Hello and its HelloAck.
func TranscriptFor(h Hello, a HelloAck) Transcript {
	return Transcript{
		HostVersions: h.Versions, Version: a.Version, Features: a.Features,
		VMID: h.VMID, Generation: h.Generation, Challenge: h.Challenge, GuestNonce: a.GuestNonce,
	}
}

// bytes is the unambiguous encoding: a domain label then fixed-width or
// length-prefixed fields.
func (t Transcript) bytes() []byte {
	b := make([]byte, 0, 256)
	putStr := func(s string) {
		b = binary.BigEndian.AppendUint32(b, uint32(len(s)))
		b = append(b, s...)
	}
	putStr(proofLabel)
	b = binary.BigEndian.AppendUint16(b, t.HostVersions.Min)
	b = binary.BigEndian.AppendUint16(b, t.HostVersions.Max)
	b = binary.BigEndian.AppendUint16(b, t.Version)
	feats := append([]string(nil), t.Features...)
	sort.Strings(feats)
	b = binary.BigEndian.AppendUint32(b, uint32(len(feats)))
	for _, f := range feats {
		putStr(f)
	}
	putStr(t.VMID)
	b = binary.BigEndian.AppendUint64(b, t.Generation)
	putStr(string(t.Challenge))
	putStr(string(t.GuestNonce))
	return b
}

// ComputeProof returns HMAC-SHA256(secret, transcript).
func ComputeProof(secret []byte, t Transcript) ([]byte, error) {
	if err := validateSecret(secret); err != nil {
		return nil, err
	}
	if len(t.Challenge) != NonceSize || len(t.GuestNonce) != NonceSize {
		return nil, invalidf("challenge and guest nonce must be %d bytes", NonceSize)
	}
	m := hmac.New(sha256.New, secret)
	m.Write(t.bytes())
	return m.Sum(nil), nil
}

// VerifyProof checks proof in constant time. Any mismatch (wrong secret,
// generation, VM ID, nonce, version or features) is ErrUnauthenticated.
func VerifyProof(secret []byte, t Transcript, proof []byte) error {
	want, err := ComputeProof(secret, t)
	if err != nil {
		return err
	}
	if !hmac.Equal(want, proof) {
		return Errorf(CodeUnauthenticated, "handshake proof mismatch")
	}
	return nil
}

// VerifyHelloAck is the host-side check of a guest's answer to hello: the
// version must be negotiable, features a subset of those offered, and the
// proof valid for this VM/generation/challenge under secret.
func VerifyHelloAck(secret []byte, hello Hello, ack HelloAck) error {
	if err := ack.Validate(); err != nil {
		return err
	}
	if ack.Version < hello.Versions.Min || ack.Version > hello.Versions.Max {
		return Errorf(CodeVersionMismatch, "guest chose version %d outside [%d,%d]",
			ack.Version, hello.Versions.Min, hello.Versions.Max)
	}
	if got := IntersectFeatures(hello.Features, ack.Features); len(got) != len(ack.Features) {
		return invalidf("guest selected features not offered by host")
	}
	return VerifyProof(secret, TranscriptFor(hello, ack), ack.Proof)
}
