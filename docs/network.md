# Networking and the tailnet gateway

Two packages, two separate concerns:

- **Guest egress**: `internal/network` (rootless provider, per-VM ruleset) and `internal/network/privileged` (the root helper boundary, which only renders commands in this build). This is how a guest reaches the internet, and nothing else uses it.
- **Access to VMs**: `internal/gateway`, an embedded tsnet node on the host. The HTTP API, SSH and forwards all arrive here. The gateway reaches guests over vsock through `GuestDialer`, never through the guest network, so a VM with egress off or broken is still fully reachable.

| Mode | Root? | Egress | LAN/host/metadata deny enforced by |
|---|---|---|---|
| rootless (`network.Rootless`) | no | pasta, else slirp4netns (both only with nft), else the built-in userspace stack | nft table inside the VM's own user-owned netns, plus the userspace stack's own per-flow check |
| privileged (`internal/network/privileged`) | yes, through the narrow helper | veth + NAT out of the uplink | nft in the VM netns **and** the host `inet davit` table |
| none (`network.None`) | no | none | n/a (vsock only) |

On top of that deny list, each VM has an egress mode (`off`, `filtered`, `allowlist`; see [Egress modes](#egress-modes)).

Per-VM namespace code: `internal/network` (contract, rootless provider, guest ruleset) and
`internal/network/privileged` (root helper boundary, render-only).

## Model

Every VM runs in its own network namespace (netns), and every netns looks the same from the guest's side:

| | Value (constant in every netns) |
|---|---|
| TAP device | `tap0`, host side `172.16.0.1/30`, MTU 1500 |
| Guest address | `172.16.0.2/30`, gateway `172.16.0.1` |
| Guest MAC | `06:00:AC:10:00:02` |
| Egress interface inside the netns | `up0` |

A fork restored from a snapshot finds exactly the network it was snapshotted with, so it needs no re-addressing (SPIKE finding 5). Host-to-guest control traffic (SSH gateway, guest agent, exec) goes over vsock and never touches this network. The netns exists only to give the guest egress, and if egress is missing or broken, the VM still works over vsock.

Host-visible names are built from `ShortID = hex(sha256("<vmID>/<generation>"))[:10]`, because Linux interface names are limited to 15 bytes: the netns is `slw-<id>` and the host veth is `slw<id>h` (14 bytes). VM IDs must match `^[a-z0-9][a-z0-9_-]{0,62}$`; anything else is rejected, never escaped. Because the generation is part of the ID, cleanup for a stale run cannot touch the run that replaced it. The `slw` interface prefix is reserved on the host.

## Egress policy (both modes)

This is the host-level destination policy (`network.EgressPolicy`, from the daemon's config). The per-VM egress mode below decides how it is used. Rules are evaluated in this order:

1. Drop `HostAddrs`, the host's own management addresses. Nothing can override this.
2. Accept operator-permitted LAN `Services` (exact address, transport and port; see [LAN services](#lan-services)).
3. Accept explicit admin `Allow` exceptions (whole prefixes, every port).
4. Drop the `Deny` list.
5. Accept everything else (the internet).

The default deny list is `0.0.0.0/8`, `10.0.0.0/8`, `100.64.0.0/10` (CGNAT/tailnet), `127.0.0.0/8`, `169.254.0.0/16` (including metadata `169.254.169.254`), `172.16.0.0/12`, `192.0.0.0/24`, `192.168.0.0/16`, `198.18.0.0/15`, `224.0.0.0/4`, `240.0.0.0/4`, `::/128`, `::1/128`, `::ffff:0:0/96`, `64:ff9b::/96` and `64:ff9b:1::/48` (NAT64), `2001::/32` (Teredo), `2002::/16` (6to4), `fc00::/7`, `fe80::/10` and `ff00::/8`.

Each VM netns gets the table `inet davit_guest`, rendered by `network.RenderGuestRuleset(cfg, policy, stackEnforces)` for the VM's egress mode (`stackEnforces` is true when the userspace backend serves egress). In `filtered` mode it is the same in both modes:

- **forward** (policy drop): accepts established/related traffic. Otherwise it accepts only `tap0 -> up0` traffic whose source is `172.16.0.2` (or a declared [guest subnet](#docker-and-other-bridges-in-the-guest)), so a spoofed source address is dropped. That traffic then goes through the policy above.
- **input:** guest-to-netns traffic is dropped (established/related is allowed back), except DNS to the resolvers below and [host services on the TAP](#host-services-on-the-tap-tapservices).
- **postrouting:** masquerades `172.16.0.2` (and declared guest subnets) out of `up0`.

### LAN services

The LAN stays denied, but the operator can permit individual services on it in the daemon config:

```json
{ "network": { "lanServices": ["tcp/192.168.1.10:5432", "udp/192.168.1.20:5353"] } }
```

Each entry is one IPv4 address, one transport and one port (`network.ParseService`); nothing else on that host, and no other port, becomes reachable. They are rendered as the concatenation set `svc4` (`ipv4_addr . inet_proto . inet_service`), matched by `ip daddr . meta l4proto . th dport @svc4 accept` right after the host-address drop, in the guest table and in the privileged host table alike. The userspace stack applies the same entries per flow (`userspace.Policy.Allows`), so all backends agree.

- Refused at config load: loopback, link-local (cloud metadata), CGNAT (tailnet peers), multicast/reserved and "this network" addresses, port 0, and port 53 (guest DNS is always answered by the built-in resolvers). IPv6 and names are refused too.
- A host-management address stays denied even if listed (the daemon logs a warning). Reaching a service on the host or the laptop is a [reverse connection](#reverse-connections-internalnetworkreverse), not egress.
- `off` reaches no service. In `allowlist` mode a service also needs its address on the VM's own allowlist (the operator permits, the VM owner opts in); the guest table orders the `svc4` accept after the `@vmallow4`/`@pinned4` check for exactly that.

### Host services on the TAP (TapServices)

The host can serve TCP services to one VM's guest only, at the netns side of its TAP (`172.16.0.1`), for example the credential proxy at `http://172.16.0.1:PORT`. `Config.TapServices` (`tapServices`, a list of ports) names them; each must be nonzero, unique and not 53 (guest DNS).

- **Rule:** the guest table's input chain accepts `iifname "tap0" ip saddr <guest sources> ip daddr 172.16.0.1 tcp dport <ports> accept` (one rule, ports sorted; a set when there are several) right before the final `iifname "tap0" drop`. The guest sources are the guest address plus any declared guest subnets, as for egress. Nothing else from the guest reaches the netns: no other port, no UDP, no ICMP.
- **Independent of the egress mode:** these are host services, not egress, so the rule is rendered in `off`, `filtered` and `allowlist` alike, in both rootless and privileged mode (same renderer). It matches every packet of the flow, so `off`, which has no established/related accept in input, needs none (tested).
- **Listener (rootless):** `Rootless.ListenTCP(ctx, h, port)` (the optional `network.TapListener` interface, type-asserted like `Reaper`; `None` does not implement it) runs `davit netns-listenfd 172.16.0.1:PORT` inside the VM's namespaces (`EnterArgv(h)` + the davit binary, registered in `nethelper.Serve`). It binds the socket there and sends the listening fd back over a socketpair (fd 3, `SCM_RIGHTS`), then exits. The daemon accepts on that socket from the host netns; no forwarding process remains. The port must be listed in `TapServices`.
- **Isolation:** every VM has its own netns, so a listener bound in one VM's netns is reachable only from that VM's guest (and from inside that netns). Another VM's guest sees its own, empty `172.16.0.1`, and the host netns cannot connect at all (tested).
- **Lifetime:** an open listener holds a reference to its netns. The daemon must close it when the VM's network is destroyed, or the namespace and its TAP outlive the holder.

### Docker and other bridges in the guest

Docker in the guest masquerades its bridges to the guest address (its default iptables `MASQUERADE`), so container traffic reaches the VM netns as the guest's own and gets exactly the VM's policy: mode, deny list, LAN services, and the allowlist resolver (container DNS to any address is answered or DNAT'd like the guest's).

A guest that routes a bridge without masquerading (Docker with `ip-masq` off, nested VMs) sends packets from addresses the netns does not know; anti-spoofing drops them all (fail closed). To give such a subnet the same policy, declare it:

```json
{ "network": { "guestSubnets": ["172.17.0.0/16"] } }
```

Entries must be private IPv4 prefixes that do not overlap the netns link (`172.16.0.0/30`), the userspace uplink (`10.0.2.0/24`) or, in privileged mode, the uplink pool (`10.200.0.0/16`). For each one the netns gets `ip route add <subnet> via 172.16.0.2 dev tap0`, and the table accepts and masquerades `ip saddr { 172.16.0.2, <subnets> }`.

Guest egress is IPv4-only for now: the ruleset includes `meta nfproto ipv6 drop`. The IPv6 sets are still rendered so the policy is complete when IPv6 is turned on.

There is no DHCP service for the guest; it is statically addressed (image: DNS 1.1.1.1). In `filtered` mode with pasta or slirp4netns the guest must use public resolvers; the slirp4netns DNS address 10.0.2.3 sits inside 10/8 and is denied. The userspace backend answers DNS (UDP/TCP port 53, whatever server the guest asks) through the host's resolvers, so with it the table also accepts `udp/tcp dport 53` before the policy rules.

## Egress modes

A VM's `domain.Egress` (`Handle.Policy`) picks one of three modes. Changing it on a running VM (`Provider.SetEgress`) replaces the whole guest table in one `nft -f -` transaction (`network.RenderGuestReplace`: `table inet davit_guest {}`, `delete table inet davit_guest`, then the new table), so packets never see a gap and a failed load leaves the old table in force.

| Mode | Meaning |
|---|---|
| `off` | Nothing leaves the VM, DNS included. The forward chain drops everything from `tap0`, established flows too; input from `tap0` is dropped except [TapServices](#host-services-on-the-tap-tapservices). |
| `filtered` (default) | The internet, minus the policy above (host addresses, LAN, link-local/metadata, CGNAT/tailnet, ...). |
| `allowlist` | Only `Allow` entries, **and** the filtered rules still apply: an allowlisted CIDR never reaches a denied range or a host address. Names are resolved by a filtering resolver that answers NXDOMAIN for anything else and pins the answers' addresses for their TTL (clamped to 60 s .. 1 h, `dnsallow.MinPin`/`MaxPin`). |

Who enforces each mode:

| Backend | `off` | `filtered` | `allowlist` |
|---|---|---|---|
| userspace (rootless) | stack + nft table (when `nft` is present) | stack + nft table | the stack: its own resolver, pins and per-flow check (`userspace.SetPolicy` switches it live); the nft table is the filtered one plus DNS |
| pasta, slirp4netns (rootless, need `nft`) | nft table | nft table | nft table (`@vmallow4`/`@pinned4`) + the in-netns resolver `davit netns-dns` |
| veth-nat (privileged, render-only) | nft table + host table | nft table + host table | nft table + resolver (a helper daemon) + host table |

The allowlist table for the nft-only backends (`internal/network/testdata/guest_allowlist.nft`):

- `set vmallow4` (interval) holds the allowlist's CIDRs; `set pinned4` (`flags timeout`) is filled at runtime by the resolver, one element per pinned address with the name as its comment.
- **forward:** replies (`up0 -> tap0`, established/related) are accepted. Guest-originated packets pass the anti-spoofing checks, then: established/related flows carrying conntrack mark `0x1` are accepted; host addresses drop; a destination in neither `@vmallow4` nor `@pinned4` drops; then the policy above, and every accepted new flow gets mark `0x1`. Only flows admitted under an allowlist table are marked, so switching from `filtered` to `allowlist` also cuts established flows to destinations the allowlist does not admit, while flows to admitted ones continue (tested). An expired pin does not cut an established marked flow.
- **prerouting (nat):** guest `udp/tcp dport 53` to any address is DNAT'd to `172.16.0.1:53`, the resolver.
- **input:** accepts exactly `172.16.0.2 -> 172.16.0.1` udp/tcp port 53 from `tap0` (plus established/related and TapServices); everything else from the guest drops.

### The in-netns resolver (`internal/network/netnsdns`)

The helper commands (`netns-tapfd`, `netstack-egress`, `netns-dns`, `netns-listenfd`) re-execute the running binary, so its `main` (or a test binary's `TestMain`) must call `nethelper.Serve()` first. Without it the provider refuses to re-execute and reports the backend unavailable: any other binary would misread the command, and a Go test binary would run its whole suite inside the VM's namespaces.

`davit netns-dns <config json>` runs inside the VM's namespaces (`EnterArgv(h)` + the davit binary, its own session, stdout/stderr `/dev/null`), so it needs no root and its own upstream traffic leaves through `up0` like the backend's. It:

- listens on `172.16.0.1:53` UDP and TCP and writes `1` to fd 3 when ready (or the startup error, then exits);
- answers NXDOMAIN for names `dnsallow` does not allow, without forwarding them, and drops malformed queries;
- forwards allowed queries (UDP or TCP, as the guest asked) to the upstreams the daemon computed: the non-loopback IPv4 `nameserver`s of the host's `/etc/resolv.conf`, else `1.1.1.1` (systemd-resolved's `127.0.0.53` is not reachable from the netns). A response must match the query's ID and question;
- before replying, pins every A address of the answer: one `nft -f -` batch of `add element inet davit_guest pinned4 { <ip> timeout <ttl>s comment "<name>" }`, `delete element ...`, `add element ...` per address (a plain `add` would keep an existing element's old timeout). A pin never shrinks while the resolver runs. If pinning or forwarding fails, the guest gets SERVFAIL, never an answer it cannot use;
- exits when the namespace holder exits (pidfd plus start-time check; it would otherwise keep the netns alive), and on SIGTERM.

The handle records it as `Handle.DNSProc`; `Destroy` stops it with the other processes.

### SetEgress (rootless)

`Rootless.SetEgress(ctx, h, policy)` checks the holder is alive, then:

1. On an nft-only backend, stops the running resolver (re-applying the same allowlist with a live resolver is a no-op) and reads the pins, so no answer under the old list is pinned into the new table.
2. Replaces the table atomically. Allowlist to allowlist carries over the pins whose comment name the new list still allows, with their remaining time.
3. Userspace backend: `userspace.SetPolicy(ctx, h.EgressControl, mode, allow)`.
4. nft-only backend in `allowlist` mode: starts a resolver with the new list. Before any resolver starts, stray `netns-dns` processes in the same netns (found through `/proc/*/ns/net`) are stopped, so one left by a daemon that died before persisting its handle cannot hold port 53.

If step 2, 3 or 4 fails, the previous table (with its pins) is restored, the previous resolver restarted if there was one, and the error returned: the previous policy stays in force. If the restart itself fails, guest DNS fails, which is stricter than either policy.

## Rootless mode (`network.Rootless`, Linux)

Requirements:

- unprivileged user namespaces
- read/write access to `/dev/net/tun`
- `unshare`, `nsenter` and `ip` (`nft` and `sysctl` for egress)

`Probe` actually creates a throwaway namespace and reports the reason if that fails.

Create, with no root and no named netns:

1. Start a holder process: `unshare --user --map-root-user --net -- sleep infinity`. It runs in its own session, so it outlives the daemon.
2. The handle records the holder's PID plus its `/proc` start time, so it is never identified by PID alone. It also exposes `/proc/<pid>/ns/{net,user}`.
3. Inside the namespace (`nsenter --preserve-credentials --user=… --net=… --`):
   - `ip link set dev lo up`
   - `ip tuntap add dev tap0 mode tap`
   - `ip addr add 172.16.0.1/30 dev tap0`
   - `ip link set dev tap0 mtu 1500 up`
4. Egress backend: with `nft`, pasta if it is on PATH, otherwise slirp4netns; otherwise (or without `nft`, since those two cannot filter on their own) the built-in userspace stack.
5. If `nft` is present, run `nft -f -` with the guest ruleset for the VM's egress mode and backend.
6. Start the backend (in every mode, `off` included, so `SetEgress` can relax it later). Only if that cannot start is egress off, and `Egress.Reason` says why. `Egress.Enabled` means a backend is attached; `Policy.Mode` decides what may leave.
7. pasta/slirp4netns in `allowlist` mode: start the resolver. If it cannot start, `Egress.Reason` says so and guest DNS fails (only allowlisted CIDRs are reachable).

`Enter(h)` returns the `nsenter` argv prefix the runtime uses to start the VMM inside the netns. `ListenTCP(ctx, h, port)` returns a listener bound to `172.16.0.1:port` inside the netns (see [Host services on the TAP](#host-services-on-the-tap-tapservices)). `Destroy` stops the resolver, the egress process and the holder; the kernel then frees the netns, TAP and ruleset. Destroy is idempotent, and it never signals a process whose start time doesn't match the handle.

Leak recovery: if `Config.StateDir` is set (an absolute path), each Create makes `<StateDir>/h-<ShortID>-<random>` and starts the holder and the egress process with that directory as cwd. The handle records it as `Handle.StateDir`, and Destroy removes it once the processes are gone. `Rootless.Reap(ctx, keep)` (the optional `network.Reaper` interface) scans `/proc/*/cwd` and SIGKILLs every live process inside StateDir that isn't a holder or egress process (PID and start time) of a handle in `keep`. The signal goes through a pidfd and the start time is checked again first. Reap then waits for the processes to exit, removes their directories and any empty unreferenced ones, and returns how many it killed. It never touches processes outside StateDir. It covers holders leaked by a crash between Create and the caller persisting the handle, and leaked golden-capture networks. Run it once at daemon start, before any Create, because it would kill the holders of Creates still in flight. pasta may change its cwd when it daemonizes, so Reap doesn't count on finding it; it exits by itself once its holder is gone.

What each backend guarantees by itself:

| Backend | argv (`network.PastaArgv` / `Slirp4netnsArgv`) | Guarantees on its own |
|---|---|---|
| pasta | `pasta --quiet --config-net --ipv4-only --no-map-gw --tcp-ports none --udp-ports none --tcp-ns none --udp-ns none --ns-ifname up0 --pid <file> --userns /proc/<pid>/ns/user --netns /proc/<pid>/ns/net` | The gateway is not mapped to host loopback. There is no inbound port forwarding and no forwarding to host loopback ports (pasta defaults those to `auto`, so `none` has to be explicit). pasta exits when the netns goes away. |
| slirp4netns | `slirp4netns --configure --mtu=65520 --disable-host-loopback --ready-fd=3 --netns-type=path --userns-path=/proc/<pid>/ns/user /proc/<pid>/ns/net up0` | 10.0.2.2 does not reach host 127.0.0.1. `--enable-sandbox` is left out because it fails when `/etc/resolv.conf` links outside /etc or /run. |
| userspace | inside the netns: `ip tuntap add dev up0 mode tap`, `ip addr add 10.0.2.15/24 dev up0`, default route via 10.0.2.2; then `davit netns-tapfd up0` (in the namespaces) hands the TAP fd over a socketpair to `davit netstack-egress <config>` (host netns, own session) | A gVisor netstack (`internal/network/userspace`) at 10.0.2.2 turns guest TCP/UDP into host sockets and checks every destination against the same policy (host addresses, then allows, then the deny list) before connecting, so LAN/host/metadata/tailnet stay denied even without nft (TCP gets a reset, UDP is dropped). It exits when the namespace holder exits (watched with a pidfd), because its open TAP fd would otherwise keep the namespace alive; `Destroy` also stops it. IPv4 TCP and UDP only, no ICMP. |

Neither pasta nor slirp4netns blocks the host's LAN addresses, the LAN, tailnet CGNAT addresses or cloud metadata: both turn guest packets into host `connect()` calls. Rootless LAN-deny therefore depends entirely on the in-netns nftables table. Here the namespace owner holds CAP_NET_ADMIN, so the table needs no root; it cannot be enforced in the host netns.

**Ubuntu 23.10 and later:** `kernel.apparmor_restrict_unprivileged_userns=1` blocks unprivileged user namespaces by default. `davit init --apparmor` provides a profile scoped to the davit binary; see [Ubuntu (AppArmor)](#ubuntu-apparmor). Disabling the sysctl host-wide is not a supported fix.

## Ubuntu (AppArmor)

Ubuntu turns on `kernel.apparmor_restrict_unprivileged_userns=1` by default from 24.04 (the knob was introduced in 23.10). With it on, a process whose AppArmor profile does not grant `userns` still gets its user namespace, but without capabilities inside it: the kernel moves the process into the `unprivileged_userns` profile, which denies every capability. The rootless provider then cannot build the VM network. `doctor` reports this in the `userns` and `apparmor` checks.

The fix follows the pattern Ubuntu uses for its own userns profiles (for example `/etc/apparmor.d/chrome`): a profile that confines nothing (`flags=(unconfined)`), attaches to one binary path and adds `userns,`. `davit init --apparmor` renders it for the real path of the running binary (symlinks resolved, because AppArmor matches the resolved path at exec). `--apparmor-binary PATH` selects a different binary. The profile is named `davit-` plus the first 12 hex digits of the SHA-256 of that path (`packaging.AppArmorProfileName`), so each binary path has its own profile and file: two users who each install `~/.local/bin/davit` on one host never replace each other's profile. Example (`packaging/testdata/apparmor-davit.golden`):

```text
abi <abi/4.0>,
include <tunables/global>

profile davit-778380318dd9 /usr/local/bin/davit flags=(unconfined) {
  userns,

  # Site-specific additions and overrides. See local/README for details.
  include if exists <local/davit-778380318dd9>
}
```

The template is `packaging/apparmor/davit.tmpl`, embedded in the binary. The path must be absolute, clean and printable ASCII. A path with spaces is double-quoted. Characters that AppArmor reads as globs, variables, escapes or syntax (`" \ * ? [ ] { } ^ @ # ,`) are rejected rather than escaped, so the profile always names exactly one file.

### Why one profile on the davit binary is enough

- AppArmor's `userns` rule is checked against the profile of the task that creates the namespace. Here that is the `unshare --user --map-root-user --net` holder that davit starts, plus `doctor`'s probe, which clones the namespace from the davit process itself.
- A `flags=(unconfined)` profile does not restrict exec. A program it starts keeps the profile unless a profile of its own attaches to that program's path. Ubuntu deliberately ships no profile for `/usr/bin/unshare`, because one would give every user a trivial bypass ([LP #2036128](https://bugs.launchpad.net/ubuntu/+source/apparmor/+bug/2036128)). So the `unshare` davit starts runs under davit's profile and may create the namespace. [Qualys' 2025 advisory](https://seclists.org/oss-sec/2025/q1/253) shows the same inheritance: `/usr/bin/unshare` started from busybox's unconfined `userns` profile got full capabilities.
- `nsenter` only joins existing namespaces (setns), and the `userns` rule mediates creation only. `ip` and `nft` run inside the namespace through `nsenter`. Firecracker is exec'd by `nsenter`.
- pasta: Ubuntu's `passt` package ships its own profile for pasta, so pasta runs under that profile, not davit's. pasta joins the holder's namespaces (`--userns`, `--netns`) and does not create one. This has not been tested on Ubuntu.

Rejected alternatives:

- A profile for `/usr/bin/unshare`: it would grant userns to every user and every program on the host, which is the bypass Ubuntu refuses to ship.
- Turning the sysctl off: it affects the whole host.
- Making davit create the namespace itself instead of exec'ing `unshare`: it would work under the same profile, but it changes the network provider and gains nothing for the restriction.

The trade-off: everything davit executes runs unconfined and may create user namespaces, which is what davit needs. The profile covers whatever file sits at that path. If the binary or its directory is writable by you (for example `~/.local/bin`), any program running as you can replace it and inherit the permission, which amounts to lifting the restriction for your user. `init --apparmor` warns in that case. For the tight setup, install the binary root-owned (`sudo install -m 0755 davit /usr/local/bin/davit`) and run `/usr/local/bin/davit init --apparmor`.

If a future Ubuntu release attaches a profile to `/usr/bin/unshare`, that exec would leave davit's profile. The `network` check in `doctor` creates a real namespace through `unshare`, so it would show the failure.

### One-time setup

`init --apparmor` runs nothing as root and writes only `<config dir>/apparmor/<profile name>`, as you. It prints the profile and these commands (`--json`: `profile`, `profileFile`, `installPath`, `commands`, `removeCommands`, `restricted`, `attached`, `warnings`):

```sh
davit init --apparmor
sudo install -m 0644 -o root -g root ~/.config/davit/apparmor/davit-778380318dd9 /etc/apparmor.d/davit-778380318dd9
sudo apparmor_parser -r /etc/apparmor.d/davit-778380318dd9
systemctl --user restart davit    # profiles attach at exec; a running daemon keeps its old label
davit doctor                      # apparmor and userns now pass
```

(The printed lines have the real, shell-quoted path of the rendered file and the profile name for your binary; `/usr/local/bin/davit` gives `davit-778380318dd9`.) `davit doctor --fixes` renders the profile the same way and prints the two `sudo` lines when the `apparmor` check fails. `apparmor_parser -r` loads or replaces the profile, and the file in `/etc/apparmor.d` is loaded again at boot. Host-specific rules go in `/etc/apparmor.d/local/<profile name>`. After moving or renaming the binary, run `init --apparmor` again and repeat the two `sudo` lines; the new path gets a new profile name, so remove the old one.

`install.sh --host` prints these `sudo` lines (from `davit doctor --fixes`) and runs them with `--yes-sudo`. Plain `davit init` adds `init --apparmor` as a next step.

Removal:

```sh
sudo apparmor_parser -R /etc/apparmor.d/davit-778380318dd9    # the name init --apparmor printed
sudo rm /etc/apparmor.d/davit-778380318dd9
```

### What `doctor` checks

`apparmor` reads `/proc/sys/kernel/apparmor_restrict_unprivileged_userns`, its own AppArmor label (`/proc/self/attr/apparmor/current`, else `/proc/self/attr/current`) and, when readable, the kernel's profile list (`/sys/kernel/security/apparmor/profiles`):

| State | Result |
|---|---|
| no restriction (knob absent or `0`) | skip |
| `doctor` runs as `davit-<hash> (unconfined)`, the name for its own path | pass: a loaded profile attaches to this binary's path. A process's own label is the one rootless proof of that. |
| restriction on, other label | fail (warn for the fake runtime). The detail says whether this binary's profile is loaded (then the process started before it: restart the daemon), is not loaded, or, when the profile list is root-only as it usually is, that this is unknown. |
| label unreadable | fail (warn), with the hint |

The `userns` check keeps its meaning: the restriction fails it unless the davit profile is attached, and then the real namespace probe decides.

Not verified: none of this has run on an Ubuntu host, and `apparmor_parser` is not installed on the dev host. The profile is checked against golden files, and the test runs `apparmor_parser -Q -K` (parse only, no kernel load, no cache) when the parser is on PATH.

## Privileged mode (`internal/network/privileged`)

A narrow root helper accepts only these typed requests:

- `EnsureHost{Uplink}`
- `CreateVMNet{VMID, Generation, Index, TapOwner, Egress}` (a zero `Egress` means `filtered`)
- `SetVMEgress{VMID, Generation, Index, Egress}`
- `DestroyVMNet{VMID, Generation, Index}`

It validates them (VM ID, generation ≥ 1, index 1..16383, uplink interface name not using `slw`, egress policy through `domain.Egress.Normalize`) and turns them into a fixed plan of argv commands; there is never a shell and never a caller-supplied string beyond the validated fields. The guest config and egress policy come from the helper's own configuration, not from requests.

Uplink links are /30 blocks of `10.200.0.0/16`. For index `i`, the host end is `10.200.0.0 + 4i + 1` and the netns end is `+2`. This build is render-only: the `Executor` interface is how a root helper would run a plan, `DryRun` records plans, and nothing in the package runs sudo.

The exact commands below are copied from the golden files in `internal/network/privileged/testdata`. The example is VM `vm_example`, generation 1, index 1, uplink `eth0`, host management address `192.168.1.10`.

`EnsureHost{Uplink: "eth0"}` runs once at helper start and is safe to repeat. Re-applying it swaps chains and policy sets in one atomic transaction and keeps the `vm_src` elements of running VMs:

```text
sysctl -q -w net.ipv4.ip_forward=1
nft -f -   # stdin:
```
```nft
add table inet davit
add set inet davit vm_src { type ifname . ipv4_addr; }
add set inet davit host4 { type ipv4_addr; flags interval; auto-merge; }
flush set inet davit host4
add element inet davit host4 { 192.168.1.10/32 }
add set inet davit host6 { type ipv6_addr; flags interval; auto-merge; }
flush set inet davit host6
add set inet davit allow4 { type ipv4_addr; flags interval; auto-merge; }
flush set inet davit allow4
add set inet davit allow6 { type ipv6_addr; flags interval; auto-merge; }
flush set inet davit allow6
add set inet davit deny4 { type ipv4_addr; flags interval; auto-merge; }
flush set inet davit deny4
add element inet davit deny4 { 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12, 192.0.0.0/24, 192.168.0.0/16, 198.18.0.0/15, 224.0.0.0/4, 240.0.0.0/4, 10.200.0.0/16 }
add set inet davit deny6 { type ipv6_addr; flags interval; auto-merge; }
flush set inet davit deny6
add element inet davit deny6 { ::/128, ::1/128, ::ffff:0.0.0.0/96, 64:ff9b::/96, 64:ff9b:1::/48, 2001::/32, 2002::/16, fc00::/7, fe80::/10, ff00::/8 }
add chain inet davit vm_out
flush chain inet davit vm_out
add rule inet davit vm_out ct state established,related accept
add rule inet davit vm_out ct state invalid drop
add rule inet davit vm_out iifname . ip saddr != @vm_src drop
add rule inet davit vm_out oifname != "eth0" drop
add rule inet davit vm_out ip daddr @host4 drop
add rule inet davit vm_out ip6 daddr @host6 drop
add rule inet davit vm_out ip daddr @allow4 accept
add rule inet davit vm_out ip6 daddr @allow6 accept
add rule inet davit vm_out ip daddr @deny4 drop
add rule inet davit vm_out ip6 daddr @deny6 drop
add rule inet davit vm_out accept
add chain inet davit forward { type filter hook forward priority filter; policy accept; }
flush chain inet davit forward
add rule inet davit forward iifname "slw*" jump vm_out
add rule inet davit forward oifname "slw*" ct state established,related accept
add rule inet davit forward oifname "slw*" drop
add chain inet davit input { type filter hook input priority filter; policy accept; }
flush chain inet davit input
add rule inet davit input iifname "slw*" drop
add chain inet davit postrouting { type nat hook postrouting priority srcnat; policy accept; }
flush chain inet davit postrouting
add rule inet davit postrouting oifname "eth0" ip saddr 10.200.0.0/16 masquerade
```

What the host table guarantees:

- A VM veth whose `(ifname, source)` pair is not registered in `vm_src` is dropped, so an unregistered VM fails closed.
- Traffic may leave only through the uplink: no VM-to-VM traffic and no `tailscale0`/`docker0`.
- The policy above applies, and the whole uplink pool is also denied.
- New connections into VM veths are dropped.
- VM-to-host input is dropped.
- Traffic is NATed once in the netns (`172.16.0.2` → `10.200.x.y`) and once on the host (→ the uplink address).

Other firewalls with a forward policy of drop (ufw, Docker's `DOCKER-USER`/iptables-nft chains) still apply. Such a firewall can block VM egress, and our accept verdict cannot override it.

`CreateVMNet{VMID: "vm_example", Generation: 1, Index: 1}`:

```text
ip netns add slw-07534e10c0
ip link add slw07534e10c0h type veth peer name up0 netns slw-07534e10c0
ip addr add 10.200.0.5/30 dev slw07534e10c0h
ip link set dev slw07534e10c0h up
ip -n slw-07534e10c0 link set dev lo up
ip -n slw-07534e10c0 addr add 10.200.0.6/30 dev up0
ip -n slw-07534e10c0 link set dev up0 up
ip -n slw-07534e10c0 route add default via 10.200.0.5 dev up0
ip -n slw-07534e10c0 tuntap add dev tap0 mode tap        # + "user <uid>" when TapOwner != 0
ip -n slw-07534e10c0 addr add 172.16.0.1/30 dev tap0
ip -n slw-07534e10c0 link set dev tap0 mtu 1500 up
ip netns exec slw-07534e10c0 sysctl -q -w net.ipv4.ip_forward=1
ip netns exec slw-07534e10c0 nft -f -                     # stdin: the inet davit_guest table for the mode (see testdata/create_vm.txt)
ip netns exec slw-07534e10c0 davit netns-dns {...} &    # allowlist only (testdata/create_vm_allowlist.txt)
nft -f -                                                  # stdin: add element inet davit vm_src { "slw07534e10c0h" . 10.200.0.6 }
```

The VM is registered in `vm_src` last, so the host forward path opens only after the netns ruleset (and, for an allowlist, the resolver) is in place. If any step fails, the helper runs the destroy plan.

A `Command` with `Daemon` set (rendered with a trailing `&`) is the allowlist resolver: the helper starts it in the background once it reports ready, supervises it, and stops the VM's daemons before running its `SetVMEgress` or `DestroyVMNet` plan. In privileged mode it gets no holder to watch and uses `1.1.1.1:53` as upstream (the host table denies LAN resolvers to VM traffic anyway). Its own upstream traffic leaves through the veth and the host table like the guest's.

`SetVMEgress` (`PlanSetEgress`, goldens `set_egress_off.txt`, `set_egress_allowlist.txt`) is the same tail: `ip netns exec <ns> nft -f -` with the atomic replace form of the table, then, for an allowlist, the resolver daemon. Pins start empty under the new policy in this mode.

`DestroyVMNet{VMID: "vm_example", Generation: 1, Index: 1}` should run after the VMM exits. Every step tolerates the object already being gone:

```text
nft -f -                         # stdin: delete element inet davit vm_src { "slw07534e10c0h" . 10.200.0.6 }
ip netns del slw-07534e10c0      # removes tap0, up0 and, with it, the host veth peer
ip link del dev slw07534e10c0h   # covers a netns that vanished earlier
```

## Namespace verification status

**Run for real, rootless, on the dev host:**

- the rootless lifecycle: two VMs, identical `tap0` with 172.16.0.1/30 in separate namespaces, Destroy leaves no holder, and repeated Destroy is fine
- the guest ruleset with packet tests: internet and allowed targets are reachable; LAN, host address, metadata, CGNAT, the netns itself and a spoofed source are all blocked; masquerade is proven by the replies arriving
- the host ruleset loaded into a kernel inside `unshare -rn`, including re-applying it while keeping a VM element
- every mode's guest table (loaded twice in its replace form) inside `unshare -rn`
- the modes with packet tests on one live netns, switched atomically (`TestGuestRulesetModes`): `off` drops everything including DNS; `allowlist` drops an unpinned internet address, the host-level allow, an allowlisted CIDR inside the deny list and a pinned host address, admits a `@vmallow4` address and an address after it is added to `@pinned4`, and DNATs guest DNS to any address (UDP and TCP) to `172.16.0.1:53`
- tightening `filtered` to `allowlist`/`off` cuts an established flow to a non-admitted address; one to an admitted CIDR continues (`TestGuestRulesetTightenCutsFlows`)
- the real `davit netns-dns` resolver in a netns with the allowlist table and a netns-local fake upstream (`TestNetnsDNSResolver`): an allowed name is answered over UDP and TCP and its address appears in `pinned4` (TTL 5 s pinned for 60 s) and becomes reachable; a disallowed name gets NXDOMAIN and is not pinned; the resolver exits with the holder
- `Rootless.SetEgress` on a live network (`TestRootlessSetEgress`): atomic table switch; a failing `userspace.SetPolicy` (stubbed in the test) keeps the old table; on a simulated nft-only backend the resolver starts, is left alone when the same policy is re-applied, restarts on a list change carrying over only still-allowed pins, a resolver that cannot start restores the old table and pins, `filtered` stops it, and `Destroy` stops it

- LAN services with packet tests (`TestGuestRulesetLANServices`, real TCP/UDP listeners in the world netns): the permitted `tcp` and `udp` service is reachable in `filtered`, and in `allowlist` only with its address on the VM list; another port on the same host, another LAN host, a host-management address listed as a service, metadata, a tailnet address and another VM's uplink address (`10.200.0.6`) stay blocked in every mode; `off` reaches nothing
- the userspace stack's LAN service check end to end (`TestUserspaceEgressLANService`): exact port and transport, plus the allowlist opt-in, switched live
- a bridge inside the guest (`TestGuestBridgeEgress`, a container netns behind the guest netns): masqueraded container traffic gets exactly the guest's results in `filtered` and `allowlist`; an undeclared routed subnet reaches nothing; a declared one (`GuestSubnets` + return route) gets the guest's results, and nothing in `off`

**Not run:**

- pasta and slirp4netns (not installed on the dev host). Their argv is unit-tested against the upstream man pages only, and the allowlist path through them (ruleset + resolver) is tested only with veths standing in for their uplink.
- every root command (render-only by design)

## Tailnet gateway (`internal/gateway`)

One persistent `tsnet.Server` per host, with its state directly in `tailnet.stateDir` (default `<dataDir>/tailnet`, 0700, checked; `doctor` detects enrollment there). It never binds a host interface: every listener is `tsnet.Server.Listen`.

| Setting | Source |
|---|---|
| Hostname | `Config.Hostname` (DNS label). The *registered* name can differ; `Node.FQDN()` reports the actual one. |
| Control server | `Config.ControlURL` (empty = Tailscale). Headscale should work through this, but it is untested and not claimed. |
| Enrollment key | `Config.AuthKeyFile` (refused if group- or world-accessible), else env `Config.AuthKeyEnv` (default `DAVIT_TS_AUTHKEY`). Not needed once the state dir holds a logged-in node. Never logged. |
| Tags | `Config.AdvertiseTags`; tailnet policy and the key must permit them. |

### Identity

`gateway.Identify` calls `LocalClient.WhoIs` on the **accepted connection's peer address** and fails closed: on error, a missing node or user, or an empty stable node ID, the connection is refused. `Identity` carries `UserID`, `LoginName`, `NodeID` (stable), `NodeName`, `Tags` and `Addr`. Map principals by `UserID`/`NodeID`; tagged nodes (`Identity.Tagged()`) are workloads and need explicit grants. Headers (`X-Forwarded-For`, `Tailscale-User-*`), SSH usernames and VM names never feed identity. A test sends forged headers and proves they are ignored.

### Listeners

| Listener | Default | Behavior |
|---|---|---|
| HTTP API | `:443` with TLS (`tailnet.https`, or `tailnet.tlsCertFile`/`tlsKeyFile`), else `:80`; never both (see [Remote access](#remote-access-one-configuration-for-cli-ssh-and-sdks)) | `RequireIdentity` performs WhoIs on every request and returns 401 JSON without calling the handler on failure. The handler reads `gateway.IdentityFromContext`. The daemon's `httpapi.Authenticator` maps that identity to a principal and applies owner/token policy. |
| SSH front door | `:22` | `ssh <vm>@<host>` (guest user `user`) or `ssh <vm>+<guest-user>@<host>`. See below. |
| Allocated forward ports | `Gateway.Forward(":8022", vm, guestPort)` | Every connection gets WhoIs, then `Authorize(ActionForward, port)`, then `DialGuest`, then a splice. |
| Tunnel primitive | `Gateway.Streams().Open/Tunnel` | For the HTTP `/vms/{id}/tunnels` stream endpoints and for `forward`. The identity comes from the request context. |

Every stream is authorized again for each connection: a revoked grant takes effect on the next connection, and the tests prove it. `Limits{MaxBytes, IdleTimeout}` bound a stream, and the copies are backpressured.

### SSH: two paths

1. **Shared front door (terminating).** The gateway is an SSH server with a stable ed25519 host key (`<StateDir>/ssh_host_ed25519_key`, 0600). It runs WhoIs before the handshake. During client auth it parses the login, authorizes `(identity, vm, ActionSSH)`, then dials the guest sshd over `GuestDialer` and logs in with the credential from `GuestSSH.ClientConfig`. That credential is meant to be a short-lived cert scoped to VM, generation and guest user, and the guest host key is checked against the VM's identity record (`InsecureIgnoreHostKey` is refused). Only after both checks succeed does the client's auth succeed. A denial looks like an ordinary SSH auth failure with a banner that does not reveal whether the VM exists. The client's own keys grant nothing, since identity is the tailnet node. Channels, channel requests (pty, exec, signals, exit-status), stderr and global requests (`-L`, `-R`) are relayed unchanged, so guest sshd policy and the certificate's extensions (no agent or X11 forwarding) still apply. Plain OpenSSH works with no client config.
2. **Per-VM endpoint nodes (`Endpoints`, optional, `endpoint.mode=node`).** One host-resident `tsnet.Server` per VM, with its identity in `<endpoints>/<vmID>/`. Its `:22` is a **raw byte tunnel** to the guest sshd after WhoIs and authorization, so the client verifies the *guest* host key end to end. `Ensure` enrolls in the background and `Info` reports `pending`/`ready`/`error` with the registered FQDN and addresses, separately from VM readiness. `Detach` (stop) keeps the identity; `Delete` logs the node out and erases its state. A fork is a new VM ID, so it gets a new identity.

SPEC §4 prefers an authorized byte tunnel to guest sshd with client-side OpenSSH. Path 2 and the tunnel primitive provide exactly that. Path 1 is the zero-config `ssh vm@host` UX that boat.dev-style users expect. Its trade-off is that the gateway, not the client, verifies the guest host key. It does not use `tsnet.ListenSSH`/Tailscale SSH (SPEC §4).

### Service tokens

`/tokens` (owner only: local socket or the tailnet WhoIs owner, never a token) creates, lists, rotates and revokes
service tokens for automation, such as a CI runner that is a tagged node on the tailnet. `davit token create NAME
--action view|exec|admin [--vm VM]... [--ttl 30d]` (or `--preset readonly|ci|admin`) prints a `slw_…` secret once;
only its SHA-256 is stored. `token rotate TOKEN_ID [--overlap 10m]` issues a replacement secret and `token usage`
shows each token's use count and last use.

On the tailnet listener a WhoIs identity is still required for every request. Tailnet ACLs decide who can reach the
host; with `Authorization: Bearer <secret>` the request runs as the token's issuer, limited to the token's scopes,
whether or not the identity is the owner. Scopes are actions, nesting as `exec` ⊇ `view` and `admin` ⊇ both,
optionally on one VM (stored as its ID). Every API route declares the action it needs and how its VM is found (path
VM, operation, snapshot, tunnel); a VM-scoped token reaches only that VM's endpoints, and listing or creating across
VMs needs an all-VM scope. Expired, revoked or unknown tokens are 401. The CLI sends `DAVIT_TOKEN` as the bearer to
http(s) API URLs (`--host`) only; the local socket is owner-only and ignores bearers.

### Contracts the daemon wires

Defined in `internal/gateway/contracts.go` so this package does not import lifecycle, state or runtime:

- `Authorizer.Authorize(ctx, Identity, Request{VMRef, Action, Port}) (Target{VMID, Generation}, error)` resolves a name or ID within the principal's scope. It returns `ErrNotFound` or `ErrDenied` and must not reveal invisible VMs.
- `GuestDialer.DialGuest(ctx, Target, port)` reaches a TCP port on the guest's loopback over vsock or the guest agent. It must refuse a stale generation. ctx bounds only the dial. This maps onto `runtime.Dialer` for the VM's current handle.
- `GuestSSH.ClientConfig(ctx, Identity, Target, guestUser)` returns the guest credential and host-key verification for the front door.

### Tests (no real tailnet)

`internal/gateway/tailnet_test.go` starts Tailscale's `tstest/integration/testcontrol` coordination server and an in-process DERP/STUN relay on 127.0.0.1, then brings up four tsnet nodes in-process: the gateway, `alice`, `mallory` (testcontrol gives each a distinct user), and a per-VM endpoint. Fake guest sshd and echo servers sit behind a fake `GuestDialer`. The tests prove:

- WhoIs identity reaches the HTTP handler, and forged headers are ignored.
- The authorized peer runs a command on the guest through the front door, with stdout, stderr, exit status and guest-user selection intact.
- These are all denied: an unauthorized peer, an ungranted VM, an absent VM, a malformed login, and a raw ID used without a grant.
- A revoked grant is denied on the next connection.
- An allocated forward port works for alice and gives nothing to mallory.
- The per-VM endpoint enrolls; alice verifies the guest host key end to end and mallory is refused; `Delete` erases the identity.

Nothing touches the host's tailscaled or any real tailnet. One run takes about 2–3 s of test time. `tailscale.com` is pinned at v1.102.5 because v1.104+ requires Go 1.27 and this repo pins 1.26.8.

## Runtime integration

- **`runtime.NetworkHandle`** carries `Namespace`, `TapName`, `UserNamespace`, `GuestMAC` and `EnterArgv` as proposed. Zero values mean "no guest NIC, vsock only".
- **Lifecycle** owns the network (`lifecycle.Config.Network`, a `network.Provider`).
  - It calls `Create` per instance generation before the VMM starts, and persists the handle in the VM record (`VMRecord.Network`, inside the JSON doc, so no migration).
  - A replayed boot reuses the handle if it belongs to the same generation and `Enter` still works, so an adopted live VMM keeps its namespace.
  - `Destroy` runs after stop or delete has verified the VMM is gone, when a boot fails, and on reconcile for VMs that cannot be running (including instances lost while the daemon was down).
- **Firecracker adapter:** the VMM argv is `EnterArgv + firecracker …` (no jailer in rootless mode). `nsenter` execs Firecracker, so its pid and start-time identity are unchanged.
- **Golden captures** use a network of their own, created by the daemon through `firecracker.Options.CaptureNetwork` with the same device layout.
- **The guest NIC layout** (`firecracker.Options.NIC`: `tap0`, `06:00:AC:10:00:02`) is part of the golden fingerprint. A golden only restores into the layout it was captured with.
- **Network mode:** `network.mode` is `rootless` or `none`, and `""` means auto: rootless when `Probe` allows it, otherwise no NIC with a warning. Privileged mode is not wired yet.
- **Guest IP traffic to the namespace is dropped by the guest ruleset** (`input`), so the host cannot ping the guest. ARP to 172.16.0.1 does resolve, which is how the tests prove the link.
- **Egress** is reported by `doctor` (`network` and `egress` checks), including which backend serves it. Without pasta and slirp4netns the built-in userspace stack does; egress is off only if that cannot start, and the VM keeps working over vsock.
- **Guest transport for the gateway:**
  - `GuestDialer` is the daemon's default whenever the runtime has a guest transport. It resolves the VM's current instance, refuses a stale generation, and opens an authenticated agent session that sends `tcp.dial{port}`. The vsock stream then becomes a raw pipe to the guest's `127.0.0.1:port` (guest protocol feature `tcp`).
  - `GuestSSH` is `internal/guestssh`. A daemon-owned user CA's public key goes to every restore barrier (`TrustedUserCAKeys`). Each front-door login gets a fresh ed25519 key with a 5-minute certificate for that VM, generation and user. The guest host key must match the ed25519 fingerprint the barrier recorded for that exact generation.
  - `ClientConfig.User` may resolve `DefaultGuestUser` ("user") to the image's account from the manifest's `guest.user.name`.

## Reverse connections (`internal/network/reverse`)

The host side of `davit forward --reverse`: a service on the user's laptop, or an operator-permitted service on the host, becomes reachable from inside a VM. The guest cannot pick the far end.

A reverse grant is an explicit owner authorization over the host<->guest control channel (vsock), not guest egress. It is deliberately independent of the VM's egress mode and the deny list: it works in `off` and `allowlist` too, and an `off` VM reaches exactly its granted services and nothing else. Revoking the grant (or stopping the VM) is how the owner takes it back.

- `reverse.Manager` holds grants. `Open(ctx, Grant{VM, Generation, GuestPort, Target})` asks the guest agent to listen on a guest loopback port (`GuestListen.ListenGuest`) and splices every connection the guest makes there to the grant's `Target` (half-close supported). At most one grant per (VM, generation, port).
- `Target` is the authorized far end. The laptop end (a CLI session through the gateway) belongs to the gateway/CLI; this package provides `HostService`, obtainable only from `reverse.Policy.HostService` for an address:port the operator listed exactly.
- `Close(id)` revokes a grant and cuts its live connections; `CloseVM(vm, keepGeneration)` is what stop/delete (`0`) and a new generation (its own number) call, so nothing granted to an older run survives. A failed dial drops only that guest connection; a failing guest listener ends the grant.
- `GuestListen`/`GuestListener` are satisfied by the guest agent's listen feature: the daemon's `agentListen` resolves the VM's current instance and calls `guestexec.ListenGuest` (guest protocol `listen`; each accepted connection is claimed with `ReverseConnect` on a fresh authenticated session). A stale generation is refused, and the listener ends with that instance's agent session. `capabilities.features` reports `reverseForward` available whenever the runtime has a guest transport; a VM whose guest agent predates the feature answers 422 `unsupported_capability`. The fake runtime's in-process agents (`fake.guestAgent`) run the same path and listen on the host's `127.0.0.1:port`, which is their guest loopback.

### Reverse forward API and `davit forward --reverse` (`internal/reversefwd`)

`internal/reversefwd` turns API requests into grants on the VM's **current generation** and supplies the laptop end.
Every operation is owner-only or needs a token with `admin` on the VM (`exec` and `view` tokens get 403): a reverse
forward reaches out of the VM to the caller's machine or a host service.

| Operation | |
|---|---|
| `createReverseForward` `POST /vms/{vm}/reverse-forwards` | `{guestPort, target: {kind: "session"\|"host", address}}`. One forward per guest port (409). |
| `listReverseForwards` `GET …/reverse-forwards` | open forwards with `state` (`awaiting_session`, `active`) and `active` connections |
| `revokeReverseForward` `DELETE …/{id}` | revokes and cuts every bridged connection |
| `attachReverseSession` `GET …/{id}/session` | WebSocket of JSON events: `connection {connectionId}`, `ping` (20 s), `revoked {reason}` |
| `attachReverseConnection` `GET …/{id}/connections/{connectionId}` | one guest connection as binary messages; an empty one is a half-close either way |

Targets:

- **session** (the laptop or CI runner): the client attaches the session within 30 s (else the forward is revoked),
  and for each `connection` event opens `attachReverseConnection` within 10 s and splices it to its own local
  service. The daemon never dials the session's `address`; it is a label. Closing the session revokes the forward.
  It works over the local socket and over the tailnet (HTTPS with the [remote config](#remote-access-one-configuration-for-cli-ssh-and-sdks)), like every other stream.
- **host**: the daemon dials `address`, which must be listed exactly in the operator's config, else 403:

  ```json
  { "network": { "reverseHostServices": ["127.0.0.1:5432"] } }
  ```

  Specific `ip:port` entries only (no wildcard address, no port 0, no link-local, metadata, multicast or reserved
  address); nothing else on the host can be a target.

Revocation is lifecycle-driven: the service watches lifecycle changes (plus a 2 s backstop) and revokes every forward
whose VM left `ready`/`running` (stop, delete) or runs another generation, or whose guest listener ended. Revoking
closes the guest listener and every bridged connection; a session gets `revoked` with the reason, then the close.

CLI:

```sh
davit forward --reverse [VM] 8080:localhost:3000   # guest 127.0.0.1:8080 -> this machine's localhost:3000, until Ctrl-C
davit forward --reverse --host-service [VM] 5432:127.0.0.1:5432   # operator-listed host service; until revoked or stop
davit forward --reverse --list [VM]
davit forward --reverse --revoke rev_… [VM]
```

`GUESTPORT:PORT` means `127.0.0.1:PORT`. `--json` emits `reverse.listening`, then per guest connection
`reverse.connection`, `reverse.closed` (bytes each way) or `reverse.error` (e.g. the local service refused), then
`result` on Ctrl-C, or an `unavailable` error naming the reason when the forward was revoked or the VM stopped.

Proof (hermetic, fake runtime with in-process guest agents, so through the real guest listen protocol): `cmd/davit/reverse_e2e_test.go` (round trips with half-close larger than a
WebSocket chunk, one forward per port, revoke, Ctrl-C and VM stop cutting live connections, host-service
allowlist) and `TestRemoteHTTPSOverTailnet` (a non-owner and an `exec` token are refused; an admin token runs a
session over tailnet HTTPS); `internal/reversefwd` unit tests cover attach expiry, generation change and scoping.

## Credential proxy (`internal/credproxy`)

A VM never holds a real API key. It holds a grant's random placeholder (`davit-placeholder-` + 48 hex) in the credential's environment variable, and sends its API requests to the host proxy on its own network, `http://172.16.0.1:15080` (`credproxy.GuestBaseURL`; set it as the client's base URL, e.g. `ANTHROPIC_BASE_URL`). The proxy:

- listens once per running VM, on a socket created inside that VM's namespace (`TapServices` + `Rootless.ListenTCP`), so it knows the VM from the listener, never from the request, and no other VM or host process can reach it. It is attached when lifecycle provisions the network (and for running VMs at daemon start) and detached before the network is released, so a stopped VM's grants are dormant;
- needs exactly one placeholder in the request headers (`x-api-key`, `Authorization: Bearer`, ...), resolves it for that VM only (a fork child's copied placeholder, an unknown or a revoked one get the same 403; no placeholder is 401) and never contacts an upstream otherwise;
- sends the request to the grant's upstream only (the credential's `baseUrl`, else `https://api.anthropic.com` / `https://api.openai.com` for those providers), whatever URL, `Host` or `CONNECT` the guest tries. The placeholder is replaced by the secret only in `Authorization`, `X-Api-Key`, `Api-Key` and `X-Goog-Api-Key`; a placeholder anywhere else (another header, the path, the query) is 400, so the guest cannot reflect the secret. Paths with dot segments (`..`, `%2e`, `%2f`, `%5c`) are 400, and guest `X-Forwarded-*` headers are dropped;
- streams the answer back as it arrives (SSE, chunked) and scrubs any echo of the secret from response headers, the body (also when split across reads) and trailers;
- connects to the upstream only if its address passes the upstream policy, checked at connect time after DNS (no environment proxy). By default an upstream must be `https` and not resolve to loopback, private (RFC 1918), tailnet (CGNAT `100.64.0.0/10`, ULA) addresses; link-local, cloud metadata, multicast and reserved addresses are refused always. A model server on the LAN or on the host needs `network.allowPrivateUpstreams` (below);
- cuts requests in flight when their grant is revoked (explicitly, by VM delete, by profile or credential removal).

Reaching the proxy is an explicit owner grant, like a reverse connection, so it works in every egress mode, `off` included; the proxy's own upstream connection is made by the daemon from the host.

`network.allowPrivateUpstreams` (default `false`) lets credential-proxy upstreams and webhook URLs ([events.md](events.md#webhooks)) reach loopback, RFC 1918, tailnet and ULA addresses, and lets credential upstreams use plain `http`:

```json
{ "network": { "allowPrivateUpstreams": true } }
```

**Behaviour change:** before this setting existed both reached any address. A `baseUrl` or webhook URL pointing at the host or the LAN (e.g. `http://192.168.1.20:11434`, `http://localhost:8080/hook`) now fails until it is set; credential requests answer 502 and webhook deliveries dead-letter with `refused by policy`. Link-local, cloud metadata (`169.254.169.254`, `fd00:ec2::254`), multicast and reserved addresses stay refused with it on.

Secrets are write-only: `putCredential` stores them as 0600 files under `<dataDir>/credentials` (0700, hex-encoded names, atomic writes; `credproxy.FileSecrets`), never in the state database, and no API answer, log line or guest ever carries them. Grants and profiles are metadata in the state database. Lifecycle events follow `domain.GrantAfter`: stop and resume keep a grant, a fork child inherits none, delete revokes (`vm_deleted`). Per-request accounting is not exposed (the grant contract has no field for it).

CLI: `davit credentials profile create work`, `davit credentials set work anthropic --provider anthropic --env ANTHROPIC_API_KEY < key.txt` (the secret is read from stdin only), `davit credentials grant VM work anthropic`, `grants`, `revoke`, `rm`, `profile delete`.

## Daemon configuration

```json
{
  "tailnet": {
    "enabled": true,
    "hostname": "davit-myhost",
    "controlURL": "",
    "authKeyFile": "/home/me/.config/davit/ts-authkey",
    "ownerLogins": ["me@example.com"],
    "ownerTags": ["tag:ci"],
    "https": true,
    "tlsCertFile": "",
    "tlsKeyFile": "",
    "appPorts": "10000-10999",
    "defaultVisibility": "private"
  }
}
```

- `davit init --auth-key-stdin --owner-login you@example.com` (`--owner-login`/`--owner-tag` are repeatable) writes this block and sets `enabled` once an owner and a key are both present; plain re-runs never change `enabled`.
- `hostname` defaults to `davit-<os hostname>`.
- `stateDir` defaults to `<dataDir>/tailnet`.
- The auth key can also come from `DAVIT_TS_AUTHKEY`. It is needed only for the first enrollment. Without any key, the login URL is logged at info level.
- `ownerLogins` is only the bootstrap. The first identity that matches a configured login pins that login to its
  stable tailnet user ID (`<stateDir>/owners.json`, 0600). From then on the owner is whoever holds a pinned user ID,
  whatever its login is now: a renamed login keeps working, while a login that is recycled or reassigned to another
  user at the coordination server (or a Headscale user renamed onto it) is refused and logged. Removing a login from
  the config retires its pin; adding it back restores the same pin. `ownerTags` still match by ACL tag, which the
  tailnet policy asserts. If the pin cannot be written, the match is refused.
- Owners map to the same principal as the local socket owner, so both transports see the same VMs. Any other tailnet identity gets 403 from the API and an auth failure from SSH.
- Enrollment runs in the background, so the Unix socket works while the node waits for login or for the network.

## Remote access: one configuration for CLI, SSH and SDKs

A laptop or CI runner reaches a host with three settings, read the same way by the CLI (including `davit ssh`,
`scp`, `forward`), the Python SDK and the TypeScript SDK. Explicit arguments (`--host`, `base_url`/`baseUrl`,
`token`, `ca_file`/`caFile`) win over the environment.

| Variable | Meaning |
|---|---|
| `DAVIT_API_URL` | `https://<gateway MagicDNS name>`; `davit doctor` on the host prints it (`tailnet_gateway`). Plain `http://` is accepted only for loopback by the SDKs, and `status` flags it. |
| `DAVIT_TOKEN` | optional `slw_…` service token (CI). Without one the request runs as this machine's tailnet identity, which must be an owner. Sent only to http(s) URLs, never to the socket. |
| `DAVIT_CA_FILE` | optional PEM bundle trusted in addition to the system roots, for an operator certificate from a private CA. Not needed with `tailnet.https`. |

The principal is stable either way: an owner identity resolves through its pinned tailnet user ID
(`owners.json`), and a token runs as its issuer limited to its scopes. A WhoIs identity is required for every
request, so a token copied off the tailnet is useless, and an expired or removed device is refused even with a
valid token.

TLS on the gateway is one of three modes (`doctor` → `tailnet_tls`, also in the daemon log and the status report):

| Mode | Config | Certificate |
|---|---|---|
| `tailscale` | `"https": true` | the tailnet's own (Let's Encrypt) certificate for the MagicDNS name; HTTPS certificates must be enabled in the tailnet admin console. No client setup. |
| `files` | `tlsCertFile` + `tlsKeyFile` (absolute paths, both or neither) | an operator certificate, for Headscale, tailnets without HTTPS certificates or a private CA. Must cover the registered name; checked at start and reloaded when the files change (a broken rotation keeps the last good pair). Clients set `DAVIT_CA_FILE` unless a system root signed it. |
| `off` | neither | plain HTTP on `:80` (WireGuard-encrypted). The CLI works; the SDKs refuse it for non-loopback hosts. |

### Enrollment diagnostics: `doctor` and `status`

The daemon writes the gateway's enrollment to `<stateDir>/gateway-status.json` (0600) as it changes: `starting`,
`needs-login` (with the login URL), `needs-approval` (device approval), `up` (registered FQDN, addresses, the
URL clients use) or `error`. `doctor` on the host reads it (`doctor -v` also lists the passing checks, such as the
`up` state with the URL clients use):

- `tailnet_gateway` fails with the fix for each state: open the login URL or store a key (`init --auth-key-stdin`),
  approve the device, a rejected or expired auth key, an unreachable control server, enrollment stuck for over two
  minutes, or a port in use. A report from a daemon that is no longer running is called stale.
- `tailnet_tls` checks the operator certificate (pair, validity, renewal within 14 days, and that it covers the
  registered name) or warns that plain HTTP blocks the SDKs.

`davit status` runs on the client with the same three settings and checks the endpoint, credential (the secret is
never printed), CA file and one authenticated request, mapping each failure to its fix: certificate not trusted →
set `DAVIT_CA_FILE` or use `tailnet.https`; wrong name → use the name the certificate covers; name does not resolve
→ MagicDNS or the 100.x address; refused or unreachable → host `doctor`, `tailscale status`, ACLs; 401 with a token →
it is revoked or expired, create a new one; 401 without → not over the tailnet; 403 → add the login to
`tailnet.ownerLogins` or use a token. On the host (local socket) it adds the two gateway checks.

The hermetic proof is `cmd/davit/tailnet_https_e2e_test.go` (`TestRemoteHTTPSOverTailnet`): the real daemon on the
fake runtime, the in-process test tailnet, an operator certificate from a throwaway CA, and tsnet peers that relay a
loopback port into the tailnet so the unmodified CLI binary, `davit ssh` and the SDK examples run end to end over
TLS. It covers the owner, a missing CA, a non-owner refused until it gets a token, the token path (`status`, `exec`,
`ssh`, both SDK examples), a revoked token and a device expired on the control server. The real laptop → host run is
[`docs/acceptance-g2.md`](acceptance-g2.md).

## App endpoints: stable tailnet URLs for apps in a VM (`internal/appendpoint`, gateway `apps.go`)

`davit expose [VM] 3000 --name web` gives the app on guest port 3000 a stable, tailnet-private URL on the
**shared gateway node**: `https://<gateway MagicDNS name>:<gatewayPort>/` (plain `http://` when the gateway's TLS
is off). The gateway port comes from `tailnet.appPorts` (default `10000-10999`) and stays with the endpoint for
life, including across daemon restarts (`<tailnet stateDir>/app-endpoints.json`, 0600).

| | |
|---|---|
| CLI | `expose [VM] PORT [--name N]`, `endpoints [VM]`, `endpoints hide\|show\|delete [VM] NAME`, `endpoints rename [VM] NAME NEW` |
| API | `listAppEndpoints` (view), `createAppEndpoint`, `updateAppEndpoint` (rename, `hidden`), `deleteAppEndpoint` (owner or admin token) on `/vms/{vm}/app-endpoints`; feature `appEndpoints`, `unsupported` without `tailnet.enabled` |
| Requests | tailnet identity required, then the owner, or `Authorization: Bearer <token>` with `exec` on the VM (the same right as a tunnel). Others get 403, a bad token 401, a stopped VM 503. The bearer is removed before the app sees the request. |

**Why port-based URLs.** Tailscale issues a certificate and a MagicDNS name only for the node itself: there are no
wildcard certificates, and `web--box.<gateway>.ts.net` would not resolve. That rules out per-app sub-names on the
shared node. A path prefix (`https://<gateway>/apps/web/`) would break every app that uses root-relative URLs,
cookies or redirects, and would share the API's origin. A port gives each app its own browser origin with the
node's own certificate, root paths work unchanged, and nothing extra is enrolled. Trade-offs:

- Cookies are not port-scoped in browsers, so apps on the same gateway see each other's cookies (the API itself uses
  no cookies). Put mutually distrusting apps behind different gateways, or use per-VM nodes when they are wanted
  (`Endpoints`, opt-in) for real names.
- The URL contains a port. Operators with their own wildcard DNS and certificate (Headscale, private CA) could add
  host-based routing later; it is not built.

**Lifecycle.** An endpoint belongs to its VM, not to an instance generation, because a stable URL is the point:

- stop: live connections (requests, streams, WebSockets) are cut; the URL answers 503 and lists as `vm_not_running`;
- resume, or any new generation: connections to the old instance are cut and the same URL serves the new one (the app
  must be running again inside the VM);
- delete: the endpoint is removed and its port freed; a fork inherits none (new VM ID);
- hide: the port is closed and its connections cut; show serves the same URL again; rename keeps the URL.

The service watches lifecycle changes (plus a 2 s backstop) and also cuts on first sight after a daemon restart.

**Proxying.** `httputil.ReverseProxy` over the guest dialer (the guest agent's `tcp.dial` to guest loopback), with
kept-alive guest connections pooled per VM instance and port. HTTP/1.1 requests (h2 is not tested), streamed responses (SSE,
chunked: flushed as written) and WebSocket upgrades pass through. The app sees the browser's `Host` (and `Origin`)
unchanged, so absolute URLs and same-origin checks match what the browser uses; the proxy sets `X-Forwarded-For`,
`X-Forwarded-Host` and `X-Forwarded-Proto` itself (client values are dropped).

**Identity forwarding is opt-in** (`expose --forward-identity`, `forwardIdentity` on create and update). The guest is
untrusted, so by default the app learns nothing about who is on the tailnet: a client-sent `Tailscale-User-Login`
(and the other `Tailscale-User-*` / `Tailscale-App-Capabilities` headers) is always dropped and nothing is sent.
With the flag the gateway sets `Tailscale-User-Login` to the caller's WhoIs login (not for tagged nodes), as
`tailscale serve` does, for apps that want per-user behaviour. Changing the flag re-serves the endpoint, cutting its
live connections. The tailnet identity is resolved once per
TCP connection (its peer cannot change); authorization runs on every request.

**Measured overhead (hermetic).** `DAVIT_BENCH_APPS=200 go test -run TestAppEndpointOverhead ./cmd/davit`:
sequential keep-alive GETs from a tailnet peer on the in-process simulated tailnet, 2 interleaved rounds (400
samples per path), endpoint versus a baseline tailnet node that terminates TLS with the same certificate and
splices to the same app. AMD Ryzen 7 6800U (16 threads), 27 GiB, load average about 8 during the runs:

| Run | Baseline p50 / p95 | Endpoint p50 / p95 | Added p50 / p95 |
|---|---|---|---|
| 1 | 257 / 351 µs | 476 / 596 µs | 218 / 245 µs |
| 2 | 249 / 334 µs | 515 / 694 µs | 265 / 360 µs |

The addition is authorization (a VM lookup per request) and the guest-agent hop. Before caching the WhoIs per
connection it was 435 / 559 µs. These are simulated-tailnet numbers on a loaded box, not a real tailnet.

Proof: `TestAppEndpointsOverTailnet` (real daemon, fake runtime with guest agents, operator TLS on the simulated
tailnet, an HTTP app in the "VM"): Host, Origin and forwarded headers, a streamed response, a WebSocket echo, a
no login forwarded by default (a forged one dropped) and the real login with `--forward-identity`, a
non-owner refused, an exec token admitted with its bearer stripped, view and revoked tokens refused, an exec token
unable to create endpoints, rename/hide/show, stop cutting a live WebSocket with 503, serving again after resume, the
store, and delete. `internal/appendpoint` tests cover allocation, validation, reload and lifecycle cuts.

## Members: other tailnet users with separate rights (`internal/members`)

The owner stays the superuser. **Members** are other tailnet users the owner adds; they reach only what the owner
**shares** with them, by role. Members are an access layer over the owner's resources, not tenants: they act
within the owner's VMs (as the owner principal, limited to their shares) and never own VMs of their own.

```sh
davit members add bob@example.com --role exec   # role for VMs shared with all members
davit share box bob@example.com --role viewer   # viewer | exec | admin; MEMBER "all" = every member
davit share --snapshot golden bob@example.com   # a snapshot without its VM
davit share --environment web all --role viewer # an environment template
davit shares [box]                              # what is shared
davit unshare box bob@example.com               # cuts bob's live sessions on box at once
davit members role bob@example.com viewer       # cuts all of bob's live sessions
davit members remove bob@example.com            # removes bob's shares too
```

API: `listMembers`, `addMember`, `updateMember`, `removeMember`, `listShares`, `createShare` (an upsert per
resource and member), `deleteShare`; all owner only. Feature `members`, `unsupported` without `tailnet.enabled`.

**Identity.** A member is added by tailnet login and pinned to its stable tailnet user ID on its first connection
(like the owner pins in `owners.json`): a renamed login keeps working, a login recycled to another user is refused.
Tagged nodes are never members; use service tokens for automation.

**Where it lives, and why.** `<tailnet stateDir>/members.json` (0600), not the daemon config and not the SQL state
store: membership is meaningless without the gateway, it changes at runtime (share and unshare take effect and cut
sessions without a restart), and its identity pins are gateway state like the owner's.

**Roles** are the token actions, so every route already declares what it needs:

| Role | Adds |
|---|---|
| `viewer` | VM state and lists, operations, process records and history, prompt events, snapshots of the VM |
| `exec` | exec and attach, files, tunnels (so `davit ssh`/`scp`/`forward`) and the SSH front door, prompts, app endpoint requests, snapshot file reads |
| `admin` | stop, resume, update, delete, snapshot, recover, reverse forwards, app endpoint management |

Whatever the role, members never: create VMs or runs, fork (a fork is a new VM of the owner's), see or manage
credential grants, profiles or tokens, manage members and shares, or read the host event stream and webhooks
(host-wide, owner only). A VM's **privacy policy** is part of the VM a member sees at every role and,
like every VM setting (`PATCH /vms/{id}`), changes only with `admin`; members cannot set one at creation because
they create no VMs. Anything not shared answers **404** (it is
invisible, not just forbidden), and `GET /vms`, `/snapshots` and `/environments` list only what is shared (a page
may come back short). A snapshot share (`viewer`: metadata, `exec`: its files) and an environment share (`viewer`:
the manifest and revisions) work without access to the source VM.

**Default visibility of new VMs** is `tailnet.defaultVisibility`: `private` (default) or `members` (every new VM is
shared with every member at each member's own role). VMs that existed when members were first enabled stay
private; deleting a VM drops its shares.

**Revocation cuts live sessions.** Every member request and stream is tracked by tailnet user and VM
(`gateway.Sessions`): API requests and their hijacked WebSockets (exec attach, tunnels and so `davit ssh`, scp and
forward, reverse sessions), SSH front-door sessions, gateway forwards and app endpoint connections. Unsharing a
resource, changing a share's role or credentials, changing a member's role and removing a member close them at
once; reverse forwards the member created on that VM are revoked too. A short-lived SSH certificate the member
already holds is useless without a tunnel or the front door, which it can no longer open.

**Managed secrets.** One guest cannot tell its users apart, so a shared VM's credential grants would let the
member use the owner's API keys through the proxy. Therefore members never see the grants, and **an `exec` or
`admin` share without `--credentials` pauses the VM's credential grants for everyone** while it exists (the proxy
refuses them; in-flight proxied requests are cut). The owner opts in per share with `--credentials`. Viewer shares
change nothing.

**What stays on a shared VM's disk.** Sharing does not scrub the guest. Anything written to the VM's disk stays
there for the member (and in its snapshots): files, shell history, git credentials, tokens the owner pasted, the
harness's conversation files, and credential *placeholders* (useless outside that VM's proxy, and paused while
shared). Real proxy secrets are never in the guest. Unsharing does not remove what the member already read or copied,
and **secure erasure of a disk is not possible** (copy-on-write storage, reflinked snapshots and SSD wear levelling
keep old blocks); share a fresh VM or a fork made for the purpose instead of one with private history.

Proof: `cmd/davit/members_e2e_test.go` (two users on the simulated tailnet, the real daemon): refused before
membership, pinned on first connection, nothing visible until shared, viewer/exec/admin separation, 404 for the
unshared VM, credential grants refused at every role, fork/create/tokens/members/shares refused to an admin, a
shared snapshot readable without its VM, a live tunnel stream cut by `unshare`, and removal. `internal/members` tests
cover pinning, recycled logins, tagged nodes, role resolution, the credentials pause and default visibility.

## Desktop: the VM's display in a browser (`gateway/desktop.go`, `gateway/rfb.go`)

```sh
davit desktop open [VM] [--port 5900] [--browser]   # prints the viewer URL (opens it with --browser)
davit desktop url [VM]
davit desktop close [VM]                            # cuts every viewer
```

The desktop is the VM's **app endpoint of kind `desktop`** (named `desktop`, `openDesktop` / `getDesktop` /
`closeDesktop`, feature `desktop`). It reuses everything app endpoints provide: the stable port-based URL on the shared gateway,
tailnet identity on every request, authorization per request, lifecycle cuts (stop cuts every viewer and answers
503 at the same URL; a new generation cuts the old instance; delete and `desktop close` remove it), and the
store. Its URL serves a built-in viewer (`gateway/desktopui`, one self-contained page, no third-party code, no
external URLs, a strict CSP) and `<URL>websockify`, a websockify-compatible binary WebSocket (subprotocol
`binary`) carrying RFB 3.8, so noVNC (`vnc.html?path=websockify`) or any websockify client works too.

**Who sees what.** The owner, or a token with `view` (**view-only**) or `exec`/`admin` (input) on the VM;
members follow their role on the VM the same way (viewer: view-only; exec, admin: input), and unsharing
cuts their viewers. View-only is **enforced by the gateway**, not the viewer or the guest:
the gateway parses every viewer message and drops KeyEvent, PointerEvent and ClientCutText (clipboard into the
guest), forwarding only pixel format, encodings, update requests, continuous-updates and fences; an unknown
message ends the session (fail closed). The view-only page also hides its input, but that is cosmetic.

**Two RFB handshakes.** The gateway completes the handshake with the guest's VNC server itself (as a client,
RFB 3.3/3.7/3.8, security None) and with the viewer (as a server, RFB 3.8, offering **only** None): the tailnet
identity is the authentication, and no VNC password exists to leak or guess. A guest server that demands a
password is refused with the fix ("run it without a password on guest loopback"). It never listens on a guest
NIC: the gateway reaches it through the guest agent's loopback tunnel.

**Guest side (declarative).** `images/manifests/profiles/ubuntu-24.04-desktop.json` describes the desktop image
profile: Xvfb, Openbox, x11vnc on `127.0.0.1:5900` (`-localhost -nopw -shared -forever`) and Chromium (Chrome for
Testing, pinned by digest at build time, because Ubuntu 24.04's `chromium-browser` deb is a snap shim). Its
`davit-desktop.service` runs as the image user and does not depend on the guest agent. Nothing builds it yet:
`images/build.sh` does not read `profiles/`. A test checks that the profile's port and flags match what the gateway
expects.

**Viewer limits.** Raw and CopyRect encodings plus DesktopSize; no Tight/ZRLE or cursor pseudo-encoding (so large
screens use more bandwidth), no clipboard either way, keys from the browser's key values (no IME composition).
**WebRTC is a later option** (lower bandwidth and latency through a media pipeline in the guest); the transport
seam is the desktop endpoint, so it can be added beside RFB.

**First-frame latency (SIMULATED).** `DAVIT_BENCH_DESKTOP=100 go test -run TestDesktopFirstFrameLatency
./cmd/davit`: a new WebSocket per sample, from connect to the first full 64x48 raw frame, through the gateway on
the in-process simulated tailnet (TLS, WhoIs, authorization, both RFB handshakes, the fake guest's agent tunnel),
against the same RFB handshake straight to the fake VNC server on loopback. AMD Ryzen 7 6800U, 16 threads, load
average about 5, 2 runs of 100 samples each:

| | p50 | p95 |
|---|---|---|
| Direct to the fake VNC server | 0.32–0.37 ms | 0.53–0.66 ms |
| Through the gateway | 3.1–3.7 ms | 3.8–4.3 ms |

No real VM, display or network was involved; real first frames are dominated by the guest's display and the
tailnet path, which this does not measure.

Proof: `TestDesktopOverTailnet` (real daemon, fake runtime, a fake RFB server behind the fake guest, the simulated
tailnet): the viewer page (and its view-only variant), the RFB handshake and frames through the gateway, owner and
exec-token input reaching the guest, view-token key/pointer/clipboard dropped while frames continue, a non-owner
refused, `desktop close` cutting three live viewers, and stop cutting the stream with 503. `internal/gateway`
tests cover RFB 3.3/3.7/3.8 guests, view-only dropping, a password-protected guest refused and unknown messages
ending the session.

## Client side: `ssh`, `ssh-config`, `scp`, `forward`

All four use one primitive: `openTunnel` + `attachTunnel` (API v1). The daemon asks the guest agent to `tcp.dial` a
port on the **guest's loopback** (the agent never dials anything else) on a dedicated authenticated agent session. The
CLI then carries the bytes as binary WebSocket messages. The same endpoints serve the Unix socket and the tailnet
listener, with the same owner authorization.

| Command | Local socket | Remote (`--api-url http(s)://<gateway>`) |
|---|---|---|
| `davit ssh [VM] [-- CMD]` | runs OpenSSH with `ProxyCommand=davit ssh --proxy <vm-id>` to host `davit-<vm-id>` | the same, through the remote API (so `DAVIT_TOKEN` works for CI); plain `ssh <vm>[+user]@<gateway>` (the SSH front door) stays available |
| `davit ssh-config` | prints `Host davit-*` with a ProxyCommand through the API; `ssh`/`scp`/`rsync`/`sftp davit-<name>` then work | same block, through the tailnet API |
| `davit scp [-r] SRC DST` | OpenSSH `scp` with the same options; one side is `VM:/path` | the same, through the remote API |
| `davit forward [VM] [LOCAL:]REMOTE` | local listener (`--bind`, default 127.0.0.1) to guest port REMOTE, one tunnel per connection; `--json` emits `forward.listening`, `forward.connection`, `forward.closed` (bytes each way), `forward.error`, then `result` on Ctrl-C | same, over the tailnet API |

Notes:

- **Guest host keys are pinned, never trusted on first use.** Each boot's restore barrier reports the guest's host-key
  fingerprints over the authenticated agent session, and they are published in `VM.endpoint.sshHostKeyFingerprints`.
  `issueSSHCredential` reads the key the guest sshd presents through a tunnel and returns it only if its fingerprint
  is one the barrier reported. The CLI writes exactly those keys to a per-host `known_hosts` and runs OpenSSH with
  `StrictHostKeyChecking=yes`. Forks rotate their keys and get new fingerprints with their new generation.
- **Guest login uses short-lived certificates** (`internal/guestssh`). The persistent user CA key is a root: each
  (VM, generation) signs with an ed25519 key derived from it, and the barrier installs that generation's public key
  as the guest sshd's `TrustedUserCAKeys` on every boot (`SetIdentity.SSHUserCAKeys`). `issueSSHCredential` signs the
  caller's own public key with principal = guest user, valid for 5 minutes by default and 10 at most. A certificate
  therefore logs in only to the VM and generation it was issued for: another VM trusts another key, and the next boot
  installs the next generation's. Certificates permit PTYs and port forwarding, but deliberately not agent or X11
  forwarding (the guest is untrusted). The CLI keeps its client key in `<config dir>/ssh/id_ed25519` and fetches a
  fresh certificate for every connection; `ssh-config` does this from `Match exec "davit ssh --prepare %n --user
  %r"`. The tailnet SSH front door logs in the same way, with an ephemeral key per session.
- `forward --reverse` is the other direction (guest → this machine); see
  [Reverse forward API](#reverse-forward-api-and-davit-forward---reverse-internalreversefwd).
- **On the fake runtime** (`fake.guestAgent: true`), port 22 is an in-process SSH server that, like a barrier-configured
  sshd, accepts only certificates from the installed user CA. Exec and shell run `/bin/sh` on the host as the daemon
  user, like fake exec, with no PTY or SFTP. Other ports are the host's loopback.
