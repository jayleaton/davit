#!/bin/sh
# Starts the profile's desktop: an X server without a GPU (Xvfb), a
# lightweight window manager, a VNC server bound to guest loopback only, and
# optionally Chromium. The gateway authenticates viewers and enforces
# view-only, so x11vnc runs without a password but never on a guest NIC.
set -eu

display=${DAVIT_DESKTOP_DISPLAY:-:1}
geometry=${DAVIT_DESKTOP_GEOMETRY:-1280x800x24}
port=${DAVIT_DESKTOP_PORT:-5900}

Xvfb "$display" -screen 0 "$geometry" -nolisten tcp -noreset &
xvfb=$!
trap 'kill "$xvfb" 2>/dev/null || true' EXIT INT TERM

# Wait for the X server's socket.
n=0
while [ ! -S "/tmp/.X11-unix/X${display#:}" ]; do
	n=$((n + 1))
	if [ "$n" -gt 100 ]; then
		echo "davit-desktop: Xvfb did not start on $display" >&2
		exit 1
	fi
	sleep 0.1
done
export DISPLAY="$display"

openbox &

if [ "${DAVIT_DESKTOP_BROWSER:-0}" = 1 ] && command -v chromium >/dev/null 2>&1; then
	chromium --no-first-run --no-default-browser-check --password-store=basic about:blank &
fi

# -localhost: 127.0.0.1 only. -shared/-forever: several viewers, survive
# disconnects. -nopw: the gateway is the authentication.
exec x11vnc -display "$display" -rfbport "$port" -localhost -nopw -shared -forever -noxdamage -quiet
