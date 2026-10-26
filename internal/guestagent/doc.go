// Package guestagent is the in-guest side of internal/guestproto: the
// `davit guest-agent` mode of the static davit binary, installed in
// images as /usr/local/bin/davit-guest-agent. It is Linux-only; other
// platforms build a stub.
//
// # Transport
//
// The agent listens on AF_VSOCK port 1024 (Port) for any local CID and
// accepts only peers with CID 2 (the host). Firecracker/Cloud Hypervisor
// expose that port to the host as a hybrid-vsock Unix socket ("CONNECT
// 1024"); the Mac backend uses native vsock. `-listen unix:<path>` serves a
// Unix socket instead, for development.
//
// # systemd
//
// A unit like the following starts the agent early, before sshd, so the
// restore barrier can gate identity-dependent services:
//
//	[Unit]
//	Description=davit guest agent
//	DefaultDependencies=no
//	After=local-fs.target
//	Before=ssh.service
//
//	[Service]
//	ExecStart=/usr/local/bin/davit-guest-agent
//	Restart=always
//	RestartSec=100ms
//
//	[Install]
//	WantedBy=multi-user.target
//
// sshd_config must Include /etc/ssh/sshd_config.d/*.conf (Ubuntu's default):
// the agent writes 50-davit.conf there pinning HostKey to the ed25519 key
// it manages and, when the host supplies user CA keys, TrustedUserCAKeys.
//
// # Sessions and secrets
//
// The first session of a generation carries Hello.BootstrapSecret; the agent
// binds to (vmId, generation, secret), keeps it in -run-dir (tmpfs, 0600, so
// it never reaches a disk snapshot, and survives agent restarts) and
// discards every other session. Later sessions in the generation must prove
// possession without a bootstrap. A bootstrap for an already provisioned
// generation is refused unless it repeats the held secret; a Hello for an
// older generation of the bound VM is refused. Trust assumption: only the
// host can reach the agent's vsock port, and guests cannot reach each other's
// vsock; under it the bootstrap is confidential and the proof binds the
// session to the host's vmId and generation.
//
// # Restore barrier
//
// SetIdentity runs, in order: step the wall clock to host time; write
// /etc/hostname and the 127.0.1.1 line of /etc/hosts and set the kernel
// hostname; rotate /etc/machine-id (and a regular-file
// /var/lib/dbus/machine-id); delete every ssh_host_*_key and generate fresh
// ed25519 and ECDSA P-256 keys in-process (RSA too, only if the image had
// one); write the sshd drop-in and user CA file; start, in the background,
// the restarts of identity-caching units (ssh.service after a key/config
// change, systemd-journald.service after machine-id rotation); syncfs the
// identity files, then stamp the disk (/var/lib/davit/identity.json) with
// the vmId; then rotate the session secret and report Agent ready.
// Workload RPCs (exec, files, signals, shutdown) are refused until the
// barrier has completed for the session's generation.
//
// SSH readiness is tracked separately and conservatively: Readiness.SSH is
// false until the ssh.service restart started by this generation's barrier
// has finished, so the pre-barrier sshd (still holding the golden's keys)
// never counts as ready even though it answers banners. A failed restart is
// reported as sshd.restart=failed. The journald restart only refreshes the
// _MACHINE_ID journald stamps on entries, so nothing waits for it.
//
// Speed (measured on golden Firecracker restores, 1 vCPU): identity work is
// ~40 ms, the sshd restart ~110 ms when the golden was captured after boot
// finished (guestclient.SettledGate) and 500-700 ms when captured mid-boot,
// because the restart then queues behind the guest's remaining boot jobs.
//
// Rotation happens when the host asks (new VM, fork) and also whenever the
// disk stamp names a different VM or the files are missing or invalid, so
// a disk cloned from another VM never keeps its identity even if the host's
// plan was wrong. Resume of the same disk VM keeps both.
//
// Not handled here, deliberately: PID 1 and dbus-daemon keep the machine-id
// they read at boot (writing the file does not reset them); golden
// preparation must snapshot before anything depends on it. boot_id is not
// writable and is reported for information only. systemd-networkd derives
// its DHCP DUID from machine-id; images that use DHCP should restart it via
// -machine-id-units.
//
// # Exec and files
//
// Exec runs argv directly (no shell) in its own process group as the
// requested user (setuid/setgid/groups; an unprivileged agent can only run
// as itself). TTY exec is unsupported: interactive use goes through SSH.
// Output is retained per process (bounded) for ProcessAttach; a deadline
// kills the whole group with SIGKILL and reports status "killed". Processes
// that are not Detached are killed when their session closes.
//
// File RPCs run their filesystem calls with the requested user's
// fsuid/fsgid/groups on a dedicated OS thread, so the kernel enforces that
// user's permissions. Writes go to a temp file in the target directory and
// are renamed into place only after size and SHA-256 match; WriteFile.Mode 0
// keeps an existing target's mode, else 0644. With Archive "tar"
// (feature files.tar) directories travel as tar (internal/tarx): reads walk
// the tree as the user, writes are verified in full and then extracted with
// escape checks. An empty User means -default-user.
//
// # TCP dial
//
// DialTCP (feature tcp) connects to 127.0.0.1:port inside the guest, never
// another address, with a 10 s timeout bounded by the session. It is gated
// by the restore barrier like exec and must be the session's first and only
// request. A failed connect is answered with an error frame (not_found when
// nothing listens) and the session stays usable. On success the agent sends
// Ack, stops reading frames and splices the vsock connection and the TCP
// connection in both directions: EOF on one side half-closes the other
// (CloseWrite), any error closes both, and the session then ends normally.
//
// # Managed jobs, process samples, reverse connections, workspaces
//
// Feature jobs (jobs.go): an Exec.Handle runs at most once per boot, even
// across agent restarts (started handles are journaled in -run-dir), and
// ProcessLookup finds it; processes of an earlier agent process are
// reported lost. Feature ps (ps.go): ProcessListRequest samples /proc,
// bounded in count and encoded size. Feature listen (listen.go): Listen
// opens a session-owned 127.0.0.1 listener whose accepted connections wait,
// bounded and for a limited time, for the host's ReverseConnect, which
// splices like DialTCP. Feature workspace (workspace.go): WorkspacePrepare
// clones repos, writes secret files and runs setup steps in order, one
// preparation at a time, never logging or echoing file contents or env
// values. All four are gated by the restore barrier like exec.
package guestagent

// Port is the vsock port the agent listens on.
const Port uint32 = 1024
