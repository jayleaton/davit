#!/usr/bin/env bash
# Build the guest agent from this repository: the static, CGO-free `davit` binary, which the
# image installs as /usr/local/bin/davit-guest-agent and runs as `... guest-agent`.
#   ARCH=x86_64|aarch64 images/scripts/build-agent.sh
#
# Version gate (fails the build): the agent is stamped with
# Version=$(git describe --tags --always --dirty), the same stamp `make build` uses, and must report
# exactly that and the protocol range images/agentinfo reports for this commit
# (`davit guest-agent -version`; cross builds are checked by smoke's handshake on their own arch). build-rootfs.sh refuses an agent that is not from HEAD and re-checks the
# version inside the image; smoke checks the version the running agent sends in its handshake.
# A tree with uncommitted or untracked files is dirty (untracked .go files would be compiled in)
# and refused unless DAVIT_ALLOW_DIRTY=1 (development only: the manifest then says dirty).
# Toolchain: the Go version pinned in mise.toml; -trimpath and pinned GOAMD64/GOARM64 make the
# output independent of the caller's path and environment.
set -euo pipefail
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_lock
set_arch
REPO=$(git -C "$IMAGES_DIR" rev-parse --show-toplevel)
GO=(mise exec -- go) # run from $REPO so mise.toml pins the toolchain
goarch=$DEB_ARCH

commit=$(git -C "$REPO" rev-parse HEAD)
version=$(git -C "$REPO" describe --tags --always --dirty)
dirty=false
if [[ -n $(git -C "$REPO" status --porcelain) ]]; then
  [[ $version == *-dirty ]] || version+=-dirty # untracked files only: describe misses them
  dirty=true
  [[ ${DAVIT_ALLOW_DIRTY:-0} == 1 ]] ||
    die "working tree is dirty: the baked agent must match a commit (commit first, or DAVIT_ALLOW_DIRTY=1 for a development image)"
  log "agent: WARNING building from a dirty tree ($version)"
fi

awork=$WORK/agent-$ARCH
rm -rf "$awork"
mkdir -p "$awork/gotmp"
log "agent: building davit $version ($commit) for linux/$goarch"
(cd "$REPO" && env -u GOEXPERIMENT -u GOFLAGS GOTMPDIR="$awork/gotmp" CGO_ENABLED=0 GOOS=linux \
  GOARCH="$goarch" GOAMD64=v1 GOARM64=v8.0 GOFLAGS=-mod=readonly \
  "${GO[@]}" build -trimpath -buildvcs=true \
  -ldflags "-s -w -X davit/internal/buildinfo.Version=$version" -o "$awork/davit" ./cmd/davit)

# Guest-agent protocol of this commit (versions + features) for the manifest; the daemon refuses
# an image whose agent shares no protocol version with it and warns on missing features.
protocol=$(cd "$REPO" && env GOTMPDIR="$awork/gotmp" CGO_ENABLED=0 "${GO[@]}" run ./images/agentinfo)
pmin=$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["min"])' "$protocol")
pmax=$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["max"])' "$protocol")
if [[ $DEB_ARCH == "$HOST_DEB_ARCH" ]]; then
  out=$("$awork/davit" guest-agent -version)
  [[ $out == "davit guest-agent $version protocol $pmin-$pmax" ]] ||
    die "agent -version printed '$out', want 'davit guest-agent $version protocol $pmin-$pmax'"
fi
gov=$(cd "$REPO" && "${GO[@]}" env GOVERSION)

{
  echo "AGENT_BLOB=$(put_blob "$awork/davit")"
  echo "AGENT_VERSION=$version"
  echo "AGENT_COMMIT=$commit"
  echo "AGENT_DIRTY=$dirty"
  echo "AGENT_GO=$gov"
  echo "AGENT_GOARCH=$goarch"
  echo "AGENT_PROTOCOL=$protocol"
} >"$WORK/agent-$ARCH.env.part"
mv "$WORK/agent-$ARCH.env.part" "$WORK/agent-$ARCH.env"
rm -rf "$awork"
log "agent: done"
cat "$WORK/agent-$ARCH.env" >&2
