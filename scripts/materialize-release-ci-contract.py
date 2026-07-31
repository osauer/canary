#!/usr/bin/env python3

"""Materialize and verify the CI contract owned by an immutable release tag."""

from __future__ import annotations

import json
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys
import tempfile


VERSION = re.compile(r"^v[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?$")
OBJECT_ID = re.compile(rb"^[0-9a-f]{40,64}$")
CONTRACT_PATH = "scripts/release-ci-contract.json"
LEGACY_PATH = "scripts/release-ci-legacy-contracts.json"
WORKFLOW_ROOT = ".github/workflows/"


class ContractError(Exception):
    pass


def git(root: Path, *args: str, input_bytes: bytes | None = None) -> bytes:
    command = ["git", "-C", str(root), *args]
    try:
        result = subprocess.run(
            command,
            input=input_bytes,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
    except OSError as error:
        raise ContractError(f"cannot run git: {error}") from error
    if result.returncode != 0:
        detail = result.stderr.decode("utf-8", "backslashreplace").strip()
        raise ContractError(f"git {' '.join(args)} failed: {detail}")
    return result.stdout


def resolve_release_commit(root: Path, version: str) -> str:
    raw = git(root, "rev-parse", "--verify", f"refs/tags/{version}^{{commit}}")
    commit = raw.decode("ascii", "strict").strip()
    if re.fullmatch(r"[0-9a-f]{40,64}", commit) is None:
        raise ContractError("release tag did not resolve to an exact commit")
    return commit


def tree_entries(root: Path, commit: str, prefix: str) -> dict[str, tuple[str, bytes]]:
    raw = git(root, "ls-tree", "-rz", commit, "--", prefix)
    entries: dict[str, tuple[str, bytes]] = {}
    for record in raw.split(b"\0"):
        if not record:
            continue
        try:
            metadata, raw_path = record.split(b"\t", 1)
            mode, object_type, object_id = metadata.split(b" ", 2)
            path = raw_path.decode("utf-8", "strict")
        except (ValueError, UnicodeDecodeError) as error:
            raise ContractError("release tree contains a malformed entry") from error
        if mode not in (b"100644", b"100755") or object_type != b"blob":
            raise ContractError(f"release tree path is not a regular file: {path!r}")
        if OBJECT_ID.fullmatch(object_id) is None:
            raise ContractError(f"release tree path has an invalid object ID: {path!r}")
        if path in entries:
            raise ContractError(f"release tree contains duplicate path: {path!r}")
        entries[path] = (object_id.decode("ascii"), git(root, "cat-file", "blob", object_id.decode("ascii")))
    return entries


def strict_json(data: bytes, label: str) -> object:
    def unique_object(pairs: list[tuple[str, object]]) -> dict[str, object]:
        result: dict[str, object] = {}
        for key, value in pairs:
            if key in result:
                raise ContractError(f"{label} contains duplicate key {key!r}")
            result[key] = value
        return result

    try:
        return json.loads(data.decode("utf-8"), object_pairs_hook=unique_object)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise ContractError(f"{label} is not strict UTF-8 JSON: {error}") from error


def select_contract(root: Path, commit: str, entries: dict[str, tuple[str, bytes]]) -> bytes:
    tagged = entries.get(CONTRACT_PATH)
    if tagged is not None:
        strict_json(tagged[1], "tagged CI contract")
        return tagged[1]

    legacy_path = root / LEGACY_PATH
    if legacy_path.is_symlink() or not legacy_path.is_file():
        raise ContractError("legacy CI contracts are missing or unsafe")
    try:
        legacy_raw = legacy_path.read_bytes()
    except OSError as error:
        raise ContractError(f"cannot read legacy CI contracts: {error}") from error
    document = strict_json(legacy_raw, "legacy CI contracts")
    if type(document) is not dict:
        raise ContractError("legacy CI contracts must be an object")
    if commit not in document:
        raise ContractError(
            f"release {commit} predates tag-owned CI contracts and has no pinned legacy contract"
        )
    contract = document[commit]
    if type(contract) is not dict:
        raise ContractError(f"legacy CI contract for {commit} must be an object")
    return (json.dumps(contract, indent=2, ensure_ascii=False) + "\n").encode("utf-8")


def safe_output(path: Path) -> None:
    parent = path.parent.resolve()
    if not parent.is_dir() or path.parent.is_symlink():
        raise ContractError(f"output directory is missing or unsafe: {path.parent}")
    if path.is_symlink() or (path.exists() and not path.is_file()):
        raise ContractError(f"output is unsafe: {path}")


def main() -> int:
    if len(sys.argv) != 3:
        print(
            "usage: materialize-release-ci-contract.py vX.Y.Z output",
            file=sys.stderr,
        )
        return 2
    version = sys.argv[1]
    if VERSION.fullmatch(version) is None:
        raise ContractError(f"version must look like vX.Y.Z (got {version!r})")

    root = Path(__file__).resolve().parent.parent
    raw_output = Path(sys.argv[2])
    if raw_output.is_symlink():
        raise ContractError(f"output is unsafe: {raw_output}")
    output = raw_output.resolve(strict=False)
    safe_output(output)
    commit = resolve_release_commit(root, version)
    entries = tree_entries(root, commit, ".github/workflows")
    entries.update(tree_entries(root, commit, CONTRACT_PATH))
    contract = select_contract(root, commit, entries)

    with tempfile.TemporaryDirectory(prefix="canary-release-ci-contract-") as raw_temp:
        temp_root = Path(raw_temp)
        workflow_dir = temp_root / WORKFLOW_ROOT
        workflow_dir.mkdir(parents=True)
        for path, (_object_id, data) in entries.items():
            if not path.startswith(WORKFLOW_ROOT):
                continue
            relative = PurePosixPath(path).relative_to(PurePosixPath(WORKFLOW_ROOT))
            if len(relative.parts) != 1 or relative.suffix not in (".yml", ".yaml"):
                continue
            (workflow_dir / relative.name).write_bytes(data)
        contract_file = temp_root / CONTRACT_PATH
        contract_file.parent.mkdir(parents=True)
        contract_file.write_bytes(contract)

        checker = root / "scripts/check-release-ci-contract.sh"
        result = subprocess.run(
            [str(checker), "--authority-only", str(temp_root)],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
        if result.returncode != 0:
            detail = result.stderr.decode("utf-8", "backslashreplace").strip()
            raise ContractError(
                f"tag-era workflow inventory does not match its CI contract: {detail}"
            )

    output.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temp_name = tempfile.mkstemp(
        prefix=f".{output.name}.", dir=str(output.parent)
    )
    try:
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(contract)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temp_name, output)
    finally:
        try:
            os.unlink(temp_name)
        except FileNotFoundError:
            pass
    print(
        f"materialize-release-ci-contract: OK version={version} sha={commit}",
        file=sys.stderr,
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except ContractError as error:
        print(f"materialize-release-ci-contract: {error}", file=sys.stderr)
        raise SystemExit(1)
