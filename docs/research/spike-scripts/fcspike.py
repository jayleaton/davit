#!/usr/bin/env python3
"""Firecracker spike harness: cold boot, snapshot, restore, parallel fork, memory.

Each VM runs in its own directory (cwd) inside its own user+net namespace
(`unshare -rn`), so it can own a tap0 without root and every fork keeps the
same guest IP/MAC. Drive and vsock paths are relative, so a snapshot restored
in another directory picks up that directory's reflinked rootfs.ext4.

Best run under a low-priority wrapper (e.g. nice/ionice) on a shared host. Usage:
  fcspike.py all [--forks 1,5,10] [--mem 384] [--vcpus 2]
"""
import argparse, concurrent.futures as cf, http.client, json, os, shutil, signal
import socket, statistics, subprocess, sys, time

S = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
FC = f"{S}/bin/firecracker"
KERNEL = f"{S}/dl/kernel/vmlinux-6.1.155"
BASE = f"{S}/images/rootfs.ext4"
KEY = f"{S}/work/id_ed25519"
PROXY = f"{S}/scripts/vsock-proxy.py"
RUN = f"{S}/run"
# Short symlinks for client-side socket paths (sun_path is limited to 108 bytes).
SOCKDIR = os.path.expanduser("~/.cache/vmspike")
BOOT_ARGS = ("console=ttyS0 reboot=k panic=1 pci=off quiet loglevel=1 "
             "i8042.noaux i8042.nomux i8042.dumbkbd root=/dev/vda rw")
GUEST_MAC = "06:00:ac:10:00:02"

# Netns bootstrap: tap0 for the guest, plus a unix-socket bridge to guest :22 so
# the host (outside the netns) can reach sshd over the real virtio-net path.
NETNS_SH = """set -e
ip link set lo up
ip tuntap add tap0 mode tap
ip addr add 172.16.0.1/24 dev tap0
ip link set tap0 up
rm -f tcp22.sock
socat UNIX-LISTEN:tcp22.sock,fork TCP:172.16.0.2:22 &
exec "$@"
"""


class UHTTP(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("localhost", timeout=30)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX)
        self.sock.connect(self.path)


def now():
    return time.monotonic()


def reflink(src, dst):
    subprocess.run(["cp", "--reflink=always", src, dst], check=True)


class VM:
    def __init__(self, name):
        self.dir = f"{RUN}/{name}"
        shutil.rmtree(self.dir, ignore_errors=True)
        os.makedirs(self.dir)
        os.makedirs(SOCKDIR, exist_ok=True)
        self.short = f"{SOCKDIR}/{name}"
        if os.path.lexists(self.short):
            os.remove(self.short)
        os.symlink(self.dir, self.short)
        self.proc = None

    def sp(self, f):
        return f"{self.short}/{f}"

    def p(self, f):
        return f"{self.dir}/{f}"

    def start(self, config=None):
        args = [FC, "--api-sock", "api.sock", "--level", "Warning"]
        if config:
            with open(self.p("vm.json"), "w") as f:
                json.dump(config, f)
            args += ["--config-file", "vm.json"]
        self.console = open(self.p("console.log"), "wb")
        self.proc = subprocess.Popen(
            ["unshare", "-rn", "sh", "-c", NETNS_SH, "netns"] + args,
            cwd=self.dir, stdin=subprocess.DEVNULL, stdout=self.console,
            stderr=subprocess.STDOUT, start_new_session=True)

    def fc_pid(self):
        # unshare execs sh which execs firecracker: same pid as Popen.
        return self.proc.pid

    def api(self, method, path, body=None):
        c = UHTTP(self.sp("api.sock"))
        c.request(method, path, json.dumps(body) if body is not None else None,
                  {"Content-Type": "application/json"})
        r = c.getresponse()
        data = r.read()
        if r.status >= 300:
            raise RuntimeError(f"{method} {path} -> {r.status} {data!r}")
        return data

    def wait_api(self, timeout=10):
        end = now() + timeout
        while now() < end:
            try:
                self.api("GET", "/")
                return
            except (FileNotFoundError, ConnectionRefusedError):
                time.sleep(0.001)
        raise TimeoutError("api socket")

    def banner(self, via="vsock", timeout=60):
        """Poll until sshd's 'SSH-' banner arrives; returns monotonic time."""
        end = now() + timeout
        while now() < end:
            if self.proc.poll() is not None:
                raise RuntimeError(f"firecracker exited: {open(self.p('console.log'),'rb').read()[-2000:]!r}")
            s = socket.socket(socket.AF_UNIX)
            s.settimeout(2)
            try:
                if via == "vsock":
                    s.connect(self.sp("v.sock"))
                    s.sendall(b"CONNECT 22\n")
                    buf = b""
                    while b"\n" not in buf:
                        c = s.recv(64)
                        if not c:
                            raise ConnectionError
                        buf += c
                    if not buf.startswith(b"OK"):
                        raise ConnectionError
                    rest = buf.split(b"\n", 1)[1]
                else:
                    s.connect(self.sp("tcp22.sock"))
                    rest = b""
                while len(rest) < 4:
                    c = s.recv(64)
                    if not c:
                        raise ConnectionError
                    rest += c
                if rest.startswith(b"SSH-"):
                    return now()
            except (OSError, ConnectionError):
                pass
            finally:
                s.close()
            time.sleep(0.002)
        raise TimeoutError(f"ssh banner via {via}")

    def ssh(self, cmd, via="vsock", check=True):
        sock = self.sp("v.sock") if via == "vsock" else self.sp("tcp22.sock")
        proxy = (f"python3 -I {PROXY} {sock} 22" if via == "vsock"
                 else f"socat - UNIX-CONNECT:{sock}")
        r = subprocess.run(
            ["ssh", "-q", "-i", KEY, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
             "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
             "-o", "ConnectTimeout=5", "-o", f"ProxyCommand={proxy}", "root@vm", cmd],
            capture_output=True, text=True, timeout=60)
        if check and r.returncode != 0:
            raise RuntimeError(f"ssh {cmd!r} rc={r.returncode} {r.stderr}")
        return r.stdout.strip()

    def ssh_ready(self, via="vsock", timeout=30):
        end = now() + timeout
        while now() < end:
            try:
                self.ssh("true", via=via)
                return now()
            except (RuntimeError, subprocess.TimeoutExpired):
                time.sleep(0.01)
        raise TimeoutError("ssh")

    def mem(self):
        out = {}
        with open(f"/proc/{self.fc_pid()}/smaps_rollup") as f:
            for line in f:
                k, _, v = line.partition(":")
                if k in ("Rss", "Pss", "Pss_Anon", "Pss_File", "Private_Dirty",
                         "Private_Clean", "Shared_Clean", "Anonymous", "Swap"):
                    out[k] = int(v.split()[0]) // 1024  # MiB
        return out

    def kill(self):
        if self.proc and self.proc.poll() is None:
            os.killpg(self.proc.pid, signal.SIGKILL)
            self.proc.wait()


def vm_config(mem, vcpus, balloon=False):
    cfg = {
        "boot-source": {"kernel_image_path": KERNEL, "boot_args": BOOT_ARGS},
        "drives": [{"drive_id": "rootfs", "path_on_host": "rootfs.ext4",
                    "is_root_device": True, "is_read_only": False}],
        "machine-config": {"vcpu_count": vcpus, "mem_size_mib": mem, "smt": False},
        "network-interfaces": [{"iface_id": "eth0", "guest_mac": GUEST_MAC,
                                "host_dev_name": "tap0"}],
        "vsock": {"guest_cid": 3, "uds_path": "v.sock"},
    }
    if balloon:
        # Free page reporting: guest hands freed pages back (host THP=always otherwise
        # makes a cold VM's RSS approach its full mem_size).
        cfg["balloon"] = {"amount_mib": 0, "deflate_on_oom": True,
                          "free_page_reporting": True}
    return cfg


def cold_boot(name, mem, vcpus, balloon=False):
    vm = VM(name)
    t = {}
    t0 = now()
    reflink(BASE, vm.p("rootfs.ext4"))
    t["reflink_disk"] = now() - t0
    t0 = now()
    vm.start(vm_config(mem, vcpus, balloon))
    t["banner_vsock"] = vm.banner("vsock") - t0
    t["ssh_vsock"] = vm.ssh_ready("vsock") - t0
    t["ssh_tap"] = vm.ssh_ready("tap") - t0
    return vm, t


def take_snapshot(vm, snapdir):
    shutil.rmtree(snapdir, ignore_errors=True)
    os.makedirs(snapdir)
    t = {}
    t0 = now()
    vm.api("PATCH", "/vm", {"state": "Paused"})
    t["pause"] = now() - t0
    t1 = now()
    vm.api("PUT", "/snapshot/create", {"snapshot_type": "Full",
                                       "snapshot_path": f"{snapdir}/vmstate",
                                       "mem_file_path": f"{snapdir}/mem"})
    t["snapshot_create_mem"] = now() - t1
    t1 = now()
    reflink(vm.p("rootfs.ext4"), f"{snapdir}/rootfs.ext4")
    t["snapshot_disk_reflink"] = now() - t1
    t["snapshot_total"] = now() - t0
    # Same paused VM again without fsync of the mem file (page cache only).
    t1 = now()
    vm.api("PUT", "/snapshot/create", {"snapshot_type": "Full",
                                       "snapshot_path": f"{snapdir}/vmstate.nosync",
                                       "mem_file_path": f"{snapdir}/mem.nosync",
                                       "sync_snapshot_files": False})
    t["snapshot_create_mem_nosync"] = now() - t1
    os.remove(f"{snapdir}/vmstate.nosync"); os.remove(f"{snapdir}/mem.nosync")
    return t


def restore(name, snapdir, via="vsock"):
    vm = VM(name)
    t = {}
    t0 = now()
    reflink(f"{snapdir}/rootfs.ext4", vm.p("rootfs.ext4"))
    t["disk_reflink"] = now() - t0
    vm.start()
    vm.wait_api()
    t["fc_api_up"] = now() - t0
    vm.api("PUT", "/snapshot/load", {
        "snapshot_path": f"{snapdir}/vmstate",
        "mem_backend": {"backend_type": "File", "backend_path": f"{snapdir}/mem"},
        "resume_vm": True})
    t["loaded_resumed"] = now() - t0
    t["banner_vsock"] = vm.banner(via) - t0
    t["ssh_vsock"] = vm.ssh_ready(via) - t0
    return vm, t


def fmt(d):
    return {k: (round(v * 1000, 1) if isinstance(v, float) else v) for k, v in d.items()}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("cmd", choices=["all"])
    ap.add_argument("--forks", default="1,5,10")
    ap.add_argument("--mem", type=int, default=384)
    ap.add_argument("--vcpus", type=int, default=2)
    ap.add_argument("--boots", type=int, default=3)
    ap.add_argument("--restores", type=int, default=3)
    ap.add_argument("--idle", type=float, default=15)
    ap.add_argument("--balloon", action="store_true")
    ap.add_argument("--out", default="fc-results.json")
    a = ap.parse_args()
    forks = [int(x) for x in a.forks.split(",")]
    assert max(forks) * a.mem <= 4096, "VM RAM cap 4 GiB"
    os.makedirs(RUN, exist_ok=True)
    res = {"params": vars(a), "loadavg_start": open("/proc/loadavg").read().split()[:3]}
    live = []
    try:
        # (a) cold boots
        boots = []
        for i in range(a.boots):
            vm, t = cold_boot(f"boot{i}", a.mem, a.vcpus, a.balloon)
            if i < a.boots - 1:
                vm.kill()
            boots.append(fmt(t))
            print("cold_boot", fmt(t), flush=True)
        res["cold_boot"] = boots
        src = vm
        live.append(src)
        res["guest"] = {
            "uname": src.ssh("uname -r"),
            "os": src.ssh(". /etc/os-release; echo $PRETTY_NAME"),
            "systemd_analyze": src.ssh("systemd-analyze 2>&1 | head -1", check=False),
            "free_mib": src.ssh("free -m | awk '/Mem:/{print $2,$3,$7}'"),
        }
        t0 = now()
        src.ssh("until docker info >/dev/null 2>&1; do sleep 0.05; done")
        src.ssh("tar -C /usr/bin -c busybox | docker import - bb >/dev/null")
        res["guest"]["docker_run"] = src.ssh("docker run --rm bb /busybox echo docker-ok")
        res["guest"]["docker_version"] = src.ssh("docker version --format '{{.Server.Version}}'")
        src.ssh("echo from-source > /root/marker; sync")
        print("guest", res["guest"], flush=True)
        time.sleep(a.idle)
        res["mem_cold_booted_idle"] = src.mem()
        print("mem cold", res["mem_cold_booted_idle"], flush=True)

        # (b) snapshot (stop = snapshot)
        snapdir = f"{RUN}/snap"
        res["snapshot"] = fmt(take_snapshot(src, snapdir))
        res["snapshot_sizes_mib"] = {
            f: round(os.stat(f"{snapdir}/{f}").st_blocks * 512 / 2**20, 1)
            for f in os.listdir(snapdir)}
        print("snapshot", res["snapshot"], res["snapshot_sizes_mib"], flush=True)
        src.kill()
        live.remove(src)

        # (c) single restores, sequential
        rs = []
        for i in range(a.restores):
            vm, t = restore(f"restore{i}", snapdir)
            assert vm.ssh("cat /root/marker") == "from-source"
            t["tap_ssh"] = vm.ssh("echo ok", via="tap")
            vm.kill()
            rs.append(fmt(t))
            print("restore", fmt(t), flush=True)
        res["restore"] = rs

        # (d) parallel forks
        res["fork"] = {}
        for n in forks:
            t0 = now()
            with cf.ThreadPoolExecutor(n) as ex:
                out = list(ex.map(lambda i: restore(f"fork{n}-{i}", snapdir), range(n)))
            wall = now() - t0
            vms = [v for v, _ in out]
            live.extend(vms)
            ssh_times = [t["ssh_vsock"] * 1000 for _, t in out]
            # isolation: write in fork 0, confirm absent in fork 1; tap path in each
            vms[0].ssh("echo only-fork0 > /root/f0")
            iso = n == 1 or vms[1].ssh("test -e /root/f0 && echo leak || echo isolated")
            tap_ok = sum(1 for v in vms if v.ssh("echo ok", via="tap") == "ok")
            time.sleep(a.idle)
            mems = [v.mem() for v in vms]
            entry = {
                "wall_all_ssh_ready_ms": round(wall * 1000, 1),
                "ssh_ready_ms_min_med_max": [round(min(ssh_times), 1),
                                             round(statistics.median(ssh_times), 1),
                                             round(max(ssh_times), 1)],
                "per_vm": [fmt(t) for _, t in out],
                "isolation": iso, "tap_ssh_ok": f"{tap_ok}/{n}",
                "mem_mib_per_vm_avg": {k: round(statistics.mean(m[k] for m in mems), 1)
                                       for k in mems[0]},
                "pss_total_mib": sum(m["Pss"] for m in mems),
            }
            res["fork"][n] = entry
            print("fork", n, {k: v for k, v in entry.items() if k != "per_vm"}, flush=True)
            for v in vms:
                v.kill()
                live.remove(v)
    finally:
        for v in live:
            v.kill()
        res["loadavg_end"] = open("/proc/loadavg").read().split()[:3]
        with open(f"{S}/results/{a.out}", "w") as f:
            json.dump(res, f, indent=1)
    print(json.dumps({k: v for k, v in res.items() if k != "fork"}, indent=1))


if __name__ == "__main__":
    main()
