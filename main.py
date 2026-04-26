#!/usr/bin/env python3
"""Set the module's `go` directive to TARGET, then move every dep to the highest
version whose own go.mod declares `go <= TARGET`. Works both directions
(downgrade if TARGET < current, upgrade-within-cap if TARGET > current).
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import re
import subprocess
import sys
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

__version__ = "0.0.2"
__author__ = "lczyk"

GoVersion = tuple[int, int, int]


def run(
    *args: str, check: bool = True, capture: bool = True
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        args,
        check=check,
        text=True,
        capture_output=capture,
        env={**os.environ, "GOTOOLCHAIN": "local"},
    )


def norm(v: str) -> GoVersion:
    """Normalize a Go version like '1.24' or '1.24.0' to a 3-tuple for comparison."""
    parts = re.split(r"[.\-]", v)
    nums: list[int] = []
    for p in parts:
        m = re.match(r"\d+", p)
        nums.append(int(m.group()) if m else 0)
    while len(nums) < 3:
        nums.append(0)
    return (nums[0], nums[1], nums[2])


def declared_go(mod: str, ver: str) -> str | None:
    """Return the `go` directive from <mod>@<ver>'s go.mod, or None on failure."""
    cp = run("go", "mod", "download", "-json", f"{mod}@{ver}", check=False)
    try:
        info = json.loads(cp.stdout or "{}")
    except json.JSONDecodeError:
        return None
    gomod = info.get("GoMod")
    if not gomod or not Path(gomod).is_file():
        return None
    for line in Path(gomod).read_text().splitlines():
        m = re.match(r"^go\s+(\S+)", line)
        if m:
            return m.group(1)
    return "1.0"


def list_versions(mod: str) -> list[str]:
    out = run("go", "list", "-m", "-versions", mod, check=False).stdout.strip()
    if not out:
        return []
    return list(reversed(out.split()[1:]))  # newest first; drop module path


def pick_version(mod: str, target: GoVersion) -> tuple[str, str] | None:
    for ver in list_versions(mod):
        gv = declared_go(mod, ver)
        if gv is None:
            continue
        if norm(gv) <= target:
            return ver, gv
    return None


def list_modules(direct_only: bool) -> list[tuple[str, str, str]]:
    """Return (path, version, goversion) for selected modules; fields may be ''."""
    fmt = (
        "{{if not .Main}}{{.Path}}\t{{.Version}}\t{{.GoVersion}}\t{{.Indirect}}{{end}}"
    )
    out = run(
        "go", "list", "-mod=mod", "-e", "-m", "-f", fmt, "all", check=False
    ).stdout
    rows: list[tuple[str, str, str]] = []
    for line in out.splitlines():
        if not line.strip():
            continue
        parts = (line.split("\t") + ["", "", "", ""])[:4]
        path, ver, gv, indirect = parts
        if direct_only and indirect == "true":
            continue
        rows.append((path, ver, gv))
    return rows


def pin_batch(mods: list[str], target: GoVersion, label: str, workers: int) -> set[str]:
    """Probe versions for each mod in parallel, then `go get` them serially.
    Returns the set of mods for which no compatible version exists.
    """
    if not mods:
        return set()
    with ThreadPoolExecutor(max_workers=max(1, min(workers, len(mods)))) as ex:
        picks = list(ex.map(lambda m: (m, pick_version(m, target)), mods))
    unresolvable: set[str] = set()
    for mod, pick in picks:
        if not pick:
            logging.warning("no compatible version for %s", mod)
            unresolvable.add(mod)
            continue
        ver, gv = pick
        logging.info("%s%s -> %s  (declares go %s)", label, mod, ver, gv)
        cp = run("go", "get", f"{mod}@{ver}", check=False)
        if cp.returncode != 0:
            err = (cp.stderr.strip().splitlines() or ["unknown"])[-1]
            logging.warning("go get failed for %s: %s", mod, err)
    return unresolvable


def backup_modfiles() -> dict[Path, bytes | None]:
    """Snapshot go.mod and go.sum (None if a file doesn't exist)."""
    return {
        p: (p.read_bytes() if p.exists() else None)
        for p in (Path("go.mod"), Path("go.sum"))
    }


def restore_modfiles(snapshot: dict[Path, bytes | None]) -> None:
    for path, data in snapshot.items():
        if data is None:
            path.unlink(missing_ok=True)
        else:
            path.write_bytes(data)


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    parser.add_argument(
        "--version", action="version", version=f"%(prog)s {__version__}"
    )
    parser.add_argument("dir", help="Path to module directory containing go.mod")
    parser.add_argument(
        "target", nargs="?", default="1.24", help="Target Go version (default: 1.24)"
    )
    parser.add_argument(
        "--rounds", type=int, default=5, help="Max indirect-fixup rounds (default: 5)"
    )
    parser.add_argument(
        "-j", "--jobs", type=int, default=8, help="Parallel version probes (default: 8)"
    )
    parser.add_argument(
        "--no-tidy", action="store_true", help="Skip the final `go mod tidy`"
    )
    return parser.parse_args(argv)


def _main(args: argparse.Namespace) -> None:
    target = norm(args.target)
    target_canonical = f"{target[0]}.{target[1]}.{target[2]}"

    run("go", "mod", "edit", f"-go={args.target}", "-toolchain=none")

    logging.info(
        "Pinning direct deps to highest version with go <= %s", target_canonical
    )
    direct = [p for p, _, _ in list_modules(direct_only=True)]
    unresolvable = pin_batch(direct, target, label="", workers=args.jobs)
    if unresolvable:
        raise RuntimeError(
            f"no version compatible with go {target_canonical} for direct dep(s): {', '.join(sorted(unresolvable))}"
        )

    skip: set[str] = set()
    for round_n in range(1, args.rounds + 1):
        offenders = [
            path
            for path, _, gv in list_modules(direct_only=False)
            if gv and norm(gv) > target and path not in skip
        ]
        if not offenders:
            break
        logging.info("Round %d: %d offending indirect(s)", round_n, len(offenders))
        skip |= pin_batch(
            offenders, target, label=f"[round {round_n}] ", workers=args.jobs
        )
    else:
        raise RuntimeError(
            f"hit max rounds ({args.rounds}); indirects still violate target"
        )

    if not args.no_tidy:
        run("go", "mod", "tidy", f"-go={target_canonical}", capture=False)
    logging.info("Done. go directive: %s", target_canonical)


COLORS = {
    "green": "\033[32m",
    "red": "\033[31m",
    "yellow": "\033[33m",
    "reset": "\033[0m",
}


def main() -> None:
    logging.basicConfig(
        level=logging.INFO,
        format="%(levelname)s: %(message)s",
        handlers=[logging.StreamHandler(sys.stderr)],
    )
    logging.addLevelName(logging.INFO, f"{COLORS['green']}INFO{COLORS['reset']}")
    logging.addLevelName(logging.WARNING, f"{COLORS['yellow']}WARNING{COLORS['reset']}")
    logging.addLevelName(logging.ERROR, f"{COLORS['red']}ERROR{COLORS['reset']}")

    args = parse_args()
    try:
        os.chdir(args.dir)
    except OSError as e:
        logging.error("chdir %s: %s", args.dir, e)
        sys.exit(1)
    if not Path("go.mod").is_file():
        logging.error("no go.mod in %s", args.dir)
        sys.exit(1)
    snapshot = backup_modfiles()
    try:
        _main(args)
    except KeyboardInterrupt:
        logging.error("Interrupted; restoring go.mod and go.sum")
        restore_modfiles(snapshot)
        os._exit(130)
    except (RuntimeError, subprocess.CalledProcessError) as e:
        msg = e if isinstance(e, RuntimeError) else f"command failed: {' '.join(e.cmd)}"
        logging.error("%s", msg)
        logging.error("Restoring go.mod and go.sum to original state")
        restore_modfiles(snapshot)
        sys.exit(1)


## ENTRYPOINT ##

if __name__ == "__main__":
    main()
