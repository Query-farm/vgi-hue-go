"""Run an exact public Git revision with a pinned Go toolchain and verified cache."""

from __future__ import annotations

import argparse
import fcntl
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import time
from pathlib import Path
from tempfile import TemporaryDirectory


def digest(path: Path) -> str:
    """Return a file's SHA256 without interpreting its contents."""
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def pinned_binary(repository: str, revision: str, package: str, toolchain: str) -> Path:
    """Fetch the exact revision, verify dependencies, and build once in a private cache."""
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
        raise ValueError("repository must be owner/name")
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise ValueError("revision must be a full lowercase Git SHA")
    if not re.fullmatch(r"\./[A-Za-z0-9_/-]+", package) or ".." in package:
        raise ValueError("package must be a relative Go package path beginning ./")
    if not re.fullmatch(r"go1\.\d+\.\d+", toolchain):
        raise ValueError("toolchain must be an exact Go version such as go1.27.4")
    if not shutil.which("git") or not shutil.which("go"):
        raise RuntimeError("Install Git and Go 1.21+; Go can provision the pinned toolchain.")
    env = os.environ.copy()
    env.update(GOTOOLCHAIN=toolchain, GOWORK="off", GOFLAGS="-mod=readonly", CGO_ENABLED="0", GIT_TERMINAL_PROMPT="0")
    platform = subprocess.check_output(["go", "env", "GOOS", "GOARCH"], env=env, text=True).split()
    root = Path(os.environ.get("XDG_CACHE_HOME", Path.home() / ".cache")) / "vgi-notebook-workers" / repository
    root.mkdir(parents=True, exist_ok=True)
    key = f"{revision}-{toolchain}-{'-'.join(platform)}-{hashlib.sha256(package.encode()).hexdigest()[:10]}"
    target = root / key
    expected = {
        "repository": repository,
        "revision": revision,
        "package": package,
        "toolchain": toolchain,
        "platform": platform,
        "cgo": False,
    }

    def valid() -> bool:
        try:
            marker = json.loads((target / "ready.json").read_text())
            return all(marker.get(k) == v for k, v in expected.items()) and marker["binary_sha256"] == digest(
                target / "worker"
            )
        except (OSError, KeyError, ValueError):
            return False

    def run(args: list[str], cwd: Path) -> None:
        subprocess.run(args, cwd=cwd, env=env, check=True, stdout=sys.stderr, stderr=sys.stderr)

    with (root / f"{key}.lock").open("a") as lock:
        deadline = time.monotonic() + 600
        while True:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if time.monotonic() >= deadline:
                    raise TimeoutError(f"Timed out waiting for Go build cache lock: {target}") from None
                time.sleep(0.1)
        if valid():
            return target / "worker"
        if target.exists():
            shutil.rmtree(target)
        with TemporaryDirectory(prefix=f"{revision[:12]}-build-", dir=root) as temporary:
            stage = Path(temporary)
            source = stage / "source"
            source.mkdir()
            print(f"Preparing pinned {repository}@{revision} with {toolchain}", file=sys.stderr)
            run(["git", "init", "-q"], source)
            run(["git", "remote", "add", "origin", f"https://github.com/{repository}.git"], source)
            run(["git", "fetch", "--depth", "1", "origin", revision], source)
            run(["git", "checkout", "--detach", "FETCH_HEAD"], source)
            head = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=source, text=True).strip()
            if head != revision:
                raise RuntimeError("Fetched Git HEAD does not match the requested revision")
            before = {name: digest(source / name) for name in ("go.mod", "go.sum")}
            run(["go", "mod", "download"], source)
            run(["go", "mod", "verify"], source)
            run(["go", "build", "-mod=readonly", "-trimpath", "-o", str(stage / "worker"), package], source)
            if before != {name: digest(source / name) for name in before}:
                raise RuntimeError("The build changed go.mod/go.sum; refusing to cache a modified dependency graph")
            marker = {**expected, "binary_sha256": digest(stage / "worker"), "module_hashes": before}
            (stage / "ready.json").write_text(json.dumps(marker, indent=2) + "\n")
            stage.rename(target)
    return target / "worker"


def main() -> None:
    """Resolve the pinned worker and replace this process, preserving stdio and signals."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("repository")
    parser.add_argument("revision")
    parser.add_argument("package")
    parser.add_argument("toolchain")
    parser.add_argument("worker_args", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    binary = pinned_binary(args.repository, args.revision, args.package, args.toolchain)
    os.execv(binary, [str(binary), *args.worker_args])


if __name__ == "__main__":
    main()
