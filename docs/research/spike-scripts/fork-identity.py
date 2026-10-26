#!/usr/bin/env python3
"""Restore 2 forks from run/snap and report clock skew + shared identity."""
import os, sys, time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import fcspike as F  # noqa: E402

snap = f"{F.RUN}/snap"
vms = []
try:
    for i in range(2):
        vm, _ = F.restore(f"id{i}", snap)
        vms.append(vm)
    for i, vm in enumerate(vms):
        g = float(vm.ssh("date +%s.%N"))
        print(f"fork{i}: guest-host clock skew {g - time.time():+.1f}s;",
              "machine-id", vm.ssh("cat /etc/machine-id")[:12],
              "hostkey", vm.ssh("ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub | cut -d' ' -f2")[:20],
              "boot_id", vm.ssh("cat /proc/sys/kernel/random/boot_id")[:8],
              "vmgenid", vm.ssh("ls /sys/bus/acpi/devices/ 2>/dev/null | grep -ci vmgen || true"),
              "uuid", vm.ssh("cat /proc/sys/kernel/random/uuid")[:8])
finally:
    for vm in vms:
        vm.kill()
