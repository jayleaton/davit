// Drive a remote davit daemon over its tailnet HTTPS gateway.
//
// Configuration comes only from the environment shared with the CLI and the
// Python SDK:
//
//   DAVIT_API_URL  https://<host>[:port]  (required here)
//   DAVIT_TOKEN    optional bearer token (`davit token create`); without
//                    it the gateway authenticates the caller's tailnet identity
//   DAVIT_CA_FILE  optional PEM bundle of extra trusted CAs (operator certs)
//
// Usage (Node >= 22.18 runs the TypeScript source directly):
//   node sdk/typescript/examples/remote-https.ts [vm-name]   (default: sdk-https)
//
// Prints the runtime engine, the existing VMs, creates the VM (waiting until it
// is ready), runs `uname -a` in it, prints its stdout and deletes the VM.
// Exit status: 0 on success, 1 on an API error (`<code>: <message>` on stderr)
// or a failed command, 2 on bad configuration.

import { API_URL_ENV, NotFound, OperationFailed, DavitClient, DavitError, WaitTimeout } from "../src/index.ts";

async function main(argv: string[]): Promise<number> {
  const name = argv[2] || "sdk-https";
  if (!process.env[API_URL_ENV]) {
    process.stderr.write(`set ${API_URL_ENV}=https://<host>[:port]\n`);
    return 2;
  }
  let client: DavitClient;
  try {
    client = new DavitClient(); // DAVIT_API_URL, DAVIT_TOKEN, DAVIT_CA_FILE
  } catch (err) {
    process.stderr.write(`invalid configuration: ${err instanceof Error ? err.message : String(err)}\n`);
    return 2;
  }
  let created = false; // true once this run owns a VM that must be cleaned up
  try {
    console.log("engine:", (await client.getCapabilities()).runtime.engine);
    for await (const vm of client.iterVMs()) console.log("vm:", vm.name, vm.state);
    let vm;
    try {
      vm = await client.createVM({ name, ttlSeconds: 600 }); // waits for the create operation
    } catch (err) {
      // Our create was accepted: clean up what it left.
      if (err instanceof OperationFailed || err instanceof WaitTimeout) created = true;
      throw err;
    }
    created = true;
    vm = await client.waitReady(vm.id);
    console.log("created:", vm.name, vm.id, vm.state);
    const result = await client.run(vm.id, ["uname", "-a"], { deadlineSeconds: 60 });
    process.stdout.write(Buffer.from(result.stdout));
    if (result.exitCode !== 0) {
      process.stderr.write(Buffer.from(result.stderr));
      process.stderr.write(`uname exited ${result.exitCode}\n`);
      return 1;
    }
    await client.deleteVM(vm.id);
    created = false;
    console.log("deleted:", name);
    return 0;
  } catch (err) {
    if (!(err instanceof DavitError)) throw err;
    process.stderr.write(`${err.code}: ${err.message}\n`);
    return 1;
  } finally {
    if (created) {
      try {
        await client.deleteVM(name); // best effort: do not leave the VM behind
      } catch (err) {
        if (!(err instanceof NotFound)) {
          const what = err instanceof DavitError ? `${err.code}: ${err.message}` : String(err);
          process.stderr.write(`cleanup failed: ${what}\n`);
        }
      }
    }
  }
}

process.exitCode = await main(process.argv);
