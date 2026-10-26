"""Drive a remote davit daemon over its tailnet HTTPS gateway.

Configuration comes only from the environment shared with the CLI and the
TypeScript SDK:

    DAVIT_API_URL  https://<host>[:port]  (required here)
    DAVIT_TOKEN    optional bearer token (``davit token create``); without
                     it the gateway authenticates the caller's tailnet identity
    DAVIT_CA_FILE  optional PEM bundle of extra trusted CAs (operator certs)

Usage: python remote_https.py [vm-name]   (default name: sdk-https)

Prints the runtime engine, the existing VMs, creates the VM (waiting until it
is ready), runs ``uname -a`` in it, prints its stdout and deletes the VM.
Exit status: 0 on success, 1 on an API error (``<code>: <message>`` on
stderr) or a failed command, 2 on bad configuration.
"""

from __future__ import annotations

import os
import sys

from davit import (
    API_URL_ENV,
    Client,
    DavitError,
    NotFound,
    OperationFailed,
    WaitTimeout,
)


def main(argv: list[str]) -> int:
    name = argv[1] if len(argv) > 1 else "sdk-https"
    url = os.environ.get(API_URL_ENV, "")
    if not url:
        print(f"set {API_URL_ENV}=https://<host>[:port]", file=sys.stderr)
        return 2
    try:
        client = Client()  # DAVIT_API_URL, DAVIT_TOKEN, DAVIT_CA_FILE
    except ValueError as exc:
        print(f"invalid configuration: {exc}", file=sys.stderr)
        return 2
    created = False  # True once this run owns a VM that must be cleaned up
    with client:
        try:
            print("engine:", client.get_capabilities().runtime.engine)
            for vm in client.iter_vms():
                print("vm:", vm.name, vm.state)
            try:
                vm = client.create_vm(name, ttl=600)  # waits for the create operation
            except (OperationFailed, WaitTimeout):
                created = True  # our create was accepted: clean up what it left
                raise
            created = True
            vm = client.wait_ready(vm.id)
            print("created:", vm.name, vm.id, vm.state)
            result = client.run(vm.id, ["uname", "-a"], deadline_seconds=60)
            sys.stdout.write(result.stdout.decode(errors="replace"))
            if result.exit_code != 0:
                sys.stderr.write(result.stderr.decode(errors="replace"))
                print(f"uname exited {result.exit_code}", file=sys.stderr)
                return 1
            client.delete_vm(vm.id)
            created = False
            print("deleted:", name)
            return 0
        except DavitError as err:
            print(f"{err.code}: {err.message}", file=sys.stderr)
            return 1
        finally:
            if created:
                try:  # best effort: do not leave the VM behind
                    client.delete_vm(name)
                except NotFound:
                    pass
                except DavitError as err:
                    print(f"cleanup failed: {err.code}: {err.message}", file=sys.stderr)


if __name__ == "__main__":
    sys.exit(main(sys.argv))
