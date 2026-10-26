// Child process for socket-errors.test.ts: one scenario where a server resets
// (TCP RST) the client's socket at some point of a WebSocket connection. It
// prints "SETTLED <outcome>" and exits 0 once the client saw the failure; an
// error event nobody listens to crashes it (exit 1, "Unhandled 'error'").
//
//   node rst-client.ts SCENARIO
//
// accept       the server resets as soon as it accepts
// late         the server resets while the client holds the dialed socket
//              before starting the handshake (caller code awaiting between
//              transport.connect and WebSocketConnection.connect)
// mid-head     after part of the response head
// after-101    after a complete handshake
// after-401    after a complete non-101 answer with its body
// short-401    after a non-101 head whose body never fully arrives

import { createHash } from "node:crypto";
import { createServer, type Socket } from "node:net";

import { FetchTransport, WebSocketConnection } from "../../src/index.ts";

const scenario = process.argv[2] ?? "";
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

function readHead(sock: Socket): Promise<string> {
  return new Promise((resolve) => {
    let buf = "";
    const onData = (c: Buffer) => {
      buf += c.toString("latin1");
      if (buf.includes("\r\n\r\n")) {
        sock.off("data", onData);
        resolve(buf);
      }
    };
    sock.on("data", onData);
  });
}

const server = createServer(async (sock) => {
  sock.on("error", () => {});
  const reset = () => sock.resetAndDestroy();
  if (scenario === "accept") return reset();
  if (scenario === "late") return void sleep(50).then(reset); // the client is connected by then
  const head = await readHead(sock);
  const key = /sec-websocket-key: *(\S+)/i.exec(head)?.[1] ?? "";
  const accept = createHash("sha1").update(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").digest("base64");
  const ok = `HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${accept}\r\n\r\n`;
  const body = JSON.stringify({ error: { code: "unauthenticated", message: "no", retryable: false, requestId: "r" } });
  switch (scenario) {
    case "mid-head":
      sock.write("HTTP/1.1 101 Swit");
      break;
    case "after-101":
      sock.write(ok);
      break;
    case "after-401":
      sock.write(`HTTP/1.1 401 Unauthorized\r\nContent-Type: application/json\r\nContent-Length: ${body.length}\r\n\r\n${body}`);
      break;
    case "short-401":
      sock.write(`HTTP/1.1 401 Unauthorized\r\nContent-Type: application/json\r\nContent-Length: ${body.length + 100}\r\n\r\n${body}`);
      break;
  }
  await sleep(30);
  reset();
});
await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
const port = (server.address() as { port: number }).port;

let outcome: string;
try {
  const conn = await new FetchTransport(`http://127.0.0.1:${port}`).connect();
  if (scenario === "late") await sleep(150); // the RST arrives meanwhile
  const ws = await WebSocketConnection.connect(conn.socket, { host: conn.host, path: "/ws" });
  await ws.next(); // after-101: rejects once the reset arrives
  outcome = "next resolved";
} catch (err) {
  outcome = `${(err as Error).name}: ${(err as Error).message}`;
}
await sleep(200); // late error events would crash the process here
server.close();
console.log(`SETTLED ${outcome}`);
