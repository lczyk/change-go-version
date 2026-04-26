from pathlib import Path

import pytest

__project_root__ = Path(__file__).resolve().parent
VERSION_FILE = __project_root__ / "VERSION"


def test_version_matches_VERSION_file() -> None:
    from main import __version__

    file_version = VERSION_FILE.read_text().strip()
    assert __version__ == file_version, (
        f"Version mismatch: __version__={__version__!r} in main.py vs "
        f"{file_version!r} in VERSION. VERSION file is source of truth."
    )


def test_norm() -> None:
    from main import norm

    assert norm("1.22") == (1, 22, 0)
    assert norm("1.22.3") == (1, 22, 3)
    assert norm("1.22-rc1") == (1, 22, 0)  # 'rc1' has no leading digit
    assert norm("v1.22.3") == (0, 22, 3)  # 'v1' has no leading digit
    assert norm("") == (0, 0, 0)
    assert norm("1") == (1, 0, 0)


def test_minor_of() -> None:
    from main import minor_of

    assert minor_of("1.22") == 22
    assert minor_of("1.22.3") == 22
    assert minor_of("v1.22.3") == 22
    assert minor_of("") == 0


def test_read_local_go_directive(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from main import read_local_go_directive

    monkeypatch.chdir(tmp_path)
    (tmp_path / "go.mod").write_text("module x\n\ngo 1.22\n")
    assert read_local_go_directive() == "1.22"


def test_run_check(monkeypatch: pytest.MonkeyPatch) -> None:
    from main import run_check

    assert run_check("true") is True
    assert run_check("false") is False


def test_parse_args_to() -> None:
    from main import parse_args

    args = parse_args(["--to", "1.22", "--dir", "/tmp/x"])
    assert args.to == "1.22"
    assert args.auto is None
    assert args.dir == "/tmp/x"


def test_parse_args_auto() -> None:
    from main import parse_args

    args = parse_args(["--auto", "go test ./...", "--rounds", "3"])
    assert args.to is None
    assert args.auto == "go test ./..."
    assert args.dir == "."
    assert args.rounds == 3


def test_parse_args_requires_one_mode() -> None:
    from main import parse_args

    with pytest.raises(SystemExit):
        parse_args([])


def test_parse_args_mutex() -> None:
    from main import parse_args

    with pytest.raises(SystemExit):
        parse_args(["--to", "1.22", "--auto", "go test"])
