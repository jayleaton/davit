# shellcheck shell=bash
# Host side, sourced by build-rootfs.sh with lib.sh loaded: verified toolchain downloads into
# $STAGE (the layer's /.provision/layer/stage). Every URL is pinned by version and sha256 in
# inputs.lock.
node_arch=$([[ $DEB_ARCH == amd64 ]] && echo x64 || echo arm64)
sum=NODE_SHA256_$node_arch
fetch "$NODE_URL_PREFIX/node-$NODE_VERSION-linux-$node_arch.tar.xz" "${!sum}" "$DL/node-$NODE_VERSION-linux-$node_arch.tar.xz"
cp "$DL/node-$NODE_VERSION-linux-$node_arch.tar.xz" "$STAGE/node.tar.xz"

sum=GO_SHA256_$DEB_ARCH
fetch "$GO_URL_PREFIX/go$GO_VERSION.linux-$DEB_ARCH.tar.gz" "${!sum}" "$DL/go$GO_VERSION.linux-$DEB_ARCH.tar.gz"
cp "$DL/go$GO_VERSION.linux-$DEB_ARCH.tar.gz" "$STAGE/go.tar.gz"

sum=MISE_SHA256_$node_arch
fetch "https://github.com/jdx/mise/releases/download/v$MISE_VERSION/mise-v$MISE_VERSION-linux-$node_arch" "${!sum}" \
  "$DL/mise-v$MISE_VERSION-linux-$node_arch"
cp "$DL/mise-v$MISE_VERSION-linux-$node_arch" "$STAGE/mise"

sum=PNPM_SHA256_$node_arch
fetch "https://github.com/pnpm/pnpm/releases/download/v$PNPM_VERSION/pnpm-linux-$node_arch.tar.gz" "${!sum}" \
  "$DL/pnpm-$PNPM_VERSION-linux-$node_arch.tar.gz"
cp "$DL/pnpm-$PNPM_VERSION-linux-$node_arch.tar.gz" "$STAGE/pnpm.tar.gz"

sum=UV_SHA256_$ARCH
fetch "https://github.com/astral-sh/uv/releases/download/$UV_VERSION/uv-$ARCH-unknown-linux-gnu.tar.gz" "${!sum}" \
  "$DL/uv-$UV_VERSION-$ARCH.tar.gz"
cp "$DL/uv-$UV_VERSION-$ARCH.tar.gz" "$STAGE/uv.tar.gz"
