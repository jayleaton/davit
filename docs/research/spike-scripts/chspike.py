#!/usr/bin/env python3
"""Cloud Hypervisor variant of fcspike.py (same guest image, netns, vsock protocol).

Usage (under a low-priority wrapper on a shared host): chspike.py [--forks 1,5,10] [--restore-mode copyonwrite]
"""
import argparse, concurrent.futures as cf, json, os, shutil, statistics, subprocess, sys, time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import fcspike as F  # noqa: E402

CH = f"{F.S}/bin/cloud-hypervisor"
CH_ARGS = "console=ttyS0 reboot=k panic=1 quiet loglevel=1 root=/dev/vda rw net.ifnames=0"


class CHVM(F.VM):
    def start_ch(self, mem=None, vcpus=None, restore=None):
        args = [CH, "--api-socket", "path=api.sock", "--log-file", "ch.log"]
        if restore:
            args += ["--restore", restore]
        else:
            args += ["--kernel", F.KERNEL, "--cmdline", CH_ARGS,
                     "--cpus", f"boot={vcpus}", "--memory", f"size={mem}M",
                     "--disk", "path=rootfs.ext4,image_type=raw",
                     "--net", f"tap=tap0,mac={F.GUEST_MAC}",
                     "--vsock", "cid=3,socket=v.sock",
                     "--serial", "file=console.log", "--console", "off"]
        self.console = open(self.p("stdout.log"), "wb")
        open(self.p("console.log"), "ab").close()
        self.proc = subprocess.Popen(
            ["unshare", "-rn", "sh", "-c", F.NETNS_SH, "netns"] + args,
            cwd=self.dir, stdin=subprocess.DEVNULL, stdout=self.console,
            stderr=subprocess.STDOUT, start_new_session=True)

    def wait_api(self, timeout=10):
        end = F.now() + timeout
        while F.now() < end:
            try:
                self.api("GET", "/api/v1/vmm.ping")
                return
            except (FileNotFoundError, ConnectionRefusedError):
                time.sleep(0.001)
        raise TimeoutError("api socket")


def restore(name, snapdir, mode):
    vm = CHVM(name)
    t = {}
    t0 = F.now()
    F.reflink(f"{snapdir}/rootfs.ext4", vm.p("rootfs.ext4"))
    t["disk_reflink"] = F.now() - t0
    vm.start_ch(restore=f"source_url=file://{snapdir},memory_restore_mode={mode},resume=true")
    t["banner_vsock"] = vm.banner("vsock") - t0
    t["ssh_vsock"] = vm.ssh_ready("vsock") - t0
    return vm, t


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--forks", default="1,5,10")
    ap.add_argument("--mem", type=int, default=384)
    ap.add_argument("--vcpus", type=int, default=2)
    ap.add_argument("--boots", type=int, default=3)
    ap.add_argument("--restore-modes", default="copy,copyonwrite")
    ap.add_argument("--idle", type=float, default=15)
    ap.add_argument("--out", default="ch-results.json")
    a = ap.parse_args()
    forks = [int(x) for x in a.forks.split(",")]
    assert max(forks) * a.mem <= 4096
    res = {"params": vars(a), "loadavg_start": open("/proc/loadavg").read().split()[:3]}
    live = []
    try:
        boots = []
        for i in range(a.boots):
            vm = CHVM(f"ch-boot{i}")
            F.reflink(F.BASE, vm.p("rootfs.ext4"))
            t0 = F.now()
            vm.start_ch(a.mem, a.vcpus)
            live.append(vm)
            t = {"banner_vsock": vm.banner("vsock") - t0, "ssh_vsock": vm.ssh_ready("vsock") - t0,
                 "ssh_tap": vm.ssh_ready("tap") - t0}
            boots.append(F.fmt(t))
            print("cold_boot", F.fmt(t), flush=True)
            if i < a.boots - 1:
                vm.kill()
                live.remove(vm)
        res["cold_boot"] = boots
        src = vm
        src.ssh("until docker info >/dev/null 2>&1; do sleep 0.05; done")
        src.ssh("tar -C /usr/bin -c busybox | docker import - bb >/dev/null")
        res["docker_run"] = src.ssh("docker run --rm bb /busybox echo docker-ok")
        src.ssh("echo from-source > /root/marker; sync")
        time.sleep(a.idle)
        res["mem_cold_booted_idle"] = src.mem()
        print("mem cold", res["mem_cold_booted_idle"], flush=True)

        snapdir = f"{F.RUN}/ch-snap"
        shutil.rmtree(snapdir, ignore_errors=True)
        os.makedirs(snapdir)
        t0 = F.now()
        src.api("PUT", "/api/v1/vm.pause")
        t1 = F.now()
        src.api("PUT", "/api/v1/vm.snapshot", {"destination_url": f"file://{snapdir}"})
        t2 = F.now()
        F.reflink(src.p("rootfs.ext4"), f"{snapdir}/rootfs.ext4")
        t3 = F.now()
        res["snapshot"] = F.fmt({"pause": t1 - t0, "snapshot_create_mem": t2 - t1,
                                 "snapshot_disk_reflink": t3 - t2, "snapshot_total": t3 - t0})
        res["snapshot_sizes_mib"] = {f: round(os.stat(f"{snapdir}/{f}").st_blocks * 512 / 2**20, 1)
                                     for f in os.listdir(snapdir)}
        print("snapshot", res["snapshot"], res["snapshot_sizes_mib"], flush=True)
        src.kill()
        live.remove(src)

        res["restore"] = {}
        res["fork"] = {}
        for mode in a.restore_modes.split(","):
            rs = []
            for i in range(2):
                vm, t = restore(f"ch-restore-{mode}{i}", snapdir, mode)
                live.append(vm)
                assert vm.ssh("cat /root/marker") == "from-source"
                t["tap_ssh"] = vm.ssh("echo ok", via="tap")
                vm.kill()
                live.remove(vm)
                rs.append(F.fmt(t))
                print("restore", mode, F.fmt(t), flush=True)
            res["restore"][mode] = rs
            res["fork"][mode] = {}
            for n in forks:
                t0 = F.now()
                with cf.ThreadPoolExecutor(n) as ex:
                    out = list(ex.map(lambda i: restore(f"ch-fork-{mode}{n}-{i}", snapdir, mode),
                                      range(n)))
                wall = F.now() - t0
                vms = [v for v, _ in out]
                live.extend(vms)
                st = [t["ssh_vsock"] * 1000 for _, t in out]
                tap_ok = sum(1 for v in vms if v.ssh("echo ok", via="tap") == "ok")
                time.sleep(a.idle)
                mems = [v.mem() for v in vms]
                e = {"wall_all_ssh_ready_ms": round(wall * 1000, 1),
                     "ssh_ready_ms_min_med_max": [round(min(st), 1), round(statistics.median(st), 1),
                                                  round(max(st), 1)],
                     "tap_ssh_ok": f"{tap_ok}/{n}",
                     "mem_mib_per_vm_avg": {k: round(statistics.mean(m[k] for m in mems), 1)
                                            for k in mems[0]},
                     "pss_total_mib": sum(m["Pss"] for m in mems)}
                res["fork"][mode][n] = e
                print("fork", mode, n, e, flush=True)
                for v in vms:
                    v.kill()
                    live.remove(v)
    finally:
        for v in live:
            v.kill()
        res["loadavg_end"] = open("/proc/loadavg").read().split()[:3]
        with open(f"{F.S}/results/{a.out}", "w") as f:
            json.dump(res, f, indent=1)


if __name__ == "__main__":
    main()
