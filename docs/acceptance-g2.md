# Acceptance: laptop → host over Tailscale

Status: **not yet run**. It needs a Tailscale auth key for the host and a second machine on the same tailnet.
The hermetic proof (`TestRemoteHTTPSOverTailnet`, see [network.md](network.md#remote-access-one-configuration-for-cli-ssh-and-sdks))
runs on every `make check`. It does not replace this run: it uses an in-process control server, an operator
certificate from a throwaway CA, and a loopback relay instead of a real laptop.

Record every result in the table at the end. Do not report a latency number without its sample count and both
machines' hardware.

## Machines

- **Host:** a real Linux box (Ubuntu 24.04 to cover AppArmor) with KVM and a ready golden image (installed from a release).
  davit is installed and `davit doctor` is green (the `tailnet_*` checks skip while the gateway is off).
- **Laptop:** macOS or Linux, signed in to the same tailnet with `tailscale up`, with the davit CLI from the same
  release. Optional: Python ≥ 3.10 and Node ≥ 20 for the SDK examples.
- **CI identity:** a tagged node (e.g. `tag:ci`) or a second tailnet user, which is *not* an owner.

## 1. Enroll the host

On the host, with a reusable or one-off auth key (tailnet admin console → Settings → Keys) and HTTPS certificates
enabled (admin console → DNS → HTTPS Certificates):

```sh
davit init --auth-key-stdin --owner-login you@example.com <<<"tskey-auth-…"
# config: "tailnet": {"https": true, …} (or tlsCertFile/tlsKeyFile for an operator certificate)
systemctl --user restart davit        # or: davit daemon
davit doctor -v                       # -v lists passing checks too
```

Pass: `tailnet_gateway` is `pass` and prints `DAVIT_API_URL=https://<name>.<tailnet>.ts.net`; `tailnet_tls` is
`pass`; `apparmor` is `pass` on Ubuntu. Record the URL.

Diagnostics to exercise once (each must fail with the fix named):

| Do this | Expect from `doctor` |
|---|---|
| Start without a key and without saved state | `tailnet_gateway` fail, hint with the login URL |
| Enable device approval in the tailnet, enroll a fresh state dir | `needs-approval`, hint "approve <hostname>" |
| Use an expired or revoked auth key on a fresh state dir | `error`, hint "the auth key was rejected" |
| `"https": true` with HTTPS certificates disabled in the tailnet | `error`, hint "enable HTTPS certificates" |

## 2. Owner laptop: CLI, SSH, SDKs

On the laptop:

```sh
export DAVIT_API_URL=https://<name>.<tailnet>.ts.net
unset DAVIT_TOKEN DAVIT_CA_FILE
davit status                                   # every check pass
davit new g2-box
davit exec g2-box -- uname -a
davit ssh g2-box -- echo over-ssh
davit scp ./README.md g2-box:/tmp/README.md
davit forward g2-box 8080 &                    # then curl http://127.0.0.1:8080 to a guest server
ssh g2-box@<name>.<tailnet>.ts.net true          # plain OpenSSH, the front door
```

SDK examples, from a checkout of the same release:

```sh
(cd sdk/python && python3 -m pip install -e . && python3 examples/remote_https.py g2-sdk-py)
node sdk/typescript/examples/remote-https.ts g2-sdk-ts   # Node >= 22.18 runs the TypeScript source
```

Pass: every command exits 0, and each example prints the engine, the VM list and the guest's `uname -a`, then
deletes its VM.

## 3. CI identity: token, denial, revocation

On the host: `davit token create g2-ci --action exec --ttl 1d` and copy the `slw_…` secret.

On the CI machine (not an owner):

```sh
export DAVIT_API_URL=https://<name>.<tailnet>.ts.net
unset DAVIT_TOKEN
davit status            # FAIL api, fix: add the login to tailnet.ownerLogins … or use a service token
export DAVIT_TOKEN=slw_…
davit status            # pass
davit exec g2-box -- true
davit ssh g2-box -- true
```

Then deny both ways:

1. **Revoked token.** On the host `davit token revoke <id> --yes`. On CI `davit status` fails `api` with
   "it is invalid, expired or revoked"; `davit list` exits non-zero with `unauthenticated`.
2. **Revoked peer.** In the admin console, remove (or expire the key of) the CI machine, with a new token
   still set. On CI every command fails (unreachable); on the host the daemon log shows no request from it.
   Re-add the machine and confirm `status` passes again.

## 4. First-command latency

From the laptop, with the host idle and its golden image cached:

```sh
scripts/g2-first-command.sh -n 20 -o g2-latency-laptop.tsv
```

It prints p50/p95 (nearest rank, milliseconds) for `new`, the first `exec` after `new`, the two together (the
SPEC target: first authenticated command **< 1 s p50, < 2 s p95**), and a warm `exec`. Run it once as the owner
and once with `DAVIT_TOKEN` from CI. Keep the TSV files.

## Results

| Date | Host (CPU, RAM, OS, kernel) | Client (CPU, OS) | Path (RTT from `tailscale ping`, direct/DERP) | Identity | Samples | new+first p50 / p95 | first exec p50 / p95 | warm exec p50 / p95 | Pass |
|---|---|---|---|---|---|---|---|---|---|
| – | – | – | – | owner | – | – | – | – | not run |
| – | – | – | – | token | – | – | – | – | not run |

Checklist: enrollment diagnostics ☐ owner CLI/SSH/SDKs ☐ non-owner refused ☐ token works ☐ revoked token denied ☐
revoked peer denied ☐ AppArmor host ☐ latency recorded ☐
