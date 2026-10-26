// Package pidns lets a test reuse a PID on purpose, to prove that code which
// signals processes by identity never reaches a stranger that inherited the
// number. Only tests import it.
//
// Isolated runs the calling test again in fresh user, PID and mount
// namespaces (unprivileged: the test is root only inside them), with /proc
// remounted for the new PID namespace. There, Reuse writes ns_last_pid so the
// next process gets a chosen PID, deterministically.
package pidns
