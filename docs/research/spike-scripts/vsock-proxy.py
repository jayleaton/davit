#!/usr/bin/env python3
"""ssh ProxyCommand for Firecracker hybrid vsock: vsock-proxy.py <uds> <port>."""
import os, selectors, socket, sys

uds, port = sys.argv[1], int(sys.argv[2])
s = socket.socket(socket.AF_UNIX)
s.connect(uds)
s.sendall(f"CONNECT {port}\n".encode())
ack = b""
while not ack.endswith(b"\n"):
    c = s.recv(1)
    if not c:
        sys.exit(1)
    ack += c
if not ack.startswith(b"OK"):
    sys.exit(1)
sel = selectors.DefaultSelector()
sel.register(0, selectors.EVENT_READ, "in")
sel.register(s, selectors.EVENT_READ, "sock")
while True:
    for key, _ in sel.select():
        if key.data == "in":
            d = os.read(0, 65536)
            if not d:
                s.shutdown(socket.SHUT_WR)
                sel.unregister(0)
            else:
                s.sendall(d)
        else:
            d = s.recv(65536)
            if not d:
                sys.exit(0)
            os.write(1, d)
