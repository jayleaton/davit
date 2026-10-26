#!/bin/bash
# ubuntu-24.04-harness layer, run inside the chroot by provision.sh before sealing.
# Adds Node.js (pinned tarball) and the coding-agent CLIs that internal/harness drives, as image
# capabilities harness.claude and harness.codex: `claude` / `codex` on the login PATH of every
# user (internal/harness runs them via `sh -lc`). Packages come from npm through npm ci against the
# committed package-lock.json, so every tarball (including each platform binary) is sha512-pinned,
# and with --ignore-scripts no package code runs at build time.
L=$P/layer

# Node.js -> /opt/node, linked into /usr/local/bin.
mkdir -p /opt/node
tar -xJf "$L/stage/node.tar.xz" -C /opt/node --strip-components=1 --no-same-owner
for b in node npm npx corepack; do ln -sfn "/opt/node/bin/$b" "/usr/local/bin/$b"; done
export PATH=/opt/node/bin:$PATH # the build chroot's PATH lacks /usr/local/bin (npm scripts need node)
[[ $(node --version) == "$NODE_VERSION" ]]

# CLIs -> /opt/davit-harness (root-owned: users cannot drift the pinned versions).
install -d /opt/davit-harness
cp "$L/npm/package.json" "$L/npm/package-lock.json" /opt/davit-harness/
npm ci --prefix /opt/davit-harness --omit=dev --ignore-scripts --no-audit --no-fund \
  --no-update-notifier --cache /tmp/npm-cache
rm -rf /tmp/npm-cache
nm=/opt/davit-harness/node_modules
# Claude Code: the native binary from its platform package (all its skipped postinstall would do
# is copy this file over bin/claude.exe). Codex: its own Node launcher, which needs no install step.
claude_bin=$nm/@anthropic-ai/claude-code-linux-$([[ $DEB_ARCH == amd64 ]] && echo x64 || echo arm64)/claude
[[ -x $claude_bin ]]
ln -sfn "$claude_bin" /usr/local/bin/claude
ln -sfn "$nm/@openai/codex/bin/codex.js" /usr/local/bin/codex
cp -r --no-preserve=mode,ownership "$L/overlay/." /

# Prove both resolve the way internal/harness invokes them, as the image user, at the pinned
# versions (versions from package.json, which package-lock.json pins).
want_claude=$(node -p 'require("/opt/davit-harness/package.json").dependencies["@anthropic-ai/claude-code"]')
want_codex=$(node -p 'require("/opt/davit-harness/package.json").dependencies["@openai/codex"]')
# Same argv shape as internal/harness (Service.argv); HOME is a throwaway so nothing lands in /home.
install -d -o davit -g davit /tmp/harness-home
# shellcheck disable=SC2016 # $0/$@ belong to the inner login shell
# PATH as systemd gives the agent (and so its exec children) in the guest.
run_as_user() {
  runuser -u davit -- env HOME=/tmp/harness-home PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    sh -lc 'exec "$0" "$@"' "$@"
}
got_claude=$(run_as_user claude --version)
got_codex=$(run_as_user codex --version)
[[ $got_claude == "$want_claude"* ]] || { echo "claude --version: '$got_claude', want $want_claude" >&2; exit 1; }
[[ $got_codex == *"$want_codex" ]] || { echo "codex --version: '$got_codex', want $want_codex" >&2; exit 1; }
rm -rf /tmp/harness-home
