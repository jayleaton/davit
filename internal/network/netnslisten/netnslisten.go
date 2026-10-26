// Package netnslisten creates a listening TCP socket inside a VM network
// namespace for the daemon. It runs there as `davit netns-listenfd
// ADDR:PORT` (EnterArgv + the davit binary), binds ADDR:PORT (tcp4) in the
// namespace it was started in and sends the listener's fd back over the
// unix socket on fd FDSock with SCM_RIGHTS, then exits. The daemon accepts
// on the socket from its own namespace; no forwarding process remains.
package netnslisten

// Cmd is the davit subcommand that creates and sends the listener.
const Cmd = "netns-listenfd"

// FDSock is the unix socket the listener fd is sent over (os/exec
// ExtraFiles start at 3).
const FDSock = 3
