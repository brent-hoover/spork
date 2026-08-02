from pathlib import Path

from avspec.analysis import analyze
from avspec.findings import ordered


def test_example_spec_passes() -> None:
    report = analyze(Path("example"))

    assert report.pass_ is True
    assert report.counts == {"error": 0, "todo": 0, "warn": 0}


def test_empty_suite_starts_with_modules_not_stack_or_acceptance(tmp_path: Path) -> None:
    spec_dir = tmp_path
    (spec_dir / "avspec.yaml").write_text(
        """
avspec: "0.2"
metadata:
  name: suite
  status: draft
mode: greenfield
artifacts:
  constitution: constitution.md
  requirements: requirements.md
  design: design.md
  tasks: tasks.md
requirements: []
tasks: []
"""
    )

    report = analyze(spec_dir)
    queue = [finding.code for finding in ordered(report.findings) if finding.severity == "todo"]

    assert queue[:3] == ["NO_COMPONENTS", "NO_CONSTITUTION", "NO_REQUIREMENTS"]
    assert "REQ_NO_AC" not in queue
    assert "AC_NO_TEST" not in queue
    assert "NO_STACK" not in queue


def test_modules_queue_boundaries_and_contracts_before_requirements(tmp_path: Path) -> None:
    spec_dir = tmp_path
    (spec_dir / "avspec.yaml").write_text(
        """
avspec: "0.2"
metadata:
  name: suite
  status: draft
mode: greenfield
artifacts:
  constitution: constitution.md
  requirements: requirements.md
  design: design.md
  tasks: tasks.md
components:
  - id: CMP-001
    responsibility: Spec Studio - author specs
requirements: []
tasks: []
"""
    )

    report = analyze(spec_dir)
    queue = [finding.code for finding in ordered(report.findings) if finding.severity == "todo"]

    assert queue[:3] == ["NO_BOUNDARIES", "NO_CONTRACTS", "NO_CONSTITUTION"]
    assert queue.index("NO_CONTRACTS") < queue.index("NO_REQUIREMENTS")


def test_module_stack_is_supported_as_later_implementation_detail(tmp_path: Path) -> None:
    spec_dir = tmp_path
    (spec_dir / "verification").mkdir()
    (spec_dir / "verification" / "architecture-rules.yaml").write_text(
        """
layers:
  application: [CMP-001]
allow: []
"""
    )
    (spec_dir / "contracts").mkdir()
    (spec_dir / "contracts" / "api.yaml").write_text(
        "openapi: 3.1.0\ninfo: {title: x, version: '1'}\npaths: {}\n"
    )
    (spec_dir / "tests").mkdir()
    (spec_dir / "tests" / "test_author_specs.py").write_text(
        "def test_persists_modules():\n    pass\n"
    )
    (spec_dir / "constitution.md").write_text("PR-001\n")
    (spec_dir / "requirements.md").write_text("REQ-001\n")
    (spec_dir / "design.md").write_text("CMP-001\n")
    (spec_dir / "tasks.md").write_text("TSK-001\n")
    (spec_dir / "avspec.yaml").write_text(
        """
avspec: "0.2"
metadata:
  name: suite
  status: draft
mode: greenfield
artifacts:
  constitution: constitution.md
  requirements: requirements.md
  design: design.md
  tasks: tasks.md
principles:
  - id: PR-001
    statement: Boundaries are explicit.
components:
  - id: CMP-001
    responsibility: Spec Studio - author specs
    interfaces:
      - name: SpecStudioAPI
        contract: CTR-001
contracts:
  - id: CTR-001
    type: openapi
    path: contracts/api.yaml
requirements:
  - id: REQ-001
    title: Author specs
    acceptance:
      - id: AC-001
        ears: When modules are edited, the suite shall persist them.
        test: tests/test_author_specs.py::test_persists_modules
tasks:
  - id: TSK-001
    title: Implement authoring
    satisfies: [AC-001]
    touches: [CMP-001]
"""
    )

    report = analyze(spec_dir)
    queue = [finding.code for finding in ordered(report.findings) if finding.severity == "todo"]

    assert queue == ["CMP_NO_STACK"]
