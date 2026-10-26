#!/bin/sh
# davit installer.
#
#   curl -fsSL https://github.com/jayleaton/davit/releases/latest/download/install.sh | sh -s -- --host
#   curl -fsSL https://github.com/jayleaton/davit/releases/latest/download/install.sh | sh -s -- --client
#
# --host (Linux with KVM): installs the davit binary into ~/.local/bin, checks the host before
# any large download, prints (or with --yes-sudo runs) the exact root commands a missing
# prerequisite needs, downloads the prebuilt guest image, writes the config (`davit init`),
# starts the systemd --user service, enables lingering and finishes with `davit doctor`.
# --client (any Linux or macOS machine; the default on macOS): installs only the CLI; then
# `davit connect user@host`.
#
# Rerunning is safe and upgrades: the binary and changed images are replaced, the config is
# kept and the service restarts on the new version. An installed davit.service names the
# install to upgrade (its binary and --config), wherever an earlier setup put them. Run `sh install.sh --help` for options.
#
# Every download is checked against the SHA256SUMS file of the same release (integrity:
# no corruption or truncation). Authenticity rests on HTTPS from the release host; releases
# also carry GitHub build-provenance attestations (docs/install-host.md#trust).
#
# Everything is inside functions and the only top-level command is the final `main "$@"`, so a
# truncated download runs nothing.

set -eu

NAME=davit
# The GitHub repository releases come from; the one place it is defined for the installer.
REPO=jayleaton/davit
# Set by `make release` to the release's own tag, so an installer fetched from a release
# installs that release; empty means the latest release.
RELEASE_TAG=

TMP_DIR=
STAGED=
IMG_STAGE=

say() {
	printf '%s-install: %s\n' "$NAME" "$*"
}

warn() {
	printf '%s-install: warning: %s\n' "$NAME" "$*" >&2
}

die() {
	printf '%s-install: error: %s\n' "$NAME" "$*" >&2
	exit 1
}

cleanup() {
	if [ -n "$STAGED" ]; then
		rm -f "$STAGED"
	fi
	if [ -n "$TMP_DIR" ]; then
		rm -rf "$TMP_DIR"
	fi
	if [ -n "$IMG_STAGE" ]; then
		rm -rf "$IMG_STAGE"
	fi
}

usage() {
	cat <<EOF
Usage: sh install.sh --host [options]     a Linux machine with KVM that runs the VMs
       sh install.sh --client [options]   a laptop or workstation that drives a host

Options:
  --yes-sudo           run the printed root commands (sudo prompts as usual); without it
                       they are printed and nothing runs as root
  --image NAME         guest image to install (repeatable or comma-separated); default
                       ubuntu-24.04-dev, the toolchain image project boxes use
  --data-dir DIR       daemon data dir for a new install (default ~/.local/share/$NAME);
                       put it on btrfs or XFS for instant VM clones
  --tailnet-owner ID   also serve the API on your tailnet for this Tailscale login
                       (optional; the auth key comes from \$DAVIT_TS_AUTHKEY)
  --version TAG        install this release instead of the latest
  --from URL|DIR       install from another release location (https://, file://, or a
                       local directory such as a \`make release\` output)
  --install-dir DIR    where the binary goes (default ~/.local/bin; an upgrade keeps the
                       binary where the installed $NAME.service runs it)
  --fake-runtime       development only: no KVM, VMs are simulated (never for real use)

Environment: DAVIT_RELEASE_URL, DAVIT_VERSION, DAVIT_INSTALL_DIR, DAVIT_DATA_DIR and
DAVIT_IMAGES are equivalents of --from, --version, --install-dir, --data-dir and --image.
EOF
}

detect_os() {
	_os=$(uname -s)
	case "$_os" in
	Linux) echo linux ;;
	Darwin) echo darwin ;;
	*) die "unsupported operating system '$_os' (supported: Linux, macOS)" ;;
	esac
}

detect_arch() {
	_arch=$(uname -m)
	case "$_arch" in
	x86_64 | amd64) echo amd64 ;;
	aarch64 | arm64) echo arm64 ;;
	*) die "unsupported CPU architecture '$_arch' (supported: x86_64, arm64)" ;;
	esac
}

check_url() {
	case "$1" in
	https://* | file:///*) ;;
	http://*) die "refusing plain http:// release URL '$1'; use https://" ;;
	*) die "the release location must be an https:// URL or a local directory, got '$1'" ;;
	esac
}

check_version() {
	case "$1" in
	'' | . | .. | *[!A-Za-z0-9._+-]*) die "invalid version '$1' (allowed characters: A-Z a-z 0-9 . _ + -)" ;;
	esac
}

# download URL DEST: fetch URL to DEST over https or file only.
download() {
	if command -v curl >/dev/null 2>&1; then
		curl --proto '=https,file' --proto-redir '=https' --tlsv1.2 \
			-fsSL --retry 3 -o "$2" "$1" || die "download failed: $1"
	elif command -v wget >/dev/null 2>&1; then
		case "$1" in
		https://*) ;;
		*) die "wget can only fetch https:// URLs; install curl for $1" ;;
		esac
		wget --https-only --secure-protocol=TLSv1_2 -q -O "$2" "$1" || die "download failed: $1"
	else
		die "need curl or wget to download"
	fi
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		_sum=$(sha256sum "$1") || die "sha256sum failed on $1"
	elif command -v shasum >/dev/null 2>&1; then
		_sum=$(shasum -a 256 "$1") || die "shasum failed on $1"
	else
		die "need sha256sum or shasum to verify downloads"
	fi
	printf '%s\n' "${_sum%% *}" | tr 'A-F' 'a-f'
}

check_hex() {
	case "$1" in
	*[!0-9a-f]*) die "$2 is not a hex SHA-256: '$1'" ;;
	esac
	[ "${#1}" -eq 64 ] || die "$2 is not a 64-character SHA-256: '$1'"
}

# sum_of FILE: print FILE's digest from SHA256SUMS, or nothing when it has no entry.
sum_of() {
	_count=$(awk -v f="$1" '$2 == f || $2 == "*" f { n++ } END { print n + 0 }' "$TMP_DIR/SHA256SUMS")
	[ "$_count" -le 1 ] || die "SHA256SUMS has $_count entries for $1 (expected one)"
	awk -v f="$1" '$2 == f || $2 == "*" f { print $1 }' "$TMP_DIR/SHA256SUMS" | tr 'A-F' 'a-f'
}

# fetch_verified FILE DEST: download a release file and check it against SHA256SUMS.
fetch_verified() {
	_want=$(sum_of "$1")
	[ -n "$_want" ] || die "SHA256SUMS has no entry for $1"
	check_hex "$_want" "SHA256SUMS entry for $1"
	download "$RELEASE_URL/$1" "$2"
	_got=$(sha256_of "$2")
	[ "$_got" = "$_want" ] || die "checksum mismatch for $1: SHA256SUMS says $_want, the download is $_got. Nothing from it was installed."
}

# install_binary: download, verify and atomically install the binary; sets BIN and
# BIN_CHANGED (1 when the file is new or different).
install_binary() {
	_asset=$NAME-$OS-$ARCH.tar.gz
	fetch_verified "$_asset" "$TMP_DIR/$_asset"
	mkdir "$TMP_DIR/bin"
	_list=$(tar -tzf "$TMP_DIR/$_asset") || die "corrupt archive $_asset"
	case "$_list" in
	"$NAME" | "./$NAME") ;;
	*) die "unexpected contents in $_asset (want only $NAME): $(printf '%s' "$_list" | tr '\n' ' ')" ;;
	esac
	(cd "$TMP_DIR/bin" && tar -xzf "../$_asset" --no-same-owner) || die "cannot extract $_asset"
	[ -f "$TMP_DIR/bin/$NAME" ] && [ ! -L "$TMP_DIR/bin/$NAME" ] || die "$_asset did not contain a regular $NAME file"
	mkdir -p "$INSTALL_DIR" || die "cannot create $INSTALL_DIR"
	BIN=$INSTALL_DIR/$NAME
	BIN_CHANGED=1
	if [ -f "$BIN" ] && [ "$(sha256_of "$BIN")" = "$(sha256_of "$TMP_DIR/bin/$NAME")" ]; then
		BIN_CHANGED=0
	fi
	# Staged in the target directory, so the mv is an atomic same-filesystem rename.
	STAGED=$INSTALL_DIR/.$NAME.new.$$
	cp "$TMP_DIR/bin/$NAME" "$STAGED" || die "cannot write to $INSTALL_DIR"
	chmod 0755 "$STAGED"
	mv -f "$STAGED" "$BIN" || die "cannot install $BIN"
	STAGED=
	say "installed $BIN ($("$BIN" version 2>/dev/null | head -n 1))"
}

check_image_name() {
	case "$1" in
	'' | . | .. | *[!A-Za-z0-9._-]*) die "invalid image name '$1' (allowed characters: A-Z a-z 0-9 . _ -)" ;;
	esac
}

# install_image NAME: download, verify and install one image bundle into $DATA/images, unless
# the same bundle is already installed. Staged on the data dir's filesystem (bundles are GiBs,
# never a RAM-backed TMPDIR); every blob is checked against its digest name, and the manifest
# moves into place last, so the daemon never sees a manifest whose blobs are missing. Sets
# IMAGES_CHANGED when it installed something.
install_image() {
	_name=$1
	_bundle=$NAME-image-$_name-$IMG_ARCH.tar.zst
	_want=$(sum_of "$_bundle")
	[ -n "$_want" ] || die "this release has no image $_name for $IMG_ARCH (see release.json for what it has)"
	_mark=$DATA/images/.bundles/$_bundle.sha256
	if [ -f "$DATA/images/manifests/$_name.json" ] && [ "$(cat "$_mark" 2>/dev/null)" = "$_want" ]; then
		say "image $_name: up to date"
		return 0
	fi
	_old_rootfs=
	if [ -f "$DATA/images/manifests/$_name.json" ]; then
		_old_rootfs=$(rootfs_digest "$DATA/images/manifests/$_name.json")
	fi
	mkdir -p "$DATA/images/blobs/sha256" "$DATA/images/manifests" "$DATA/images/.bundles" || die "cannot create $DATA/images"
	IMG_STAGE=$(mktemp -d "$DATA/images/.install.XXXXXX") || die "cannot create a staging dir in $DATA/images"
	say "image $_name: downloading $_bundle"
	fetch_verified "$_bundle" "$IMG_STAGE/$_bundle"
	mkdir "$IMG_STAGE/x"
	# Only manifests/<name>.json and blobs/sha256/<hex> may be in a bundle.
	zstd -dcq "$IMG_STAGE/$_bundle" | tar -tf - >"$IMG_STAGE/list" || die "corrupt bundle $_bundle"
	while IFS= read -r _e; do
		case "${_e#./}" in
		"" | manifests/ | blobs/ | blobs/sha256/ | "manifests/$_name.json") ;;
		blobs/sha256/*/* | blobs/sha256/*[!0-9a-f]*) die "unexpected path in $_bundle: $_e" ;;
		blobs/sha256/*) ;;
		*) die "unexpected path in $_bundle: $_e" ;;
		esac
	done <"$IMG_STAGE/list"
	say "image $_name: unpacking and verifying"
	zstd -dcq "$IMG_STAGE/$_bundle" | (cd "$IMG_STAGE/x" && tar -xf - --no-same-owner) || die "cannot extract $_bundle"
	rm -f "$IMG_STAGE/$_bundle" "$IMG_STAGE/list"
	[ -f "$IMG_STAGE/x/manifests/$_name.json" ] || die "$_bundle has no manifests/$_name.json"
	for _b in "$IMG_STAGE"/x/blobs/sha256/*; do
		[ -f "$_b" ] && [ ! -L "$_b" ] || die "$_bundle: $_b is not a regular file"
		_h=${_b##*/}
		check_hex "$_h" "blob name in $_bundle"
		[ "$(sha256_of "$_b")" = "$_h" ] || die "$_bundle: blob $_h does not match its digest. Image not installed."
	done
	for _b in "$IMG_STAGE"/x/blobs/sha256/*; do
		_dst=$DATA/images/blobs/sha256/${_b##*/}
		[ -e "$_dst" ] || mv "$_b" "$_dst" || die "cannot install blob $_dst"
	done
	mv -f "$IMG_STAGE/x/manifests/$_name.json" "$DATA/images/manifests/$_name.json" || die "cannot install the manifest"
	printf '%s\n' "$_want" >"$_mark"
	rm -rf "$IMG_STAGE"
	IMG_STAGE=
	IMAGES_CHANGED=1
	say "image $_name: installed"
	drop_rootfs "$_old_rootfs"
}

# rootfs_digest MANIFEST: the hex digest of the rootfs blob MANIFEST names (nothing when it
# names none).
rootfs_digest() {
	sed -n '/"rootfs": *{/,/"digest"/ s/.*"digest": *"sha256:\([0-9a-f]\{64\}\)".*/\1/p' "$1" 2>/dev/null | head -n 1
}

# drop_rootfs HEX: remove a replaced image's rootfs blob when no manifest in the data dir or
# the config uses it any more. Only creates and goldens read an image rootfs (a VM's disk is
# its own clone, a resume boots that), so the daemon's golden sweep then drops the goldens
# captured from it; kernels stay, since existing VMs boot them.
drop_rootfs() {
	[ -n "$1" ] && [ -f "$DATA/images/blobs/sha256/$1" ] || return 0
	for _m in "$DATA"/images/manifests/*.json $(sed -n 's/^ *"manifest": *"\(.*\)",\{0,1\} *$/\1/p' "$CONFIG" 2>/dev/null); do
		if [ -f "$_m" ] && grep -q "sha256:$1" "$_m"; then
			return 0
		fi
	done
	rm -f "$DATA/images/blobs/sha256/$1"
	say "removed the replaced rootfs blob $(printf '%.12s' "$1")"
}

# unit_exec N: field N (1 the binary, 2 the config) of the installed unit's
# ExecStart="BIN" daemon --config "CONFIG" (`davit init --service` writes it; systemd's %%
# is a literal %).
unit_exec() {
	_re='^ExecStart="\([^"]*\)" daemon --config "\([^"]*\)" *$'
	sed -n "s/$_re/\\$1/p" "$UNIT" | head -n 1 | sed 's/%%/%/g'
}

# existing_install: an installed davit.service is the install to upgrade, wherever an earlier
# install (or `davit init --config ... --service`) put it: its config (and so its data dir)
# and its binary stay where they are, so a rerun never starts a second, empty setup beside
# the one that runs.
existing_install() {
	[ -f "$UNIT" ] || return 0
	_ubin=$(unit_exec 1)
	_ucfg=$(unit_exec 2)
	[ -n "$_ubin" ] && [ -n "$_ucfg" ] ||
		die "$UNIT has no ExecStart=\"BIN\" daemon --config \"CONFIG\" line, so this installer cannot tell which setup it runs; fix or remove it, then rerun"
	[ "${_ubin##*/}" = "$NAME" ] || die "$NAME.service runs $_ubin; an upgrade installs $NAME. Fix $UNIT (or remove it), then rerun"
	if [ "$INSTALL_DIR_SET" = 1 ] && [ "${INSTALL_DIR%/}/$NAME" != "$_ubin" ]; then
		die "$NAME.service runs $_ubin, not ${INSTALL_DIR%/}/$NAME: rerun without --install-dir to upgrade it in place"
	fi
	INSTALL_DIR=${_ubin%/*}
	if [ "$_ucfg" != "$CONFIG" ]; then
		[ ! -f "$CONFIG" ] || warn "$CONFIG is not the config $NAME.service runs; upgrading the one it runs ($_ucfg)"
		CONFIG=$_ucfg
	fi
	say "upgrading the install $NAME.service runs: $_ubin --config $CONFIG"
}

# free_kib DIR: free KiB on the filesystem holding DIR (or its nearest existing ancestor).
free_kib() {
	_d=$1
	while [ ! -d "$_d" ]; do _d=$(dirname "$_d"); done
	df -Pk "$_d" | awk 'NR == 2 { print $4 }'
}

# config_field FILE FIELD: a top-level string field (dataDir, socket) a config names.
config_field() {
	sed -n 's/^ *"'"$2"'": *"\(.*\)",\{0,1\} *$/\1/p' "$1" | head -n 1
}

# quote ARG: ARG as one single-quoted shell word.
quote() {
	printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
}

# davit_run ARGS...: run the installed binary. Right after --yes-sudo added this user to
# /dev/kvm's group, this shell still has its old groups: run through `sg` (no password for
# members) so the checks see what a new login (and the restarted service) will see.
davit_run() {
	if [ -n "$SG_GROUP" ]; then
		_cmd=$(quote "$BIN")
		for _a in "$@"; do _cmd="$_cmd $(quote "$_a")"; done
		sg "$SG_GROUP" -c "$_cmd"
	else
		"$BIN" "$@"
	fi
}

# refresh_sg: use sg for later checks when /dev/kvm's group lists this user but this shell
# does not have it yet.
refresh_sg() {
	SG_GROUP=
	[ -e /dev/kvm ] && [ ! -w /dev/kvm ] || return 0
	command -v sg >/dev/null 2>&1 || return 0
	_g=$(stat -c %G /dev/kvm 2>/dev/null) || return 0
	if getent group "$_g" | cut -d: -f4 | tr ',' '\n' | grep -qx "$USER_NAME"; then
		SG_GROUP=$_g
	fi
}

# root_fixes FILE: show the root commands in FILE (`davit doctor --fixes`: "# why" comments,
# then commands) and run them with --yes-sudo. Without --yes-sudo it stops the install, since
# the service cannot work until they ran.
root_fixes() {
	[ -s "$1" ] || return 0
	echo
	say "this host needs a one-time change as root:"
	sed 's/^/    /' "$1"
	echo
	if [ "$YES_SUDO" != 1 ]; then
		say "run those commands (or rerun this installer with --yes-sudo to run them), then rerun"
		say "the same install command; it picks up where it stopped. $NAME never runs sudo on its own."
		exit 3
	fi
	while IFS= read -r _line; do
		case "$_line" in '#'* | '') continue ;; esac
		say "running: $_line"
		sh -c "$_line" </dev/null || die "a root command failed: $_line"
	done <"$1"
	refresh_sg
}

# need_tools: the tools a host install needs before any download, with the package command
# that adds the missing ones.
need_tools() {
	_missing=
	for _t in tar zstd; do
		command -v "$_t" >/dev/null 2>&1 || _missing="$_missing $_t"
	done
	[ -n "$_missing" ] || return 0
	if command -v apt-get >/dev/null 2>&1; then
		_cmd="sudo apt-get install -y$_missing"
	elif command -v dnf >/dev/null 2>&1; then
		_cmd="sudo dnf install -y$_missing"
	else
		die "install$_missing with your package manager, then rerun"
	fi
	printf '# unpacking the guest image needs:%s\n%s\n' "$_missing" "$_cmd" >"$TMP_DIR/tools"
	root_fixes "$TMP_DIR/tools"
}

# check_systemd_user: the service needs a systemd user manager, which a real login (ssh or
# console) starts; `sudo -u` and `su` sessions have none.
check_systemd_user() {
	command -v systemctl >/dev/null 2>&1 || die "systemd is required for the $NAME service (use --client on machines without it)"
	if ! systemctl --user show-environment >/dev/null 2>&1; then
		die "no systemd user session: log in as $USER_NAME over ssh or on a console (not with sudo -u or su), then rerun"
	fi
}

linger_enabled() {
	[ -e "/var/lib/systemd/linger/$USER_NAME" ] ||
		[ "$(loginctl show-user "$USER_NAME" -p Linger --value 2>/dev/null)" = yes ]
}

# enable_linger: keep the user manager (and the service) running without a login session and
# start it at boot. polkit often lets users do this for themselves; else it needs root.
enable_linger() {
	linger_enabled && return 0
	if loginctl enable-linger "$USER_NAME" >/dev/null 2>&1 && linger_enabled; then
		say "enabled lingering: $NAME.service keeps running after you log out and starts at boot"
		return 0
	fi
	printf '# lingering keeps %s.service running after you log out and starts it at boot\nsudo loginctl enable-linger %s\n' \
		"$NAME" "$(quote "$USER_NAME")" >"$TMP_DIR/linger"
	root_fixes "$TMP_DIR/linger"
}

# wait_daemon: wait for the service to answer on the socket its config names (its first
# start downloads Firecracker and verifies the image blobs).
wait_daemon() {
	_i=0
	_sock=$(config_field "$CONFIG" socket)
	set -- capabilities
	if [ -n "$_sock" ]; then set -- --api-url "unix://$_sock" capabilities; fi
	while [ "$_i" -lt 300 ]; do
		if davit_run "$@" >/dev/null 2>&1; then
			return 0
		fi
		if ! systemctl --user is-active --quiet "$NAME.service" && [ "$_i" -gt 10 ]; then
			systemctl --user status --no-pager "$NAME.service" >&2 || true
			journalctl --user -u "$NAME.service" -n 30 --no-pager >&2 || true
			die "$NAME.service is not running (log above); fix what it reports, then rerun"
		fi
		sleep 1
		_i=$((_i + 1))
	done
	die "$NAME.service did not answer within 300s: journalctl --user -u $NAME.service"
}

path_note() {
	case ":${PATH:-}:" in
	*":$INSTALL_DIR:"*) ;;
	*)
		echo
		say "$INSTALL_DIR is not on your PATH. Add it to your shell profile:"
		# shellcheck disable=SC2016 # literal $PATH is intended
		printf '    export PATH="%s:$PATH"\n' "$INSTALL_DIR"
		;;
	esac
}

install_client() {
	install_binary
	path_note
	echo
	say "next: connect to your host (the one you ran install.sh --host on):"
	printf '    %s connect user@host\n' "$NAME"
}

install_host() {
	[ "$OS" = linux ] || die "--host needs Linux with KVM; on macOS use --client"
	[ -n "${HOME:-}" ] || die "HOME is not set"
	if [ "$RUNTIME" != fake ] && [ -z "$(sum_of "$NAME-image-$DEFAULT_IMAGE-$IMG_ARCH.tar.zst")" ]; then
		die "this release has no guest image for $IMG_ARCH: hosts are x86_64 only for now. Use --client on this machine and an x86_64 host."
	fi
	check_systemd_user
	need_tools

	_xdg=${XDG_CONFIG_HOME:-}
	case "$_xdg" in /*) ;; *) _xdg=$HOME/.config ;; esac
	CONFIG=$_xdg/$NAME/config.json
	UNIT=$_xdg/systemd/user/$NAME.service
	DEFAULT_CONFIG=$CONFIG
	existing_install
	install_binary

	# 1. Configuration (no large downloads yet). The runtime is firecracker: a host whose KVM
	#    is broken gets fixes and a failing doctor, never simulated VMs (--fake-runtime is
	#    for development only).
	if [ -f "$CONFIG" ]; then
		DATA=$(config_field "$CONFIG" dataDir)
		[ -n "$DATA" ] || die "cannot read dataDir from $CONFIG"
		[ -z "$DATA_ARG" ] || [ "$DATA_ARG" = "$DATA" ] || warn "keeping the data dir of the existing config ($DATA); --data-dir applies to a new install"
	else
		DATA=$DATA_ARG
		if [ -z "$DATA" ]; then
			case "${XDG_DATA_HOME:-}" in
			/*) DATA=$XDG_DATA_HOME/$NAME ;;
			*) DATA=$HOME/.local/share/$NAME ;;
			esac
		fi
		case "$DATA" in /*) ;; *) die "--data-dir must be an absolute path" ;; esac
	fi

	if [ ! -f "$CONFIG" ]; then
		set -- init --config "$CONFIG" --data-dir "$DATA" --runtime "$RUNTIME"
		if [ -n "$TAILNET_OWNER" ]; then
			set -- "$@" --owner-login "$TAILNET_OWNER"
		fi
		if [ -n "$TAILNET_OWNER" ] && [ -n "${DAVIT_TS_AUTHKEY:-}" ]; then
			printf '%s\n' "$DAVIT_TS_AUTHKEY" | davit_run "$@" --auth-key-stdin >"$TMP_DIR/init" 2>&1 ||
				{ cat "$TMP_DIR/init" >&2; die "$NAME init failed"; }
		else
			davit_run "$@" >"$TMP_DIR/init" 2>&1 || { cat "$TMP_DIR/init" >&2; die "$NAME init failed"; }
		fi
		say "wrote $CONFIG (data in $DATA)"
	fi

	# 2. Host prerequisites before any large download: the root fixes `davit doctor` knows
	#    (KVM access, AppArmor, namespaces, TUN), then the space the image needs.
	davit_run doctor --config "$CONFIG" --fixes >"$TMP_DIR/fixes" || die "$NAME doctor --fixes failed"
	root_fixes "$TMP_DIR/fixes"

	if [ "$RUNTIME" != fake ]; then
		_need=$((10 * 1024 * 1024))
		_free=$(free_kib "$DATA")
		if [ "$_free" -lt "$_need" ]; then
			die "$DATA has $((_free / 1024 / 1024)) GiB free; the image and VM disks need at least 10 GiB. Free space or rerun with --data-dir on a bigger disk."
		fi
		[ -n "$IMAGES" ] || IMAGES=$DEFAULT_IMAGE
		for _img in $IMAGES; do
			install_image "$_img"
		done
	fi

	# 3. The service, with lingering so it outlives this login and starts at boot.
	_was_active=0
	if systemctl --user is-active --quiet "$NAME.service"; then _was_active=1; fi
	davit_run init --config "$CONFIG" --service >"$TMP_DIR/init" 2>&1 || { cat "$TMP_DIR/init" >&2; die "$NAME init --service failed"; }
	if [ "$_was_active" = 1 ] && { [ "$BIN_CHANGED" = 1 ] || [ "$IMAGES_CHANGED" = 1 ]; }; then
		say "restarting $NAME.service on the new version (running VMs keep running)"
		systemctl --user restart "$NAME.service" || die "systemctl --user restart $NAME.service failed"
	elif ! systemctl --user is-active --quiet "$NAME.service"; then
		systemctl --user restart "$NAME.service" || die "systemctl --user restart $NAME.service failed"
	fi
	enable_linger
	say "waiting for $NAME.service"
	wait_daemon

	# 4. Health.
	echo
	if ! davit_run doctor --config "$CONFIG"; then
		echo
		die "$NAME doctor reports a problem (above); fix it, then rerun this installer"
	fi
	path_note
	if [ "$CONFIG" != "$DEFAULT_CONFIG" ]; then
		echo
		say "this host's config is $CONFIG; host commands need it, e.g. $NAME doctor --config $(quote "$CONFIG")"
	fi
	echo
	say "this host is ready. From your laptop:"
	printf '    curl -fsSL %s | sh -s -- --client\n' "$INSTALLER_URL"
	printf '    %s connect %s@%s\n' "$NAME" "$USER_NAME" "$(hostname 2>/dev/null || echo host)"
	printf '    cd your-repo && %s box run -- make test\n' "$NAME"
	if [ -n "$SG_GROUP" ]; then
		say "log out and back in (or run \`newgrp $SG_GROUP\`) before using $NAME in this shell: it predates your new $SG_GROUP group"
	fi
}

main() {
	ROLE='' YES_SUDO=0 IMAGES=${DAVIT_IMAGES:-} DATA_ARG=${DAVIT_DATA_DIR:-} TAILNET_OWNER='' RUNTIME=firecracker
	RELEASE_URL=${DAVIT_RELEASE_URL:-} VERSION=${DAVIT_VERSION:-$RELEASE_TAG} INSTALL_DIR=${DAVIT_INSTALL_DIR:-}
	while [ $# -gt 0 ]; do
		case "$1" in
		--host) ROLE=host ;;
		--client) ROLE=client ;;
		--yes-sudo) YES_SUDO=1 ;;
		--fake-runtime) RUNTIME=fake ;;
		--image | --data-dir | --tailnet-owner | --version | --from | --install-dir)
			[ $# -ge 2 ] || die "$1 needs a value"
			case "$1" in
			--image) IMAGES="$IMAGES,$2" ;;
			--data-dir) DATA_ARG=$2 ;;
			--tailnet-owner) TAILNET_OWNER=$2 ;;
			--version) VERSION=$2 ;;
			--from) RELEASE_URL=$2 ;;
			--install-dir) INSTALL_DIR=$2 ;;
			esac
			shift
			;;
		-h | --help)
			usage
			return 0
			;;
		*) die "unknown argument '$1' (see --help)" ;;
		esac
		shift
	done

	if [ "$(id -u)" -eq 0 ]; then
		die "run this as the user who will own the VMs, not root; it asks for sudo only where needed"
	fi
	OS=$(detect_os)
	ARCH=$(detect_arch)
	if [ -z "$ROLE" ]; then
		[ "$OS" = darwin ] || {
			usage >&2
			die "choose a role: --host on the Linux machine that runs the VMs, --client on a laptop"
		}
		ROLE=client
	fi
	USER_NAME=$(id -un)
	IMAGES=$(printf '%s' "$IMAGES" | tr ',' ' ')
	for _img in $IMAGES; do check_image_name "$_img"; done
	case "$ARCH" in amd64) IMG_ARCH=x86_64 ;; arm64) IMG_ARCH=aarch64 ;; esac
	INSTALL_DIR_SET=0
	if [ -n "$INSTALL_DIR" ]; then INSTALL_DIR_SET=1; fi
	[ -n "$INSTALL_DIR" ] || INSTALL_DIR=${HOME:?HOME is not set}/.local/bin

	if [ -n "$VERSION" ]; then
		check_version "$VERSION"
		INSTALLER_URL=https://github.com/$REPO/releases/download/$VERSION/install.sh
	else
		INSTALLER_URL=https://github.com/$REPO/releases/latest/download/install.sh
	fi
	if [ -z "$RELEASE_URL" ]; then
		RELEASE_URL=${INSTALLER_URL%/install.sh}
	fi
	RELEASE_URL=${RELEASE_URL%/}
	case "$RELEASE_URL" in
	/*) RELEASE_URL=file://$RELEASE_URL ;;
	*://*) ;;
	*) [ -d "$RELEASE_URL" ] && RELEASE_URL=file://$(cd "$RELEASE_URL" && pwd) ;;
	esac
	check_url "$RELEASE_URL"

	trap cleanup EXIT
	trap 'cleanup; exit 130' INT
	trap 'cleanup; exit 143' TERM
	TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/$NAME-install.XXXXXX") || die "cannot create a temporary directory"
	BIN_CHANGED=0 IMAGES_CHANGED=0 SG_GROUP=
	say "installing the $ROLE from $RELEASE_URL"
	download "$RELEASE_URL/SHA256SUMS" "$TMP_DIR/SHA256SUMS"
	DEFAULT_IMAGE=ubuntu-24.04-dev
	if [ -z "$(sum_of "$NAME-image-$DEFAULT_IMAGE-$IMG_ARCH.tar.zst")" ]; then
		DEFAULT_IMAGE=ubuntu-24.04-base
	fi
	refresh_sg

	case "$ROLE" in
	client) install_client ;;
	host) install_host ;;
	esac
}

main "$@"
