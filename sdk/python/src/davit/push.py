"""Tree pushes: select a local directory's files like ``davit push`` and write them as a tar."""

from __future__ import annotations

import os
import stat
import subprocess
import tarfile
from collections.abc import Sequence
from typing import IO

__all__ = ["DEFAULT_PUSH_PATH", "git_selection", "write_tree_tar"]

DEFAULT_PUSH_PATH = "/home/davit/work"
"""Guest directory of a push when none is given (the CLI's default)."""


# Variables that tell git which repository, index or object store to use, or
# where discovery stops. Inherited from a caller (a git hook, an editor, a
# script inside another checkout) they would point every command at that
# repository instead of the tree being pushed; git finds it from -C alone.
_REPO_LOCATING_ENV = frozenset(
    {
        "GIT_DIR",
        "GIT_WORK_TREE",
        "GIT_INDEX_FILE",
        "GIT_OBJECT_DIRECTORY",
        "GIT_ALTERNATE_OBJECT_DIRECTORIES",
        "GIT_COMMON_DIR",
        "GIT_CEILING_DIRECTORIES",
    }
)


def _git(root: str, *args: str) -> bytes:
    env = {k: v for k, v in os.environ.items() if k not in _REPO_LOCATING_ENV}
    try:
        done = subprocess.run(
            ["git", "-C", root, *args],
            check=False,
            stdin=subprocess.DEVNULL,
            capture_output=True,
            env=env,
        )
    except FileNotFoundError as exc:
        raise ValueError("push: git is not installed") from exc
    if done.returncode != 0:
        msg = done.stderr.decode(errors="replace").strip() or f"exit {done.returncode}"
        raise ValueError(f"push: git {args[0]} in {root}: {msg}")
    return done.stdout


def _split_z(data: bytes) -> list[str]:
    return [os.fsdecode(p) for p in data.split(b"\0") if p]


def _select(root: str, prefix: str) -> list[str]:
    subs = set()
    for rec in _split_z(_git(root, "ls-files", "-s", "-z")):
        meta, _, path = rec.partition("\t")
        if meta.startswith("160000 "):  # a submodule (gitlink)
            subs.add(path)
    out: list[str] = []
    seen: set[str] = set()
    for path in _split_z(
        _git(root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
    ):
        # Unmerged paths are listed once per stage; an untracked nested
        # repository is listed as "dir/" and is not part of this tree.
        if path in seen or path.endswith("/") or ".git" in path.split("/"):
            continue
        seen.add(path)
        full = os.path.join(root, *path.split("/"))
        if path in subs:
            if os.path.lexists(os.path.join(full, ".git")):  # checked out
                out += _select(full, f"{prefix}{path}/")
            continue
        try:
            mode = os.lstat(full).st_mode
        except FileNotFoundError:
            continue  # tracked but deleted from the work tree
        if stat.S_ISREG(mode) or stat.S_ISLNK(mode):
            out.append(prefix + path)
    return out


def git_selection(root: str | os.PathLike[str]) -> list[str]:
    """Select what a commit of the work tree at ``root`` could hold, like ``davit push``.

    Tracked files plus untracked files that are not ignored (``git ls-files -z
    --cached --others --exclude-standard``), recursing into checked-out
    submodules, never ``.git``. Only regular files and symlinks; tracked files
    deleted from the work tree are left out. Paths are relative to ``root``
    (which may be a subdirectory of a work tree), with ``/`` separators.

    Raises:
        ValueError: ``root`` is not in a git work tree, or git is missing.
    """
    top = os.fspath(root)
    if not os.path.isdir(top):
        raise NotADirectoryError(top)
    _git(top, "rev-parse", "--is-inside-work-tree")
    return _select(top, "")


def write_tree_tar(root: str | os.PathLike[str], paths: Sequence[str], fileobj: IO[bytes]) -> int:
    """Write ``paths`` (relative to ``root``) to ``fileobj`` as the tar pushTree takes.

    Regular files and symlinks under their relative names with permission bits
    and mtimes, no owners (the receiver creates directories). Returns the
    number of entries.
    """
    base = os.fspath(root)
    n = 0
    with tarfile.open(fileobj=fileobj, mode="w", format=tarfile.PAX_FORMAT) as tar:
        for rel in paths:
            if rel.startswith("/") or ".." in rel.split("/"):
                raise ValueError(f"push: {rel!r} is not a relative path inside the tree")
            full = os.path.join(base, *rel.split("/"))
            info = tar.gettarinfo(full, arcname=rel)
            if not (info.isreg() or info.issym()):
                raise ValueError(f"push: {rel} is not a regular file or symlink")
            info.uid = info.gid = 0
            info.uname = info.gname = ""
            if info.isreg():
                with open(full, "rb") as f:
                    tar.addfile(info, f)
            else:
                tar.addfile(info)
            n += 1
    return n
