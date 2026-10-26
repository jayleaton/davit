//go:build !linux

package network

import (
	"context"
	"net"
	"runtime"

	"davit/internal/domain"
)

// Rootless is the Linux user+net namespace provider. On this OS it reports
// itself unusable and every operation returns ErrUnsupported; the host
// virtualization backend's own networking is used instead (SPEC §7).
type Rootless struct{ cfg Config }

// NewRootless returns a provider whose operations fail with ErrUnsupported.
func NewRootless(cfg Config) (*Rootless, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Rootless{cfg: cfg}, nil
}

// Probe reports that only ModeNone is usable on this OS.
func (r *Rootless) Probe(context.Context) Capabilities {
	why := "network namespaces require Linux (GOOS=" + runtime.GOOS + ")"
	return Capabilities{
		Modes: []ModeStatus{
			{Mode: ModeRootless, Reason: why},
			{Mode: ModePrivileged, Reason: why},
			{Mode: ModeNone, Usable: true},
		},
		Egress: []BackendStatus{{Name: BackendPasta, Reason: why}, {Name: BackendSlirp4netns, Reason: why}},
		Filter: BackendStatus{Name: "nft", Reason: why},
	}
}

// Create returns ErrUnsupported.
func (r *Rootless) Create(context.Context, domain.VMID, int64, domain.Egress) (Handle, error) {
	return Handle{}, ErrUnsupported
}

// SetEgress returns ErrUnsupported.
func (r *Rootless) SetEgress(context.Context, Handle, domain.Egress) (Handle, error) {
	return Handle{}, ErrUnsupported
}

// Enter returns ErrUnsupported.
func (r *Rootless) Enter(Handle) ([]string, error) { return nil, ErrUnsupported }

// ListenTCP returns ErrUnsupported.
func (r *Rootless) ListenTCP(context.Context, Handle, uint16) (net.Listener, error) {
	return nil, ErrUnsupported
}

// Destroy returns ErrUnsupported.
func (r *Rootless) Destroy(context.Context, Handle) error { return ErrUnsupported }

// Reap returns ErrUnsupported.
func (r *Rootless) Reap(context.Context, []Handle) (int, error) { return 0, ErrUnsupported }
