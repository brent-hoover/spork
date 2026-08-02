from typer.testing import CliRunner

from avspec import __version__
from avspec.cli import app


def test_version_command() -> None:
    result = CliRunner().invoke(app, ["version"])

    assert result.exit_code == 0
    assert result.stdout.strip() == __version__
