#!/bin/bash
# ubuntu-24.04-dev layer, run inside the chroot by provision.sh before sealing: developer
# toolchains on every login PATH (/usr/local/bin), root-owned and pinned by the image, so a box
# starts with them and nothing is downloaded at `box up`. Projects pin their own versions with
# mise (mise.toml) on top. Apt packages come from packages.txt (installed by provision.sh).
L=$P/layer

# Node.js LTS -> /opt/node (npm, npx, corepack too).
mkdir -p /opt/node
tar -xJf "$L/stage/node.tar.xz" -C /opt/node --strip-components=1 --no-same-owner
for b in node npm npx corepack; do ln -sfn "/opt/node/bin/$b" "/usr/local/bin/$b"; done

# Go -> /usr/local/go (the official layout); go and gofmt on PATH.
tar -xzf "$L/stage/go.tar.gz" -C /usr/local --no-same-owner
for b in go gofmt; do ln -sfn "/usr/local/go/bin/$b" "/usr/local/bin/$b"; done

# mise, pnpm (standalone executable; its bundled dist/ beside it), uv + uvx.
install -m 0755 "$L/stage/mise" /usr/local/bin/mise
mkdir -p /opt/pnpm
tar -xzf "$L/stage/pnpm.tar.gz" -C /opt/pnpm --no-same-owner
ln -sfn /opt/pnpm/pnpm /usr/local/bin/pnpm
tar -xzf "$L/stage/uv.tar.gz" -C /usr/local/bin --strip-components=1 --no-same-owner
chmod 0755 /usr/local/bin/uv /usr/local/bin/uvx

cp -r --no-preserve=mode,ownership "$L/overlay/." /

# Prove each resolves the way a box runs commands (login shell, as the image user), at the
# pinned version.
install -d -o davit -g davit /tmp/dev-home
run_as_user() {
  # shellcheck disable=SC2016 # $0/$@ belong to the inner login shell
  runuser -u davit -- env HOME=/tmp/dev-home PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    sh -lc 'exec "$0" "$@"' "$@"
}
want() { # COMMAND... -- TOKEN: TOKEN is a word of the command's output
  local out tok=${*: -1}
  out=$(run_as_user "${@:1:$#-2}")
  [[ " $out " == *" $tok "* ]] || { echo "$*: got '$out'" >&2; exit 1; }
}
want go version -- "go$GO_VERSION"
want node --version -- "$NODE_VERSION"
want pnpm --version -- "$PNPM_VERSION"
want uv --version -- "$UV_VERSION"
want mise --version -- "$MISE_VERSION"
rm -rf /tmp/dev-home
