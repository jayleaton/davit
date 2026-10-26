// Socket error discipline. A socket whose 'error' event fires with no
// listener crashes the process, and a socket passes through hands (dial,
// caller code, handshake, connection) with gaps between their listeners: a
// peer reset in such a gap took the whole program down. Every socket the
// SDK dials or is given is guarded from the start: a listener that records
// the first error stays for the socket's life, and each owner checks for an
// already-recorded failure before it waits, surfacing it through its own
// promise or connection.

import type { Socket } from "node:net";

const guarded = new WeakMap<Socket, { error?: Error }>();

/** Attaches the lifelong error recorder to `socket` (idempotent) and returns `socket`. */
export function guardSocket(socket: Socket): Socket {
  if (!guarded.has(socket)) {
    const g: { error?: Error } = {};
    guarded.set(socket, g);
    socket.on("error", (err: Error) => {
      g.error ??= err;
    });
  }
  return socket;
}

/**
 * How `socket` already failed, if it did: the first recorded error, or
 * "closed" when it was destroyed without one. An owner attaching its
 * listeners now would otherwise wait forever for events that are past. (A
 * half-closed socket has not failed: with allowHalfOpen the peer's end is
 * data, and without it Node destroys the socket.)
 */
export function socketFailure(socket: Socket): { error?: Error; closed: boolean } | undefined {
  const error = guarded.get(socket)?.error;
  if (error) return { error, closed: true };
  if (socket.destroyed) return { closed: true };
  return undefined;
}
