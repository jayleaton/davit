# shellcheck shell=sh
# davit dev image: toolchains are pinned by the image; projects pin their own with mise, and Go
# fetches a newer toolchain a go.mod asks for (GOTOOLCHAIN=auto, Go's default) into ~/go/pkg/mod.
# Every cache stays at its tool's default on the box's disk, so a project image (a snapshot after
# setup) carries them warm: ~/.cache/go-build, ~/go/pkg/mod, ~/.local/share/pnpm/store, ~/.cache/uv,
# ~/.npm, /var/lib/docker.
export MISE_YES=1
export PNPM_HOME="$HOME/.local/share/pnpm"
case ":$PATH:" in *":$HOME/go/bin:"*) ;; *) PATH="$HOME/go/bin:$PNPM_HOME:$HOME/.local/bin:$PATH" ;; esac
export PATH
# Interactive shells pick up a project's mise.toml (non-interactive commands use `mise exec`).
# mise activate has bash/zsh/fish modes only, no plain sh.
if [ -n "${PS1:-}" ] && command -v mise >/dev/null 2>&1; then
  if [ -n "${BASH_VERSION:-}" ]; then eval "$(mise activate bash)"
  elif [ -n "${ZSH_VERSION:-}" ]; then eval "$(mise activate zsh)"; fi
fi
