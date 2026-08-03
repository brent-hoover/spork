import copy
from pathlib import Path

from avspec.analysis import Spec
from avspec.findings import Severity
from avspec.rules import completeness
from tests.conftest import MINIMAL, make_spec


def codes(findings) -> list[str]:
    return sorted(f.code for f in findings)


def base() -> dict:
    return copy.deepcopy(MINIMAL)


def test_empty_spec_fires_all_absence_todos(tmp_path: Path) -> None:
    spec = make_spec(tmp_path, base())
    found = sorted(
        codes(completeness.stack_declared(spec))
        + codes(completeness.constitution_present(spec))
        + codes(completeness.requirements_present(spec))
        + codes(completeness.modules_present(spec))
        + codes(completeness.data_present(spec))
        + codes(completeness.apps_present(spec))
    )
    assert found == ["NO_CONSTITUTION", "NO_DATA", "NO_MODULES", "NO_REQUIREMENTS", "NO_STACK"]


def test_every_todo_has_a_question(tmp_path: Path) -> None:
    spec = make_spec(tmp_path, base())
    for run in (
        completeness.stack_declared,
        completeness.constitution_present,
        completeness.requirements_present,
        completeness.modules_present,
        completeness.data_present,
        completeness.apps_present,
    ):
        for finding in run(spec):
            assert finding.question, f"{finding.code} has no question"


def test_req_no_ac_and_ac_no_test(tmp_path: Path) -> None:
    data = base()
    data["requirements"] = [
        {"id": "REQ-empty", "title": "no acs"},
        {"id": "REQ-untested", "title": "x", "acceptance": [{"id": "AC-u", "statement": "s"}]},
    ]
    spec = make_spec(tmp_path, data)
    assert codes(completeness.requirements_present(spec)) == ["AC_NO_TEST", "REQ_NO_AC"]


def test_test_file_missing_and_present(tmp_path: Path) -> None:
    data = base()
    data["requirements"] = [
        {
            "id": "REQ-x",
            "title": "x",
            "acceptance": [{"id": "AC-x", "statement": "s", "test": "verification/x.feature#ok"}],
        }
    ]
    spec = make_spec(tmp_path, data)
    assert codes(completeness.test_files_exist(spec)) == ["TEST_FILE_MISSING"]
    (tmp_path / "verification").mkdir(parents=True)
    (tmp_path / "verification" / "x.feature").write_text(
        "Feature: x\n\n  Scenario: ok\n", encoding="utf-8"
    )
    assert codes(completeness.test_files_exist(spec)) == []


def _spec_with_test(tmp_path: Path, test_ref: str) -> Spec:
    data = base()
    data["requirements"] = [
        {
            "id": "REQ-x",
            "title": "x",
            "acceptance": [{"id": "AC-x", "statement": "s", "test": test_ref}],
        }
    ]
    return make_spec(tmp_path, data)


def test_test_ref_without_scenario_is_invalid(tmp_path: Path) -> None:
    spec = _spec_with_test(tmp_path, "verification/x.feature")
    assert codes(completeness.test_files_exist(spec)) == ["TEST_REF_INVALID"]


def test_test_ref_with_blank_scenario_is_invalid(tmp_path: Path) -> None:
    spec = _spec_with_test(tmp_path, "verification/x.feature#   ")
    assert codes(completeness.test_files_exist(spec)) == ["TEST_REF_INVALID"]


def test_test_ref_non_feature_path_is_invalid(tmp_path: Path) -> None:
    spec = _spec_with_test(tmp_path, "verification/x.txt#ok")
    assert codes(completeness.test_files_exist(spec)) == ["TEST_REF_INVALID"]


def test_test_scenario_missing(tmp_path: Path) -> None:
    spec = _spec_with_test(tmp_path, "verification/x.feature#nope")
    (tmp_path / "verification").mkdir(parents=True)
    (tmp_path / "verification" / "x.feature").write_text(
        "Feature: x\n\n  Scenario: ok\n", encoding="utf-8"
    )
    assert codes(completeness.test_files_exist(spec)) == ["TEST_SCENARIO_MISSING"]


def test_test_scenario_outline_matches(tmp_path: Path) -> None:
    spec = _spec_with_test(tmp_path, "verification/x.feature#ok")
    (tmp_path / "verification").mkdir(parents=True)
    (tmp_path / "verification" / "x.feature").write_text(
        "Feature: x\n\n  Scenario Outline: ok\n", encoding="utf-8"
    )
    assert codes(completeness.test_files_exist(spec)) == []


def test_test_scenario_example_alias_matches(tmp_path: Path) -> None:
    spec = _spec_with_test(tmp_path, "verification/x.feature#ok")
    (tmp_path / "verification").mkdir(parents=True)
    (tmp_path / "verification" / "x.feature").write_text(
        "Feature: x\n\n  Example: ok\n", encoding="utf-8"
    )
    assert codes(completeness.test_files_exist(spec)) == []


def test_test_scenario_template_alias_matches(tmp_path: Path) -> None:
    spec = _spec_with_test(tmp_path, "verification/x.feature#ok")
    (tmp_path / "verification").mkdir(parents=True)
    (tmp_path / "verification" / "x.feature").write_text(
        "Feature: x\n\n  Scenario Template: ok\n", encoding="utf-8"
    )
    assert codes(completeness.test_files_exist(spec)) == []


def test_test_scenario_localized_matches(tmp_path: Path) -> None:
    spec = _spec_with_test(tmp_path, "verification/x.feature#ok")
    (tmp_path / "verification").mkdir(parents=True)
    (tmp_path / "verification" / "x.feature").write_text(
        "# language: de\nFunktionalität: x\n\n  Szenario: ok\n", encoding="utf-8"
    )
    assert codes(completeness.test_files_exist(spec)) == []


def test_test_scenario_in_docstring_does_not_count(tmp_path: Path) -> None:
    spec = _spec_with_test(tmp_path, "verification/x.feature#phantom")
    (tmp_path / "verification").mkdir(parents=True)
    (tmp_path / "verification" / "x.feature").write_text(
        "Feature: x\n\n"
        "  Scenario: real\n"
        "    Given a thing\n"
        '      """\n'
        "      Scenario: phantom\n"
        '      """\n',
        encoding="utf-8",
    )
    assert codes(completeness.test_files_exist(spec)) == ["TEST_SCENARIO_MISSING"]


def test_test_scenario_unparseable_feature_file(tmp_path: Path) -> None:
    spec = _spec_with_test(tmp_path, "verification/x.feature#ok")
    (tmp_path / "verification").mkdir(parents=True)
    (tmp_path / "verification" / "x.feature").write_text(
        "this is not gherkin @@@ :::\n", encoding="utf-8"
    )
    found = list(completeness.test_files_exist(spec))
    assert codes(found) == ["TEST_SCENARIO_MISSING"]
    assert "could not be parsed as Gherkin" in found[0].message
    assert "Parser errors:" not in found[0].message
    assert "(1:1)" in found[0].message


def test_test_ref_absolute_path_escapes(tmp_path: Path) -> None:
    spec = _spec_with_test(tmp_path, "/etc/passwd#x")
    found = list(completeness.test_files_exist(spec))
    assert codes(found) == ["PATH_ESCAPE"]
    assert found[0].severity is Severity.ERROR


def test_test_ref_traversal_escapes(tmp_path: Path) -> None:
    spec = _spec_with_test(tmp_path, "../outside.feature#x")
    found = list(completeness.test_files_exist(spec))
    assert codes(found) == ["PATH_ESCAPE"]
    assert found[0].severity is Severity.ERROR


def test_contract_path_escapes(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [
        {
            "id": "MOD-api",
            "name": "api",
            "responsibility": "r",
            "boundaries": {"may_import": []},
            "contracts": [{"id": "CTR-x", "type": "openapi", "path": "../outside.yaml"}],
        }
    ]
    spec = make_spec(tmp_path, data)
    found = list(completeness.contract_files(spec))
    assert codes(found) == ["PATH_ESCAPE"]
    assert found[0].severity is Severity.ERROR


def test_module_todos(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [{"id": "MOD-bare", "name": "bare"}]
    spec = make_spec(tmp_path, data)
    assert codes(completeness.modules_present(spec)) == [
        "MOD_NO_BOUNDARIES",
        "MOD_NO_RESPONSIBILITY",
    ]


def test_empty_may_import_is_a_valid_answer(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [
        {"id": "MOD-leaf", "name": "leaf", "responsibility": "r", "boundaries": {"may_import": []}}
    ]
    assert codes(completeness.modules_present(make_spec(tmp_path, data))) == []


def test_explicit_empty_entities_is_not_no_data(tmp_path: Path) -> None:
    data = base()
    data["data"] = {"entities": []}
    assert codes(completeness.data_present(make_spec(tmp_path, data))) == []


def test_entity_no_fields(tmp_path: Path) -> None:
    data = base()
    data["data"] = {"entities": [{"id": "ENT-a", "name": "A"}]}
    assert codes(completeness.data_present(make_spec(tmp_path, data))) == [
        "ENT_NO_FIELDS",
        "ENT_UNOWNED",
    ]


def test_entity_with_fields_is_clean(tmp_path: Path) -> None:
    data = base()
    data["data"] = {
        "entities": [{"id": "ENT-a", "name": "A", "fields": [{"name": "x", "type": "string"}]}]
    }
    data["modules"] = [{"id": "MOD-a", "name": "a", "owns": ["ENT-a"]}]
    assert codes(completeness.data_present(make_spec(tmp_path, data))) == []


def test_entity_unowned(tmp_path: Path) -> None:
    data = base()
    data["data"] = {
        "entities": [{"id": "ENT-a", "name": "A", "fields": [{"name": "x", "type": "string"}]}]
    }
    assert codes(completeness.data_present(make_spec(tmp_path, data))) == ["ENT_UNOWNED"]


def test_no_apps_without_modules_is_quiet(tmp_path: Path) -> None:
    assert codes(completeness.apps_present(make_spec(tmp_path, base()))) == []


def test_no_apps_when_modules_exist(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [{"id": "MOD-a", "name": "a"}]
    assert codes(completeness.apps_present(make_spec(tmp_path, data))) == ["NO_APPS"]


def test_mod_no_app(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [{"id": "MOD-a", "name": "a"}, {"id": "MOD-b", "name": "b"}]
    data["apps"] = [{"id": "APP-a", "name": "a", "modules": ["MOD-a"]}]
    assert codes(completeness.apps_present(make_spec(tmp_path, data))) == ["MOD_NO_APP"]


def test_all_modules_assigned_is_clean(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [{"id": "MOD-a", "name": "a"}]
    data["apps"] = [{"id": "APP-a", "name": "a", "modules": ["MOD-a"]}]
    assert codes(completeness.apps_present(make_spec(tmp_path, data))) == []


def test_contract_file_missing_unparseable_and_ok(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [
        {
            "id": "MOD-api",
            "name": "api",
            "responsibility": "r",
            "boundaries": {"may_import": []},
            "contracts": [{"id": "CTR-x", "type": "openapi", "path": "contracts/x.yaml"}],
        }
    ]
    spec = make_spec(tmp_path, data)
    assert codes(completeness.contract_files(spec)) == ["CONTRACT_FILE_MISSING"]
    contract = tmp_path / "contracts" / "x.yaml"
    contract.parent.mkdir(parents=True)
    contract.write_text("a: [unclosed", encoding="utf-8")
    found = list(completeness.contract_files(spec))
    assert codes(found) == ["CONTRACT_UNPARSEABLE"]
    assert found[0].severity is Severity.ERROR
    contract.write_text(
        "openapi: 3.0.3\npaths:\n  /x:\n    get:\n      operationId: getX\n",
        encoding="utf-8",
    )
    assert codes(completeness.contract_files(spec)) == []


def test_contract_empty_openapi_paths(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [
        {
            "id": "MOD-api",
            "name": "api",
            "responsibility": "r",
            "boundaries": {"may_import": []},
            "contracts": [{"id": "CTR-x", "type": "openapi", "path": "contracts/x.yaml"}],
        }
    ]
    spec = make_spec(tmp_path, data)
    contract = tmp_path / "contracts" / "x.yaml"
    contract.parent.mkdir(parents=True)
    contract.write_text("openapi: 3.0.3\npaths: {}\n", encoding="utf-8")
    found = list(completeness.contract_files(spec))
    assert codes(found) == ["CONTRACT_EMPTY"]
    assert found[0].ref == "CTR-x"
    assert found[0].question


def test_contract_with_real_paths_is_clean(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [
        {
            "id": "MOD-api",
            "name": "api",
            "responsibility": "r",
            "boundaries": {"may_import": []},
            "contracts": [{"id": "CTR-x", "type": "openapi", "path": "contracts/x.yaml"}],
        }
    ]
    spec = make_spec(tmp_path, data)
    contract = tmp_path / "contracts" / "x.yaml"
    contract.parent.mkdir(parents=True)
    contract.write_text(
        "openapi: 3.0.3\npaths:\n  /x:\n    get:\n      operationId: getX\n",
        encoding="utf-8",
    )
    assert codes(completeness.contract_files(spec)) == []


def test_contract_empty_openapi_missing_paths_key(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [
        {
            "id": "MOD-api",
            "name": "api",
            "responsibility": "r",
            "boundaries": {"may_import": []},
            "contracts": [{"id": "CTR-x", "type": "openapi", "path": "contracts/x.yaml"}],
        }
    ]
    spec = make_spec(tmp_path, data)
    contract = tmp_path / "contracts" / "x.yaml"
    contract.parent.mkdir(parents=True)
    contract.write_text("openapi: 3.0.3\n", encoding="utf-8")
    assert codes(completeness.contract_files(spec)) == ["CONTRACT_EMPTY"]


def test_contract_empty_asyncapi_channels(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [
        {
            "id": "MOD-events",
            "name": "events",
            "responsibility": "r",
            "boundaries": {"may_import": []},
            "contracts": [{"id": "CTR-y", "type": "asyncapi", "path": "contracts/y.yaml"}],
        }
    ]
    spec = make_spec(tmp_path, data)
    contract = tmp_path / "contracts" / "y.yaml"
    contract.parent.mkdir(parents=True)
    contract.write_text("asyncapi: 2.6.0\nchannels: {}\n", encoding="utf-8")
    found = list(completeness.contract_files(spec))
    assert codes(found) == ["CONTRACT_EMPTY"]
    assert found[0].ref == "CTR-y"


def test_contract_empty_jsonschema_is_exempt(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [
        {
            "id": "MOD-x",
            "name": "x",
            "responsibility": "r",
            "boundaries": {"may_import": []},
            "contracts": [{"id": "CTR-z", "type": "jsonschema", "path": "contracts/z.yaml"}],
        }
    ]
    spec = make_spec(tmp_path, data)
    contract = tmp_path / "contracts" / "z.yaml"
    contract.parent.mkdir(parents=True)
    contract.write_text("{}\n", encoding="utf-8")
    assert codes(completeness.contract_files(spec)) == []


def _module_with_action(invokes: str) -> dict:
    return {
        "id": "MOD-api",
        "name": "api",
        "responsibility": "r",
        "boundaries": {"may_import": []},
        "contracts": [{"id": "CTR-x", "type": "openapi", "path": "contracts/x.yaml"}],
        "ui": {
            "kind": "web",
            "entry": "VIEW-a",
            "views": [
                {
                    "id": "VIEW-a",
                    "name": "a",
                    "route": "/",
                    "actions": ["ACT-a"],
                }
            ],
            "actions": [{"id": "ACT-a", "name": "a", "invokes": invokes}],
        },
    }


def test_action_unresolved_operation(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [_module_with_action("CTR-x#nope")]
    spec = make_spec(tmp_path, data)
    contract = tmp_path / "contracts" / "x.yaml"
    contract.parent.mkdir(parents=True)
    contract.write_text(
        "openapi: 3.0.3\npaths:\n  /x:\n    get:\n      operationId: getX\n",
        encoding="utf-8",
    )
    found = list(completeness.action_operations_resolve(spec))
    assert codes(found) == ["ACTION_UNRESOLVED"]
    assert found[0].ref == "ACT-a"
    assert found[0].question


def test_action_resolved_operation_is_clean(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [_module_with_action("CTR-x#getX")]
    spec = make_spec(tmp_path, data)
    contract = tmp_path / "contracts" / "x.yaml"
    contract.parent.mkdir(parents=True)
    contract.write_text(
        "openapi: 3.0.3\npaths:\n  /x:\n    get:\n      operationId: getX\n",
        encoding="utf-8",
    )
    assert codes(completeness.action_operations_resolve(spec)) == []


def test_action_unresolved_quiet_when_contract_file_missing(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [_module_with_action("CTR-x#getX")]
    spec = make_spec(tmp_path, data)
    assert codes(completeness.action_operations_resolve(spec)) == []
    assert codes(completeness.contract_files(spec)) == ["CONTRACT_FILE_MISSING"]


def test_contract_empty_when_document_is_top_level_list(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [
        {
            "id": "MOD-api",
            "name": "api",
            "responsibility": "r",
            "boundaries": {"may_import": []},
            "contracts": [{"id": "CTR-x", "type": "openapi", "path": "contracts/x.yaml"}],
        }
    ]
    spec = make_spec(tmp_path, data)
    contract = tmp_path / "contracts" / "x.yaml"
    contract.parent.mkdir(parents=True)
    contract.write_text("- just\n- a\n- list\n", encoding="utf-8")
    found = list(completeness.contract_files(spec))
    assert codes(found) == ["CONTRACT_EMPTY"]
    assert found[0].ref == "CTR-x"


def test_action_unresolved_quiet_when_contract_unparseable(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [_module_with_action("CTR-x#getX")]
    spec = make_spec(tmp_path, data)
    contract = tmp_path / "contracts" / "x.yaml"
    contract.parent.mkdir(parents=True)
    contract.write_text("a: [unclosed", encoding="utf-8")
    assert codes(completeness.action_operations_resolve(spec)) == []
    assert codes(completeness.contract_files(spec)) == ["CONTRACT_UNPARSEABLE"]


def test_action_unresolved_quiet_when_path_item_is_ref(tmp_path: Path) -> None:
    # A $ref path item hides whatever operations live behind it; _operation_ids
    # can't see through it, so enumeration is partial. Firing ACTION_UNRESOLVED
    # against a partial enumeration would false-positive on real operations the
    # rule simply couldn't see — so the rule stays quiet for the whole contract.
    data = base()
    data["modules"] = [_module_with_action("CTR-x#getY")]
    spec = make_spec(tmp_path, data)
    contract = tmp_path / "contracts" / "x.yaml"
    contract.parent.mkdir(parents=True)
    contract.write_text(
        "openapi: 3.0.3\n"
        "paths:\n"
        "  /x:\n"
        "    get:\n"
        "      operationId: getX\n"
        "  /y:\n"
        "    $ref: '#/components/pathItems/Y'\n",
        encoding="utf-8",
    )
    assert codes(completeness.action_operations_resolve(spec)) == []


def test_action_unresolved_missing_fragment_fires_even_with_ref_path_item(
    tmp_path: Path,
) -> None:
    # The missing-fragment check doesn't depend on operation enumeration, so it
    # must fire before the $ref-path-item guard kicks in and silences the rule.
    data = base()
    data["modules"] = [_module_with_action("CTR-x")]
    spec = make_spec(tmp_path, data)
    contract = tmp_path / "contracts" / "x.yaml"
    contract.parent.mkdir(parents=True)
    contract.write_text(
        "openapi: 3.0.3\n"
        "paths:\n"
        "  /x:\n"
        "    get:\n"
        "      operationId: getX\n"
        "  /y:\n"
        "    $ref: '#/components/pathItems/Y'\n",
        encoding="utf-8",
    )
    found = list(completeness.action_operations_resolve(spec))
    assert codes(found) == ["ACTION_UNRESOLVED"]
    assert found[0].ref == "ACT-a"

    # But a fragmented invokes against the same $ref-bearing contract still
    # stays quiet, since operation enumeration is partial in that case.
    data_fragmented = base()
    data_fragmented["modules"] = [_module_with_action("CTR-x#getX")]
    spec_fragmented = make_spec(tmp_path, data_fragmented)
    assert codes(completeness.action_operations_resolve(spec_fragmented)) == []


def test_action_unresolved_empty_operation_name_is_not_silent(tmp_path: Path) -> None:
    # Pins current behavior for `invokes: CTR-x#` (empty operation name after
    # the hash): it is treated as an unresolved operation reference rather than
    # ignored outright.
    data = base()
    data["modules"] = [_module_with_action("CTR-x#")]
    spec = make_spec(tmp_path, data)
    contract = tmp_path / "contracts" / "x.yaml"
    contract.parent.mkdir(parents=True)
    contract.write_text(
        "openapi: 3.0.3\npaths:\n  /x:\n    get:\n      operationId: getX\n",
        encoding="utf-8",
    )
    found = list(completeness.action_operations_resolve(spec))
    assert codes(found) == ["ACTION_UNRESOLVED"]
    assert found[0].ref == "ACT-a"


def test_action_unresolved_missing_fragment_is_not_silent(tmp_path: Path) -> None:
    # `invokes: CTR-x` with no '#operation' fragment at all used to pass silently
    # (the old guard skipped anything without a '#'). It now fires ACTION_UNRESOLVED
    # like any other unresolved reference, since the contract is a real, enumerable
    # openapi doc and the action names no operation within it.
    data = base()
    data["modules"] = [_module_with_action("CTR-x")]
    spec = make_spec(tmp_path, data)
    contract = tmp_path / "contracts" / "x.yaml"
    contract.parent.mkdir(parents=True)
    contract.write_text(
        "openapi: 3.0.3\npaths:\n  /x:\n    get:\n      operationId: getX\n",
        encoding="utf-8",
    )
    found = list(completeness.action_operations_resolve(spec))
    assert codes(found) == ["ACTION_UNRESOLVED"]
    assert found[0].ref == "ACT-a"
    assert found[0].question


def test_non_yaml_contract_type_is_existence_only(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [
        {
            "id": "MOD-api",
            "name": "api",
            "responsibility": "r",
            "boundaries": {"may_import": []},
            "contracts": [{"id": "CTR-gql", "type": "graphql", "path": "contracts/x.graphql"}],
        }
    ]
    spec = make_spec(tmp_path, data)
    contract = tmp_path / "contracts" / "x.graphql"
    contract.parent.mkdir(parents=True)
    # Valid GraphQL SDL, but not parseable YAML (would raise YAMLError if loaded).
    contract.write_text(
        "type Query {\n  foo: String!\n  bar(id: ID!): [Bar!]!\n}\n", encoding="utf-8"
    )
    assert codes(completeness.contract_files(spec)) == []
