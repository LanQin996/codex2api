import asyncio
import importlib
import json
import os
import sys

import pytest

def test_rejection_diagnostics_redact_unknown_reasons(modules, caplog):
    p, _ = modules
    for mode in ("stream", "nonstream"):
        message = p._excel_tool_rejection_message("private-tool-arguments", mode)
        assert "private-tool-arguments" not in message
    assert "private-tool-arguments" not in caplog.text
    assert "reason=invalid_tool_call" in caplog.text
    p._excel_tool_rejection_message("required_tool_call_missing", "stream")
    assert "reason=required_tool_call_missing" in caplog.text

@pytest.mark.parametrize("status", ["failed", "incomplete"])
def test_failure_event_overrides_completed_status(modules, status):
    p, _ = modules
    async def chunks():
        event = {"type": "response." + status, "response": {"status": "completed", "output": [], "error": {"code": "original"}}}
        yield b"data: " + json.dumps(event).encode() + bytes([10, 10])
    async def collect():
        return b"".join([x async for x in p._excel_tool_stream_transform({})(chunks())])
    result = asyncio.run(collect())
    events = [json.loads(line[5:]) for line in result.splitlines() if line.startswith(b"data:") and line[5:].strip() != b"[DONE]"]
    assert events[-1]["response"]["status"] == status
    assert events[-1]["response"]["error"]["code"] == "original"

@pytest.mark.parametrize("value", ["line1" + chr(10) + "line2", chr(92) + "n", "C:" + chr(92) + "temp" + chr(92) + "new"])
def test_transport_preserves_exact_custom_input(modules, value):
    _, u = modules
    native = {"type": "function_call", "name": "run_officejs", "call_id": "escaped",
              "arguments": json.dumps({"code": json.dumps({"name": "exec", "input": value})})}
    call = u.extract_native_client_tool_call({"output": [native]}, {"tools": [{"type": "custom", "name": "exec"}]}, remember=False)
    assert call["input"] == value

@pytest.mark.parametrize("parallel", [True, False])
def test_batch_validation(modules, parallel):
    p, u = modules
    source = {"tools": [{"type": "custom", "name": "exec"}], "parallel_tool_calls": parallel}
    calls = [{"type": "function_call", "id": "fc_batch_"+str(i), "call_id": "call_batch_"+str(i),
              "name": "run_officejs", "arguments": json.dumps({"code": json.dumps({"name": "exec", "input": str(i)})})}
             for i in range(2)]
    if not parallel:
        with pytest.raises(ValueError, match="parallel_calls_disabled"):
            p._excel_convert_native_batch({"status": "completed", "output": calls}, source)
        return
    translated, converted = p._excel_convert_native_batch({"status": "completed", "output": calls}, source)
    assert [x[1]["input"] for x in converted] == ["0", "1"]
    assert all(x["type"] == "custom_tool_call" for x in translated["output"])
    calls[1]["call_id"] = calls[0]["call_id"]
    with pytest.raises(ValueError, match="duplicate_call_id"):
        p._excel_convert_native_batch({"status": "completed", "output": calls}, source)

@pytest.fixture
def modules():
    source = os.environ.get("EXCEL_UPSTREAM_SOURCE")
    if not source:
        pytest.skip("Set EXCEL_UPSTREAM_SOURCE")
    sys.path.insert(0, source)
    return importlib.import_module("proxy"), importlib.import_module("excel_upstream")

@pytest.mark.parametrize("policy", [None, [], [{"type": "compaction", "compact_threshold": 475000}], "invalid"])
def test_context_policy_not_invented(modules, policy):
    _, u = modules
    source = {"input": "hello", "context_management": policy}
    wire = u.prepare_responses_body(source)
    if policy is None or policy == []:
        assert "context_management" not in wire
    else:
        assert wire["context_management"] == policy
    assert "context_management" not in u.prepare_responses_body({"input": "hello"})

@pytest.mark.parametrize("catalog", [False, True])
@pytest.mark.parametrize("ending", ["completed", "failed", "truncated"])
def test_invalid_native_never_reaches_client(modules, catalog, ending):
    p, _ = modules
    source = {"tools": [{"type": "custom", "name": "exec"}]} if catalog else {}
    native = {"type": "function_call", "name": "run_officejs", "call_id": "bad", "arguments": "private-invalid"}
    events = [{"type": "response.output_item.done", "output_index": 0, "item": native}]
    if ending != "truncated":
        events.append({"type": "response." + ending, "response": {"output": [native],
                       "error": {"code": "original_failure"}}})
    async def chunks():
        for event in events:
            yield b"data: " + json.dumps(event).encode() + bytes([10, 10])
    async def collect():
        return b"".join([x async for x in p._excel_tool_stream_transform(source)(chunks())])
    result = asyncio.run(collect())
    expected = {"completed": b"invalid_tool_call", "failed": b"original_failure",
                "truncated": b"incomplete_tool_stream"}[ending]
    assert expected in result
    assert b"private-invalid" not in result
    assert b"run_officejs" not in result
    assert b"response.completed" not in result

def test_compacted_tool_roundtrip_continues(modules):
    p, u = modules
    source = {"tools": [{"type": "custom", "name": "exec"}]}
    native = {"type": "function_call", "id": "fc_contract", "call_id": "call_contract", "name": "run_officejs",
              "arguments": json.dumps({"code": json.dumps({"name": "exec", "input": "test"})})}
    async def chunks():
        yield b"data: " + json.dumps({"type": "response.completed", "response": {"output": [native]}}).encode() + bytes([10, 10])
    async def collect():
        return b"".join([x async for x in p._excel_tool_stream_transform(source)(chunks())])
    result = asyncio.run(collect())
    assert b"custom_tool_call" in result and b"invalid_tool_call" not in result
    compact = {"type": "compaction", "encrypted_content": "opaque-test"}
    wire = u.prepare_responses_body({**source, "input": [compact, {"type": "custom_tool_call_output", "call_id": "call_contract", "output": "done"}]})
    assert compact in wire["input"]
    assert any(x.get("call_id") == "call_contract" and x.get("output") == "done" for x in wire["input"])
    def mock_upstream(next_wire):
        items = next_wire["input"]
        assert compact in items
        assert any(x.get("type") == "function_call" and x.get("call_id") == "call_contract" for x in items)
        assert any(x.get("type") == "function_call_output" and x.get("call_id") == "call_contract" and x.get("output") == "done" for x in items)
        assert any("exec" in json.dumps(x) and x.get("role") == "developer" for x in items)
        return {"output": [{**native, "id": "fc_contract_next", "call_id": "call_contract_next"}]}
    async def next_chunks():
        yield b"data: " + json.dumps({"type": "response.completed", "response": mock_upstream(wire)}).encode() + bytes([10, 10])
    async def next_collect():
        return b"".join([x async for x in p._excel_tool_stream_transform(source)(next_chunks())])
    next_result = asyncio.run(next_collect())
    assert b"call_contract_next" in next_result
    assert b"custom_tool_call" in next_result
    assert b"invalid_tool_call" not in next_result
