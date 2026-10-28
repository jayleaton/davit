#!/bin/bash
# ubuntu-24.04-ci layer, run inside the chroot by provision.sh before sealing, after the dev layer:
# the GitHub Actions runner in /opt/actions-runner for a dedicated user `runner`, set up the way
# actions expect from GitHub's hosted runners (passwordless sudo, the docker group, a tool cache
# at /opt/hostedtoolcache). Nothing here registers it or talks to GitHub: the host starts
# /opt/actions-runner/run.sh as runner with a JIT config. The runner's .NET dependencies come
# from packages.txt (installed by provision.sh).
L=$P/layer

# uid 1001 like GitHub's runner user; the password stays locked (no login, sudo needs none).
useradd --create-home --uid 1001 --user-group --shell /bin/bash --groups docker runner
cp -r --no-preserve=mode,ownership "$L/overlay/." /
chmod 0440 /etc/sudoers.d/runner
visudo -cf /etc/sudoers.d/runner

# The runner writes beside itself (_diag, _work, run-helper.sh), so runner owns its directory;
# setup-* actions install into the tool cache.
install -d -o runner -g runner /opt/actions-runner /opt/hostedtoolcache
tar -xzf "$L/stage/actions-runner.tar.gz" -C /opt/actions-runner --no-same-owner
chown -R runner:runner /opt/actions-runner

# Prove the pinned runner starts as runner, which also proves its .NET dependencies resolve:
# config.sh checks them with ldd (relative to the current directory, hence the cd), and both
# print the listener's version and exit (no network, so no self-update).
install -d -o runner -g runner /tmp/runner-home
want_version() {
  local out
  out=$(cd /opt/actions-runner && runuser -u runner -- env HOME=/tmp/runner-home \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin "$@" --version 2>&1) ||
    { echo "$* --version failed: '$out'" >&2; exit 1; }
  grep -qxF "$ACTIONS_RUNNER_VERSION" <<<"$out" || { echo "$* --version: got '$out'" >&2; exit 1; }
}
want_version /opt/actions-runner/config.sh
want_version /opt/actions-runner/run.sh
# What those runs left (diag logs, config.sh's .path and .env, run.sh's helper copy).
rm -rf /tmp/runner-home /opt/actions-runner/_diag /opt/actions-runner/.path /opt/actions-runner/.env \
  /opt/actions-runner/run-helper.sh

# The listener loads .env (KEY=VALUE, no comments) into its environment and every job's. The tool
# cache is where actions/setup-node, setup-go and setup-python install; ImageOS is what
# setup-ruby and others read to pick prebuilt binaries on GitHub's images.
cat >/opt/actions-runner/.env <<'ENV'
LANG=C.UTF-8
RUNNER_TOOL_CACHE=/opt/hostedtoolcache
AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache
ImageOS=ubuntu24
ENV
chown runner:runner /opt/actions-runner/.env
