#!/usr/bin/env python3
"""Wrap one rootfs layer tar into a docker-archive image tar: oci-archive.py <layer.tar> <out.tar> <repo:tag>."""
import hashlib, io, json, sys, tarfile

layer, out, tag = sys.argv[1:4]
h = hashlib.sha256()
with open(layer, "rb") as f:
    for chunk in iter(lambda: f.read(1 << 20), b""):
        h.update(chunk)
diff_id = "sha256:" + h.hexdigest()
config = json.dumps({
    "architecture": "amd64", "os": "linux",
    "config": {"Cmd": ["/bin/bash"], "Env": ["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"]},
    "rootfs": {"type": "layers", "diff_ids": [diff_id]},
}).encode()
cfg_name = hashlib.sha256(config).hexdigest() + ".json"
manifest = json.dumps([{"Config": cfg_name, "RepoTags": [tag], "Layers": ["layer.tar"]}]).encode()
with tarfile.open(out, "w") as t:
    for name, data in ((cfg_name, config), ("manifest.json", manifest)):
        ti = tarfile.TarInfo(name); ti.size = len(data); t.addfile(ti, io.BytesIO(data))
    t.add(layer, arcname="layer.tar")
print(out, diff_id)
