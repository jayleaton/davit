#!/usr/bin/env python3
"""Write images/manifests/<image>.json from the build's .env records and the recipe files.

Usage: make-manifest.py <image> <arch>   (arch: x86_64|aarch64)
Writes manifests/<image>.json for x86_64 and manifests/<image>-<arch>.json otherwise
($DAVIT_MANIFESTS overrides the directory, e.g. for development builds).

Content addressed: kernel/rootfs/etc. are referenced by sha256 digest and live in
$DAVIT_IMAGES_OUT/blobs/sha256/<hex>. The manifest carries no build timestamp; its "id" is the
sha256 of its canonical JSON (sorted keys, compact separators) with "id" omitted, so identical
inputs and outputs give an identical manifest.
"""
import hashlib, json, os, sys

IMAGES = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
WORK = os.environ.get("DAVIT_IMAGES_WORK",
                      os.path.join(os.environ.get("XDG_CACHE_HOME") or os.path.expanduser("~/.cache"), "davit/images"))
OUT = os.environ.get("DAVIT_IMAGES_OUT", f"{IMAGES}/out")

# Image contract shared with the runtime adapter, guest agent and network adapter (README.md).
BOOT_ARGS = ("console=ttyS0 reboot=k panic=1 pci=off quiet loglevel=1 "
             "i8042.noaux i8042.nomux i8042.dumbkbd root=/dev/vda rw")
GUEST_AGENT_PATH = "/usr/local/bin/davit-guest-agent"
GUEST_AGENT_VSOCK_PORT = 1024


def kv(path):
    out = {}
    with open(path) as f:
        for line in f:
            line = line.rstrip("\n")
            if line and not line.startswith("#") and "=" in line:
                k, v = line.split("=", 1)
                out[k] = v
    return out


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def blob(hexdigest):
    path = f"{OUT}/blobs/sha256/{hexdigest}"
    if sha256_file(path) != hexdigest:
        sys.exit(f"blob {path} does not match its digest")
    return {"digest": f"sha256:{hexdigest}", "size": os.path.getsize(path)}


def chain(image):
    """The recipes that build image, root first: each derived recipe names its parent in
    rootfs/<image>/base, and the root (no base) provisions."""
    names = [image]
    while os.path.exists(f"{IMAGES}/rootfs/{names[-1]}/base"):
        parent = open(f"{IMAGES}/rootfs/{names[-1]}/base").read().strip()
        if parent in names:
            sys.exit(f"{image}: base cycle {' -> '.join(names + [parent])}")
        if not parent or not os.path.isdir(f"{IMAGES}/rootfs/{parent}"):
            sys.exit(f"{names[-1]}: base names no recipe (got {parent!r})")
        names.append(parent)
    return names[::-1]


def base_of(image):
    """The parent recipe of a derived image (rootfs/<image>/base), or None."""
    names = chain(image)
    return names[-2] if len(names) > 1 else None


# Files that shape each output (not test or manifest tooling, so editing smoke.py needs no rebuild).
KERNEL_RECIPE = ["inputs.lock", "kernel", "scripts/lib.sh", "scripts/builder.sh",
                 "scripts/build-kernel.sh", "scripts/kernel-inner.sh"]


def rootfs_recipe(image):
    paths = ["inputs.lock", "scripts/lib.sh", "scripts/builder.sh", "scripts/build-rootfs.sh",
             "scripts/build-agent.sh"]
    return paths + [f"rootfs/{name}" for name in chain(image)]


def recipe_files(paths):
    """Digest of every file under paths (relative to images/), sorted by path."""
    files = []
    for rel in paths:
        p = f"{IMAGES}/{rel}"
        if os.path.isfile(p):
            files.append(p)
            continue
        for d, dirs, names in os.walk(p):
            dirs[:] = [x for x in dirs if x != "__pycache__"]
            files += [os.path.join(d, n) for n in names]
    return {os.path.relpath(f, IMAGES): "sha256:" + sha256_file(f) for f in sorted(set(files))}


def recipe_digest(files):
    return "sha256:" + hashlib.sha256("".join(f"{p} {d}\n" for p, d in sorted(files.items())).encode()).hexdigest()


def main():
    if sys.argv[1] == "--recipe-digest":  # used by the build scripts: <kernel|image>
        scope = sys.argv[2]
        print(recipe_digest(recipe_files(KERNEL_RECIPE if scope == "kernel" else rootfs_recipe(scope))))
        return
    if sys.argv[1] == "--chain":  # used by build-rootfs.sh: <image> -> its recipes, root first
        print(" ".join(chain(sys.argv[2])))
        return
    image, arch = sys.argv[1], sys.argv[2]
    deb_arch = {"x86_64": "amd64", "aarch64": "arm64"}[arch]
    lock = kv(f"{IMAGES}/inputs.lock")
    k = kv(f"{WORK}/kernel-{arch}.env")
    r = kv(f"{WORK}/rootfs-{image}-{deb_arch}.env")

    packages = {}
    with open(f"{OUT}/blobs/sha256/{r['ROOTFS_PACKAGES_BLOB']}") as f:
        for line in f:
            name, version, parch = line.rstrip("\n").split("\t")
            packages[name if parch in ("all", deb_arch) else f"{name}:{parch}"] = version

    # The manifest must describe the recipe that actually built the blobs.
    kfiles, rfiles = recipe_files(KERNEL_RECIPE), recipe_files(rootfs_recipe(image))
    for what, recorded, files in (("kernel", k.get("KERNEL_RECIPE"), kfiles),
                                  (f"rootfs {image}", r.get("ROOTFS_RECIPE"), rfiles)):
        if recorded != recipe_digest(files):
            sys.exit(f"{what} recipe changed since it was built (or predates recipe tracking): rebuild it")
    mdir = os.environ.get("DAVIT_MANIFESTS", f"{IMAGES}/manifests")
    if r["ROOTFS_AGENT_DIRTY"] == "true" and os.path.realpath(mdir) == os.path.realpath(f"{IMAGES}/manifests"):
        sys.exit("refusing to write a dirty-agent image into images/manifests (set DAVIT_MANIFESTS)")
    if r["ROOTFS_KERNEL_BLOB"] != k["KERNEL_BLOB"]:
        sys.exit(f"rootfs was built against kernel {r['ROOTFS_KERNEL_BLOB'][:12]} (modules.builtin), "
                 f"but the current kernel is {k['KERNEL_BLOB'][:12]}: rebuild the rootfs")

    # Image capabilities (e.g. harness.claude): declared by each recipe in the chain, root first
    # (a derived image keeps its parents' and may redeclare one), versions from its npm lock.
    capabilities = {}
    # An npm capability takes its version and integrity from the declaring recipe's npm lock; any
    # other names the inputs.lock variable that pins it (versionVar), formatted as the command
    # prints it (versionFormat, default "{}").
    for recipe_name in chain(image):
        cap_path = f"{IMAGES}/rootfs/{recipe_name}/capabilities.json"
        if not os.path.exists(cap_path):
            continue
        npm_lock = None
        for name, c in json.load(open(cap_path)).items():
            if "npmPackage" in c:
                if npm_lock is None:
                    npm_lock = json.load(open(f"{IMAGES}/rootfs/{recipe_name}/npm/package-lock.json"))["packages"]
                pkg = npm_lock[f"node_modules/{c['npmPackage']}"]
                capabilities[name] = {**c, "version": pkg["version"], "integrity": pkg["integrity"],
                                      "node": lock["NODE_VERSION"]}
            else:
                fmt = c.get("versionFormat", "{}")
                capabilities[name] = {k: v for k, v in c.items() if k not in ("versionVar", "versionFormat")}
                capabilities[name]["version"] = fmt.format(lock[c["versionVar"]])

    recipe = {**kfiles, **rfiles}
    m = {
        "schemaVersion": 1,
        "name": image,
        "base": base_of(image),
        "arch": arch,
        "capabilities": capabilities,
        "kernel": {
            **blob(k["KERNEL_BLOB"]),
            "format": k["KERNEL_IMAGE_FORMAT"],
            "release": k["KERNEL_RELEASE"],
            "bootArgs": BOOT_ARGS,
            "config": blob(k["KERNEL_CONFIG_BLOB"]),
            "modulesBuiltin": blob(k["KERNEL_MODULES_BLOB"]),
            "toolchain": k["KERNEL_TOOLCHAIN"],
        },
        "rootfs": {
            **blob(r["ROOTFS_BLOB"]),
            "fsType": "ext4",
            "fsUuid": r["ROOTFS_FS_UUID"],
            "label": "rootfs",
            "rootDevice": "/dev/vda",
            "sparse": True,
            "packageList": blob(r["ROOTFS_PACKAGES_BLOB"]),
            "packages": packages,
        },
        "guest": {
            "os": f"Ubuntu {lock['UBUNTU_BASE_VERSION'][:5]} ({lock['UBUNTU_SUITE']})",
            "user": {"name": "davit", "uid": 1000, "sudo": "nopasswd", "groups": ["sudo", "docker"]},
            "agent": {
                "path": GUEST_AGENT_PATH,
                "vsockPort": GUEST_AGENT_VSOCK_PORT,
                "unit": "davit-guest-agent.service",
                "command": [GUEST_AGENT_PATH, "-default-user", "davit"],
                "binary": blob(r["ROOTFS_AGENT_BLOB"]),
                "version": r["ROOTFS_AGENT_VERSION"],
                "commit": r["ROOTFS_AGENT_COMMIT"],
                "dirty": r["ROOTFS_AGENT_DIRTY"] == "true",
                "go": r["ROOTFS_AGENT_GO"],
                # Protocol versions + features: the daemon refuses an image whose agent
                # shares no version with it ("major" skew) and warns on missing features.
                "protocol": json.loads(r["ROOTFS_AGENT_PROTOCOL"]),
            },
            "network": {"interface": "eth0", "address": "172.16.0.2/24", "gateway": "172.16.0.1",
                        "dns": ["1.1.1.1"]},
            "identity": {"machineId": "generated-on-first-boot; rotated by the agent's restore barrier",
                         "sshHostKeys": "ed25519 generated on first boot; rotated by the agent's restore barrier"},
            "stop": "SendCtrlAltDel (x86_64) -> systemd ctrl-alt-del.target -> clean shutdown, VMM exits",
        },
        "inputs": {
            "lock": lock,
            "recipe": recipe,
            "recipeDigest": recipe_digest(recipe),
        },
    }
    canonical = json.dumps(m, sort_keys=True, separators=(",", ":")).encode()
    m = {"id": "sha256:" + hashlib.sha256(canonical).hexdigest(), **m}
    os.makedirs(mdir, exist_ok=True)
    dest = f"{mdir}/{image}{'' if arch == 'x86_64' else '-' + arch}.json"
    with open(dest + ".part", "w") as f:
        json.dump(m, f, indent=2)
        f.write("\n")
    os.replace(dest + ".part", dest)
    print(dest, m["id"])


if __name__ == "__main__":
    main()
