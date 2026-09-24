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

KEY = "test-only-bridge-key-" + "x" * 32
AUTH = {"Authorization": "Bearer " + KEY}


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
    client, _, _, calls = setup
    assert client.post("/v1/responses", content="{", headers=AUTH).status_code == 400
    assert client.post("/v1/responses", content=b"x" * (16 * 1024 * 1024 + 1), headers=AUTH).status_code == 413
    assert not calls


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


@pytest.mark.parametrize("kind", ["function", "custom"])
def test_native_officejs_identity_restored_for_tool_output(kind):
    source = os.environ.get("EXCEL_UPSTREAM_SOURCE")
    if not source:
        pytest.skip("Set EXCEL_UPSTREAM_SOURCE to the pinned ghcp_proxy checkout")
    sys.path.insert(0, source)
    module = importlib.import_module("excel_upstream")
    from bridge import install_scoped_backend, request_scope
    install_scoped_backend(SimpleNamespace(excel_upstream=module))
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
    # Codex commonly omits the original item ID when replaying its local call.
    client.pop("id", None)
    output = {
        "type": "function_call_output" if kind == "function" else "custom_tool_call_output",
        "call_id": client["call_id"], "output": "local execution result",
    }
    wire = module.prepare_responses_body({
        "model": "gpt-5.6-sol-excel", "tools": [tool],
        "input": [{"role": "user", "content": "run tool"}, client, output],
    })
    calls = [item for item in wire["input"] if item.get("type") == "function_call"]
    results = [item for item in wire["input"] if item.get("type") == "function_call_output"]
    assert calls == [native]  # Exact name, arguments, native item ID and call_id.
    assert len(results) == 1
    assert results[0]["call_id"] == native["call_id"]
    assert results[0]["output"] == "local execution result"
