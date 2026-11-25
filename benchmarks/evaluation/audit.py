"""Check that the scheduler actually received the saved workload classes."""


def audit_classes(start, requests, gateway_records, jobs):
    if start["mode"] == "direct":
        return []
    expected = {job["id"]: job["class"] for job in jobs}
    issues = []
    for request in requests:
        if request["sent_ms"] is None:
            continue
        actual = gateway_records.get(request["request_id"], {}).get("request_class")
        intended = expected[request["job_id"]]
        if actual != intended or request["class"] != intended:
            issues.append(f'{start["trial_id"]}/{request["job_id"]}: request class mismatch or missing evidence '
                          f'(planned={intended}, client={request["class"]}, gateway={actual}).')
    return issues
