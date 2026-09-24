"""Linux-only deployment adapter for the pinned GHCP Excel Responses bridge.

Deliberately does not mount proxy.app: no desktop lifecycle, dashboard,
credential mutation API, Copilot routes, or automatic updates are exposed.
"""

import hmac
import json
import os
import copy
import hashlib
import logging
import sqlite3
import threading
import time
from collections import OrderedDict
from contextvars import ContextVar
from contextlib import asynccontextmanager
from pathlib import Path

from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse

MAX_BODY = 16 * 1024 * 1024
MAX_SESSION = 128 * 1024
request_store = ContextVar("excel_store")
request_scope = ContextVar("excel_scope")
logger = logging.getLogger("excel_bridge")


class NativeCallStore:
    """Keep exact native calls across restarts; expire whole idle sessions."""

    def __init__(self, path, ttl_seconds=7 * 86400, clock=time.time):
        if ttl_seconds <= 0:
            raise ValueError("Excel history TTL must be positive")
        path = Path(path)
        path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        fd = os.open(path, os.O_CREAT | os.O_RDWR, 0o600)
        os.close(fd)
        os.chmod(path, 0o600)
        self.lock = threading.Lock()
        self.clock = clock
        self.ttl = ttl_seconds
        self.next_prune = 0
        self.db = sqlite3.connect(str(path), timeout=10, check_same_thread=False)
        self.db.execute("PRAGMA journal_mode=WAL")
        self.db.execute("PRAGMA foreign_keys=ON")
        self.db.executescript("""
            CREATE TABLE IF NOT EXISTS sessions (
                scope TEXT PRIMARY KEY, touched REAL NOT NULL
            );
            CREATE TABLE IF NOT EXISTS calls (
                scope TEXT NOT NULL REFERENCES sessions(scope) ON DELETE CASCADE,
                call_id TEXT NOT NULL, item TEXT NOT NULL,
                PRIMARY KEY (scope, call_id)
            );
            CREATE INDEX IF NOT EXISTS sessions_touched ON sessions(touched);
            CREATE INDEX IF NOT EXISTS calls_id ON calls(call_id);
        """)

    @staticmethod
    def key(scope):
        return json.dumps(scope, separators=(",", ":"))

    def remember(self, scope, item):
        key, now = self.key(scope), self.clock()
        payload = json.dumps(item, ensure_ascii=False, separators=(",", ":"))
        with self.lock, self.db:
            if now >= self.next_prune:
                self.db.execute("DELETE FROM sessions WHERE touched <= ?", (now - self.ttl,))
                self.next_prune = now + 3600
            self.db.execute("DELETE FROM sessions WHERE scope = ? AND touched <= ?",
                            (key, now - self.ttl))
            self.db.execute("INSERT INTO sessions VALUES (?, ?) ON CONFLICT(scope) "
                            "DO UPDATE SET touched=excluded.touched", (key, now))
            self.db.execute("INSERT INTO calls VALUES (?, ?, ?) ON CONFLICT(scope, call_id) "
                            "DO UPDATE SET item=excluded.item", (key, item["call_id"], payload))

    def recall(self, scope, call_id):
        key, now = self.key(scope), self.clock()
        with self.lock, self.db:
            row = self.db.execute(
                "SELECT c.item, s.touched FROM calls c JOIN sessions s USING(scope) "
                "WHERE c.scope = ? AND c.call_id = ? AND s.touched > ?",
                (key, call_id, now - self.ttl),
            ).fetchone()
            if row is None:
                return None
            if now - row[1] >= min(60, self.ttl / 2):
                self.db.execute("UPDATE sessions SET touched = ? WHERE scope = ?", (now, key))
            return json.loads(row[0])

    def miss_reason(self, scope, call_id):
        with self.lock:
            rows = self.db.execute(
                "SELECT c.scope, s.touched FROM calls c JOIN sessions s USING(scope) "
                "WHERE c.call_id = ?", (call_id,),
            ).fetchall()
        for key, touched in rows:
            other = tuple(json.loads(key))
            if other == tuple(scope):
                return "session_expired" if touched <= self.clock() - self.ttl else "unknown"
            if other[:2] == tuple(scope[:2]):
                return "credential_changed"
        for key, _ in rows:
            other = tuple(json.loads(key))
            if other[1] == scope[1] and other[0] != scope[0]:
                return "account_changed"
        return "scope_changed" if rows else "record_missing"

    def close(self):
        with self.lock:
            self.db.close()


class ScopedStore:
    def __getattr__(self, name):
        return getattr(request_store.get(), name)


def install_scoped_backend(backend, cache_path=None, ttl_seconds=7 * 86400):
    """Replace upstream globals with request-local credentials and scoped cache."""
    module = backend.excel_upstream
    module.excel_session_store = ScopedStore()
    cache = OrderedDict()
    lock = threading.Lock()
    disk = NativeCallStore(cache_path, ttl_seconds) if cache_path else None
    module._excel_native_cache = disk

    def remember(item):
        call_id = item.get("call_id")
        if not isinstance(call_id, str) or not call_id:
            return
        if disk is not None:
            disk.remember(request_scope.get(), item)
            return
        key = (request_scope.get(), call_id)
        with lock:
            cache[key] = copy.deepcopy(item)
            cache.move_to_end(key)
            while len(cache) > 4096:
                cache.popitem(last=False)

    def recall(call_id):
        if not isinstance(call_id, str) or not call_id:
            return None
        if disk is not None:
            return disk.recall(request_scope.get(), call_id)
        key = (request_scope.get(), call_id)
        with lock:
            value = cache.get(key)
            if value is not None:
                cache.move_to_end(key)
            return copy.deepcopy(value)

    module._remember_native_call = remember
    module._remembered_native_call = recall


def error(status, message):
    return JSONResponse(
        {"error": {"message": message, "type": "invalid_request_error"}},
        status_code=status,
    )


def load_session(path, backend):
    # Reload every request so rotated directory-mounted secrets take effect.
    # Fail closed: never silently continue using a previously loaded session.
    backend.excel_upstream.excel_session_store.clear()
    try:
        with Path(path).open("rb") as handle:
            raw = handle.read(MAX_SESSION + 1)
        if len(raw) > MAX_SESSION:
            raise ValueError("oversized session")
        payload = json.loads(raw)
        if not isinstance(payload, dict):
            raise ValueError("invalid session")
        headers = payload.get("headers")
        if not isinstance(headers, dict) or any(
            not isinstance(value, str) or "\r" in value or "\n" in value
            for value in headers.values()
        ):
            raise ValueError("invalid headers")
        backend.excel_upstream.excel_session_store.configure(
            headers, tools_version_id=payload.get("tools_version_id"), persist=False
        )
    except (OSError, ValueError, UnicodeError):
        return False
    return True


def create_app(backend, key, session_path, *, scoped=False):
    if not key or len(key) < 32:
        raise RuntimeError("EXCEL_BRIDGE_API_KEY must contain at least 32 characters")

    @asynccontextmanager
    async def lifespan(app):
        yield
        client = getattr(backend, "_EXCEL_UPSTREAM_CLIENT", None)
        if client is not None:
            await client.aclose()
            backend._EXCEL_UPSTREAM_CLIENT = None
        cache = getattr(backend.excel_upstream, "_excel_native_cache", None)
        if cache is not None:
            cache.close()

    app = FastAPI(docs_url=None, redoc_url=None, openapi_url=None, lifespan=lifespan)

    @app.get("/health")
    async def health():
        # Liveness only; intentionally does not reveal credential status.
        return {"status": "ok"}

    @app.get("/v1/models")
    async def models():
        return backend.excel_upstream.merge_local_models_payload({})

    @app.get("/session/status")
    async def session_status():
        if scoped:
            return error(400, "Account-scoped sessions are validated on requests")
        ready = load_session(session_path, backend)
        return JSONResponse({"ready": ready}, status_code=200 if ready else 503)

    @app.post("/v1/responses")
    async def responses(request: Request):
        raw = bytearray()
        async for chunk in request.stream():
            raw.extend(chunk)
            if len(raw) > MAX_BODY:
                return error(413, "Request body exceeds 16 MiB")
        try:
            body = json.loads(raw)
        except (ValueError, UnicodeError):
            return error(400, "Invalid JSON request")
        if not isinstance(body, dict):
            return error(400, "Request must be a JSON object")
        if not backend.excel_upstream.is_excel_model(body.get("model")):
            return error(400, "Select an Excel model returned by /v1/models")
        if not isinstance(body.get("stream", False), bool):
            return error(400, "stream must be a boolean")
        if body.get("previous_response_id"):
            return error(400, "Excel requires full input history, not previous_response_id")
        if scoped:
            account = request.headers.get("x-excel-account", "")
            scope = request.headers.get("x-excel-session", "")
            mode = request.headers.get("x-excel-credential-mode")
            if not account.isdecimal() or not scope or len(scope) > 128:
                return error(400, "Account and session identity are required")
            store = backend.excel_upstream.ExcelSessionStore()
            request_store.set(store)
            if mode == "oauth":
                try:
                    store.configure({
                        "authorization": "Bearer " + request.headers.get("x-excel-access-token", ""),
                        "chatgpt-account-id": request.headers.get("x-excel-chatgpt-account", ""),
                    }, persist=False)
                except ValueError:
                    return error(503, "OAuth credential unavailable or expired")
            elif mode == "session_file":
                if not load_session(str(Path(session_path).parent / (account + ".json")), backend):
                    return error(503, "Account Excel session unavailable or expired")
            else:
                return error(400, "Unknown Excel credential mode")
            headers = store.request_headers(stream=True)
            expected_account = request.headers.get("x-excel-chatgpt-account", "")
            if not expected_account or headers["chatgpt-account-id"] != expected_account:
                return error(409, "Excel session does not match the selected OAuth account")
            generation = hashlib.sha256(headers["authorization"].encode()).hexdigest()
            request_scope.set((account, scope, generation))
            # Never fabricate native identities. Durable records survive restarts,
            # while credential, account and session isolation remain intact.
            for item in body.get("input", []) if isinstance(body.get("input"), list) else []:
                if isinstance(item, dict) and item.get("type") in {
                    "function_call", "custom_tool_call", "function_call_output", "custom_tool_call_output"
                }:
                    call_id = item.get("call_id")
                    if not isinstance(call_id, str) or not call_id:
                        return error(400, "Tool history requires a non-empty string call_id")
                    if backend.excel_upstream._remembered_native_call(call_id) is None:
                        cache = getattr(backend.excel_upstream, "_excel_native_cache", None)
                        reason = cache.miss_reason(request_scope.get(), call_id) if cache else "memory_record_missing"
                        logger.warning(
                            "excel_history_miss account=%s scope_hash=%s call_hash=%s reason=%s",
                            account, hashlib.sha256(scope.encode()).hexdigest()[:12],
                            hashlib.sha256(call_id.encode()).hexdigest()[:12], reason,
                        )
                        return error(409, "Excel tool history expired or belongs to another session; start a new conversation")
            body["prompt_cache_key"] = hashlib.sha256(
                (account + ":" + scope).encode()
            ).hexdigest()
            # Derive iteration from current history rather than trusting stale
            # caller state. Preserve other metadata and explicit task/turn IDs.
            metadata = body.get("metadata")
            if isinstance(metadata, dict):
                metadata.pop("agent_iteration", None)
        elif not load_session(session_path, backend):
            return error(503, "Excel session unavailable or expired; replace session.json")
        # The upstream handler obtains a snapshot of session headers before
        # its first await. One worker preserves its native tool-call cache.
        response = await backend._handle_excel_responses(request, body)
        return response

    class AuthenticatedApp:
        async def __call__(self, scope, receive, send):
            if scope["type"] == "websocket":
                await send({"type": "websocket.close", "code": 1008})
                return
            if scope["type"] == "http" and scope["path"] != "/health":
                headers = dict(scope.get("headers", []))
                supplied = headers.get(b"authorization", b"")
                if not hmac.compare_digest(supplied, ("Bearer " + key).encode()):
                    await error(401, "Invalid bridge API key")(scope, receive, send)
                    return
            await app(scope, receive, send)

    return AuthenticatedApp()


def production_app():
    # Import only in the factory, after setting isolated runtime directories.
    # Do not load the desktop application's startup/shutdown event handlers.
    import proxy

    # Extend the pinned adapter's routing table without overriding other models.
    proxy.excel_upstream.EXCEL_MODEL_UPSTREAMS["gpt-6-astra-excel"] = "gpt-6-astra"
    proxy.excel_upstream.EXCEL_MODEL_UPSTREAMS["gpt-6-sol-excel"] = "gpt-6-sol"
    proxy.excel_upstream.MODEL_IDS = tuple(proxy.excel_upstream.EXCEL_MODEL_UPSTREAMS)

    # This adapter always uses explicitly supplied session headers, on every OS.
    scoped = os.environ.get("EXCEL_ACCOUNT_ROUTES", "0") == "1"
    if scoped:
        cache_path = os.environ.get("EXCEL_TOOL_CACHE_PATH")
        ttl_seconds = int(os.environ.get("EXCEL_TOOL_CACHE_TTL_SECONDS", str(7 * 86400)))
        install_scoped_backend(proxy, cache_path=cache_path, ttl_seconds=ttl_seconds)
        if not cache_path:
            logger.warning("Excel history is memory-only; configure EXCEL_TOOL_CACHE_PATH for restart safety")
    else:
        proxy.excel_upstream.excel_session_store = proxy.excel_upstream.ExcelSessionStore()
    proxy.excel_session_capture.refresh_macos_excel_session = lambda *a, **kw: None
    proxy.excel_session_capture.refresh_windows_excel_session = lambda *a, **kw: None
    return create_app(
        proxy,
        os.environ.get("EXCEL_BRIDGE_API_KEY", ""),
        os.environ.get("EXCEL_SESSION_FILE", "/run/excel/session.json"),
        scoped=scoped,
    )
