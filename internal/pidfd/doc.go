// Package pidfd signals a process identified by PID and start time, and
// never another that later gets its PID: the network provider's helpers,
// the Firecracker adapter's VMMs and the test leak sweep all kill through
// it. Linux only.
package pidfd
