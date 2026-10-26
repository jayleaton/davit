# packaging

Release artifacts, the installer, and the templates `davit init` renders. The user-facing install guide is
[docs/install-host.md](../docs/install-host.md).

## `make release`

`make release` runs `packaging/release.sh` (under `$DAVIT_HEAVY` when set). It needs a clean tree (or
`DAVIT_ALLOW_DIRTY=1`) and built images. It writes one flat directory, laid out like a GitHub release, into
`.build/release/<version>/`; `.build/release/latest` names the version. Older version directories are deleted
(bundles are GiBs).

| File | What |
| --- | --- |
| `davit-<os>-<arch>.tar.gz` | static, CGO-free binaries for linux/amd64, linux/arm64, darwin/arm64 and darwin/amd64. Deterministic tar (sorted, commit-time mtimes, root owner, `gzip -n`) |
| `davit-image-<name>-<arch>.tar.zst` | per image: the manifest plus every blob it references (kernel and rootfs), each verified against its digest before packing. Sparse tar, so a 4 GiB rootfs packs to its allocated size. Manifests built from a dirty tree, or without a recorded agent protocol, are refused |
| `install.sh` | the installer, stamped with this version (`RELEASE_TAG`) and `buildinfo.Repo` (`REPO`) |
| `release.json` | version, commit, every asset's sha256 and size, and per image the manifest id, kernel/rootfs digests, the baked agent's version, commit and protocol, and capabilities |
| `SHA256SUMS` | every file above |

Environment: `DAVIT_IMAGES_OUT` (holds `blobs/sha256`, default `images/out`), `DAVIT_MANIFESTS` (default
`images/manifests`), `RELEASE_IMAGES` (comma-separated names, default every manifest there; `none` for
binaries only), `ZSTD_LEVEL` (default 12).

`.github/workflows/release.yml` builds the images and runs `make release` on a pushed `v*` tag, then publishes
the directory as a GitHub release with a build-provenance attestation per asset.

## `install.sh`

POSIX `sh` (dash and bash). Roles: `--host` (Linux with KVM: binary, root-fix check, image, config, systemd
user service, lingering, `davit doctor`) and `--client` (the binary only; the default on macOS). Behaviour,
options and the root commands it may print are in [docs/install-host.md](../docs/install-host.md). Design
points:

- The release location defaults to `https://github.com/$REPO/releases/latest/download`, or
  `.../releases/download/$RELEASE_TAG` for a stamped copy; `--from` takes `https://`, `file://` or a directory.
  curl may only use `https` and `file`, and redirects must be `https`; plain `http://` is refused.
- Every file is checked against `SHA256SUMS` from the same release (exactly one entry). The binary archive must
  hold exactly one regular file, `davit`; an image bundle only `manifests/<name>.json` and
  `blobs/sha256/<hex>`, each blob hashing to its name. Bundles are staged on the data dir's filesystem, never
  in `TMPDIR`, and the manifest moves into place last.
- The root commands come from `davit doctor --fixes`, so the installer and doctor never disagree. They run
  only with `--yes-sudo`; otherwise the install stops (exit 3) before any large download.
- Idempotent: an image whose bundle digest matches `images/.bundles/<bundle>.sha256` is skipped; the service
  restarts only when the binary or an image changed.
- It refuses to run as root. All logic is in functions and the last line is `main "$@"`, so a truncated
  download runs nothing.

## Tests

```sh
mise exec -- go test -run 'Install|Release' ./packaging/
```

`install_test.go` covers the client role and the failure cases (tampered binary, extra archive entries,
missing or duplicate `SHA256SUMS` entries, `http://`, bad versions, no role) under sh, bash and dash.
`install_host_test.go` runs the host role against a fake `davit` with stub `sudo`, `systemctl`, `loginctl`
and `sg`: fixes before downloads, `--yes-sudo`, image verification failures, a failing doctor, and the
idempotent rerun.

`TestReleaseInstallAndBoot` (opt-in: `DAVIT_TEST_RELEASE=$PWD/.build/release`, plus
`DAVIT_TEST_FIRECRACKER=1` on a VM host) installs a real release and boots a VM from the bundled image.
`scripts/fresh-host-e2e.sh` runs the whole `--host` flow as a throwaway Linux user on a VM host.

## AppArmor profile

`apparmor/davit.tmpl` is the user-namespace profile for Ubuntu's `kernel.apparmor_restrict_unprivileged_userns`.
`davit init --apparmor` (and `davit doctor --fixes`) render it for the resolved binary path; the profile name
is unique per binary path, so several users' installs on one host do not collide. Golden output is in
`testdata/apparmor-*.golden`; design and removal in [docs/network.md](../docs/network.md#ubuntu-apparmor).
