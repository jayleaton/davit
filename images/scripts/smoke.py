#!/usr/bin/env python3
"""Boot a built davit image under Firecracker, rootless, and prove it works.

  smoke.py --manifest images/manifests/ubuntu-24.04-base.json --firecracker PATH [--restores 3]

Each VM runs in its own user+net namespace (`unshare -rn`) with a tap0 at 172.16.0.1 and the
guest at 172.16.0.2 (the image's network contract), so no root is needed. Socat bridges inside
the namespace expose guest :22 and :8080 to the host as Unix sockets (Unix sockets cross netns
boundaries) and serve a fake "internet" at 172.16.0.1:9000 for container egress.

Gates (exit 1 if any fails): blob digests; sealed image (no machine-id, no host keys, no Docker
raw-table workaround); cold boot -> SSH; systemd healthy; kernel features (moby check-config.sh
against /proc/config.gz, nft inet/fib/masq, iptables-nft raw, IPVS, VXLAN, FUSE, overlay, vsock,
balloon free-page reporting, VMGenID); Docker offline build + run on the default bridge, container
egress through MASQUERADE, published-port DNAT reached from the host over the tap, user-defined
network DNS; snapshot; restore -> SSH with the container still serving.
"""
import argparse, hashlib, http.client, json, os, platform, re, shutil, signal, socket
import subprocess, sys, threading, time

IMAGES = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
GUEST_IP = "172.16.0.2"
GUEST_MAC = "06:00:ac:10:00:02"
WEB_BODY = "davit-smoke-web"

NETNS_SH = """set -e
ip link set lo up
ip tuntap add tap0 mode tap
ip addr add 172.16.0.1/24 dev tap0
ip link set tap0 up
rm -f tcp22.sock tcp8080.sock
socat UNIX-LISTEN:tcp22.sock,fork TCP:172.16.0.2:22 &
socat UNIX-LISTEN:tcp8080.sock,fork TCP:172.16.0.2:8080 &
socat TCP-LISTEN:9000,bind=172.16.0.1,fork,reuseaddr SYSTEM:'sh egress.sh' &
exec "$@"
"""

# Fake "internet" endpoint for container egress: drain the HTTP request (closing with unread
# data would RST), then report the peer address the host saw (the guest IP if MASQUERADE works).
EGRESS_SH = r"""cr=$(printf '\r')
while IFS= read -r l; do [ -z "$l" ] || [ "$l" = "$cr" ] && break; done
printf 'HTTP/1.0 200 OK\r\n\r\negress-ok-from-%s' "$SOCAT_PEERADDR"
"""

DOCKERFILE = f"""FROM scratch
COPY busybox /bin/busybox
COPY index.html /www/index.html
CMD ["/bin/busybox", "echo", "hello from docker"]
"""


def now():
    return time.monotonic()


def ms(t):
    return round(t * 1000, 1)


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


class UHTTP(http.client.HTTPConnection):
    def __init__(self, path, timeout=30):
        super().__init__("localhost", timeout=timeout)
        self.upath = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.upath)


class Checks:
    def __init__(self):
        self.results = {}

    def record(self, name, ok, detail="", gate=True):
        self.results[name] = {"ok": bool(ok), "detail": detail, "gate": gate}
        mark = "PASS" if ok else ("FAIL" if gate else "info")
        print(f"  [{mark}] {name}: {str(detail)[:200]}", flush=True)
        return ok

    def run(self, name, fn, gate=True):
        try:
            ok, detail = fn()
        except Exception as e:  # a crashed check is a failed check
            ok, detail = False, f"{type(e).__name__}: {e}"
        return self.record(name, ok, detail, gate)

    def failed(self):
        return [k for k, v in self.results.items() if v["gate"] and not v["ok"]]


class VM:
    def __init__(self, ctx, name):
        self.ctx = ctx
        self.dir = f"{ctx.run}/{name}"
        os.makedirs(self.dir)
        # sun_path is 108 bytes: talk to sockets through a short symlink.
        self.short = f"{ctx.sockdir}/{name}"
        if os.path.lexists(self.short):
            os.remove(self.short)
        os.symlink(self.dir, self.short)
        self.proc = None

    def p(self, f):
        return f"{self.dir}/{f}"

    def sp(self, f):
        return f"{self.short}/{f}"

    def start(self, config=None):
        args = [self.ctx.fc, "--api-sock", "api.sock", "--level", "Warning"]
        if config:
            with open(self.p("vm.json"), "w") as f:
                json.dump(config, f, indent=1)
            args += ["--config-file", "vm.json"]
        with open(self.p("egress.sh"), "w") as f:
            f.write(EGRESS_SH)
        self.console = open(self.p("console.log"), "wb")
        self.proc = subprocess.Popen(
            ["unshare", "-rn", "sh", "-c", NETNS_SH, "netns"] + args,
            cwd=self.dir, stdin=subprocess.DEVNULL, stdout=self.console,
            stderr=subprocess.STDOUT, start_new_session=True)

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
        raise TimeoutError("firecracker api socket")

    def alive(self):
        if self.proc.poll() is not None:
            tail = open(self.p("console.log"), "rb").read()[-3000:].decode(errors="replace")
            raise RuntimeError(f"firecracker exited rc={self.proc.returncode}:\n{tail}")

    def banner(self, timeout=60):
        """Poll guest :22 over the tap until sshd's banner arrives; returns monotonic time."""
        end = now() + timeout
        while now() < end:
            self.alive()
            s = socket.socket(socket.AF_UNIX)
            s.settimeout(2)
            try:
                s.connect(self.sp("tcp22.sock"))
                buf = b""
                while len(buf) < 4:
                    c = s.recv(64)
                    if not c:
                        break
                    buf += c
                if buf.startswith(b"SSH-"):
                    return now()
            except OSError:
                pass
            finally:
                s.close()
            time.sleep(0.002)
        raise TimeoutError("ssh banner")

    def ssh(self, cmd, check=True, input=None, timeout=120):
        r = subprocess.run(
            ["ssh", "-q", "-i", self.ctx.key, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
             "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
             "-o", "ConnectTimeout=5", "-o", f"ProxyCommand=socat - UNIX-CONNECT:{self.sp('tcp22.sock')}",
             "davit@vm", cmd],
            capture_output=True, text=True, timeout=timeout, input=input)
        if check and r.returncode != 0:
            raise RuntimeError(f"ssh {cmd!r} rc={r.returncode}: {r.stderr.strip()} {r.stdout.strip()[-500:]}")
        return r.stdout.strip()

    def ssh_ready(self, timeout=60):
        end = now() + timeout
        while now() < end:
            self.alive()
            try:
                self.ssh("true", timeout=10)
                return now()
            except (RuntimeError, subprocess.TimeoutExpired):
                time.sleep(0.01)
        raise TimeoutError("ssh true")

    def http_8080(self, path="/"):
        c = UHTTP(self.sp("tcp8080.sock"), timeout=10)
        c.request("GET", path)
        return c.getresponse().read().decode().strip()

    def kill(self):
        """Kill the whole session (firecracker + the netns socat helpers), even if FC already exited."""
        if self.proc:
            try:
                os.killpg(self.proc.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            self.proc.wait()


class Ctx:
    pass


def debugfs(img, cmd):
    """One read-only debugfs request (-R does not echo the command back, unlike -f)."""
    r = subprocess.run(["debugfs", "-R", cmd, img], capture_output=True, text=True)
    return r.stdout + r.stderr.replace("debugfs 1.", "").strip()


def debugfs_exists(img, path):
    return "Inode:" in debugfs(img, f"stat {path}")


def probe(ctx, vm, vmid, harness=()):
    """Run the restore barrier with the product's host code (images/smokeprobe)."""
    args = [ctx.probe, "-uds", vm.sp("v.sock"), "-vm", vmid, "-generation", "1",
            "-secrets", f"{ctx.run}/secrets/{vmid}"]
    if harness:
        args += ["-harness", ",".join(harness)]
    r = subprocess.run(args, capture_output=True, text=True, timeout=180)
    try:
        out = json.loads(r.stdout)
    except ValueError:
        out = {"error": f"no JSON from smokeprobe: {r.stdout[-300:]!r}"}
    out["rc"], out["stderr"] = r.returncode, r.stderr[-500:]
    return out


class Background(threading.Thread):
    """Run fn in a thread and keep its result and finish time (time.monotonic)."""

    def __init__(self, fn):
        super().__init__(daemon=True)
        self.fn, self.result, self.error, self.done_at = fn, None, None, None
        self.start()

    def run(self):
        try:
            self.result = self.fn()
        except Exception as e:  # reported by the caller
            self.error = e
        self.done_at = now()

    def get(self, timeout=240):
        self.join(timeout)
        if self.is_alive():
            raise TimeoutError("background step")
        if self.error:
            raise self.error
        return self.result


def barrier_checks(ctx, chk, label, rep, vmid, previous=()):
    """Gate one barrier report: authenticated, rotated, root-capable, clock set, ready."""
    want_v = ctx.manifest["guest"]["agent"]["version"]
    idn, rd = rep.get("identity") or {}, rep.get("readiness") or {}
    chk.record(f"{label}: barrier completed", rep.get("rc") == 0 and not rep.get("error"),
               rep.get("error") or f"{rep.get('barrierMs')} ms")
    chk.record(f"{label}: agent version == manifest", rep.get("agentVersion") == want_v,
               f"{rep.get('agentVersion')} (protocol {rep.get('protocolVersion')})")
    proto = ctx.manifest["guest"]["agent"].get("protocol") or {}
    want_f = sorted(set(proto.get("features", [])) & set(rep.get("hostFeatures") or []))
    chk.record(f"{label}: handshake matches manifest protocol (version in range, negotiated features)",
               proto.get("min", 0) <= (rep.get("protocolVersion") or -1) <= proto.get("max", 0)
               and sorted(rep.get("features") or []) == want_f,
               f"v{rep.get('protocolVersion')} in {proto.get('min')}-{proto.get('max')}; {rep.get('features')}")
    chk.record(f"{label}: identity assigned", idn.get("vmId") == vmid and idn.get("hostname") == vmid
               and len(idn.get("machineId", "")) == 32 and bool(idn.get("sshHostKeyFingerprints")),
               {k: idn.get(k) for k in ("vmId", "hostname", "machineId")})
    for prev in previous:
        chk.record(f"{label}: machine-id and host key rotated vs {prev['vmId']}",
                   idn.get("machineId") != prev["machineId"] and
                   not set(idn.get("sshHostKeyFingerprints") or []) & set(prev["sshHostKeyFingerprints"]),
                   f"{prev['machineId'][:8]} -> {idn.get('machineId', '')[:8]}")
    chk.record(f"{label}: readiness (agent + sshd)", rd.get("agent") and rd.get("ssh"), rd)
    chk.record(f"{label}: exec as root via agent", rep.get("execRootUid") == "0", rep.get("execRootUid"))
    chk.record(f"{label}: exec default user is davit", rep.get("execDefaultUser") == "davit",
               rep.get("execDefaultUser"))
    skew = rep.get("clockSkewMs")
    chk.record(f"{label}: guest clock set by barrier (|skew| < 2 s)", skew is not None and abs(skew) < 2000,
               f"{skew} ms")


def static_checks(ctx, chk):
    img = ctx.rootfs
    out = debugfs(img, "stat /etc/machine-id")
    chk.record("image: /etc/machine-id empty", "Inode:" in out and "Size: 0" in out, out.splitlines()[:2])
    out = debugfs(img, "ls -l /etc/ssh")
    keys = [w for w in out.split() if w.startswith("ssh_host_")]
    chk.record("image: no SSH host keys", "sshd_config" in out and not keys, keys or "none")
    drop = debugfs_exists(img, "/etc/systemd/system/docker.service.d")
    chk.record("image: no docker.service drop-ins (no raw-table workaround)", not drop,
               "present" if drop else "absent")
    bak = debugfs_exists(img, "/etc/.resolv.conf.systemd-resolved.bak")
    chk.record("image: no build-host resolv.conf backup", not bak, "present" if bak else "absent")
    out = debugfs(img, "stat /etc/resolv.conf")
    chk.record("image: resolv.conf is the resolved stub symlink", "Type: symlink" in out and
               "stub-resolv.conf" in out, out.splitlines()[:1])
    unit = debugfs(img, "cat /etc/systemd/system/davit-guest-agent.service")
    chk.record("image: guest agent unit runs davit-guest-agent (argv[0] dispatch, agent flags only)",
               "ExecStart=/usr/local/bin/davit-guest-agent -default-user davit" in unit, "")
    agent = f"{ctx.run}/agent-from-image"
    subprocess.run(["debugfs", "-R", f"dump /usr/local/bin/davit-guest-agent {agent}", img],
                   capture_output=True)
    want = ctx.manifest["guest"]["agent"]["binary"]["digest"].split(":", 1)[1]
    got = sha256_file(agent) if os.path.exists(agent) else "missing"
    chk.record("image: baked agent binary == manifest digest", got == want, got[:16])


def inject_ssh_key(img, pubkey_path):
    cmds = ("cd /home/davit/.ssh\n"
            f"write {pubkey_path} authorized_keys\n"
            "sif authorized_keys uid 1000\nsif authorized_keys gid 1000\n"
            "sif authorized_keys mode 0100600\n")
    r = subprocess.run(["debugfs", "-w", "-f", "-", img], input=cmds, capture_output=True, text=True)
    out = r.stdout + r.stderr
    if not debugfs_exists(img, "/home/davit/.ssh/authorized_keys"):
        raise RuntimeError(f"debugfs key injection failed: {out}")


def vm_config(ctx):
    return {
        "boot-source": {"kernel_image_path": ctx.kernel, "boot_args": ctx.manifest["kernel"]["bootArgs"]},
        "drives": [{"drive_id": "rootfs", "path_on_host": "rootfs.ext4",
                    "is_root_device": True, "is_read_only": False}],
        "machine-config": {"vcpu_count": ctx.args.vcpus, "mem_size_mib": ctx.args.mem, "smt": False},
        "network-interfaces": [{"iface_id": "eth0", "guest_mac": GUEST_MAC, "host_dev_name": "tap0"}],
        "vsock": {"guest_cid": 3, "uds_path": "v.sock"},
        "balloon": {"amount_mib": 0, "deflate_on_oom": True, "free_page_reporting": True},
    }


def vsock_guest_to_host(vm, port=5000):
    """Guest connects to host CID 2:<port>; Firecracker forwards to <uds>_<port> on the host."""
    path = vm.sp(f"v.sock_{port}")
    if os.path.exists(path):
        os.remove(path)
    srv = socket.socket(socket.AF_UNIX)
    srv.bind(path)
    srv.listen(1)
    srv.settimeout(15)
    got = {}

    def accept():
        try:
            c, _ = srv.accept()
            c.settimeout(10)
            got["data"] = c.recv(64).decode().strip()
            c.close()
        except OSError as e:
            got["err"] = str(e)

    t = threading.Thread(target=accept)
    t.start()
    vm.ssh(f"echo vsock-ok | socat - VSOCK-CONNECT:2:{port}")
    t.join()
    srv.close()
    return got.get("data") == "vsock-ok", got


def guest_checks(ctx, vm, chk):
    m = ctx.manifest
    g = {}
    chk.run("systemd: is-system-running", lambda: (
        lambda s: (s == "running", s if s == "running" else
                   s + " failed=" + vm.ssh("systemctl --failed --no-legend --plain | cut -d' ' -f1 | xargs", check=False)))(
        vm.ssh("systemctl is-system-running --wait", check=False)))
    g["uname"] = vm.ssh("uname -r")
    chk.record("kernel: release matches manifest", g["uname"] == m["kernel"]["release"], g["uname"])
    g["os"] = vm.ssh(". /etc/os-release; echo $PRETTY_NAME")
    g["systemd_analyze"] = vm.ssh("systemd-analyze 2>&1 | head -1", check=False)
    g["critical_chain_ssh"] = vm.ssh("systemd-analyze critical-chain ssh.service 2>&1 | tail -n +3", check=False)
    chk.run("agent: davit-guest-agent.service active", lambda: (
        lambda o: (o == "active", o))(vm.ssh("systemctl is-active davit-guest-agent", check=False)))
    chk.run("agent: identity stamp on disk", lambda: (
        lambda o: (ctx.src_id in o, o[:120]))(vm.ssh("sudo cat /var/lib/davit/identity.json", check=False)))
    chk.run("agent: kernel hostname set by barrier", lambda: (
        lambda o: (o == ctx.src_id, o))(vm.ssh("hostname")))
    mid = vm.ssh("cat /etc/machine-id")
    chk.record("identity: machine-id generated on boot", len(mid) == 32, mid)
    chk.run("identity: SSH host keys generated on boot", lambda: (
        lambda o: ("ssh_host_ed25519_key.pub" in o, o))(
        vm.ssh("ls /etc/ssh | grep ssh_host_ | xargs")))

    # Kernel features, from the running kernel.
    # check-config.sh exits non-zero when anything (even optional) is missing; parse instead,
    # and refuse to pass on output that does not look like a complete report.
    cc = vm.ssh("bash -s; echo rc=$?", input=open(ctx.check_config).read(), timeout=60)
    cc = re.sub(r"\x1b\[[0-9;]*m", "", cc)
    complete = "Generally Necessary:" in cc and "Optional Features:" in cc and "CONFIG_NAMESPACES" in cc
    necessary = cc.split("Generally Necessary:")[-1].split("Optional Features:")[0]
    missing = [l.strip() for l in necessary.splitlines() if "missing" in l]
    chk.record("kernel: docker check-config.sh generally-necessary all enabled", complete and not missing,
               missing or ("all enabled" if complete else "incomplete check-config output"))
    opt_missing = [l.strip("- ").split(":")[0] for l in cc.split("Optional Features:")[-1].splitlines()
                   if ": missing" in l]
    chk.record("kernel: docker check-config.sh optional missing (informational)", True,
               opt_missing, gate=False)
    g["docker_check_config_optional_missing"] = opt_missing
    chk.run("kernel: iptables uses nf_tables backend", lambda: (
        lambda o: ("nf_tables" in o, o))(vm.ssh("sudo iptables -V")))
    chk.run("kernel: iptables raw table (IP_NF_RAW) usable", lambda: (
        lambda o: ("-P PREROUTING ACCEPT" in o, o.splitlines()[0] if o else o))(vm.ssh("sudo iptables -t raw -S")))
    chk.run("kernel: ip6tables raw table usable", lambda: (
        lambda o: ("-P PREROUTING ACCEPT" in o, o.splitlines()[0] if o else o))(vm.ssh("sudo ip6tables -t raw -S")))
    nft = ("table inet davit_smoke {\n set s { type ipv4_addr; }\n"
           " chain f { type filter hook input priority 0; ip saddr @s counter; fib daddr type local accept; ct state established accept; }\n"
           " chain n { type nat hook postrouting priority 100; oifname \"eth0\" masquerade; }\n}\n")
    chk.run("kernel: nftables inet table with set/fib/ct/masquerade", lambda: (
        vm.ssh("sudo nft -f - && sudo nft list table inet davit_smoke >/dev/null && sudo nft delete table inet davit_smoke && echo ok",
               input=nft) == "ok", "inet set+fib+ct+masq"))
    chk.run("kernel: VXLAN", lambda: (vm.ssh(
        f"sudo ip link add vx0 type vxlan id 42 dstport 4789 local {GUEST_IP} dev eth0 && sudo ip link del vx0 && echo ok") == "ok", "vxlan link add/del"))
    chk.run("kernel: IPVS", lambda: (vm.ssh("test -e /proc/net/ip_vs && echo ok") == "ok", "/proc/net/ip_vs"))
    chk.run("kernel: FUSE", lambda: (
        lambda o: (o == "ok", o))(vm.ssh("test -c /dev/fuse && grep -qw fuse /proc/filesystems && echo ok", check=False)))
    chk.run("kernel: overlayfs", lambda: (vm.ssh("grep -qw overlay /proc/filesystems && echo ok") == "ok", "overlay in /proc/filesystems"))
    chk.run("kernel: vsock guest->host", lambda: vsock_guest_to_host(vm))
    chk.run("kernel: virtio-balloon bound with free page reporting", lambda: (
        lambda o: (o.endswith(" 1"), o))(vm.ssh(
            "d=$(ls -d /sys/bus/virtio/drivers/virtio_balloon/virtio* | head -1); "
            "echo $(basename $d) $(cut -c6 $d/features)")))
    chk.run("kernel: VMGenID device bound", lambda: (
        lambda o: (o.splitlines()[0] != "bound:", o.replace("\n", " | ")))(vm.ssh(
            "echo bound:$(ls /sys/bus/*/drivers/vmgenid/ 2>/dev/null | grep -v -E '^(bind|unbind|uevent|module|new_id|remove_id)$' | xargs);"
            "echo acpi:$(ls /sys/bus/acpi/devices 2>/dev/null | xargs); echo dmesg:$(sudo dmesg | grep -i -E 'vmgen|genid' | xargs)", check=False)))

    # Docker, fully offline.
    vm.ssh("until docker info >/dev/null 2>&1; do sleep 0.05; done", timeout=60)
    g["docker_version"] = vm.ssh("docker version --format '{{.Server.Version}}'")
    env = vm.ssh("systemctl show docker -p Environment")
    chk.record("docker: no DOCKER_INSECURE_NO_IPTABLES_RAW", "NO_IPTABLES_RAW" not in env, env or "Environment=")
    info = vm.ssh("docker info --format '{{.Driver}} {{.CgroupVersion}} {{json .Warnings}}'")
    g["docker_info"] = info
    chk.record("docker: info has no warnings", info.endswith("null") or info.endswith("[]"), info)
    vm.ssh(f"mkdir -p ~/ctx && cp /usr/bin/busybox ~/ctx/ && echo {WEB_BODY} > ~/ctx/index.html && "
           f"cat > ~/ctx/Dockerfile", input=DOCKERFILE)
    t0 = now()
    chk.run("docker: offline build (BuildKit, FROM scratch)", lambda: (
        lambda o: (o.startswith("sha256:"), o))(vm.ssh("docker build -q -t davit-smoke/hello ~/ctx")))
    g["docker_build_ms"] = ms(now() - t0)
    t0 = now()
    chk.run("docker: run on default bridge (hello-world equivalent)", lambda: (
        lambda o: (o == "hello from docker", o))(vm.ssh("docker run --rm davit-smoke/hello")))
    g["docker_run_ms"] = ms(now() - t0)
    chk.run("docker: container egress via MASQUERADE", lambda: (
        lambda o: (o == f"egress-ok-from-{GUEST_IP}", o))(vm.ssh(
            "docker run --rm davit-smoke/hello /bin/busybox wget -qO- http://172.16.0.1:9000/")))
    vm.ssh("docker run -d --name web -p 8080:80 davit-smoke/hello /bin/busybox httpd -f -p 80 -h /www >/dev/null")
    chk.run("docker: published port DNAT reached from host over tap", lambda: (
        lambda o: (o == WEB_BODY, o))(retry(lambda: vm.http_8080())))
    chk.run("docker: raw-table direct-access protection rules present", lambda: (
        lambda o: ("-j DROP" in o, o.replace("\n", " | ")[:300]))(vm.ssh("sudo iptables -t raw -S PREROUTING")))
    chk.run("docker: user-defined network + embedded DNS", lambda: (
        lambda o: (o == WEB_BODY, o))(vm.ssh(
            "docker network create smoke-net >/dev/null && "
            "docker run -d --name srv --network smoke-net davit-smoke/hello /bin/busybox httpd -f -p 80 -h /www >/dev/null && "
            "docker run --rm --network smoke-net davit-smoke/hello /bin/busybox wget -qO- http://srv/")))
    errs = vm.ssh("sudo journalctl -b -u docker -u containerd --no-pager -p err -o cat | grep -v -i 'failed to read /var/lib/docker' | tail -5", check=False)
    chk.record("docker: no daemon errors in journal", not errs, errs or "none")
    vm.ssh("docker rm -f srv >/dev/null; docker network rm smoke-net >/dev/null")
    return g


def nft_backend_check(vm, chk):
    """Docker's native nftables firewall backend (informational: Docker-side feature status)."""
    def run():
        vm.ssh("echo '{\"firewall-backend\": \"nftables\"}' | sudo tee /etc/docker/daemon.json >/dev/null && "
               "sudo systemctl restart docker && until docker info >/dev/null 2>&1; do sleep 0.05; done", timeout=90)
        try:
            out = vm.ssh("docker run --rm davit-smoke/hello && sudo nft list tables | xargs", check=False)
            ok = out.startswith("hello from docker") and "docker" in out
            return ok, out.replace("\n", " ")
        finally:
            vm.ssh("sudo rm -f /etc/docker/daemon.json && sudo systemctl restart docker && "
                   "until docker info >/dev/null 2>&1; do sleep 0.05; done", timeout=90)
            vm.ssh("docker start web >/dev/null")
    chk.run("docker: native nftables firewall backend", run, gate=False)


def retry(fn, tries=50, delay=0.1):
    for i in range(tries):
        try:
            return fn()
        except OSError:
            if i == tries - 1:
                raise
            time.sleep(delay)


def reflink(src, dst):
    subprocess.run(["cp", "--reflink=auto", "--sparse=always", src, dst], check=True)
    os.chmod(dst, 0o644)  # blobs are stored read-only


def host_facts(ctx):
    cpu = next((l.split(":", 1)[1].strip() for l in open("/proc/cpuinfo") if l.startswith("model name")), "?")
    mem = next(int(l.split()[1]) // 1024 for l in open("/proc/meminfo") if l.startswith("MemTotal"))
    fs = subprocess.run(["stat", "-f", "-c", "%T", ctx.run], capture_output=True, text=True).stdout.strip()
    fcv = subprocess.run([ctx.fc, "--version"], capture_output=True, text=True).stdout.splitlines()[0]
    return {"cpu": cpu, "cpus": os.cpu_count(), "mem_mib": mem, "kernel": platform.release(),
            "run_fs": fs, "firecracker": fcv,
            "thp": open("/sys/kernel/mm/transparent_hugepage/enabled").read().strip(),
            "systemd_unit": bool(os.environ.get("INVOCATION_ID")) or "unknown"}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--manifest", default=f"{IMAGES}/manifests/ubuntu-24.04-base.json")
    ap.add_argument("--firecracker", required=True)
    ap.add_argument("--check-config", required=True, help="moby contrib/check-config.sh (pinned)")
    ap.add_argument("--probe", required=True, help="images/smokeprobe binary")
    ap.add_argument("--blobs", default=os.environ.get("DAVIT_IMAGES_OUT", f"{IMAGES}/out") + "/blobs/sha256")
    work = os.environ.get("DAVIT_IMAGES_WORK") or os.path.join(
        os.environ.get("XDG_CACHE_HOME") or os.path.expanduser("~/.cache"), "davit/images")
    ap.add_argument("--work", default=f"{work}/smoke")
    ap.add_argument("--mem", type=int, default=1024)
    ap.add_argument("--vcpus", type=int, default=2)
    ap.add_argument("--restores", type=int, default=3)
    ap.add_argument("--keep", action="store_true", help="keep VM dirs/snapshot for debugging")
    ap.add_argument("--out", default=None, help="results JSON (default images/results/smoke-<ts>.json)")
    a = ap.parse_args()
    assert a.mem <= 4096, "guest RAM cap is 4 GiB"

    ctx = Ctx()
    ctx.args = a
    ctx.fc = os.path.abspath(a.firecracker)
    ctx.check_config = a.check_config
    ctx.probe = os.path.abspath(a.probe)
    ctx.src_id = "smoke-src"
    ctx.manifest = json.load(open(a.manifest))
    # Harness CLIs answer `--version`; other capabilities (toolchain.*) are proven at build time
    # by their layer.
    ctx.harness = [c["command"] for n, c in ctx.manifest.get("capabilities", {}).items() if n.startswith("harness.")]
    stamp = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    ctx.run = f"{a.work}/{stamp}"
    ctx.sockdir = os.path.expanduser(f"~/.cache/davit-smoke/{stamp}")
    os.makedirs(ctx.run)
    os.makedirs(ctx.sockdir)
    chk = Checks()
    res = {"manifest": {"path": os.path.relpath(a.manifest, os.path.dirname(IMAGES)), "id": ctx.manifest["id"]},
           "params": {"mem_mib": a.mem, "vcpus": a.vcpus, "restores": a.restores},
           "host": host_facts(ctx), "loadavg_start": open("/proc/loadavg").read().split()[:3]}
    live = []
    try:
        print("verify blobs", flush=True)
        for part in ("kernel", "rootfs"):
            d = ctx.manifest[part]["digest"].split(":", 1)[1]
            path = f"{a.blobs}/{d}"
            chk.record(f"blob: {part} digest", os.path.exists(path) and sha256_file(path) == d, d[:16])
        if chk.failed():
            raise SystemExit("blob verification failed")
        ctx.kernel = f"{a.blobs}/{ctx.manifest['kernel']['digest'].split(':', 1)[1]}"
        ctx.rootfs = f"{a.blobs}/{ctx.manifest['rootfs']['digest'].split(':', 1)[1]}"
        static_checks(ctx, chk)

        ctx.key = f"{ctx.run}/id_ed25519"
        subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", ctx.key], check=True)

        print("cold boot", flush=True)
        vm = VM(ctx, "boot")
        live.append(vm)
        reflink(ctx.rootfs, vm.p("rootfs.ext4"))
        inject_ssh_key(vm.p("rootfs.ext4"), ctx.key + ".pub")
        t0 = now()
        vm.start(vm_config(ctx))
        # The barrier starts with the VM, as the product's readiness probe does; SSH is timed
        # from the same t0 (sshd restarts once inside the barrier when host keys rotate).
        bar = Background(lambda: probe(ctx, vm, ctx.src_id, ctx.harness))
        cold = {"banner": ms(vm.banner() - t0), "ssh_true": ms(vm.ssh_ready() - t0)}
        src_rep = bar.get()
        cold["barrier_ready"] = ms(bar.done_at - t0)
        cold["barrier_only"] = src_rep.get("barrierMs")
        res["cold_boot"] = cold
        res["barrier_cold"] = src_rep
        chk.record("cold boot -> ssh / barrier ready", True, cold)
        barrier_checks(ctx, chk, "cold boot", src_rep, ctx.src_id)
        for cmd, cap in ctx.manifest.get("capabilities", {}).items():
            if not cmd.startswith("harness."):
                continue
            got = (src_rep.get("harness") or {}).get(cap["command"], "")
            chk.record(f"capability {cmd}: `{cap['command']} --version` via agent login shell == {cap['version']}",
                       cap["version"] in got.split(), got)

        res["guest"] = guest_checks(ctx, vm, chk)
        nft_backend_check(vm, chk)
        chk.run("docker: web container serving before snapshot", lambda: (
            lambda o: (o == WEB_BODY, o))(retry(lambda: vm.http_8080())))
        vm.ssh("echo from-source > ~/marker; sync")
        src_boot_id = vm.ssh("cat /proc/sys/kernel/random/boot_id")

        print("snapshot", flush=True)
        snap = f"{ctx.run}/snap"
        os.makedirs(snap)
        t0 = now()
        vm.api("PATCH", "/vm", {"state": "Paused"})
        vm.api("PUT", "/snapshot/create", {"snapshot_type": "Full", "snapshot_path": f"{snap}/vmstate",
                                           "mem_file_path": f"{snap}/mem"})
        reflink(vm.p("rootfs.ext4"), f"{snap}/rootfs.ext4")
        res["snapshot"] = {"create_ms": ms(now() - t0),
                           "mem_mib": round(os.path.getsize(f"{snap}/mem") / 2**20)}
        chk.record("snapshot create", True, res["snapshot"])
        vm.kill()
        live.remove(vm)

        res["restore"] = []
        snap_at = now()
        ids = [src_rep.get("identity") or {}]
        for i in range(a.restores):
            if i == 0:
                # Let the snapshot age so the barrier's clock step is measurable, not noise.
                time.sleep(max(0.0, 10 - (now() - snap_at)))
            print(f"restore {i}", flush=True)
            r = VM(ctx, f"restore{i}")
            live.append(r)
            t0 = now()
            reflink(f"{snap}/rootfs.ext4", r.p("rootfs.ext4"))
            r.start()
            r.wait_api()
            t_api = now()
            r.api("PUT", "/snapshot/load", {"snapshot_path": f"{snap}/vmstate",
                                            "mem_backend": {"backend_type": "File", "backend_path": f"{snap}/mem"},
                                            "resume_vm": True})
            t_res = now()
            fork_id = f"smoke-fork-{i}"
            bar = Background(lambda: probe(ctx, r, fork_id))
            t = {"snapshot_age_s": round(t0 - snap_at, 1), "fc_api_up": ms(t_api - t0), "resumed": ms(t_res - t0),
                 "banner": ms(r.banner() - t0), "ssh_true": ms(r.ssh_ready() - t0)}
            frep = bar.get()
            t["barrier_ready"] = ms(bar.done_at - t0)
            t["barrier_only"] = frep.get("barrierMs")
            t["clock_skew_after_barrier_ms"] = frep.get("clockSkewMs")
            res["restore"].append(t)
            chk.record(f"restore {i} -> ssh / barrier ready", True, t)
            barrier_checks(ctx, chk, f"fork {i}", frep, fork_id, [x for x in ids if x])
            ids.append(frep.get("identity") or {})
            if i == 0:
                chk.run("restore: disk state carried over", lambda: (
                    lambda o: (o == "from-source", o))(r.ssh("cat ~/marker")))
                chk.run("restore: web container still serving (DNAT, from host)", lambda: (
                    lambda o: (o == WEB_BODY, o))(retry(lambda: r.http_8080())))
                chk.run("restore: docker run works", lambda: (
                    lambda o: (o == "hello from docker", o))(r.ssh("docker run --rm davit-smoke/hello")))
                same = r.ssh("cat /proc/sys/kernel/random/boot_id") == src_boot_id
                chk.record("restore: boot_id shared with source (not rotatable; SPEC uses generation)", True,
                           f"boot_id same={same}", gate=False)
            if i == a.restores - 1:
                # Stop without the agent: Firecracker injects Ctrl+Alt+Del (i8042), systemd runs
                # ctrl-alt-del.target, the guest shuts down cleanly and reboots, Firecracker exits.
                # The test containers' busybox httpd ignores SIGTERM as PID 1, so docker would wait
                # out its 10 s stop timeout; remove them so the number measures the image.
                r.ssh("docker rm -f $(docker ps -aq) >/dev/null 2>&1; echo cad-flushed > ~/cad-marker")  # no sync
                t_cad = now()
                r.api("PUT", "/actions", {"action_type": "SendCtrlAltDel"})
                try:
                    rc = r.proc.wait(timeout=60)
                    res["ctrl_alt_del"] = {"vmm_exit_ms": ms(now() - t_cad), "exit_code": rc}
                except subprocess.TimeoutExpired:
                    res["ctrl_alt_del"] = {"vmm_exit_ms": None, "exit_code": None}
                chk.record("stop: SendCtrlAltDel -> clean guest shutdown, Firecracker exits 0",
                           res["ctrl_alt_del"]["exit_code"] == 0, res["ctrl_alt_del"])
                marker = debugfs(r.p("rootfs.ext4"), "cat /home/davit/cad-marker")
                chk.record("stop: unsynced write flushed to disk by the shutdown", "cad-flushed" in marker,
                           marker.strip()[:60])
            r.kill()
            live.remove(r)
    finally:
        for v in live:
            v.kill()
        res["loadavg_end"] = open("/proc/loadavg").read().split()[:3]
        res["checks"] = chk.results
        res["failed"] = chk.failed()
        out = a.out or f"{IMAGES}/results/smoke-{stamp}.json"
        os.makedirs(os.path.dirname(out), exist_ok=True)
        with open(out, "w") as f:
            json.dump(res, f, indent=1)
            f.write("\n")
        shutil.rmtree(ctx.sockdir, ignore_errors=True)
        if not a.keep:
            shutil.rmtree(ctx.run, ignore_errors=True)
        print(f"results: {out}")
    if res["failed"]:
        print("FAILED:", ", ".join(res["failed"]))
        sys.exit(1)
    print(f"PASS  cold boot->barrier ready {res['cold_boot']['barrier_ready']} ms (ssh {res['cold_boot']['ssh_true']}); "
          f"restore->barrier ready {[t['barrier_ready'] for t in res['restore']]} ms; "
          f"ctrl-alt-del stop {res.get('ctrl_alt_del', {}).get('vmm_exit_ms')} ms")


if __name__ == "__main__":
    main()
