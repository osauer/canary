#!/usr/bin/env python3

"""Copy one regular file from an immutable release tag to a safe local path."""

from __future__ import annotations

import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys
import tempfile


VERSION = re.compile(r"^v[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?$")
OBJECT_ID = re.compile(rb"^[0-9a-f]{40,64}$")


class MaterializeError(Exception):
    pass


def git(root: Path, *args: str) -> bytes:
    try:
        result = subprocess.run(
            ["git", "-C", str(root), *args],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
    except OSError as error:
        raise MaterializeError(f"cannot run git: {error}") from error
    if result.returncode != 0:
        detail = result.stderr.decode("utf-8", "backslashreplace").strip()
        raise MaterializeError(f"git {' '.join(args)} failed: {detail}")
    return result.stdout


def atomic_write(path: Path, data: bytes) -> None:
    parent = path.parent
    if not parent.is_dir() or parent.is_symlink():
        raise MaterializeError(f"output directory is missing or unsafe: {parent}")
    if path.is_symlink() or (path.exists() and not path.is_file()):
        raise MaterializeError(f"output is unsafe: {path}")

    descriptor, temp_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=str(parent))
    try:
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temp_name, path)
    finally:
        try:
            os.unlink(temp_name)
        except FileNotFoundError:
            pass


def main() -> int:
    if len(sys.argv) != 4:
        print(
            "usage: materialize-release-tag-file.py vX.Y.Z repo-relative-path output",
            file=sys.stderr,
        )
        return 2
    version, source_path, raw_output = sys.argv[1:]
    if VERSION.fullmatch(version) is None:
        raise MaterializeError(f"version must look like vX.Y.Z (got {version!r})")
    pure_path = PurePosixPath(source_path)
    if (
        pure_path.is_absolute()
        or not pure_path.parts
        or any(part in ("", ".", "..") for part in pure_path.parts)
    ):
        raise MaterializeError(f"source path is not a safe repository path: {source_path!r}")

    root = Path(__file__).resolve().parent.parent
    raw_commit = git(root, "rev-parse", "--verify", f"refs/tags/{version}^{{commit}}")
    commit = raw_commit.decode("ascii", "strict").strip()
    if re.fullmatch(r"[0-9a-f]{40,64}", commit) is None:
        raise MaterializeError("release tag did not resolve to an exact commit")

    raw_entry = git(root, "ls-tree", "-z", commit, "--", source_path)
    records = [record for record in raw_entry.split(b"\0") if record]
    if len(records) != 1:
        raise MaterializeError(f"tagged source is missing or ambiguous: {source_path}")
    try:
        metadata, raw_path = records[0].split(b"\t", 1)
        mode, object_type, object_id = metadata.split(b" ", 2)
        listed_path = raw_path.decode("utf-8", "strict")
    except (ValueError, UnicodeDecodeError) as error:
        raise MaterializeError("tagged source has malformed tree metadata") from error
    if (
        mode not in (b"100644", b"100755")
        or object_type != b"blob"
        or OBJECT_ID.fullmatch(object_id) is None
        or listed_path != source_path
    ):
        raise MaterializeError(f"tagged source is not a regular file: {source_path}")

    data = git(root, "cat-file", "blob", object_id.decode("ascii"))
    output = Path(raw_output)
    atomic_write(output, data)
    print(
        f"materialize-release-tag-file: OK version={version} path={source_path}",
        file=sys.stderr,
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (MaterializeError, UnicodeError) as error:
        print(f"materialize-release-tag-file: {error}", file=sys.stderr)
        raise SystemExit(1)
