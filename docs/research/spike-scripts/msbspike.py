#!/usr/bin/env python3
"""microsandbox (msb) measurements with the same Ubuntu 24.04 + systemd + Docker guest.

Needs MSB_HOME (short path, sun_path limit) and the image loaded as
localhost/ubuntu-dev:24.04 (scripts/build-rootfs.sh images/ubuntu-dev.tar; msb load).
Best run under a low-priority wrapper on a shared host. Usage: msbspike.py [--mem 352] [--forks 1,5,10]
"""
import argparse, concurrent.futures as cf, json, os, statistics, subprocess, time

S = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
IMAGE = "localhost/ubuntu-dev:24.04"
now = time.monotonic


def msb(*args, check=True, timeout=300):
    r = subprocess.run(["msb", *args], capture_output=True, text=True, timeout=timeout)
    if check and r.returncode != 0:
        raise RuntimeError(f"msb {' '.join(args)} rc={r.returncode}: {r.stderr.strip()[-800:]}")
    return r.stdout.strip()


def sh(name, cmd, check=True):
    return msb("exec", name, "--", "sh", "-c", cmd, check=check)


def wait_exec(name, timeout=60):
    end = now() + timeout
    while now() < end:
        if subprocess.run(["msb", "exec", name, "--", "true"], capture_output=True).returncode == 0:
            return now()
        time.sleep(0.01)
    raise TimeoutError(f"exec {name}")


def wait_docker(name):
    sh(name, "until docker info >/dev/null 2>&1; do sleep 0.05; done")
    return now()


def machine_pid(name):
    out = subprocess.run(["pgrep", "-f", f"msb machine --name {name} "], capture_output=True, text=True).stdout.split()
    return int(out[0]) if out else None


def mem(name):
    pid = machine_pid(name)
    out = {}
    with open(f"/proc/{pid}/smaps_rollup") as f:
        for line in f:
            k, _, v = line.partition(":")
            if k in ("Rss", "Pss", "Pss_Anon", "Pss_File", "Pss_Shmem", "Private_Dirty", "Anonymous", "Swap"):
                out[k] = int(v.split()[0]) // 1024
    return out


def ms(x):
    return round(x * 1000, 1)


def rm(*names):
    if names:
        msb("rm", "-f", *names, check=False)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--mem", type=int, default=352)
    ap.add_argument("--cpus", type=int, default=2)
    ap.add_argument("--boots", type=int, default=3)
    ap.add_argument("--forks", default="1,5,10")
    ap.add_argument("--idle", type=float, default=15)
    ap.add_argument("--out", default="msb-results.json")
    a = ap.parse_args()
    forks = [int(x) for x in a.forks.split(",")]
    assert (max(forks) + 1) * a.mem <= 4096, "VM RAM cap 4 GiB (source stays running during fork)"
    create = ["create", "--pull", "never", "--cpus", str(a.cpus), "--memory", f"{a.mem}M",
              "--mount-owned", "/var/lib/docker:kind=disk,size=4G",
              "--mount-owned", "/var/lib/containerd:kind=disk,size=4G", "--init", "auto"]
    res = {"params": vars(a), "msb": msb("--version"),
           "loadavg_start": open("/proc/loadavg").read().split()[:3]}
    live = set()
    try:
        boots = []
        for i in range(a.boots):
            n = f"b{i}"
            rm(n)
            t0 = now()
            msb(*create, "--name", n, IMAGE)
            live.add(n)
            t = {"create_returned": now() - t0}
            t["first_exec"] = wait_exec(n) - t0
            t["docker_ready"] = wait_docker(n) - t0
            t1 = now()
            msb("ssh", n, "--", "true")
            t["ssh_cmd_after_ready"] = now() - t1
            boots.append({k: ms(v) for k, v in t.items()})
            print("cold", boots[-1], flush=True)
            if i < a.boots - 1:
                rm(n); live.discard(n)
        res["cold_create"] = boots
        src = n
        res["guest"] = {"pid1": sh(src, "cat /proc/1/comm"), "kernel": sh(src, "uname -r"),
                        "os": sh(src, ". /etc/os-release; echo $PRETTY_NAME"),
                        "systemd": sh(src, "systemctl is-system-running", check=False),
                        "free_mib": sh(src, "free -m | awk '/Mem:/{print $2,$3,$7}'")}
        sh(src, "tar -C /usr/bin -c busybox | docker import - bb >/dev/null")
        res["guest"]["docker_run"] = sh(src, "docker run --rm bb /busybox echo docker-ok")
        sh(src, "echo from-source > /root/marker; sync")
        time.sleep(a.idle)
        res["mem_cold_idle_mib"] = mem(src)
        print("guest", res["guest"], res["mem_cold_idle_mib"], flush=True)

        # pause / resume (RAM stays resident, no snapshot)
        t0 = now(); msb("pause", src); tp = now() - t0
        t0 = now(); msb("resume", src); wait_exec(src)
        res["pause_resume"] = {"pause": ms(tp), "resume_to_exec": ms(now() - t0)}
        print("pause/resume", res["pause_resume"], flush=True)

        # (d) live fork x N (source keeps running)
        res["fork"] = {}
        for nf in forks:
            names = [f"f{nf}-{j}" for j in range(nf)]
            rm(*names)
            t0 = now()
            msb("fork", src, "--names", *names, timeout=600)
            live.update(names)
            t_ret = now() - t0
            with cf.ThreadPoolExecutor(nf) as ex:
                ready = list(ex.map(lambda x: wait_exec(x) - t0, names))
            wall = now() - t0
            sh(names[0], "echo only-fork0 > /root/f0")
            iso = nf == 1 or sh(names[1], "test -e /root/f0 && echo leak || echo isolated")
            marker = sh(names[-1], "cat /root/marker")
            dock = sh(names[-1], "docker run --rm bb /busybox echo docker-ok", check=False)
            time.sleep(a.idle)
            mems = [mem(x) for x in names]
            e = {"fork_cmd_returned_ms": ms(t_ret), "wall_all_exec_ms": ms(wall),
                 "exec_ready_ms_min_med_max": [ms(min(ready)), ms(statistics.median(ready)), ms(max(ready))],
                 "isolation": iso, "marker": marker, "docker_in_fork": dock,
                 "mem_mib_per_vm_avg": {k: round(statistics.mean(m[k] for m in mems), 1) for k in mems[0]},
                 "pss_total_mib": sum(m["Pss"] for m in mems),
                 "source_mem_mib": mem(src)}
            res["fork"][nf] = e
            print("fork", nf, e, flush=True)
            rm(*names); live.difference_update(names)

        # (b) full snapshot (RAM + disk) of the running source
        rm()
        t0 = now()
        msb("snap", "create", "full1", "--sandbox", src, "--full", timeout=600)
        res["snapshot_full_ms"] = ms(now() - t0)
        res["snapshot_list"] = msb("snap", "ls", check=False)[-1500:]
        print("snapshot full", res["snapshot_full_ms"], flush=True)

        # (c) restores from the full snapshot
        res["restore"] = {}
        for mode, extra in (("full_copy", []), ("full_cow", ["--cow-mem"]), ("disk_only", ["--disk-only"])):
            rs = []
            for i in range(2):
                n = f"r-{mode}-{i}".replace("_", "")
                rm(n)
                t0 = now()
                msb("snap", "restore", f"{src}:full1", "--name", n, *extra, timeout=600)
                live.add(n)
                t = {"restore_returned": now() - t0, "first_exec": wait_exec(n) - t0}
                if mode == "disk_only":
                    t["docker_ready"] = wait_docker(n) - t0
                r = {k: ms(v) for k, v in t.items()}
                r["marker"] = sh(n, "cat /root/marker", check=False)
                r["clock_skew_s"] = round(float(sh(n, "date +%s.%N")) - time.time(), 1)
                r["machine_id"] = sh(n, "cat /etc/machine-id", check=False)[:12]
                rs.append(r)
                print("restore", mode, r, flush=True)
                if i == 1 and mode == "full_cow":
                    time.sleep(a.idle)
                    res["mem_cow_restore_idle_mib"] = mem(n)
                rm(n); live.discard(n)
            res["restore"][mode] = rs

        # Boat-style stop: processes end, disk persists; then start again
        t0 = now(); msb("stop", src); res["stop_ms"] = ms(now() - t0)
        t0 = now(); msb("snap", "create", "disk1", "--sandbox", src, timeout=600)
        res["snapshot_disk_after_stop_ms"] = ms(now() - t0)
        t0 = now(); msb("start", src)
        res["start_stopped"] = {"start_returned": ms(now() - t0), "first_exec": ms(wait_exec(src) - t0),
                                "docker_ready": ms(wait_docker(src) - t0),
                                "marker": sh(src, "cat /root/marker", check=False)}
        print("stop/start", res["stop_ms"], res["snapshot_disk_after_stop_ms"], res["start_stopped"], flush=True)
        res["df"] = msb("df", check=False)[-1500:]
    finally:
        rm(*live)
        res["loadavg_end"] = open("/proc/loadavg").read().split()[:3]
        os.makedirs(f"{S}/results", exist_ok=True)
        with open(f"{S}/results/{a.out}", "w") as f:
            json.dump(res, f, indent=1)


if __name__ == "__main__":
    main()
