#!/usr/bin/env bash
# Release artifacts (`make release`; .github/workflows/release.yml publishes them on a v* tag):
# one flat directory laid out like a GitHub release, in .build/release/<version>/ (outside git).
#
#   davit-<os>-<arch>.tar.gz                 linux/amd64, linux/arm64, darwin/arm64, darwin/amd64
#   davit-image-<name>-<arch>.tar.zst        one per image: manifests/<name>.json + every blob it
#                                            references (kernel, rootfs), digest-verified
#   install.sh                               packaging/install.sh, stamped with this version
#   release.json                             version, commit, assets, images
#   SHA256SUMS                               every file above
#   .build/release/latest                    the version (local convenience, not an asset)
#
# Install from it: sh .build/release/<version>/install.sh --host --from .build/release/<version>
#
# Archives are reproducible for a given commit (sorted entries, commit-time mtimes, root
# ownership, gzip -n; image blobs are themselves content-addressed).
# Env: DAVIT_ALLOW_DIRTY=1 (development release from a dirty tree); DAVIT_IMAGES_OUT (holds
# blobs/sha256, default images/out); DAVIT_MANIFESTS (manifest dir, default images/manifests);
# RELEASE_IMAGES (comma-separated image names, default every manifest there; "none" for
# binaries only); ZSTD_LEVEL (default 12).
set -euo pipefail
umask 022
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo"
log() { printf '[release %s] %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
die() { log "error: $*"; exit 1; }

GO=(mise exec -- go)
version=$(git describe --tags --always --dirty)
commit=$(git rev-parse HEAD)
if [[ -n $(git status --porcelain) ]]; then
  [[ $version == *-dirty ]] || version+=-dirty
  [[ ${DAVIT_ALLOW_DIRTY:-0} == 1 ]] || die "working tree is dirty: release from a commit (or DAVIT_ALLOW_DIRTY=1 for a development release)"
fi
epoch=$(git log -1 --format=%ct)
out=$repo/.build/release
dir=$out/$version
blobs=${DAVIT_IMAGES_OUT:-$repo/images/out}/blobs/sha256
manifest_dir=${DAVIT_MANIFESTS:-$repo/images/manifests}
work=$repo/.build/release-work
for t in tar gzip zstd sha256sum python3; do command -v "$t" >/dev/null || die "need $t"; done

rm -rf "$dir" "$work"
mkdir -p "$dir" "$work"
done_ok=
# An incomplete release must never look installable: drop it unless we reach the end.
trap 'rm -rf "$work"; [[ -n $done_ok ]] || rm -rf "$dir"' EXIT

# Deterministic tar: sorted names, commit-time mtimes, root ownership, no atime/ctime headers.
dtar() { tar --sort=name --mtime="@$epoch" --owner=0 --group=0 --numeric-owner \
  --pax-option=exthdr.name=%d/PaxHeaders/%f,delete=atime,delete=ctime "$@"; }

# --- host binaries (same flags as `make build`: CGO-free, -trimpath, version stamp) ---
for target in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64; do
  os=${target%/*} arch=${target#*/}
  log "binary $os/$arch"
  b=$work/bin-$os-$arch
  mkdir -p "$b" "$work/gotmp"
  env -u GOFLAGS GOTMPDIR="$work/gotmp" CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" GOAMD64=v1 GOARM64=v8.0 \
    "${GO[@]}" build -trimpath -ldflags "-s -w -X davit/internal/buildinfo.Version=$version -X davit/internal/buildinfo.Commit=$commit" -o "$b/davit" ./cmd/davit
  dtar -C "$b" -cf - davit | gzip -n -9 >"$dir/davit-$os-$arch.tar.gz"
done

# --- guest image bundles: manifest + every blob it references, verified before packing ---
manifests=()
if [[ ${RELEASE_IMAGES:-} != none ]]; then
  mapfile -t manifests < <(
    if [[ -n ${RELEASE_IMAGES:-} ]]; then for n in ${RELEASE_IMAGES//,/ }; do echo "$manifest_dir/$n.json"; done
    else ls "$manifest_dir"/*.json; fi
  )
fi
for m in ${manifests[@]+"${manifests[@]}"}; do
  [[ -f $m ]] || die "no manifest $m"
  info=$(python3 -I - "$m" <<'PY'
import json, re, sys
m = json.load(open(sys.argv[1]))
agent = m["guest"]["agent"]
if agent.get("dirty"):
    sys.exit(f"{sys.argv[1]}: built from a dirty tree (agent {agent['version']}); rebuild images from a commit")
if not agent.get("protocol"):
    sys.exit(f"{sys.argv[1]}: records no guest-agent protocol; rebuild it (images/build.sh)")
digests = sorted(set(re.findall(r"sha256:([0-9a-f]{64})", json.dumps({k: v for k, v in m.items() if k not in ("id", "inputs")}))))
print(m["name"], m["arch"], " ".join(digests))
PY
  ) || die "image manifest $m rejected"
  read -r name arch digests <<<"$info"
  log "image $name ($arch)"
  s=$work/img-$name
  mkdir -p "$s/manifests" "$s/blobs/sha256"
  cp "$m" "$s/manifests/$name.json"
  for h in $digests; do
    [[ -f $blobs/$h ]] || die "$name: blob $h missing from $blobs (build the images first: images/build.sh)"
    [[ $(sha256sum "$blobs/$h" | cut -c1-64) == "$h" ]] || die "$name: blob $h does not match its digest"
    cp --reflink=auto --sparse=always "$blobs/$h" "$s/blobs/sha256/$h"
  done
  # GNU tar --sparse keeps the 4 GiB rootfs to its allocated size; install.sh extracts it sparse.
  dtar --sparse -C "$s" -cf - manifests blobs |
    zstd -q -T0 "-${ZSTD_LEVEL:-12}" -o "$dir/davit-image-$name-$arch.tar.zst"
  rm -rf "$s"
done

# The installer installs the release it came from: its RELEASE_TAG names this version.
# Its REPO comes from buildinfo.Repo, the one definition of where releases live.
repo_name=$(sed -n 's/^const Repo = "\(.*\)"$/\1/p' internal/buildinfo/buildinfo.go)
[[ $repo_name =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]] || die "cannot read buildinfo.Repo"
grep -q '^RELEASE_TAG=$' packaging/install.sh || die "packaging/install.sh has no empty RELEASE_TAG= line to stamp"
sed -e "s/^RELEASE_TAG=\$/RELEASE_TAG=$version/" -e "s|^REPO=.*|REPO=$repo_name|" packaging/install.sh >"$dir/install.sh"
chmod 0755 "$dir/install.sh"

# --- release.json: what this release is (no build timestamp: reproducible) ---
python3 -I - "$dir" "$version" "$commit" ${manifests[@]+"${manifests[@]}"} <<'PY'
import hashlib, json, os, sys
d, version, commit, mans = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4:]
def sha(p):
    h = hashlib.sha256()
    with open(p, "rb") as f:
        for c in iter(lambda: f.read(1 << 20), b""):
            h.update(c)
    return h.hexdigest()
assets = {f: {"sha256": sha(os.path.join(d, f)), "size": os.path.getsize(os.path.join(d, f))}
          for f in sorted(os.listdir(d))}
images = []
for p in mans:
    m = json.load(open(p))
    a = m["guest"]["agent"]
    images.append({"name": m["name"], "arch": m["arch"], "manifestId": m["id"],
                   "bundle": f"davit-image-{m['name']}-{m['arch']}.tar.zst",
                   "kernel": m["kernel"]["digest"], "rootfs": m["rootfs"]["digest"],
                   "agent": {"version": a["version"], "commit": a["commit"], "protocol": a["protocol"]},
                   "capabilities": sorted(m.get("capabilities", {}))})
rel = {"schemaVersion": 1, "version": version, "commit": commit, "signed": False,
       "assets": assets, "images": images}
with open(os.path.join(d, "release.json"), "w") as f:
    json.dump(rel, f, indent=2)
    f.write("\n")
for i in images:
    if i["agent"]["commit"] != commit:
        print(f"note: {i['name']} bakes agent {i['agent']['version']} (commit {i['agent']['commit'][:12]}), "
              f"release is {commit[:12]}; compatibility is by protocol {i['agent']['protocol']['min']}-"
              f"{i['agent']['protocol']['max']}", file=sys.stderr)
PY

(cd "$dir" && find . -maxdepth 1 -type f -printf '%f\n' | LC_ALL=C sort | xargs sha256sum) >"$work/SHA256SUMS"
mv "$work/SHA256SUMS" "$dir/SHA256SUMS"

# No detached signatures: SHA256SUMS gives integrity, and the release workflow adds GitHub
# build-provenance attestations for authenticity (docs/install-host.md#trust).

done_ok=1
echo "$version" >"$out/latest"
# Superseded local releases: keep only this one (bundles are GiBs).
for old in "$out"/*/; do
  [[ ${old%/} == "$dir" ]] || rm -rf "${old%/}"
done
log "done: $dir"
ls -l "$dir" >&2
