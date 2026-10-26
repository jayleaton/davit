// Package helperproto is the versioned control protocol between the Mac
// runtime adapter (internal/runtime/mac, in the Go daemon) and the per-VM
// helper (a signed Swift process that owns one VZVirtualMachine). Both sides
// implement it; docs/research/MAC-HELPER-PROTOCOL.md is the normative spec
// the Swift helper is written from, and this package is its Go reference.
//
// One helper process runs one VM for one (vmId, generation). The adapter
// starts it as `helper serve --dir DIR --vm ID --generation N`; it survives
// the daemon (no parent-death signal) and exits once its VM has stopped. A
// one-shot `helper probe` prints a ProbeResult (JSON) for capabilities and
// doctor checks.
//
// # Files in DIR (owner-only 0700; sockets 0600)
//
//	helper.json   Record, written atomically by the helper before it listens
//	ctl.sock      control RPC (this package's frames)
//	str.sock      guest byte streams: Hello, Dial, then raw bytes
//	console.log   guest serial console
//	exit.json     Exit, written atomically by the helper before it exits
//
// Socket names are short because macOS limits a socket path to 103 bytes;
// the adapter checks the full length before it starts a helper.
//
// # Framing
//
//	 0    1    2      3      4                 8
//	+----+----+------+------+-----------------+------------------------+
//	| 'S'| 'H'| fmt=1| flags| length (u32 BE) | payload: JSON envelope |
//	|    |    |      |  =0  | 2..MaxFrameSize |                        |
//	+----+----+------+------+-----------------+------------------------+
//
// The envelope is {"v":1,"id":7,"type":"boot","seq":0,"body":{...},
// "error":{...}}. Requests carry a non-zero id; the reply carries the same
// id and type, with either a body or an error. Frames over MaxFrameSize are
// rejected before allocation; after any framing error the connection is
// unsynchronised and must be closed. Unknown fields are ignored; an unknown
// type is answered with an error of code "unsupported".
//
// # Sessions
//
// Every connection starts with Hello (the client's protocol range); the
// helper answers with the chosen version and its identity (incarnation,
// key, build, operations). The client checks the incarnation and key against
// helper.json before anything else. One request is in flight per control
// connection; a Watch request turns its connection into an event stream.
// A str.sock connection carries Hello, then one Dial, then raw guest
// bytes in both directions until either side closes; it never blocks the
// control socket.
package helperproto
