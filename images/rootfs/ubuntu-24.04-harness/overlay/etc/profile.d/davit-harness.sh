# shellcheck shell=sh
# davit harness image: the CLIs are pinned by the image (manifest capabilities); upgrade the
# image rather than letting a CLI update itself inside one VM.
export DISABLE_AUTOUPDATER=1
