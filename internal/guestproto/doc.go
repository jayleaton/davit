// Package guestproto defines the versioned, length-framed RPC spoken between
// the host daemon and the guest agent over vsock (SPEC §5 "Guest agent and
// restore barrier"). It holds message types, the frame codec and handshake
// helpers only: no agent logic and no transport dialing. Firecracker/CH
// hybrid-vsock UDS and Mac native vsock are transports that hand this package
// an io.ReadWriter.
//
// # Version
//
// ProtocolVersion is 1 (supported range MinProtocolVersion..ProtocolVersion).
// The 8-byte frame header and the JSON envelope's field names are stable
// across protocol versions; the handshake picks the message semantics.
//
// # Framing
//
//	 0    1    2      3      4                 8
//	+----+----+------+------+-----------------+------------------------+
//	| 'S'| 'W'| fmt=1| flags| length (u32 BE) | payload: JSON envelope |
//	|    |    |      |  =0  | MinFrameSize..  | (length bytes)         |
//	|    |    |      |      | MaxFrameSize    |                        |
//	+----+----+------+------+-----------------+------------------------+
//
// The payload is a JSON envelope:
//
//	{"v":1, "id":7, "type":"exec", "stream":7, "seq":3, "body":{...}, "error":{"code":"...","message":"..."}}
//
// Payloads over MaxFrameSize (1 MiB) are rejected on encode and on decode
// before allocation; payloads under MinFrameSize, bad magic, non-zero flags
// and bad JSON are malformed. A reader sees io.EOF only at a frame boundary
// and io.ErrUnexpectedEOF for a truncated frame. After any framing error the
// stream is unsynchronised and must be closed. Byte data (stdout, stderr,
// stdin, file chunks) is []byte, i.e. base64 in JSON, at most MaxChunkSize
// per frame.
//
// Requests carry a non-zero id; the response (or an "error" frame with the
// Error member set) carries the same id. A request that opens a stream uses
// its own id as the stream id; stream frames carry (stream, seq) with seq
// starting at 1 and increasing by one per frame, so a receiver can drop
// replays and detect gaps (SeqTracker). Unknown JSON fields are ignored; an
// unknown message type is answered with an error frame of code
// "unsupported" (UnsupportedFrame) rather than dropping the session.
//
// # Handshake
//
// The first exchange on every new control session is:
//
//	host  -> guest  Hello{versions, features, vmId, generation, challenge[32], bootstrapSecret?}
//	guest -> host   HelloAck{version, features ∩, agentVersion, guestNonce[32], proof}
//
// proof = HMAC-SHA256(secret, transcript), where the transcript is a
// length-prefixed encoding of (label, host version range, chosen version,
// sorted features, vmId, generation, challenge, guestNonce). The secret is a
// per-generation key the host generated and provisioned (BootstrapSecret on
// the first session of a generation, or SetIdentity.NewAgentSecret); the host
// verifies with VerifyHelloAck/VerifyProof (hmac.Equal) using vmId,
// generation and challenge from its own records. HelloAck has no identity
// fields: the guest cannot choose or claim its vmId/generation, and a proof
// keyed for another VM or an older generation fails.
//
// No reusable secret lives in golden images. A golden/fork restore resumes
// RAM that holds no current secret, so the host provisions a fresh one over
// the per-VM vsock it owns; every instance and generation rotates it.
//
// Trust assumption for the bootstrap: only the host can reach the guest
// agent's vsock port (the agent accepts peer CID 2 only), and guests cannot
// reach each other's vsock (each VM's device is private to its VMM). Under
// it the bootstrap secret is confidential; the proof binds the session to the
// host's vmId/generation/challenge, so a stale or misrouted guest cannot
// answer for another instance. The agent accepts a bootstrap only for a
// generation it has not been provisioned for (or repeating the held secret),
// and SetIdentity.NewAgentSecret rotates it again during the barrier. The
// agent listens on vsock port 1024 (internal/guestagent.Port).
//
// # TCP dial (feature "tcp")
//
// DialTCP{port} reaches a TCP port on the guest's loopback (127.0.0.1 only;
// the message has no address field) over the agent's authenticated vsock
// transport. It is answered by Ack or an error frame. After the Ack the
// connection stops carrying frames and becomes a raw byte stream to guest
// 127.0.0.1:port in both directions, half-close aware. So DialTCP must be the
// only request on its session: a dedicated control session authenticated by
// the normal Hello/HelloAck, on which the host sends nothing after DialTCP
// until it has read the Ack (Conn reads frames without read-ahead, so no
// stream byte is consumed as framing). Like exec, it is refused until the
// restore barrier completed for the session's generation.
//
// # Managed-job handles (feature "jobs")
//
// Exec.Handle is an optional host-assigned durable handle (at most MaxIDLen
// bytes of [A-Za-z0-9_.:-]). The agent records it with the process and
// echoes it in ProcessStarted and ProcessStatus. An Exec whose Handle matches
// a process the agent already started this boot is answered with that
// process's ProcessStarted and is never run twice. ProcessLookup{handle} is
// answered by ProcessStatus (ProcessID and Handle set) or an error frame with
// code "not_found", so a host that lost its records can find a process again
// instead of marking it lost.
//
// # Process samples (feature "ps")
//
// ProcessListRequest is answered by ProcessList: a sample of every guest
// process (pid, ppid, user, args, state, CPU time, RSS, start time, and the
// handle of managed jobs) taken at SampledAtUnixNano. The list holds at most
// MaxProcessSamples entries; Truncated says the agent cut it.
//
// # Reverse connections (feature "listen")
//
// Listen{port} opens a listener on guest 127.0.0.1:port (loopback only; port
// 0 lets the agent pick) and a stream on its request id: first ListenStarted
// {listenerId, port}, then one ListenAccepted{listenerId, connId} per accepted
// guest connection. The host claims a connection with ReverseConnect
// {listenerId, connId} on a dedicated, freshly authenticated control session,
// exactly like DialTCP: it is answered by Ack or an error frame, after which
// the session is a raw, half-close-aware byte stream to that guest
// connection, so ReverseConnect must be the only request on its session and
// the host sends nothing after it until it has read the Ack. Unlisten
// {listenerId} is answered by Ack and ends the listener's stream. Listen and
// ReverseConnect are refused until the restore barrier completed for the
// session's generation.
//
// # Workspace preparation (feature "workspace")
//
// WorkspacePrepare{revision, repos, files, env, setup} clones each repo (URL
// at Ref into absolute Path), writes each file, then runs the setup steps in
// order with env. Revision is an opaque host label (e.g. "env_x@3") echoed in
// the result. The request opens a stream on its id: WorkspaceProgress
// {step, phase clone|file|setup, state running|done|failed, message} frames,
// then exactly one WorkspaceResult{revision, state ready|failed, failedStep,
// exitCode, message} ending it. A failed step stops the sequence.
//
// Files carry secret contents over the authenticated session; the agent never
// logs Files data or Env values. Each file's data is at most MaxChunkSize
// bytes in this message; larger files are a later chunked extension. Repo
// credentials are not in this message: they arrive through a credential-proxy
// placeholder in Env, owned by the credentials work.
//
// # Restore barrier (SPEC §5 steps 1-5)
//
// Before admitting SSH, forwarding or exec after cold boot, clone or restore:
//
//  1. Fresh session: the host discards old streams, opens a new vsock
//     connection and runs Hello/HelloAck with a new challenge.
//  2. Clock: SetIdentity.WallClockUnixNano carries trusted host time; the
//     agent steps the clock (and reconciles timers/RNG) before workloads.
//  3. Identity: SetIdentity assigns vmId, generation and hostname; forks and
//     new VMs set RotateMachineID and RotateSSHHostKeys. Resume of the same
//     disk VM does not rotate them.
//  4. Credentials: SetIdentity.NewAgentSecret and SSHUserCAKeys refresh
//     agent/SSH credentials; the agent restarts identity-dependent services.
//  5. Verify: the host checks IdentityResult (vmId, generation, machineId,
//     SSH host-key fingerprints), runs a ReadinessProbe, and only then
//     publishes ready. IdentityResult.BootID is informational: RAM restore
//     does not rotate boot_id, so generation is the run identity.
package guestproto
