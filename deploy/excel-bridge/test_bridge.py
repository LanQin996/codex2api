import base64
import importlib
import json
import os
import sys
from types import SimpleNamespace

import pytest
from fastapi.responses import JSONResponse, StreamingResponse
from starlette.testclient import TestClient

from bridge import create_app


def test_opaque_state_diagnostic_redacted_and_bounded():
    from bridge import opaque_state_shape
    item = {"type": "reasoning", "encrypted_content": "private-cipher", "id": "private-id"}
    body = {"input": [item] * 20 + [{"type": "compaction", "encrypted_content": "opaque"}]}
    before = json.dumps(body)
    result = opaque_state_shape(body)
    assert result["encrypted_types"] == {"reasoning": 20, "compaction": 1}
    assert len(result["samples"]) == 16 and result["truncated"]
    assert "private" not in json.dumps(result)
    assert json.dumps(body) == before
    assert opaque_state_shape({"input": "text"})["samples"] == []


def test_encrypted_result_diagnostic_contains_no_payload():
    from bridge import encrypted_result_shape
    result = encrypted_result_shape({"input": [{"type": "function_call_output",
        "encrypted_content": "secret-ciphertext", "call_id": "private-call"}]})
    assert result == {"results": 1, "encrypted_results": 1, "missing_output": 1}
    assert encrypted_result_shape({"input": "text"})["results"] == 0


@pytest.mark.parametrize("origin", ["run_officejs", "update_plan"])
def test_native_encrypted_tool_result_is_not_rewritten(origin):
    source = os.environ.get("EXCEL_UPSTREAM_SOURCE")
    if not source:
        pytest.skip("Set EXCEL_UPSTREAM_SOURCE")
    sys.path.insert(0, source)
    module = importlib.import_module("excel_upstream")
    item = {"type": "function_call_output", "id": "native-output-id",
            "call_id": "call-encrypted", "encrypted_content": "opaque-test-state"}
    before = json.loads(json.dumps(item))
    assert module._normalized_tool_output(item, {item["call_id"]: origin}) == before
    assert item == before


def test_exec_only_catalog_uses_callable_discovery_example():
    source = os.environ.get("EXCEL_UPSTREAM_SOURCE")
    if not source:
        pytest.skip("Set EXCEL_UPSTREAM_SOURCE")
    sys.path.insert(0, source)
    module = importlib.import_module("excel_upstream")
    body = {"tools": [{"type": "custom", "name": "exec"}]}
    example = json.loads(module._client_tool_transport_example(body))
    assert example["name"] == "exec"
    assert "ALL_TOOLS" in example["input"]
    reminder = module._client_tool_protocol_reminder(body)
    assert "nested shell and filesystem tools" in reminder
    assert "does not disable current tools" in reminder
    assert module._client_tool_transport_example(body) in module._client_tool_protocol_instructions(body)


@pytest.mark.parametrize("catalog_at_end", [False, True])
def test_compaction_refreshes_live_tool_protocol(monkeypatch, catalog_at_end):
    source = os.environ.get("EXCEL_UPSTREAM_SOURCE")
    if not source:
        pytest.skip("Set EXCEL_UPSTREAM_SOURCE")
    sys.path.insert(0, source)
    module = importlib.import_module("excel_upstream")
    monkeypatch.setattr(module, "CATALOG_AT_PROMPT_END", catalog_at_end)
    compact = {"type": "compaction", "encrypted_content": "opaque"}
    body = {"model": "gpt-5.6-sol-excel", "tools": [{"type": "custom", "name": "exec"}],
            "input": [compact, {"role": "user", "content": "continue"}]}
    wire = module.prepare_responses_body(body)["input"]
    after = wire[wire.index(compact) + 1:]
    assert any(x.get("role") == "developer" and
               "nested shell and filesystem tools" in json.dumps(x) for x in after)
    if not catalog_at_end:
        body["input"].append({"role": "assistant", "content": "working"})
        assert module.prepare_responses_body(body)["input"][:len(wire)] == wire


@pytest.mark.parametrize("compacted", [False, True])
def test_live_protocol_follows_history_environment(monkeypatch, compacted):
    source = os.environ.get("EXCEL_UPSTREAM_SOURCE")
    if not source:
        pytest.skip("Set EXCEL_UPSTREAM_SOURCE")
    sys.path.insert(0, source)
    module = importlib.import_module("excel_upstream")
    monkeypatch.setattr(module, "CATALOG_AT_PROMPT_END", False)
    history = ([{"type": "compaction", "encrypted_content": "opaque"}] if compacted else [])
    history += [{"role": "developer", "content": "Legacy environment: only Excel tools."},
                {"role": "user", "content": "inspect repository"}]
    body = {"tools": [{"type": "custom", "name": "exec"}], "input": history}
    wire = module.prepare_responses_body(body)["input"]
    legacy = next(i for i,x in enumerate(wire) if "Legacy environment" in json.dumps(x))
    assert any(x.get("role") == "developer" and "ALL_TOOLS" in json.dumps(x) for x in wire[legacy+1:])
    history.append({"role": "assistant", "content": "working"})
    assert module.prepare_responses_body(body)["input"][:len(wire)] == wire
    disabled = module.prepare_responses_body({**body, "tool_choice": "none"})["input"]
    assert not any("ALL_TOOLS" in json.dumps(x) for x in disabled)


def test_new_session_namespaced_exec_discovery():
    source = os.environ.get("EXCEL_UPSTREAM_SOURCE")
    if not source:
        pytest.skip("Set EXCEL_UPSTREAM_SOURCE")
    sys.path.insert(0, source)
    module = importlib.import_module("excel_upstream")
    body = {"tools": [{"type": "namespace", "name": "functions",
                       "tools": [{"type": "custom", "name": "exec"}]}],
            "input": [{"role": "user", "content": "inspect the repository"}]}
    example = json.loads(module._client_tool_transport_example(body))
    assert example["name"] == "functions.exec"
    assert "ALL_TOOLS" in example["input"]
    assert "nested shell and filesystem tools" in json.dumps(module.prepare_responses_body(body))
    assert module._client_tool_protocol_reminder({**body, "tool_choice": "none"}) == ""


def test_reset_turn_state_preserves_unrelated_metadata():
    from bridge import reset_client_turn_state
    body = {"metadata": {"task_id": "stale", "turn_id": "changing",
                         "agent_iteration": "999", "customer_tag": "keep"}}
    reset_client_turn_state(body)
    assert body["metadata"] == {"customer_tag": "keep"}


def test_upstream_error_diagnostics_are_redacted(caplog):
    from bridge import log_upstream_failure
    response = JSONResponse({"error": {"code": "basispoints_model_access_changed",
                                       "message": "Bearer private-token"}}, status_code=403)
    log_upstream_failure(response, {"model": "gpt-6-sol-excel"}, "42")
    assert "basispoints_model_access_changed" in caplog.text
    assert "gpt-6-sol-excel" in caplog.text
    assert "private-token" not in caplog.text
    caplog.clear()
    response = JSONResponse({"error": {"code": {"secret": "private-token"}}}, status_code=500)
    log_upstream_failure(response, {"model": "bad\nprivate-token"}, "42")
    assert "private-token" not in caplog.text


def test_derived_turn_stable_for_tool_loop(tmp_path):
    source = os.environ.get("EXCEL_UPSTREAM_SOURCE")
    if not source:
        pytest.skip("Set EXCEL_UPSTREAM_SOURCE to the pinned ghcp_proxy checkout")
    sys.path.insert(0, source)
    from bridge import reset_client_turn_state
    module = importlib.import_module("excel_upstream")

    def prepare(items):
        body = {"model": "gpt-5.6-sol-excel", "prompt_cache_key": "stable",
                "input": items, "metadata": {"task_id": "wrong", "turn_id": "wrong",
                                            "agent_iteration": "999"}}
        reset_client_turn_state(body)
        return module.prepare_responses_body(body)["metadata"]

    first = [{"role": "user", "content": "do work"}]
    continued = first + [{"type": "function_call_output", "call_id": "test", "output": "done"}]
    a, b = prepare(first), prepare(continued)
    assert a["task_id"] == b["task_id"] != "wrong"
    assert a["turn_id"] == b["turn_id"] != "wrong"
    assert int(b["agent_iteration"]) > int(a["agent_iteration"])
    assert prepare(continued) == b
    next_turn = prepare(continued + [{"role": "user", "content": "next question"}])
    assert next_turn["task_id"] == b["task_id"]
    assert next_turn["turn_id"] != b["turn_id"]

KEY = "test-only-bridge-key-" + "x" * 32

def test_cache_diagnostics_prefix_and_redaction(caplog):
    import bridge
    bridge.cache_diagnostic_history.clear()
    token = bridge.request_scope.set(("21", "private-session", "private-token"))
    try:
        body = {"model": "test", "prompt_cache_key": "private-key",
                "input": [{"role": "user", "content": "private-prompt"}]}
        a = bridge.cache_diagnostic_snapshot(body)
        b = bridge.cache_diagnostic_snapshot({**body, "input": body["input"] + ["next"]})
        assert a["previous_items"] is None
        assert b["common_prefix_items"] == b["previous_items"] == 1
        bridge.log_cache_usage(b, {"response": {"usage": {"input_tokens": 100}}})
        assert '"cached_field_present": false' in caplog.text
        assert '"cached_tokens": null' in caplog.text
        bridge.log_cache_usage(b, {"response": {"usage": {"input_tokens_details": {"cached_tokens": 0}}}})
        assert '"cached_field_present": true' in caplog.text
        assert '"cached_tokens": 0' in caplog.text
        assert "private-" not in caplog.text
    finally:
        bridge.request_scope.reset(token)


def test_cache_diagnostics_stream_preserves_chunks(caplog):
    import asyncio
    import bridge
    upstream = SimpleNamespace(prepare_responses_body=lambda body: body)
    backend = SimpleNamespace(excel_upstream=upstream, _excel_tool_stream_transform=lambda body: None)
    bridge.install_wire_diagnostics(backend)
    upstream.prepare_responses_body({"input": [], "prompt_cache_key": "stable"})
    payload = {"type": "response.completed", "response": {"usage": {"input_tokens": 100,
               "input_tokens_details": {"cached_tokens": 64}}}}
    raw = b"data: " + json.dumps(payload).encode() + bytes([10, 10])
    chunks = [raw[:8], raw[8:31], raw[31:]]
    async def source():
        for chunk in chunks:
            yield chunk
    async def collect():
        return [chunk async for chunk in backend._excel_tool_stream_transform({})(source())]
    assert asyncio.run(collect()) == chunks
    assert '"cached_tokens": 64' in caplog.text

AUTH = {"Authorization": "Bearer " + KEY}

def test_image_attachments_cover_tool_results(monkeypatch):
    import asyncio
    import bridge
    calls = []
    def upload(backend, part):
        calls.append(part["image_url"])
        return {"type": "input_image", "file_id": "file-test", "detail": "auto"}
    monkeypatch.setattr(bridge, "upload_inline_image", upload)
    image = {"type": "input_image", "image_url": "data:image/png;base64,eA=="}
    body = {"input": [
        {"role": "user", "content": [image]},
        {"type": "custom_tool_call_output", "call_id": "a", "output": [image]},
    ]}
    result = asyncio.run(bridge.prepare_image_attachments(None, body))
    assert len(calls) == 2
    assert result["input"][1]["call_id"] == "a"
    assert result["input"][1]["output"][0]["type"] == "input_text"
    assert result["input"][2]["role"] == "user"
    assert result["input"][2]["content"][1]["file_id"] == "file-test"
    assert "image_url" in body["input"][0]["content"][0]
    assert "image_url" in body["input"][1]["output"][0]


@pytest.mark.parametrize("kind", ["function_call_output", "custom_tool_call_output"])
def test_tool_images_preserve_identity_order_and_text(monkeypatch, kind):
    import asyncio
    import bridge
    monkeypatch.setattr(bridge, "upload_inline_image", lambda backend, part: dict(part))
    call = {"type": "function_call", "call_id": "a", "name": "view", "arguments": "{}"}
    text = {"type": "input_text", "text": "Screenshot result"}
    image = {"type": "input_image", "file_id": "file-existing", "detail": "auto"}
    followup = {"role": "user", "content": "Describe it"}
    body = {"model": "gpt-6-astra-excel", "input": [
        call, {"type": kind, "id": "native-result", "call_id": "a",
               "output": [text, image, image]}, followup,
    ]}
    result = asyncio.run(bridge.prepare_image_attachments(None, body))
    assert result["input"][0] == call
    tool_result = result["input"][1]
    assert tool_result["type"] == kind
    assert tool_result["id"] == "native-result"
    assert tool_result["call_id"] == "a"
    assert tool_result["output"][0] == text
    assert all(part["type"] == "input_text" for part in tool_result["output"])
    assert result["input"][2]["content"][1:] == [image, image]
    assert result["input"][3] == followup
    assert body["input"][1]["output"] == [text, image, image]
    assert asyncio.run(bridge.prepare_image_attachments(None, result)) == result


@pytest.mark.parametrize("output", ["OK", [{"type": "input_text", "text": "OK"}], []])
def test_text_only_tool_results_are_unchanged(output):
    import asyncio
    import bridge
    body = {"input": [{"type": "function_call_output", "call_id": "a", "output": output}]}
    assert asyncio.run(bridge.prepare_image_attachments(None, body)) == body


def test_migrated_image_remains_image(tmp_path):
    from bridge import NativeCallStore, migrate_completed_history
    cache = NativeCallStore(tmp_path / "images.db")
    body = {"input": [
        {"type": "custom_tool_call", "call_id": "a", "name": "view_image", "input": "path"},
        {"type": "custom_tool_call_output", "call_id": "a", "output": [
            {"type": "input_image", "image_url": "data:image/png;base64,eA=="}]},
    ]}
    try:
        result = migrate_completed_history(body, cache, ("21", "session", "token"))
        content = result["input"][0]["content"]
        assert content[1]["type"] == "input_image"
        assert "data:image" not in content[0]["text"]
        assert body["input"][1]["output"][0]["image_url"] == content[1]["image_url"]
    finally:
        cache.close()


def test_attachment_upload_cache_is_scoped(monkeypatch):
    import bridge
    bridge.attachment_cache.clear()
    requests = []
    class Reply:
        status_code = 200
        content = b'{"openai_file_id":"file-test"}'
        def json(self): return json.loads(self.content)
    class Opener:
        def __enter__(self): return self
        def __exit__(self, *args): pass
        def post(self, endpoint, **kwargs):
            requests.append(SimpleNamespace(full_url=endpoint, data=kwargs["content"]))
            return Reply()
    monkeypatch.setattr(bridge.httpx, "Client", lambda **kwargs: Opener())
    headers = {"authorization": "Bearer first", "chatgpt-account-id": "a"}
    backend = SimpleNamespace(excel_upstream=SimpleNamespace(
        RESPONSES_URL="https://example.test/basispoints/api/responses",
        excel_session_store=SimpleNamespace(request_headers=lambda **kw: headers)))
    part = {"type": "input_image", "image_url": "data:image/png;base64,eA==", "detail": "original"}
    result = bridge.upload_inline_image(backend, part)
    assert result == {"type": "input_image", "file_id": "file-test", "detail": "auto"}
    bridge.upload_inline_image(backend, part)
    assert len(requests) == 1
    assert requests[0].full_url == "https://example.test/basispoints/api/attachments"
    assert b'name="file"' in requests[0].data
    headers["authorization"] = "Bearer second"
    bridge.upload_inline_image(backend, part)
    assert len(requests) == 2
    with pytest.raises(ValueError):
        bridge.upload_inline_image(backend, {**part, "file_id": "already"})

def test_wire_shape_never_contains_user_text():
    from bridge import wire_shape
    body = {"input": [
        {"role": "user", "content": [{"type": "input_text", "text": "private-token"}]},
        {"type": "private-token", "content": [{"type": "private-token"}]},
    ], "reasoning_effort": "private-token", "tools": [{"name": "private-token"}]}
    shape = wire_shape(body)
    assert "private-token" not in json.dumps(shape)
    assert shape["input_count"] == 2
    assert shape["tools_present"] is True
    assert shape["item_types"] == {"message": 1, "other": 1}

def test_compacted_checkpoint_keeps_identity_without_replaying_history(tmp_path):
    from bridge import NativeCallStore, migrate_completed_history, replay_checkpoint
    scope = ("21", "session", "generation")
    path = tmp_path / "compacted.db"
    original = {"input": [
        {"type": "function_call", "call_id": "old", "name": "write_file", "arguments": "{}"},
        {"type": "function_call_output", "call_id": "old", "output": "done"},
    ]}
    with_store = NativeCallStore(path)
    marker = migrate_completed_history(original, with_store, scope)["_excel_checkpoint"]
    with_store.close()
    cache = NativeCallStore(path)
    try:
        body = {"input": [
            {"type": "compaction", "encrypted_content": "opaque-state"},
            {"role": "user", "content": [{"type": "input_image", "file_id": "file-test"}]},
        ], "tools": [{"type": "function", "name": "probe"}]}
        before = json.loads(json.dumps(body))
        rebuilt = replay_checkpoint(body, cache, scope)
        assert rebuilt["_excel_checkpoint"] == marker
        assert rebuilt["input"] == before["input"]
        assert rebuilt["tools"] == before["tools"]
        assert body == before
        assert replay_checkpoint(body, cache, ("22", "session", "generation")) == body
    finally:
        cache.close()


def test_compacted_result_recovers_only_from_exact_checkpoint(tmp_path):
    from bridge import NativeCallStore, migrate_completed_history, replay_checkpoint
    scope = ("21", "s", "generation")
    path = tmp_path / "result.db"
    call = {"type": "function_call", "call_id": "a", "name": "write", "arguments": "{}"}
    result = {"type": "function_call_output", "call_id": "a", "output": "done"}
    cache = NativeCallStore(path)
    original = migrate_completed_history({"input": [call, result]}, cache, scope)
    cache.close()
    cache = NativeCallStore(path)
    try:
        compact = {"type": "compaction", "encrypted_content": "opaque"}
        body = {"input": [compact, result]}
        rebuilt = replay_checkpoint(body, cache, scope)
        assert rebuilt["_excel_checkpoint"] == original["_excel_checkpoint"]
        assert rebuilt["input"][0] == compact
        assert "already completed" in rebuilt["input"][1]["content"][0]["text"]
        assert body["input"] == [compact, result]
        for other in [("22", "s", "generation"), ("21", "s", "rotated"), ("21", "other", "generation")]:
            assert replay_checkpoint(body, cache, other) == body
        for bad in [[{**result, "output": "changed"}], [result, result], [call]]:
            with pytest.raises(ValueError):
                replay_checkpoint({"input": bad}, cache, scope)
    finally:
        cache.close()


def test_migration_replays_compacted_checkpoint_before_new_pairs(tmp_path):
    from bridge import NativeCallStore, migrate_completed_history
    scope = ("21", "session", "generation")
    call = {"type": "function_call", "call_id": "old", "name": "read", "arguments": "{}"}
    result = {"type": "function_call_output", "call_id": "old", "output": "done"}
    cache = NativeCallStore(tmp_path / "mixed.db")
    try:
        first = migrate_completed_history({"input": [call, result]}, cache, scope)
        compact = {"type": "compaction", "encrypted_content": "opaque"}
        new_call = {**call, "call_id": "new"}
        new_result = {**result, "call_id": "new"}
        body = {"input": [compact, result, new_call, new_result]}
        rebuilt = migrate_completed_history(body, cache, scope)
        assert rebuilt["_excel_checkpoint"] == first["_excel_checkpoint"]
        assert rebuilt["input"][0] == compact
        assert len(rebuilt["input"]) == 3
        assert body["input"] == [compact, result, new_call, new_result]
        assert migrate_completed_history(body, cache, scope) == rebuilt
        for bad in [[compact, {**result, "output": "changed"}, new_call, new_result],
                    [compact, result, {**new_call, "call_id": "pending"}]]:
            with pytest.raises(ValueError):
                migrate_completed_history({"input": bad}, cache, scope)
        with pytest.raises(ValueError):
            migrate_completed_history(body, cache, ("other", "session", "generation"))
    finally:
        cache.close()


def test_owner_refreshes_active_history_but_never_revives_expired(tmp_path):
    from bridge import NativeCallStore
    cache = NativeCallStore(tmp_path / "ttl.db", ttl_seconds=60, clock=lambda: 100)
    try:
        cache.remember(("21", "s", "g"), {"call_id": "a"})
        cache.clock = lambda: 140
        assert cache.owner("s", ["a"]) == "21"
        cache.clock = lambda: 170
        assert cache.owner("s", ["a"]) == "21"
        cache.clock = lambda: 231
        assert cache.owner("s", ["a"]) is None
        assert cache.db.execute("PRAGMA synchronous").fetchone()[0] == 2
    finally:
        cache.close()


def test_growing_migration_checkpoint_keeps_cache_identity(tmp_path):
    from bridge import NativeCallStore, migrate_completed_history
    path = tmp_path / "growing.db"
    scope = ("21", "stable-session", "generation")
    body = {"input": []}
    marker = None
    for i in range(3):
        body["input"].extend([
            {"type": "function_call", "call_id": str(i), "name": "read", "arguments": "{}"},
            {"type": "function_call_output", "call_id": str(i), "output": "done"},
        ])
        cache = NativeCallStore(path)
        try:
            result = migrate_completed_history(body, cache, scope)
            if marker is None:
                marker = result["_excel_checkpoint"]
            assert result["_excel_checkpoint"] == marker
            assert len(cache.checkpoint_items(scope)) == i + 1
            assert len(result["input"]) == i + 1
        finally:
            cache.close()


def test_migration_checkpoint_survives_restart_and_new_tool(tmp_path):
    from bridge import NativeCallStore, migrate_completed_history, replay_checkpoint
    scope = ("21", "s", "token")
    path = tmp_path / "migration.db"
    body = {"input": [
        {"role": "user", "content": "work"},
        {"type": "reasoning", "encrypted_content": "old-private-state"},
        {"type": "function_call", "call_id": "old", "name": "write_file", "arguments": "{}"},
        {"type": "function_call_output", "call_id": "old", "output": "done"},
    ]}
    db = NativeCallStore(path)
    rebuilt = migrate_completed_history(body, db, scope)
    assert not any(x.get("type") in {"reasoning", "function_call", "function_call_output"} for x in rebuilt["input"])
    assert "already completed" in json.dumps(rebuilt)
    marker = rebuilt["_excel_checkpoint"]
    db.close()
    db = NativeCallStore(path)
    continued = json.loads(json.dumps(body))
    continued["input"].append({"type": "function_call", "call_id": "new", "name": "read_file", "arguments": "{}"})
    result = replay_checkpoint(continued, db, scope)
    assert result["input"][-1]["call_id"] == "new"
    assert result["_excel_checkpoint"] == marker
    changed = json.loads(json.dumps(body))
    changed["input"][-1]["output"] = "different"
    with pytest.raises(ValueError):
        migrate_completed_history(changed, db, scope)
    with pytest.raises(ValueError):
        migrate_completed_history(continued, db, scope)
    db.close()


@pytest.mark.parametrize("history", [
    [{"type": "function_call", "call_id": "a", "name": "pay", "arguments": "{}"}],
    [{"type": "function_call_output", "call_id": "a", "output": "done"}],
    [{"type": "item_reference", "id": "opaque"}],
])
def test_migration_rejects_incomplete_history(history):
    from bridge import completed_tool_pairs
    with pytest.raises(ValueError):
        completed_tool_pairs(history)


@pytest.mark.parametrize("compacted", [False, True])
def test_http_migration_and_followup(tmp_path, monkeypatch, compacted):
    from bridge import install_scoped_backend
    monkeypatch.setenv("EXCEL_HISTORY_MIGRATION", "1")

    class RequestStore(Store):
        def request_headers(self, **kwargs):
            return dict(self.headers)

    module = SimpleNamespace(ExcelSessionStore=RequestStore, is_excel_model=lambda _: True)
    captured = []

    async def handler(request, body):
        captured.append(body)
        module._remember_native_call({"call_id": "new", "id": "native-new"})
        return JSONResponse({"output": []})

    backend = SimpleNamespace(excel_upstream=module, _handle_excel_responses=handler)
    install_scoped_backend(backend, cache_path=tmp_path / "cache.db")
    headers = {**AUTH, "X-Excel-Account": "21", "X-Excel-Session": "session",
               "X-Excel-Credential-Mode": "oauth", "X-Excel-Access-Token": "fake",
               "X-Excel-ChatGPT-Account": "workspace", "X-Excel-Migrate": "1"}
    body = {"model": "test-excel", "input": [
        {"role": "user", "content": "work"},
        {"type": "function_call", "name": "write_file", "arguments": "{}", "call_id": "old"},
        {"type": "function_call_output", "call_id": "old", "output": "done"},
    ]}
    if compacted:
        body["input"].insert(0, {"type": "compaction", "encrypted_content": "opaque-state"})
    with TestClient(create_app(backend, KEY, "unused", scoped=True)) as client:
        assert client.post("/v1/responses", headers=headers, json=body).status_code == 200
        assert not any(x.get("type") == "function_call" for x in captured[0]["input"])
        first_key = captured[0]["prompt_cache_key"]
        if compacted:
            assert captured[0]["input"][0] == body["input"][0]
        headers["X-Excel-Migrate"] = "auto"
        assert client.post("/v1/responses", headers=headers, json=body).status_code == 200
        assert captured[-1]["prompt_cache_key"] == first_key
        body["input"].extend([
            {"type": "function_call", "name": "read_file", "arguments": "{}", "call_id": "new"},
            {"type": "function_call_output", "call_id": "new", "output": "read"},
        ])
        assert client.post("/v1/responses", headers=headers, json=body).status_code == 200
        assert captured[-1]["input"][-2]["call_id"] == "new"
        assert captured[-1]["prompt_cache_key"] == first_key
        # A different account starts a new checkpoint instead of reusing native IDs.
        headers["X-Excel-Account"] = "19"
        headers["X-Excel-Migrate"] = "1"
        assert client.post("/v1/responses", headers=headers, json=body).status_code == 200
        assert captured[-1]["prompt_cache_key"] != first_key
        assert not any(x.get("type") == "function_call" for x in captured[-1]["input"])
        body["input"].append({"type": "function_call", "name": "pay", "arguments": "{}", "call_id": "pending"})
        assert client.post("/v1/responses", headers=headers, json=body).status_code == 409


def test_history_owner_failure_reasons(tmp_path):
    from bridge import NativeCallStore
    cache = NativeCallStore(tmp_path / "diagnostic.db", ttl_seconds=60, clock=lambda: 100)
    try:
        assert cache.owner_failure_reason("session", ["missing"]) == "record_missing"
        cache.remember(("21", "session", "generation"), {"call_id": "a"})
        assert cache.owner_failure_reason("other", ["a"]) == "session_mismatch"
        assert cache.owner_failure_reason("session", ["a"]) == "resolved"
        cache.remember(("19", "session", "generation"), {"call_id": "b"})
        assert cache.owner_failure_reason("session", ["a", "b"]) == "mixed_scopes"
        cache.remember(("19", "session", "generation"), {"call_id": "a"})
        assert cache.owner_failure_reason("session", ["a"]) == "ambiguous_scope"
        cache.clock = lambda: 161
        assert cache.owner_failure_reason("session", ["a"]) == "session_expired"
    finally:
        cache.close()


def test_history_owner_requires_complete_single_scope(tmp_path):
    from bridge import NativeCallStore
    cache = NativeCallStore(tmp_path / "owner.sqlite3")
    try:
        cache.remember(("21", "session", "generation"), {"call_id": "a"})
        cache.remember(("21", "session", "generation"), {"call_id": "b"})
        assert cache.owner("session", ["a", "b"]) == "21"
        assert cache.owner("other-session", ["a"]) is None
        assert cache.owner("session", ["a", "missing"]) is None
        cache.remember(("19", "session", "generation"), {"call_id": "a"})
        assert cache.owner("session", ["a"]) is None
        assert cache.owner("session", ["a", "b"]) == "21"
        cache.remember(("19", "session", "generation"), {"call_id": "c"})
        assert cache.owner("session", ["b", "c"]) is None
    finally:
        cache.close()


def test_history_owner_endpoint_auth_and_restart(tmp_path):
    from bridge import install_scoped_backend, request_scope
    module = SimpleNamespace()
    backend = SimpleNamespace(excel_upstream=module)
    path = tmp_path / "owner.sqlite3"
    install_scoped_backend(backend, cache_path=path)
    request_scope.set(("21", "session", "generation"))
    module._remember_native_call({"call_id": "a", "arguments": "secret"})
    module._excel_native_cache.close()
    install_scoped_backend(backend, cache_path=path)
    with TestClient(create_app(backend, KEY, "unused", scoped=True)) as client:
        body = {"session": "session", "call_ids": ["a"]}
        assert client.post("/internal/history-owner", json=body).status_code == 401
        result = client.post("/internal/history-owner", json=body, headers=AUTH)
        assert result.json() == {"account_id": "21"}
        assert "secret" not in result.text
        body["session"] = "other"
        assert client.post("/internal/history-owner", json=body, headers=AUTH).status_code == 409
        for bad in [[], {"session": "session", "call_ids": [{}]}, {"session": "", "call_ids": ["a"]}]:
            assert client.post("/internal/history-owner", json=bad, headers=AUTH).status_code == 400


class Store:
    def __init__(self):
        self.headers = {}

    def clear(self):
        self.headers = {}

    def configure(self, headers, **kwargs):
        if not headers or headers.get("authorization") == "expired":
            raise ValueError("invalid")
        self.headers = headers


@pytest.fixture
def setup(tmp_path):
    session = tmp_path / "session.json"
    session.write_text(json.dumps({"headers": {"authorization": "Bearer fake"}}))
    store = Store()
    calls = []

    async def handler(request, body):
        calls.append((body, dict(store.headers)))
        if body.get("stream"):
            async def stream():
                yield b'event: response.completed\ndata: {"type":"response.completed"}\n\n'
            return StreamingResponse(stream(), media_type="text/event-stream")
        return JSONResponse({"id": "resp_test", "model": body["model"], "output": []})

    backend = SimpleNamespace(
        excel_upstream=SimpleNamespace(
            excel_session_store=store,
            is_excel_model=lambda model: model == "test-excel",
            merge_local_models_payload=lambda _: {"data": [{"id": "test-excel"}]},
        ),
        _handle_excel_responses=handler,
    )
    with TestClient(create_app(backend, KEY, session)) as client:
        yield client, session, store, calls


def test_auth_and_management_isolation(setup):
    client, _, _, calls = setup
    assert client.get("/health").status_code == 200
    for path in ["/v1/models", "/v1/responses", "/session/status", "/api/config/excel-session"]:
        assert client.get(path).status_code == 401
    assert client.get("/v1/models", headers=AUTH).json()["data"][0]["id"] == "test-excel"
    for path in ["/ui", "/docs", "/openapi.json", "/api/config/excel-session"]:
        assert client.get(path, headers=AUTH).status_code == 404
    assert not calls


def test_missing_key_rejected():
    with pytest.raises(RuntimeError):
        create_app(None, "", "unused")


@pytest.mark.parametrize("body", [[], None, {"model": "copilot"}, {"model": "test-excel", "stream": "false"},
                                        {"model": "test-excel", "previous_response_id": "resp_old"}])
def test_invalid_request(setup, body):
    client, _, _, calls = setup
    assert client.post("/v1/responses", content=json.dumps(body), headers=AUTH).status_code == 400
    assert not calls


def test_bad_json_and_size_limit(setup):
    from bridge import request_body_limit
    client, _, _, calls = setup
    assert client.post("/v1/responses", content="{", headers=AUTH).status_code == 400
    result = client.post("/v1/responses", content=b"x" * (request_body_limit() + 1), headers=AUTH)
    assert result.status_code == 413
    assert f"{request_body_limit() // (1024 * 1024)} MiB" in result.text
    assert not calls


@pytest.mark.parametrize("size", [16 * 1024 * 1024 + 1, 48 * 1024 * 1024])
def test_large_request_reaches_backend(setup, size):
    client, _, _, calls = setup
    body = b'{"model":"test-excel","input":"hello"}'
    response = client.post("/v1/responses", content=body + b" " * (size - len(body)), headers=AUTH)
    assert response.status_code == 200
    assert calls[-1][0]["input"] == "hello"


@pytest.mark.parametrize("value", ["0", "-1", "abc", "1.5", ""])
def test_invalid_request_body_limit(monkeypatch, value):
    monkeypatch.setenv("EXCEL_MAX_REQUEST_BODY_SIZE_MB", value)
    with pytest.raises(RuntimeError, match="positive integer"):
        create_app(None, KEY, "unused")


def test_configured_request_body_limit(monkeypatch):
    from bridge import request_body_limit
    monkeypatch.setenv("EXCEL_MAX_REQUEST_BODY_SIZE_MB", " 64 ")
    assert request_body_limit() == 64 * 1024 * 1024
    monkeypatch.setenv("EXCEL_MAX_REQUEST_BODY_SIZE_MB", "1")
    backend = SimpleNamespace(excel_upstream=SimpleNamespace())
    with TestClient(create_app(backend, KEY, "unused")) as client:
        result = client.post("/v1/responses", content=b"x" * (1024 * 1024 + 1), headers=AUTH)
        assert result.status_code == 413
        assert "1 MiB" in result.text


def test_rotation_and_fail_closed(setup):
    client, path, store, calls = setup
    body = {"model": "test-excel", "input": "hi"}
    assert client.post("/v1/responses", json=body, headers=AUTH).status_code == 200
    replacement = path.with_suffix(".new")
    replacement.write_text(json.dumps({"headers": {"authorization": "Bearer rotated"}}))
    replacement.replace(path)
    assert client.post("/v1/responses", json=body, headers=AUTH).status_code == 200
    assert calls[-1][1]["authorization"] == "Bearer rotated"
    path.write_text("malformed")
    result = client.post("/v1/responses", json=body, headers=AUTH)
    assert result.status_code == 503
    assert not store.headers
    assert "rotated" not in result.text
    path.unlink()
    assert client.get("/session/status", headers=AUTH).json() == {"ready": False}


def test_stream_and_safe_status(setup):
    client, _, _, _ = setup
    response = client.post("/v1/responses", json={"model": "test-excel", "stream": True}, headers=AUTH)
    assert response.status_code == 200
    assert response.headers["content-type"].startswith("text/event-stream")
    assert "response.completed" in response.text
    assert client.get("/session/status", headers=AUTH).json() == {"ready": True}


@pytest.mark.parametrize("headers", [{"authorization": "expired"}, {"authorization": "Bearer a\r\nx: b"}])
def test_bad_session(setup, headers):
    client, path, _, calls = setup
    path.write_text(json.dumps({"headers": headers}))
    assert client.post("/v1/responses", json={"model": "test-excel"}, headers=AUTH).status_code == 503
    assert not calls


def test_scoped_native_cache_isolation():
    from bridge import install_scoped_backend, request_scope
    module = SimpleNamespace()
    install_scoped_backend(SimpleNamespace(excel_upstream=module))
    first = ("42", "conversation-a", "generation-1")
    request_scope.set(first)
    module._remember_native_call({"call_id": "same-id", "id": "native-id"})
    assert module._remembered_native_call("same-id")["id"] == "native-id"
    for other in [("43", "conversation-a", "generation-1"),
                  ("42", "conversation-b", "generation-1"),
                  ("42", "conversation-a", "generation-2")]:
        request_scope.set(other)
        assert module._remembered_native_call("same-id") is None
    request_scope.set(first)
    assert module._remembered_native_call("same-id")["id"] == "native-id"


def test_durable_cache_restart_and_isolation(tmp_path):
    from bridge import NativeCallStore
    path = tmp_path / "private" / "calls.sqlite3"
    scope = ("42", "conversation-a", "generation-1")
    item = {"call_id": "call-a", "id": "native-a", "arguments": "exact arguments"}
    first = NativeCallStore(path)
    first.remember(scope, item)
    first.close()
    second = NativeCallStore(path)
    try:
        assert second.recall(scope, "call-a") == item
        result = second.recall(scope, "call-a")
        result["arguments"] = "mutated"
        assert second.recall(scope, "call-a") == item
        for other, reason in [
            (("43", scope[1], scope[2]), "account_changed"),
            ((scope[0], "conversation-b", scope[2]), "scope_changed"),
            ((scope[0], scope[1], "generation-2"), "credential_changed"),
        ]:
            assert second.recall(other, "call-a") is None
            assert second.miss_reason(other, "call-a") == reason
        assert second.miss_reason(scope, "unknown") == "record_missing"
    finally:
        second.close()


def test_durable_cache_survives_global_4096_limit(tmp_path):
    from bridge import NativeCallStore
    cache = NativeCallStore(tmp_path / "calls.sqlite3")
    scope = ("42", "session", "generation")
    try:
        for i in range(4100):
            cache.remember(scope, {"call_id": str(i), "id": "native-" + str(i)})
        assert cache.recall(scope, "0")["id"] == "native-0"
        assert cache.recall(scope, "4099")["id"] == "native-4099"
    finally:
        cache.close()


def test_durable_cache_expires_idle_session_not_old_calls(tmp_path):
    from bridge import NativeCallStore
    now = [1000]
    cache = NativeCallStore(tmp_path / "calls.sqlite3", ttl_seconds=100, clock=lambda: now[0])
    scope = ("42", "session", "generation")
    try:
        cache.remember(scope, {"call_id": "old"})
        now[0] += 70
        assert cache.recall(scope, "old") is not None
        now[0] += 70
        assert cache.recall(scope, "old") is not None
        now[0] += 101
        assert cache.recall(scope, "old") is None
        assert cache.miss_reason(scope, "old") == "session_expired"
        cache.remember(scope, {"call_id": "new"})
        assert cache.recall(scope, "old") is None  # Cannot revive expired records.
        assert cache.recall(scope, "new") is not None
    finally:
        cache.close()


def test_durable_cache_shared_connections(tmp_path):
    from bridge import NativeCallStore
    first = NativeCallStore(tmp_path / "calls.sqlite3")
    second = NativeCallStore(tmp_path / "calls.sqlite3")
    scope = ("42", "session", "generation")
    try:
        first.remember(scope, {"call_id": "first"})
        assert second.recall(scope, "first") is not None
        second.remember(scope, {"call_id": "second"})
        assert first.recall(scope, "second") is not None
    finally:
        first.close()
        second.close()


@pytest.mark.parametrize("kind", ["function_call", "custom_tool_call", "function_call_output", "custom_tool_call_output"])
def test_scoped_http_history_survives_restart(tmp_path, kind, caplog):
    from bridge import install_scoped_backend

    class RequestStore(Store):
        def request_headers(self, **kwargs):
            return dict(self.headers)

    module = SimpleNamespace(ExcelSessionStore=RequestStore, is_excel_model=lambda _: True)
    calls = []

    async def handler(request, body):
        calls.append(body)
        module._remember_native_call({"call_id": "call-native", "id": "fc-native"})
        return JSONResponse({"output": []})

    backend = SimpleNamespace(excel_upstream=module, _handle_excel_responses=handler)
    headers = {**AUTH, "X-Excel-Account": "42", "X-Excel-Session": "stable-session",
               "X-Excel-Credential-Mode": "oauth", "X-Excel-Access-Token": "fake-secret",
               "X-Excel-ChatGPT-Account": "upstream-account"}
    path = tmp_path / "calls.sqlite3"
    install_scoped_backend(backend, cache_path=path)
    with TestClient(create_app(backend, KEY, "unused", scoped=True)) as client:
        assert client.post("/v1/responses", headers=headers, json={"model": "test-excel"}).status_code == 200
    install_scoped_backend(backend, cache_path=path)
    body = {"model": "test-excel", "input": [{"type": kind, "call_id": "call-native"}]}
    with TestClient(create_app(backend, KEY, "unused", scoped=True)) as client:
        assert client.post("/v1/responses", headers=headers, json=body).status_code == 200
        for name, value in [("X-Excel-Account", "43"), ("X-Excel-Session", "other"),
                            ("X-Excel-Access-Token", "rotated-secret")]:
            assert client.post("/v1/responses", headers={**headers, name: value}, json=body).status_code == 409
        for call_id in [None, [], {}, ""]:
            body["input"][0]["call_id"] = call_id
            assert client.post("/v1/responses", headers=headers, json=body).status_code == 400
    assert len(calls) == 2
    assert "account_changed" in caplog.text
    assert "credential_changed" in caplog.text
    assert "fake-secret" not in caplog.text
    assert "call-native" not in caplog.text


@pytest.mark.parametrize("scoped", [False, True])
@pytest.mark.parametrize("model", ["gpt-5.6-sol", "gpt-6-astra", "gpt-6-sol"])
@pytest.mark.parametrize("tool_fields", [
    {},
    {"tools": [{"type": "function", "name": "local_probe", "parameters": {"type": "object"}}]},
    {"tools": [{"type": "function", "name": "local_probe", "parameters": {"type": "object"}}], "tool_choice": "auto"},
    {"tools": [{"type": "function", "function": {"name": "local_probe", "parameters": {"type": "object"}}}]},
    {"tool_choice": "none"},
])
def test_pinned_backend_roundtrip(tmp_path, monkeypatch, scoped, tool_fields, model):
    """Optional integration test: real adapter code, mocked HTTP upstream."""
    source = os.environ.get("EXCEL_UPSTREAM_SOURCE")
    if not source:
        pytest.skip("Set EXCEL_UPSTREAM_SOURCE to the pinned ghcp_proxy checkout")
    sys.path.insert(0, source)
    for name in ("CONFIG", "STATE", "CACHE"):
        monkeypatch.setenv("GHCP_" + name + "_DIR", str(tmp_path / name))
    monkeypatch.setenv("GHCP_RESPONSES_UPSTREAM", "rest")
    monkeypatch.setenv("GHCP_UPSTREAM_TLS_VERIFY", "1")
    backend = importlib.import_module("proxy")
    import httpx
    from bridge import production_app

    token = base64.urlsafe_b64encode(json.dumps({"exp": 4102444800}).encode()).decode().rstrip("=")
    path = tmp_path / "session.json"
    path.write_text(json.dumps({"headers": {
        "authorization": "Bearer test." + token + ".signature",
        "chatgpt-account-id": "test-account",
    }}))
    monkeypatch.setenv("EXCEL_SESSION_FILE", str(path))
    monkeypatch.setenv("EXCEL_BRIDGE_API_KEY", KEY)
    monkeypatch.setenv("EXCEL_ACCOUNT_ROUTES", "1" if scoped else "0")
    app = production_app()
    captured = []

    def upstream(request):
        body = json.loads(request.content)
        captured.append(body)
        # Inspect the actual final HTTP payload, not just an adapter helper.
        assert "tools" not in body
        assert "tool_choice" not in body
        if tool_fields.get("tools", [{}])[0].get("name") == "local_probe":
            catalog = json.dumps(body["input"])
            assert "local_probe" in catalog
            assert "run_officejs" in catalog
        assert body["model"] == model
        assert request.headers["chatgpt-account-id"] == "test-account"
        assert KEY not in request.headers["authorization"]
        response = {"id": "resp_mock", "object": "response", "model": body["model"],
                    "status": "completed", "output": [{"id": "msg_mock", "type": "message",
                    "role": "assistant", "content": [{"type": "output_text", "text": "hello"}]}],
                    "usage": {"input_tokens": 10, "output_tokens": 2, "total_tokens": 12}}
        if body.get("stream"):
            payload = json.dumps({"type": "response.completed", "response": response})
            return httpx.Response(200, headers={"content-type": "text/event-stream"},
                                  content=f"event: response.completed\ndata: {payload}\n\n".encode())
        return httpx.Response(200, json=response)

    backend._EXCEL_UPSTREAM_CLIENT = httpx.AsyncClient(transport=httpx.MockTransport(upstream))
    with TestClient(app) as client:
        assert model + "-excel" in {item["id"] for item in client.get("/v1/models", headers=AUTH).json()["data"]}
        headers = dict(AUTH)
        if scoped:
            headers.update({
                "X-Excel-Account": "42", "X-Excel-Session": "stable-session",
                "X-Excel-Credential-Mode": "oauth",
                "X-Excel-Access-Token": "test." + token + ".signature",
                "X-Excel-ChatGPT-Account": "test-account",
            })
        for streaming in (False, True):
            result = client.post("/v1/responses", headers=headers, json={
                "model": model + "-excel", "input": "hi", "stream": streaming,
                **tool_fields})
            assert result.status_code == 200, result.text
            assert "hello" in result.text
        assert len(captured) == 2


@pytest.mark.parametrize("durable", [False, True])
@pytest.mark.parametrize("kind", ["function", "custom"])
@pytest.mark.parametrize("compacted", [False, True])
def test_native_officejs_identity_restored_for_tool_output(kind, durable, tmp_path, compacted):
    source = os.environ.get("EXCEL_UPSTREAM_SOURCE")
    if not source:
        pytest.skip("Set EXCEL_UPSTREAM_SOURCE to the pinned ghcp_proxy checkout")
    sys.path.insert(0, source)
    module = importlib.import_module("excel_upstream")
    from bridge import install_scoped_backend, request_scope
    backend = SimpleNamespace(excel_upstream=module)
    cache_path = tmp_path / "native.sqlite3" if durable else None
    install_scoped_backend(backend, cache_path=cache_path)
    request_scope.set(("account", "session", "generation"))
    tool = {"type": kind, "name": "local_tool"}
    envelope = {"name": "local_tool"}
    if kind == "function":
        tool["parameters"] = {"type": "object", "properties": {"value": {"type": "string"}}}
        envelope["arguments"] = {"value": "test"}
    else:
        envelope["input"] = "test"
    native = {
        "type": "function_call", "id": "fc_original", "call_id": "call_original",
        "name": "run_officejs",
        "arguments": json.dumps({"code": json.dumps(envelope)}),
        "status": "completed",
    }
    client = module.extract_native_client_tool_call({"output": [native]}, {"tools": [tool]})
    assert client is not None
    assert client["name"] == "local_tool"
    assert client["call_id"] == native["call_id"]
    if durable:
        module._excel_native_cache.close()
        install_scoped_backend(backend, cache_path=cache_path)
    # Codex commonly omits the original item ID when replaying its local call.
    client.pop("id", None)
    output = {
        "type": "function_call_output" if kind == "function" else "custom_tool_call_output",
        "call_id": client["call_id"], "output": "local execution result",
    }
    compact = {"type": "compaction", "encrypted_content": "opaque-state"}
    wire = module.prepare_responses_body({
        "model": "gpt-5.6-sol-excel", "tools": [tool],
        "input": [compact, output] if compacted else [
            {"role": "user", "content": "run tool"}, client, output],
    })
    if compacted:
        assert compact in wire["input"]
    calls = [item for item in wire["input"] if item.get("type") == "function_call"]
    results = [item for item in wire["input"] if item.get("type") == "function_call_output"]
    assert calls == [native]  # Exact name, arguments, native item ID and call_id.
    assert len(results) == 1
    assert results[0]["call_id"] == native["call_id"]
    assert results[0]["output"] == "local execution result"
    if durable:
        module._excel_native_cache.close()

import importlib
import os
import sys
import pytest

def test_migration_keeps_original_turn(tmp_path):
    sys.path.insert(0, os.environ['EXCEL_UPSTREAM_SOURCE'])
    u = importlib.import_module('excel_upstream')
    import bridge
    cache = bridge.NativeCallStore(tmp_path / 'turn.db')
    history = [{'role': 'user', 'content': 'work'}]
    states = []
    try:
        for n in range(2):
            history += [{'type': 'function_call', 'call_id': str(n), 'name': 'probe', 'arguments': '{}'},
                        {'type': 'function_call_output', 'call_id': str(n), 'output': 'done'}]
            state = u._agent_turn_state(history)
            body = bridge.migrate_completed_history({'input': list(history), 'prompt_cache_key': 'stable'}, cache, ('21','session','generation'))
            bridge.reset_client_turn_state(body)
            bridge.restore_logical_turn_state(body, state)
            states.append(u.prepare_responses_body(body)['metadata'])
        assert states[0]['turn_id'] == states[1]['turn_id']
        assert [s['agent_iteration'] for s in states] == ['2', '3']
        assert u.prepare_responses_body(body)['metadata'] == states[-1]
        history.append({'role': 'user', 'content': 'next'})
        body = {'input': history, 'prompt_cache_key': 'stable', 'metadata': {'turn_id': 'forged', 'customer': 'keep'}}
        bridge.reset_client_turn_state(body)
        bridge.restore_logical_turn_state(body, u._agent_turn_state(history))
        new = u.prepare_responses_body(bridge.replay_checkpoint(body, cache, ('21','session','generation')))['metadata']
        assert new['turn_id'] != states[-1]['turn_id'] and new['turn_id'] != 'forged'
        assert new['task_id'] == states[-1]['task_id']
        assert new['agent_iteration'] == '1' and new['customer'] == 'keep'
    finally:
        cache.close()

