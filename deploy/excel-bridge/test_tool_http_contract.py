"""Real bridge HTTP routes with a synthetic, non-network upstream."""
import base64
import copy
import importlib
import json
import os
import sys

import httpx
import pytest
from starlette.testclient import TestClient

TOOL_TYPES = {"function_call", "custom_tool_call"}
TOOLS = [
    {"type": "custom", "name": "exec"},
    {"type": "function", "name": "probe", "parameters": {"type": "object",
     "properties": {"value": {"type": "string"}}, "required": ["value"],
     "additionalProperties": False}},
]

def native(number, name="exec", value="  secret-input  " ):
    envelope = {"name": name, "input": value} if name == "exec" else {"name": name, "arguments": {"value": value}}
    return {"type": "function_call", "name": "run_officejs", "id": "fc_http_" + str(number),
            "call_id": "call_http_" + str(number), "status": "completed",
            "arguments": json.dumps({"code": json.dumps(envelope)})}

def response(output, status="completed"):
    return {"id": "resp_http", "object": "response", "status": status, "output": output,
            "usage": {"input_tokens": 10, "output_tokens": 3, "total_tokens": 13}}

def text_item(text="answer"):
    return {"type": "message", "id": "msg_http", "role": "assistant",
            "content": [{"type": "output_text", "text": text}]}

def sse(payload):
    events = [{"type": "response.created", "response": {**payload, "output": [], "status": "in_progress"}}]
    for index, item in enumerate(payload["output"]):
        events += [{"type": "response.output_item.added", "output_index": index, "item": item},
                   {"type": "response.output_item.done", "output_index": index, "item": item}]
    events.append({"type": "response." + payload["status"], "response": payload})
    raw = b""
    for index, event in enumerate(events):
        event["sequence_number"] = index
        raw += b"data: " + json.dumps(event).encode() + bytes([10, 10])
    return raw + b"data: [DONE]" + bytes([10, 10])

def events_of(result):
    events = [json.loads(line[5:]) for line in result.text.splitlines()
              if line.startswith("data:") and line[5:].strip() != "[DONE]"]
    assert [e.get("sequence_number") for e in events] == list(range(len(events)))
    return events

@pytest.fixture
def bridge_http(tmp_path, monkeypatch):
    root = os.environ.get("EXCEL_UPSTREAM_SOURCE")
    if not root:
        pytest.skip("Set EXCEL_UPSTREAM_SOURCE")
    sys.path.insert(0, root)
    for key in ("CONFIG", "STATE", "CACHE"):
        monkeypatch.setenv("GHCP_" + key + "_DIR", str(tmp_path / key))
    monkeypatch.setenv("GHCP_RESPONSES_UPSTREAM", "rest")
    backend = importlib.import_module("proxy")
    import bridge
    key = "synthetic-contract-key-" + "x" * 40
    token = base64.urlsafe_b64encode(json.dumps({"exp": 4102444800}).encode()).decode().rstrip("=")
    session = tmp_path / "session.json"
    session.write_text(json.dumps({"headers": {"authorization": "Bearer test." + token + ".signature",
                                             "chatgpt-account-id": "contract-account"}}))
    monkeypatch.setenv("EXCEL_BRIDGE_API_KEY", key)
    monkeypatch.setenv("EXCEL_SESSION_FILE", str(session))
    monkeypatch.setenv("EXCEL_ACCOUNT_ROUTES", "0")
    app = bridge.production_app()
    records, requests = {}, []
    monkeypatch.setattr(backend.excel_upstream, "_remember_native_call", lambda item: records.update({item["call_id"]: copy.deepcopy(item)}))
    monkeypatch.setattr(backend.excel_upstream, "_remembered_native_call", lambda call_id: copy.deepcopy(records.get(call_id)))
    state = {"payload": response([]), "force_sse": False}
    def upstream(request):
        wire = json.loads(request.content)
        requests.append(wire)
        payload = state["payload"]
        if wire.get("stream") or state["force_sse"]:
            return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=sse(payload))
        return httpx.Response(200, json=payload)
    monkeypatch.setattr(backend, "_EXCEL_UPSTREAM_CLIENT", httpx.AsyncClient(transport=httpx.MockTransport(upstream)))
    with TestClient(app) as client:
        def invoke(payload, mode="json", **overrides):
            state.update(payload=payload, force_sse=mode == "sse_json")
            body = {"model": "gpt-5.6-sol-excel", "input": "test tool contract",
                    "tools": TOOLS, "stream": mode == "stream", **overrides}
            return client.post("/v1/responses", headers={"Authorization": "Bearer " + key}, json=body)
        yield invoke, records, requests, backend.excel_upstream

def assert_rejected(result, mode, reason):
    assert reason in result.text
    if mode == "stream":
        events = events_of(result)
        assert sum(e["type"] == "response.failed" for e in events) == 1
        assert not any(e.get("item", {}).get("type") in TOOL_TYPES for e in events)
        assert not any(e["type"] == "response.completed" for e in events)
    else:
        assert result.status_code == 502
        assert result.json()["error"]["code"] == "invalid_tool_call"
    assert "secret-input" not in result.text
    assert "run_officejs" not in result.text

@pytest.mark.parametrize("mode", ["json", "sse_json", "stream"])
@pytest.mark.parametrize("fault", ["unknown", "schema", "duplicate", "parallel", "unfinished"])
def test_invalid_batch_never_cached_or_dispatched(bridge_http, mode, fault):
    invoke, records, _, _ = bridge_http
    first, second = native(1), native(2, "probe")
    options = {}
    reason = "catalog_or_schema_mismatch"
    if fault == "unknown":
        second = native(2, "missing")
    elif fault == "schema":
        second = native(2, "probe", 42)
    elif fault == "duplicate":
        second["call_id"] = first["call_id"]
        reason = "invalid_or_duplicate_call_id"
    elif fault == "parallel":
        options["parallel_tool_calls"] = False
        reason = "parallel_calls_disabled"
    else:
        second["status"] = "in_progress"
        reason = "tool_not_completed"
    result = invoke(response([first, second]), mode, **options)
    assert_rejected(result, mode, reason)
    assert records == {}

@pytest.mark.parametrize("mode", ["json", "sse_json", "stream"])
@pytest.mark.parametrize("status", ["failed", "incomplete"])
def test_failed_response_preserves_details_without_tools(bridge_http, mode, status):
    invoke, records, _, _ = bridge_http
    original = response([text_item(), native(1)], status)
    original.update(error={"code": "original_upstream_error", "message": "original reason"},
                    incomplete_details={"reason": "max_output_tokens"})
    result = invoke(original, mode, tool_choice="required")
    terminal = events_of(result)[-1]["response"] if mode == "stream" else result.json()
    assert terminal["status"] == status
    assert terminal["error"] == original["error"]
    assert terminal["incomplete_details"] == original["incomplete_details"]
    assert terminal["usage"] == original["usage"]
    assert terminal["output"] == [text_item()]
    assert records == {}
    assert "secret-input" not in result.text

@pytest.mark.parametrize("mode", ["json", "sse_json", "stream"])
def test_successful_multi_call_lifecycle(bridge_http, mode):
    invoke, records, _, _ = bridge_http
    first, second = native(1), native(2, "probe")
    result = invoke(response([text_item(), first, second]), mode)
    assert result.status_code == 200
    if mode == "stream":
        events = events_of(result)
        assert sum(e["type"] == "response.completed" for e in events) == 1
        terminal = events[-1]["response"]
        for index, field, prefix in [(1, "input", "response.custom_tool_call_input"),
                                     (2, "arguments", "response.function_call_arguments")]:
            lifecycle = [e for e in events if e.get("output_index") == index]
            assert [e["type"] for e in lifecycle] == ["response.output_item.added", prefix + ".delta", prefix + ".done", "response.output_item.done"]
            item = terminal["output"][index]
            assert lifecycle[0]["item"][field] == ""
            assert lifecycle[1]["delta"] == item[field]
            assert lifecycle[2][field] == item[field]
            assert lifecycle[3]["item"] == item
        assert result.text.count("data: [DONE]") == 1
    else:
        terminal = result.json()
    assert terminal["output"][1]["input"] == "  secret-input  "
    assert terminal["output"][2]["name"] == "probe"
    assert records == {first["call_id"]: first, second["call_id"]: second}


@pytest.mark.parametrize("mode", ["json", "sse_json", "stream"])
@pytest.mark.parametrize("choice", [
    "none", {"type": "function", "name": "probe"},
    {"type": "allowed_tools", "mode": "auto", "tools": [{"type": "function", "name": "probe"}]},
])
def test_choice_blocks_tools_outside_current_permission(bridge_http, mode, choice):
    invoke, records, requests, _ = bridge_http
    result = invoke(response([native(1)]), mode, tool_choice=choice)
    assert_rejected(result, mode, "catalog_or_schema_mismatch")
    assert records == {}
    catalog = json.dumps(requests[-1]["input"])
    assert '"name":"exec"' not in catalog


@pytest.mark.parametrize("mode", ["json", "sse_json", "stream"])
@pytest.mark.parametrize("choice", [
    "required", {"type": "custom", "name": "exec"},
    {"type": "allowed_tools", "mode": "required", "tools": [{"type": "custom", "name": "exec"}]},
])
def test_required_choice_is_enforced_and_valid_call_succeeds(bridge_http, mode, choice):
    invoke, records, _, _ = bridge_http
    result = invoke(response([text_item()]), mode, tool_choice=choice)
    assert_rejected(result, mode, "required_tool_call_missing")
    assert records == {}
    result = invoke(response([native(2)]), mode, tool_choice=choice)
    assert result.status_code == 200
    terminal = events_of(result)[-1]["response"] if mode == "stream" else result.json()
    assert terminal["output"][0]["name"] == "exec"
    assert list(records) == ["call_http_2"]


@pytest.mark.parametrize("mode", ["json", "stream"])
@pytest.mark.parametrize("choice,tools", [
    ("required", []),
    ({"type": "custom", "name": "missing"}, TOOLS),
    ({"type": "function", "name": "exec"}, TOOLS),
    ({"type": "allowed_tools", "mode": "required", "tools": []}, TOOLS),
    ({"type": "allowed_tools", "mode": "invalid", "tools": TOOLS}, TOOLS),
    ({"type": "custom", "name": "exec", "namespace": []}, TOOLS),
    ("invalid", TOOLS),
])
def test_invalid_choice_rejected_before_upstream(bridge_http, mode, choice, tools):
    invoke, records, requests, _ = bridge_http
    result = invoke(response([]), mode, tool_choice=choice, tools=tools)
    assert result.status_code == 400
    assert result.json()["error"]["code"] == "invalid_tool_choice"
    assert requests == [] and records == {}


@pytest.mark.parametrize("mode", ["json", "stream"])
def test_compacted_http_followup_restores_identity_under_none(bridge_http, mode):
    invoke, records, requests, _ = bridge_http
    first = native(1)
    initial = invoke(response([first]), mode)
    assert initial.status_code == 200 and records == {first["call_id"]: first}
    compact = {"type": "compaction", "encrypted_content": "synthetic-opaque"}
    history = [compact, {"type": "custom_tool_call_output", "call_id": first["call_id"], "output": "local-result"}]
    result = invoke(response([text_item()]), mode, input=history, tool_choice="none")
    assert result.status_code == 200
    wire = requests[-1]["input"]
    assert compact in wire and first in wire
    assert any(x.get("type") == "function_call_output" and x.get("call_id") == first["call_id"]
               and x.get("output") == "local-result" for x in wire)
    next_call = native(2)
    result = invoke(response([next_call]), mode, input=history, tool_choice="required")
    terminal = events_of(result)[-1]["response"] if mode == "stream" else result.json()
    assert terminal["output"][0]["call_id"] == next_call["call_id"]
    assert terminal["output"][0]["type"] == "custom_tool_call"
    assert records == {first["call_id"]: first, next_call["call_id"]: next_call}


@pytest.mark.parametrize("mode", ["json", "stream"])
@pytest.mark.parametrize("kind", ["custom", "function"])
def test_namespaced_choice_keeps_routing_identity(bridge_http, mode, kind):
    invoke, records, _, _ = bridge_http
    leaf = {"type": kind, "name": "run"}
    tools = [{"type": "namespace", "name": "alpha", "tools": [leaf]},
             {"type": "namespace", "name": "beta", "tools": [leaf]}]
    envelope = {"name": "alpha.run", **({"input": "value"} if kind == "custom" else {"arguments": {}})}
    call = native(1)
    call["arguments"] = json.dumps({"code": json.dumps(envelope)})
    choice = {"type": kind, "name": "run", "namespace": "alpha"}
    result = invoke(response([call]), mode, tools=tools, tool_choice=choice)
    assert result.status_code == 200
    terminal = events_of(result)[-1]["response"] if mode == "stream" else result.json()
    converted = terminal["output"][0]
    assert converted["namespace"] == "alpha" and converted["name"] == "run"
    assert records == {call["call_id"]: call}


@pytest.mark.parametrize("status", [None, "in_progress", "queued", "cancelled"])
def test_nonstream_unfinished_or_missing_status_never_dispatches(bridge_http, status):
    invoke, records, _, _ = bridge_http
    original = response([native(1)], status)
    if status is None:
        del original["status"]
    result = invoke(original)
    assert records == {}
    assert "secret-input" not in result.text
    if status is None:
        assert_rejected(result, "json", "missing_or_invalid_response_status")
    else:
        assert result.json()["status"] == status and result.json()["output"] == []
