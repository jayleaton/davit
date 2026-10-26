#!/usr/bin/env bash
# Fresh-host end-to-end: what a new user does on a Linux KVM host, as a throwaway Linux user.
#
#   scripts/fresh-host-e2e.sh [--release DIR] [--user NAME] [--transcript FILE] [--keep-user]
#
# 1. A release: DIR (a `make release` output directory), else `make release` here (it needs
#    built images: DAVIT_IMAGES_OUT / DAVIT_MANIFESTS as for packaging/release.sh).
# 2. A throwaway user NAME (default davit-e2e-<random>; must not exist) with sudo for the
#    duration, a temporary SSH key, and a copy of the release in its home.
# 3. As that user, over a real SSH login (pam_systemd: a systemd user manager, as a person
#    gets): `sh install.sh --host --yes-sudo --from ~/release`, then `davit doctor`, then
#    `davit box run -- true` in a fresh git repository, against that user's own daemon.
#    The wall time from the install's start to the box command's end is reported.
# 4. Cleanup, also on failure: the user's boxes, service, lingering, processes, home, sudoers
#    entry and group memberships. Other users' daemons and VMs are never touched.
#
# Needs: passwordless sudo for the operator, sshd on localhost, KVM. Runs only on a VM host
# (docs/vm-tier.md), never on a development machine. --keep-user leaves the user for a look (delete it
# later with a normal run's cleanup: `sudo userdel -r NAME` after stopping its processes).
set -euo pipefail

RELEASE="" TUSER="" TRANSCRIPT="" KEEP=0
while [ $# -gt 0 ]; do
	case "$1" in
	--release) RELEASE=$(realpath "$2"); shift 2 ;;
	--user) TUSER=$2; shift 2 ;;
	--transcript) TRANSCRIPT=$(realpath -m "$2"); shift 2 ;;
	--keep-user) KEEP=1; shift ;;
	-h | --help) sed -n '2,22p' "$0"; exit 0 ;;
	*) echo "unknown argument: $1" >&2; exit 2 ;;
	esac
done

REPO=$(cd "$(dirname "$0")/.." && pwd)
[ -n "$TUSER" ] || TUSER=davit-e2e-$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')
case "$TUSER" in *[!a-z0-9-]* | -*) echo "bad user name $TUSER" >&2; exit 2 ;; esac
WORK=$(mktemp -d "${TMPDIR:-/tmp}/fresh-host.XXXXXX")
CREATED=0

say() { printf '\n==> %s\n' "$*" >&2; }
die() { echo "FAIL: $*" >&2; exit 1; }
sudo -n true 2>/dev/null || die "needs passwordless sudo (creates and deletes the throwaway user)"
[ -e /dev/kvm ] || die "no /dev/kvm: run this on a VM host"
if getent passwd "$TUSER" >/dev/null; then die "user $TUSER already exists; pick another --user"; fi

SSH=(ssh -i "$WORK/key" -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
	-o LogLevel=ERROR -o IdentitiesOnly=yes "$TUSER@localhost")

# as_user CMD: run CMD (one shell string) in a login shell of the throwaway user, mirrored to
# the transcript like a shell prompt.
as_user() {
	printf '$ %s\n' "$1" >&2
	[ -z "$TRANSCRIPT" ] || printf '$ %s\n' "$1" >>"$TRANSCRIPT"
	local out rc=0
	out=$("${SSH[@]}" "bash -lc $(printf '%q' "$1")" 2>&1) || rc=$?
	printf '%s\n' "$out" >&2
	[ -z "$TRANSCRIPT" ] || printf '%s\n\n' "$out" >>"$TRANSCRIPT"
	return $rc
}

cleanup() {
	local rc=$?
	set +e
	if [ "$CREATED" = 1 ] && [ "$KEEP" = 0 ]; then
		say "cleanup: delete $TUSER and everything it owns"
		local uid
		uid=$(id -u "$TUSER")
		# Its service; its VMs (Firecracker processes owned by the user) die with the pkill.
		"${SSH[@]}" 'systemctl --user disable --now davit.service' >/dev/null 2>&1
		sudo loginctl disable-linger "$TUSER"
		sudo loginctl terminate-user "$TUSER" 2>/dev/null
		sudo pkill -KILL -u "$uid"
		sleep 1
		sudo pkill -KILL -u "$uid"
		sudo rm -f "/etc/sudoers.d/$TUSER"
		# An AppArmor profile the install loaded for its binary (only on restricted hosts).
		for p in /etc/apparmor.d/davit-*; do
			[ -f "$p" ] && sudo grep -q "/home/$TUSER/" "$p" && { sudo apparmor_parser -R "$p"; sudo rm -f "$p"; }
		done
		sudo userdel -r "$TUSER" 2>&1 | grep -v 'mail spool' >&2
		getent passwd "$TUSER" >/dev/null && echo "WARNING: user $TUSER still exists" >&2
		[ -e "/home/$TUSER" ] && echo "WARNING: /home/$TUSER still exists" >&2
		[ -e "/run/user/$uid" ] && echo "note: /run/user/$uid is removed by logind shortly" >&2
	elif [ "$CREATED" = 1 ]; then
		echo "kept user $TUSER (--keep-user)" >&2
	fi
	rm -rf "$WORK"
	[ $rc = 0 ] && echo "PASS: fresh-host e2e as $TUSER" >&2
	exit $rc
}
trap cleanup EXIT INT TERM
[ -z "$TRANSCRIPT" ] || : >"$TRANSCRIPT"

if [ -z "$RELEASE" ]; then
	say "make release"
	make -C "$REPO" release
	RELEASE=$REPO/.build/release/$(cat "$REPO/.build/release/latest")
fi
[ -f "$RELEASE/install.sh" ] && [ -f "$RELEASE/SHA256SUMS" ] || die "$RELEASE is not a release directory"

say "throwaway user $TUSER (sudo for the install's --yes-sudo, an SSH key, the release)"
sudo useradd -m -s /bin/bash "$TUSER"
CREATED=1
echo "$TUSER ALL=(ALL) NOPASSWD: ALL" | sudo tee "/etc/sudoers.d/$TUSER" >/dev/null
sudo chmod 0440 "/etc/sudoers.d/$TUSER"
ssh-keygen -q -t ed25519 -N '' -f "$WORK/key"
sudo install -d -m 0700 -o "$TUSER" -g "$TUSER" "/home/$TUSER/.ssh"
sudo install -m 0600 -o "$TUSER" -g "$TUSER" "$WORK/key.pub" "/home/$TUSER/.ssh/authorized_keys"
sudo cp -r --reflink=auto "$RELEASE" "/home/$TUSER/release"
sudo chown -R "$TUSER:$TUSER" "/home/$TUSER/release"
"${SSH[@]}" true || die "cannot ssh to $TUSER@localhost"

say "install, doctor, first box command (timed)"
start=$(date +%s.%N)
as_user 'sh ~/release/install.sh --host --yes-sudo --from ~/release' || die "install.sh --host failed"
installed=$(date +%s.%N)
as_user 'davit doctor' || die "doctor reported failures"
as_user 'mkdir -p ~/first && cd ~/first && git init -q && davit box run -- true' || die "davit box run failed"
done_at=$(date +%s.%N)
as_user 'cd ~/first && davit box down && davit list --all'

awk -v s="$start" -v i="$installed" -v d="$done_at" -v k="$(uname -r)" -v c="$(nproc)" \
	-v m="$(free -g | awk '/^Mem/ {print $2}')" 'BEGIN {
	printf "\ntimings: install %.0fs, start of install to first box command done %.0fs (kernel %s, %s CPUs, %s GiB RAM)\n", i - s, d - s, k, c, m }' |
	tee -a "${TRANSCRIPT:-/dev/null}" >&2
