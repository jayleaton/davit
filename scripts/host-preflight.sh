#!/usr/bin/env bash
# host-preflight.sh: can this machine be a davit VM host? Read-only: it
# installs nothing, uses no sudo and writes nothing on the host. See
# docs/vm-host.md.
#
#   scripts/host-preflight.sh [--dir DIR] [--fs DIR] HOST   probe HOST over SSH, print the verdict
#   scripts/host-preflight.sh --local                       probe this machine
#   scripts/host-preflight.sh --facts FILE                  verdict for recorded probe output (dry run)
#   scripts/host-preflight.sh --probe-only [HOST|--local]   print the raw probe facts (key=value)
#
# HOST is any SSH destination with key auth (BatchMode, no prompts). --dir
# and --fs are the host paths scripts/vm-tier.sh would use (default ~/.svm and
# DIR/fs); the reflink and disk checks look at --fs, or its nearest existing
# parent when it does not exist yet. --min-disk GIB is the free space the disk check
# requires there (default 20, what the real-VM tier needs; host-install asks for less).
#
# Prints a CHECK / STATUS / DETAIL table and, for every FAIL and WARN, the exact
# fix. Exit status: 0 no FAIL, 1 at least one FAIL, 2 usage, 255 SSH.
set -euo pipefail

# probe: print facts about this machine as key=value lines. It runs on the
# host (sent over SSH), so it is self-contained, never fails and changes
# nothing: no files, no modules, no sysctls. `unshare ... true` creates
# namespaces that vanish with the process.
probe() {
	set +e
	local svm_dir=${SVM_DIR:-$HOME/.svm} fs p t
	fs=${SVM_FS:-$svm_dir/fs}
	fact() { printf '%s=%s\n' "$1" "$(printf '%s' "$2" | tr '\n' ' ' | sed 's/ *$//')"; }
	fact probe_version 1
	fact user "$(id -un)"
	fact groups "$(id -nG)"
	fact hostname "$(hostname 2>/dev/null || cat /proc/sys/kernel/hostname)"
	# shellcheck disable=SC1091 # the host's file
	fact distro "$(. /etc/os-release 2>/dev/null && echo "$PRETTY_NAME")"
	fact arch "$(uname -m)"
	fact kernel "$(uname -r)"
	fact cpus "$(nproc 2>/dev/null || grep -c ^processor /proc/cpuinfo)"
	if grep -qw vmx /proc/cpuinfo 2>/dev/null; then fact cpu_virt vmx
	elif grep -qw svm /proc/cpuinfo 2>/dev/null; then fact cpu_virt svm
	else fact cpu_virt none; fi
	fact mem_total_kib "$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)"
	fact mem_avail_kib "$(awk '/^MemAvailable:/ {print $2}' /proc/meminfo)"
	for p in kvm:/dev/kvm tun:/dev/net/tun; do
		if [ ! -e "${p#*:}" ]; then fact "${p%%:*}" missing
		elif [ -r "${p#*:}" ] && [ -w "${p#*:}" ]; then fact "${p%%:*}" rw
		else fact "${p%%:*}" noaccess; fi
	done
	fact kvm_group "$(stat -c %G /dev/kvm 2>/dev/null)"
	fact kvm_module "$(for m in kvm_intel kvm_amd kvm; do [ -d /sys/module/$m ] && echo $m; done | head -n1)"
	fact cgroup_fs "$(stat -fc %T /sys/fs/cgroup 2>/dev/null)"
	fact cgroup_controllers "$(cat /sys/fs/cgroup/cgroup.controllers 2>/dev/null)"
	if t=$(unshare --user --map-root-user --net true 2>&1); then fact userns ok; else fact userns fail; fact userns_error "$t"; fi
	fact sysctl_apparmor_restrict_userns "$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null)"
	fact sysctl_unprivileged_userns_clone "$(cat /proc/sys/kernel/unprivileged_userns_clone 2>/dev/null)"
	fact sysctl_max_user_namespaces "$(cat /proc/sys/user/max_user_namespaces 2>/dev/null)"
	fact subuid "$(awk -F: -v u="$(id -un)" -v i="$(id -u)" '$1 == u || $1 == i {n += $3} END {print n + 0}' /etc/subuid 2>/dev/null)"
	fact subgid "$(awk -F: -v u="$(id -un)" -v i="$(id -u)" '$1 == u || $1 == i {n += $3} END {print n + 0}' /etc/subgid 2>/dev/null)"
	fact newuidmap "$(command -v newuidmap)"
	# The first id after every range already allocated (to anyone), at least 100000: a range
	# that starts there overlaps no other user's.
	fact subid_free "$(cat /etc/subuid /etc/subgid 2>/dev/null | awk -F: 'BEGIN {m = 100000} $2 + $3 > m {m = $2 + $3} END {print m}')"
	fact vhost_vsock "$([ -e /dev/vhost-vsock ] && echo present || echo absent)"
	fact vsock_modules "$(for m in vhost_vsock vsock vmw_vsock_virtio_transport_common; do [ -d /sys/module/$m ] && echo $m; done)"
	# The data dir may not exist yet: judge its nearest existing parent.
	# shellcheck disable=SC2088 # a literal ~/ from --fs
	case "$fs" in "~/"*) fs=$HOME/${fs#\~/} ;; esac
	p=$fs
	while [ ! -e "$p" ] && [ "$p" != / ]; do p=$(dirname "$p"); done
	fact fs_path "$fs"
	fact fs_checked "$p"
	fact fs_type "$(stat -fc %T "$p" 2>/dev/null)"
	fact fs_mount "$(df -P "$p" 2>/dev/null | awk 'NR == 2 {print $6}')"
	fact fs_free_kib "$(df -Pk "$p" 2>/dev/null | awk 'NR == 2 {print $4}')"
	case "$(stat -fc %T "$p" 2>/dev/null)" in
	btrfs | bcachefs) fact fs_reflink yes ;;
	xfs)
		if ! command -v xfs_info >/dev/null; then fact fs_reflink unknown
		elif xfs_info "$(df -P "$p" | awk 'NR == 2 {print $6}')" 2>/dev/null | grep -q 'reflink=1'; then fact fs_reflink yes
		else fact fs_reflink no; fi ;;
	zfs) fact fs_reflink unknown ;;
	*) fact fs_reflink no ;;
	esac
	fact firecracker "$(command -v firecracker)"
	fact firecracker_version "$(firecracker --version 2>/dev/null | head -n1)"
	fact jailer "$(command -v jailer)"
	fact jailer_version "$(jailer --version 2>/dev/null | head -n1)"
	fact fixture_firecracker "$(find "$fs/scratch/bin" -maxdepth 1 -name 'firecracker-*' 2>/dev/null | head -n1)"
	fact nft "$(command -v nft)"
	fact mise "$(command -v mise || { [ -x "$svm_dir/bin/mise" ] && echo "$svm_dir/bin/mise"; })"
	fact missing_tools "$(for t in git tar flock nsenter unshare ip debugfs sha256sum curl rsync python3 xz gpg make; do command -v $t >/dev/null || echo $t; done)"
	fact svm_dir "$svm_dir"
	fact built_manifest "$([ -f "$fs/manifests/ubuntu-24.04-base.json" ] && echo "$fs/manifests/ubuntu-24.04-base.json")"
	return 0
}

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
usage() { sed -n '2,19p' "$0" >&2; exit 2; }

# pinned_firecracker: the Firecracker release the repo pins.
pinned_firecracker() { sed -n 's/^FIRECRACKER_VERSION=//p' "$ROOT/images/inputs.lock" 2>/dev/null; }

# version_ge A B: dotted version A >= B (numeric prefix of each part).
version_ge() {
	local IFS=. i a b
	read -ra a <<<"${1%%[!0-9.]*}"
	read -ra b <<<"$2"
	for ((i = 0; i < ${#b[@]}; i++)); do
		local x=${a[i]:-0} y=${b[i]:-0}
		((10#$x > 10#$y)) && return 0
		((10#$x < 10#$y)) && return 1
	done
	return 0
}

# Package (Debian/Ubuntu) that provides a missing tool.
pkg_for() {
	case "$1" in
	flock | nsenter | unshare) echo util-linux ;;
	ip) echo iproute2 ;;
	debugfs) echo e2fsprogs ;;
	sha256sum) echo coreutils ;;
	xz) echo xz-utils ;;
	python3) echo python3 ;;
	gpg) echo gnupg ;;
	*) echo "$1" ;;
	esac
}

# evaluate FACTS_FILE: the table and the fixes; returns 1 on any FAIL.
evaluate() {
	local -A F=()
	local k v
	while IFS= read -r line; do
		[[ $line == *=* ]] || continue
		k=${line%%=*} v=${line#*=}
		F[$k]=$v
	done <"$1"
	# Numeric facts: anything else counts as 0.
	for k in subuid subgid subid_free fs_free_kib mem_total_kib; do
		[[ ${F[$k]:-} =~ ^[0-9]+$ ]] || F[$k]=0
	done
	if [ -z "${F[probe_version]:-}" ]; then
		echo "host-preflight: no probe facts in $1" >&2
		return 2
	fi
	local user=${F[user]:-USER} fails=0 warns=0 rows=() fixes=()
	row() { # STATUS CHECK DETAIL [FIX]
		rows+=("$(printf '%-12s %-6s %s' "$2" "$1" "$3")")
		case "$1" in
		FAIL) fails=$((fails + 1)) ;;
		WARN) warns=$((warns + 1)) ;;
		esac
		if [ -n "${4:-}" ]; then fixes+=("$1 $2: $4"); fi
	}
	local gib=1048576

	case "${F[arch]:-}" in
	x86_64) row PASS arch "x86_64" ;;
	*) row FAIL arch "${F[arch]:-unknown}: the base image, fixture and tier are x86_64 only" "use an x86_64 host" ;;
	esac

	case "${F[cpu_virt]:-none}" in
	vmx | svm) row PASS cpu-virt "${F[cpu_virt]} flag present" ;;
	*) row FAIL cpu-virt "no vmx/svm flag in /proc/cpuinfo" \
		"enable Intel VT-x / AMD-V in the firmware setup; on a cloud or nested VM, use bare metal or an instance type with nested virtualization enabled" ;;
	esac

	if version_ge "${F[kernel]:-0}" 5.10; then
		row PASS kernel "${F[kernel]}"
	else
		row FAIL kernel "${F[kernel]:-unknown}: Firecracker needs a 5.10 or newer host kernel" "upgrade the kernel to 5.10+ (Ubuntu 24.04 ships 6.8)"
	fi

	local kmod=kvm_intel
	[ "${F[cpu_virt]:-}" = svm ] && kmod=kvm_amd
	case "${F[kvm]:-missing}" in
	rw) row PASS kvm "/dev/kvm read-write for $user" ;;
	missing) row FAIL kvm "/dev/kvm does not exist${F[kvm_module]:+ (loaded: ${F[kvm_module]})}" "sudo modprobe $kmod && echo $kmod | sudo tee /etc/modules-load.d/kvm.conf" ;;
	*)
		local g=${F[kvm_group]:-kvm}
		if [[ " ${F[groups]:-} " == *" $g "* ]]; then
			row FAIL kvm "/dev/kvm not read-write for $user, though in group $g" "start a new login session (the group was added after this one began), or: sudo setfacl -m u:$user:rw /dev/kvm"
		else
			row FAIL kvm "/dev/kvm not read-write for $user (owner group $g)" "sudo usermod -aG $g $user, then log in again"
		fi
		;;
	esac

	case "${F[tun]:-missing}" in
	rw) row PASS tun "/dev/net/tun read-write" ;;
	missing) row FAIL tun "/dev/net/tun does not exist" "sudo modprobe tun && echo tun | sudo tee /etc/modules-load.d/tun.conf" ;;
	*) row FAIL tun "/dev/net/tun not read-write for $user" "sudo chmod 0666 /dev/net/tun (the usual mode)" ;;
	esac

	if [ "${F[cgroup_fs]:-}" = cgroup2fs ]; then
		row PASS cgroup "v2 (controllers: ${F[cgroup_controllers]:-none})"
	else
		row WARN cgroup "${F[cgroup_fs]:-unknown} at /sys/fs/cgroup, not the unified v2 hierarchy: per-VM CPU/RAM/PID limits cannot be enforced" \
			"add systemd.unified_cgroup_hierarchy=1 to GRUB_CMDLINE_LINUX in /etc/default/grub, sudo update-grub, reboot"
	fi

	if [ "${F[userns]:-}" = ok ]; then
		row PASS userns "unprivileged user + network namespaces"
	else
		local fix
		if [ "${F[sysctl_apparmor_restrict_userns]:-}" = 1 ]; then
			fix="AppArmor restricts them (Ubuntu 23.10+): echo kernel.apparmor_restrict_unprivileged_userns=0 | sudo tee /etc/sysctl.d/60-davit-userns.conf && sudo sysctl --system (a dedicated test host), or install davit's AppArmor profile (davit init --apparmor, docs/network.md)"
		elif [ "${F[sysctl_unprivileged_userns_clone]:-}" = 0 ]; then
			fix="echo kernel.unprivileged_userns_clone=1 | sudo tee /etc/sysctl.d/60-davit-userns.conf && sudo sysctl --system"
		elif [ "${F[sysctl_max_user_namespaces]:-}" = 0 ]; then
			fix="echo user.max_user_namespaces=15000 | sudo tee /etc/sysctl.d/60-davit-userns.conf && sudo sysctl --system"
		else
			fix="check the kernel config (CONFIG_USER_NS) and any LSM policy: unshare --user --map-root-user --net true"
		fi
		row FAIL userns "unshare --user --map-root-user --net fails: ${F[userns_error]:-no error text}" "$fix"
	fi

	local su=${F[subuid]:-0} sg=${F[subgid]:-0}
	if [ "$su" -ge 65536 ] && [ "$sg" -ge 65536 ] && [ -n "${F[newuidmap]:-}" ]; then
		row PASS subids "subuid $su, subgid $sg, newuidmap (images/build.sh)"
	else
		local f=""
		local lo=${F[subid_free]}
		[ "$lo" -ge 100000 ] || lo=100000
		{ [ "$su" -ge 65536 ] && [ "$sg" -ge 65536 ]; } || f="sudo usermod --add-subuids $lo-$((lo + 65535)) --add-subgids $lo-$((lo + 65535)) $user"
		[ -n "${F[newuidmap]:-}" ] || f="${f:+$f && }sudo apt install uidmap"
		row FAIL subids "images/build.sh needs 65536 subordinate ids and newuidmap: subuid $su, subgid $sg, newuidmap ${F[newuidmap]:-missing}" "$f"
	fi

	if [ "${F[vhost_vsock]:-}" = present ]; then
		row PASS vsock "/dev/vhost-vsock present"
	else
		row PASS vsock "vhost_vsock not loaded; not required (Firecracker uses hybrid vsock over Unix sockets)"
	fi

	local free_gib=$((${F[fs_free_kib]:-0} / gib)) where="${F[fs_checked]:-?} (${F[fs_type]:-?})"
	[ "${F[fs_checked]:-}" = "${F[fs_path]:-}" ] || where="${F[fs_path]:-?}: not created yet, judged by $where"
	case "${F[fs_reflink]:-no}" in
	yes) row PASS reflink "$where" ;;
	unknown) row WARN reflink "$where: reflink support not determined" "check it once: cp --reflink=always on a file in ${F[fs_checked]:-the data dir}; if that fails, use --fs on btrfs or XFS (reflink=1)" ;;
	*) row FAIL reflink "$where cannot reflink; test disks are reflinked from the image" "put the data on btrfs or XFS with reflink=1 and pass its path: make vm-tier HOST=... VM_TIER_FLAGS='--fs /path/on/btrfs/svm-fs'" ;;
	esac
	if [ "$free_gib" -lt "$MIN_DISK" ]; then
		row FAIL disk "$free_gib GiB free at ${F[fs_checked]:-?}, need $MIN_DISK" "free space there, or pass --fs with a larger reflink filesystem"
	elif [ "$free_gib" -lt $((MIN_DISK + 10)) ]; then
		row WARN disk "$free_gib GiB free at ${F[fs_checked]:-?}: enough ($MIN_DISK), tight with an image build on top" "free space if images/build.sh runs on this host"
	else
		row PASS disk "$free_gib GiB free at ${F[fs_checked]:-?}"
	fi

	local mem_gib=$((${F[mem_total_kib]:-0} / gib))
	if [ "$mem_gib" -lt 4 ]; then
		row FAIL memory "$mem_gib GiB RAM; the tier runs three 512 MiB guests plus a Go build" "use a host with 8 GiB or more"
	elif [ "$mem_gib" -lt 8 ]; then
		row WARN memory "$mem_gib GiB RAM (8 GiB is comfortable)" "close other workloads during the tier"
	else
		row PASS memory "$mem_gib GiB RAM, ${F[cpus]:-?} CPUs"
	fi

	local pin
	pin=$(pinned_firecracker)
	if [ -n "${F[fixture_firecracker]:-}" ]; then
		row PASS firecracker "pinned copy in place: ${F[fixture_firecracker]}"
	elif [ -n "${F[firecracker]:-}" ]; then
		if [[ "${F[firecracker_version]:-}" == *"${pin#v}"* ]]; then
			row PASS firecracker "${F[firecracker]}: ${F[firecracker_version]}"
		else
			row PASS firecracker "${F[firecracker]}: ${F[firecracker_version]:-version unknown}; the tier still uses its own pinned $pin (downloaded, sha256-verified)"
		fi
	else
		row PASS firecracker "not installed; the tier downloads the pinned $pin (sha256-verified) into its scratch"
	fi
	if [ -n "${F[jailer]:-}" ]; then
		row PASS jailer "${F[jailer]}: ${F[jailer_version]:-version unknown}"
	else
		row PASS jailer "not installed; the tier does not use it (production jailing needs root, docs/research/SPIKE.md)"
	fi

	if [ -n "${F[nft]:-}" ]; then
		row PASS nft "${F[nft]}"
	else
		row WARN nft "absent: the userspace egress stack filters alone" "sudo apt install nftables (optional)"
	fi

	local missing=${F[missing_tools]:-} t pkgs=()
	if [ -z "$missing" ]; then
		row PASS tools "git tar flock nsenter unshare ip debugfs sha256sum curl rsync python3 xz gpg make"
	else
		for t in $missing; do pkgs+=("$(pkg_for "$t")"); done
		row FAIL tools "missing: $missing" "sudo apt install $(printf '%s\n' "${pkgs[@]}" | sort -u | tr '\n' ' ' | sed 's/ $//')"
	fi

	if [ -n "${F[mise]:-}" ]; then
		row PASS toolchain "mise at ${F[mise]}"
	else
		row PASS toolchain "no mise yet; the tier's prepare step installs it into ${F[svm_dir]:-~/.svm}/bin"
	fi

	echo "host ${F[hostname]:-?}: ${F[distro]:-unknown distro}, kernel ${F[kernel]:-?}, ${F[cpus]:-?} CPUs, user $user"
	printf '%-12s %-6s %s\n' CHECK STATUS DETAIL
	printf '%s\n' "${rows[@]}"
	if [ ${#fixes[@]} -gt 0 ]; then
		echo
		echo "Fixes (run on the host; nothing here was changed):"
		printf '  %s\n' "${fixes[@]}"
	fi
	echo
	if [ "$fails" -gt 0 ]; then
		echo "preflight: FAIL ($fails failed, $warns warnings)"
		return 1
	fi
	echo "preflight: PASS ($warns warnings)"
}

MODE=ssh FACTS="" PROBE_ONLY=0 DIR="" FS="" MIN_DISK=20
while [ $# -gt 0 ]; do
	case "$1" in
	--local) MODE=local; shift ;;
	--facts) [ $# -ge 2 ] || usage; MODE=facts FACTS=$2; shift 2 ;;
	--probe-only) PROBE_ONLY=1; shift ;;
	--dir) [ $# -ge 2 ] || usage; DIR=$2; shift 2 ;;
	--fs) [ $# -ge 2 ] || usage; FS=$2; shift 2 ;;
	--min-disk) [ $# -ge 2 ] && [[ $2 =~ ^[0-9]+$ ]] || usage; MIN_DISK=$2; shift 2 ;;
	-h | --help) usage ;;
	-*) echo "host-preflight: unknown option $1" >&2; usage ;;
	*) break ;;
	esac
done
for p in "$DIR" "$FS"; do
	case "$p" in *[!A-Za-z0-9._/~-]*) echo "host-preflight: --dir/--fs must be a plain path: $p" >&2; exit 2 ;; esac
done

facts=$(mktemp "${TMPDIR:-/tmp}/host-preflight.XXXXXX")
trap 'rm -f "$facts"' EXIT
case "$MODE" in
facts)
	[ $# -eq 0 ] || usage
	[ -r "$FACTS" ] || { echo "host-preflight: cannot read $FACTS" >&2; exit 2; }
	cp "$FACTS" "$facts"
	;;
local)
	[ $# -eq 0 ] || usage
	SVM_DIR=${DIR:-${SVM_DIR:-}} SVM_FS=${FS:-${SVM_FS:-}} probe >"$facts"
	;;
ssh)
	[ $# -eq 1 ] || usage
	host=$1
	# The probe and the layout travel as a script on stdin; ~ expands on
	# the host.
	env_line=""
	[ -z "$DIR" ] || env_line+="SVM_DIR=$DIR "
	[ -z "$FS" ] || env_line+="SVM_FS=$FS "
	if ! { declare -f probe; echo "${env_line:+export $env_line}"; echo probe; } |
		ssh -o BatchMode=yes -o ConnectTimeout=15 "$host" bash -s >"$facts"; then
		echo "host-preflight: cannot run the probe on $host over SSH (key auth, BatchMode)" >&2
		exit 255
	fi
	;;
esac
if [ "$PROBE_ONLY" = 1 ]; then
	cat "$facts"
	exit 0
fi
evaluate "$facts"
