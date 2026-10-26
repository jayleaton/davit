# shellcheck shell=bash
# Host side, sourced by build-rootfs.sh with lib.sh loaded: put verified downloads into $STAGE,
# which the layer sees as /.provision/layer/stage.
node_arch=$([[ $DEB_ARCH == amd64 ]] && echo x64 || echo arm64)
node_sum=NODE_SHA256_$node_arch
fetch "$NODE_URL_PREFIX/node-$NODE_VERSION-linux-$node_arch.tar.xz" "${!node_sum}" \
  "$DL/node-$NODE_VERSION-linux-$node_arch.tar.xz"
cp "$DL/node-$NODE_VERSION-linux-$node_arch.tar.xz" "$STAGE/node.tar.xz"
