# Status

What davit does today, what is verified on real hardware and what is not. The headline use, project boxes, is
in [quickstart.md](quickstart.md) and [project-boxes.md](project-boxes.md); the VM features below are toured in
[vms-and-agents.md](vms-and-agents.md).

## What is verified

The API contract is [research/CONTRACT-v1.md](research/CONTRACT-v1.md) and `api/openapi.yaml`. Every feature in
the table below is implemented and covered by `make check`. Some are narrower than boat.dev: for example there is
no public ingress and no desktop recording.

`make check` is hermetic: a fake runtime and an in-process simulated tailnet, no KVM. On real hardware:

- **Firecracker VMs** were run on two hosts: an AMD Ryzen 7 6800U laptop (bare metal, 2026-10-22) and a
  nested-KVM VM on an Intel i5-8400 (Ubuntu 24.04, 2026-10-25). Golden `new`, fork, live fork, stop/resume,
  recovery after a daemon kill, guest egress and the guest agent's restore barrier all ran there
  ([benchmarks.md](benchmarks.md)).
- **Project boxes** ran on the nested-KVM host, installed with `make host-install` and driven from another
  machine over `ssh://` ([benchmarks.md](benchmarks.md#project-boxes-from-a-dev-machine-over-ssh-2026-10-25)).

Not yet verified on real hardware, plainly:

- **Real-VM tier on every change.** The Firecracker tier (`TestM1Acceptance`, `TestFirecracker*`,
  `fresh-host-e2e.sh` on Firecracker) runs only on a separate KVM host ([vm-tier.md](vm-tier.md)), by hand. It is
  not part of `make check` or hosted CI, so a given commit may not have been through it.
- **Real tailnet.** Gateway, members, desktop and app endpoints are proven against an in-process simulated
  tailnet. The laptop → host run on a real tailnet ([acceptance-g2.md](acceptance-g2.md)) is pending.
- **Mac backend.** The Go adapter, helper protocol and APFS clone storage exist, but the signed Swift
  Virtualization.framework helper has not been written, so `"runtime": "mac"` does not start
  ([MAC-BACKEND.md](research/MAC-BACKEND.md)).
- **Images.** `images/build.sh` builds `ubuntu-24.04-base` and `ubuntu-24.04-harness` (Claude Code, Codex). The
  OpenCode, pi and desktop images are declared profiles only; nothing builds them yet
  ([images/README.md](../images/README.md#declared-profiles)).
- **Releases.** `make release` writes checksum-verified local artifacts. Nothing is published, and signing is a
  TODO ([packaging/README.md](../packaging/README.md)).

## Features

| Feature | CLI | Docs |
| --- | --- | --- |
| Project boxes: a project image per repo, an instance per worktree, sync, run, idle stop | `box` | [quickstart](quickstart.md), [project-boxes](project-boxes.md) |
| SSH transport to a host, no tailnet needed | `connect` `client-config` `--host ssh://…` | [quickstart](quickstart.md#3-connect-the-laptop), [project-boxes](project-boxes.md#remote-hosts-over-ssh) |
| VMs: create, stop/resume, fork (`--live` too), resize, recover, batch `--count`, TTL | `new` `stop` `resume` `fork` `resize` `recover` | [tour](vms-and-agents.md#5-use-it), [benchmarks](benchmarks.md) |
| Exec, files, one-shot runs, process history | `exec` `cp` `push` `run` `history` `ps` | [tour](vms-and-agents.md#5-use-it) |
| Tailnet gateway, remote CLI/SDKs, service tokens | `--host` `status` `token` `whoami` | [network](network.md#tailnet-gateway-internalgateway), [remote access](network.md#remote-access-one-configuration-for-cli-ssh-and-sdks) |
| SSH, scp, port forwards both ways | `ssh` `ssh-config` `scp` `forward` | [client side](network.md#client-side-ssh-ssh-config-scp-forward) |
| Coding agents: Claude Code, Codex, OpenCode, pi | `prompt` `steer` `interrupt` `conversations` | [prompts](vms-and-agents.md#prompts-and-credentials), [real-model acceptance](research/G5-ACCEPTANCE.md) |
| Credential proxy (keys never enter the VM) | `credentials` | [network](network.md#credential-proxy-internalcredproxy) |
| Egress policy, LAN services, reverse forwards | `egress` `forward --reverse` | [network](network.md#egress-policy-both-modes) |
| Snapshots and their files | `snapshots` `snapshot` | [tour](vms-and-agents.md#5-use-it) |
| Environments (versioned templates) | `env` | [tour](vms-and-agents.md#environments) |
| App endpoints (stable tailnet URLs) | `expose` `endpoints` | [network](network.md#app-endpoints-stable-tailnet-urls-for-apps-in-a-vm-internalappendpoint-gateway-appsgo) |
| Members and shares | `members` `share` `unshare` `shares` | [network](network.md#members-other-tailnet-users-with-separate-rights-internalmembers) |
| Desktop in the browser | `desktop` | [network](network.md#desktop-the-vms-display-in-a-browser-gatewaydesktopgo-gatewayrfbgo) |
| Events and webhooks | `events` `webhook` | [events](events.md) |
| Privacy: ephemeral VMs, no retention | `new --ephemeral` `privacy` | [tour](vms-and-agents.md#ephemeral-and-private-vms) |
| Ops: completion, token rotation and usage, update check | `completion` `token rotate` `version --check` | [tour](vms-and-agents.md#who-am-i-tokens-and-updates) |
| Python and TypeScript SDKs | — | [`sdk/`](../sdk) |

Not built: a web console, RAM `suspend`, VM-level `image` commands (project images are `box image`). Design: [research/SPEC.md](research/SPEC.md);
measured numbers: [vms-and-agents.md#measured](vms-and-agents.md#measured) and [benchmarks.md](benchmarks.md).
