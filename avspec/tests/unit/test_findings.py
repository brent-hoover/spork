from avspec.findings import CODE_ORDER, Finding, Severity, ordered


def f(code: str, sev: Severity, ref: str = "") -> Finding:
    return Finding(code=code, severity=sev, message="m", ref=ref or None)


def test_errors_sort_before_todos() -> None:
    todo = f("NO_STACK", Severity.TODO)
    err = f("DUPLICATE_ID", Severity.ERROR)
    assert ordered([todo, err]) == [err, todo]


def test_todos_follow_authoring_order() -> None:
    later = f("VIEW_NO_AC", Severity.TODO)
    earlier = f("NO_REQUIREMENTS", Severity.TODO)
    assert ordered([later, earlier]) == [earlier, later]


def test_unknown_code_sorts_last_within_severity() -> None:
    known = f("TEST_FILE_MISSING", Severity.TODO)
    unknown = f("SOMETHING_NEW", Severity.TODO)
    assert ordered([unknown, known]) == [known, unknown]


def test_ties_break_on_ref() -> None:
    b = f("REQ_NO_AC", Severity.TODO, ref="REQ-b")
    a = f("REQ_NO_AC", Severity.TODO, ref="REQ-a")
    assert ordered([b, a]) == [a, b]


def test_code_order_is_unique() -> None:
    assert len(CODE_ORDER) == len(set(CODE_ORDER))
