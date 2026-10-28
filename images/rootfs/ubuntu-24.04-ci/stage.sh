# shellcheck shell=bash
# Host side, sourced by build-rootfs.sh with lib.sh loaded: the GitHub Actions runner tarball into
# $STAGE (the layer's /.provision/layer/stage), pinned by version and sha256 in inputs.lock.
runner_arch=$([[ $DEB_ARCH == amd64 ]] && echo x64 || echo arm64)
sum=ACTIONS_RUNNER_SHA256_$runner_arch
runner_tgz=actions-runner-linux-$runner_arch-$ACTIONS_RUNNER_VERSION.tar.gz
fetch "https://github.com/actions/runner/releases/download/v$ACTIONS_RUNNER_VERSION/$runner_tgz" "${!sum}" "$DL/$runner_tgz"
cp "$DL/$runner_tgz" "$STAGE/actions-runner.tar.gz"
