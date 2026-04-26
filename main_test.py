from pathlib import Path

__project_root__ = Path(__file__).resolve().parent
VERSION_FILE = __project_root__ / "VERSION"


def test_version_matches_VERSION_file() -> None:
    from main import __version__

    file_version = VERSION_FILE.read_text().strip()
    assert __version__ == file_version, (
        f"Version mismatch: __version__={__version__!r} in main.py vs "
        f"{file_version!r} in VERSION. VERSION file is source of truth."
    )
