# Migrating from slipway

The project was called slipway until 2026-10-26. It is now Davit: the binary is `davit`, the Go module
`davit`, the SDKs `davit-sdk` (`import davit`, `@davit/sdk`), and every `SLIPWAY_*` variable is `DAVIT_*`.
No compatibility aliases are kept for names you type (commands, variables, headers). Data you already have
keeps working: VMs, boxes, disks, snapshots and images made by slipway carry over.

## A VM host

Run the usual install from a checkout of Davit, with the same flags you installed slipway with but `davit`
in the paths:

```console
$ make host-install HOST=user@host HOST_INSTALL_FLAGS='--config .config/davit-host/config.json --bin-dir .svm/davit/bin --skip-images'
```

Before anything else, the install runs `davit init --migrate-from-slipway` on the host. That step:

1. Checks the slipway daemon for live VMs and **refuses** while any runs, naming them. Stop them first
   (`slipway box down`, `slipway stop`), or pass `--stop-vms` in `HOST_INSTALL_FLAGS` to stop them cleanly
   (a clean stop commits each disk; nothing is lost but running processes). Check `ps` in the guests first.
2. Disables and removes `slipway.service`.
3. Moves the data dir (`~/.svm/slipway` to `~/.svm/davit`). Golden memory snapshots recorded the old
   path, so the daemon frees them on start and captures new ones on the next create of each image or
   project-image generation (that create cold-boots once). Resumes are unaffected: they boot each VM's own
   committed disk.
4. Moves the config dir (`~/.config/slipway-host` to `~/.config/davit-host`, or `~/.config/slipway` to
   `~/.config/davit`) and rewrites the config's paths (data dir, socket, now `davit.sock`, images, tailnet state, auth key).
5. Removes the old `slipway` binary.

The install then continues as usual and starts `davit.service`. On first start the state database relabels
project boxes (`slipway.box*` labels become `davit.box*`), so `davit box ls` lists them, and `davit box up`
resumes them with their disks. The step does nothing on a host that is already Davit, so rerunning the
install is safe.

Without `make host-install`: stop the VMs, then on the host
`davit init --migrate-from-slipway --config ~/.config/davit/config.json --data-dir ~/.svm/davit`, then
`davit init --config ... --data-dir ... --service`.

## A client

```console
$ make client-install HOST=user@host
```

This installs `davit` into `~/.local/bin` and removes the old `slipway` binary there. On first use `davit`
moves the old config dir (`~/.config/slipway` on Linux, `~/Library/Application Support/slipway` on
macOS: the client config, the CLI's SSH key and certificates) into the `davit` one, so the default host
carries over. Rename
`SLIPWAY_*` variables in your shell profile and scripts to `DAVIT_*`.

## Guests and images

- **Images built before the rename** keep working as they are: their guest user is `slipway`, their agent is
  `slipway-guest-agent` (the binary answers to both names), and box work dirs follow the image's user
  (`/home/slipway/work` there, `/home/davit/work` in rebuilt images). Rebuilding (`images/build.sh`, or
  `make host-install` without `--skip-images`) gives new VMs the `davit` user; existing VMs keep the disk
  they have.
- **Projects** with `.slipway/setup.sh` still have it run; rename it to `.davit/setup.sh` when convenient.

## SDKs

Python: `pip install davit-sdk`, then `import davit` (`davit.Client`, `davit.DavitError`). TypeScript:
`@davit/sdk` (`DavitClient`, `DavitError`). The wire protocol is unchanged apart from the header names
(`X-Davit-Signature`, `X-Davit-Content-Sha256`, `X-Davit-File-Mode`): webhook receivers must read
`X-Davit-Signature`, and the SDKs' `verify_webhook`/`verifyWebhook` do.
