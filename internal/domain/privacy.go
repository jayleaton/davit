package domain

// PrivacyPolicy is a VM's ephemeral and privacy policy (G17). Every flag
// defaults to off (the usual retention). The flags limit what davit
// itself keeps on the host; they are not secure erasure: deleted files may
// survive in host backups, filesystem free space or SSD wear levelling,
// and a guest can always write anything to its own disk.
type PrivacyPolicy struct {
	// Ephemeral deletes the VM when it stops (an explicit stop, the TTL
	// auto-stop or a stop retry's force): no snapshot is committed and the
	// VM's automatic snapshots and working disk are collected.
	Ephemeral bool `json:"ephemeral,omitempty"`
	// NoSnapshot makes a stop commit nothing: the working disk is
	// discarded, so a stopped VM cannot be resumed (or forked) and explicit
	// snapshots are refused.
	NoSnapshot bool `json:"noSnapshot,omitempty"`
	// NoOutputRetention keeps no process output on the host (G7 history
	// shows the processes, not their output).
	NoOutputRetention bool `json:"noOutputRetention,omitempty"`
	// NoProcessSamples stores no whole-guest process samples (ps still
	// answers live from a running VM).
	NoProcessSamples bool `json:"noProcessSamples,omitempty"`
}

// KeepsDisk reports whether a stop commits the disk.
func (p PrivacyPolicy) KeepsDisk() bool { return !p.Ephemeral && !p.NoSnapshot }

// RetainsOutput reports whether managed processes' output may be stored on
// the host. It is the single check every output writer goes through.
func (p PrivacyPolicy) RetainsOutput() bool { return !p.NoOutputRetention }

// RetainsSamples reports whether process samples may be stored.
func (p PrivacyPolicy) RetainsSamples() bool { return !p.NoProcessSamples }
