"""Pluggable persistence for the gateway.

Mirrors the Go ``store.Store`` (internal/lsp2/store): pairings, devices,
idempotency keys, tasks with their events, and secrets such as the gateway
signing seed. The interface is synchronous so the engine can call it inline;
live WebSocket sessions stay in-process, so one Store serves one gateway
instance (persistence across restarts).
"""
from __future__ import annotations

import base64
import datetime
import json
import sqlite3
import threading
from typing import Any, Optional, Protocol, runtime_checkable

DeviceAuth = dict  # code_hash, user_code, tenant, pub (bytes), name, status, expires (unix s), interval, last_poll, device_id, owner
Device = dict      # id, tenant, owner, name, pub (bytes), status ("active"|"revoked"), created (datetime), e2ee_key
TaskRecord = dict  # id, tenant, owner, device, model, state, events: list[TaskEvent]
TaskEvent = dict   # seq, type, data


@runtime_checkable
class Store(Protocol):
    def get_secret(self, name: str) -> Optional[bytes]: ...
    def put_secret(self, name: str, value: bytes) -> None: ...

    def put_device_auth(self, a: DeviceAuth) -> None: ...
    def get_device_auth(self, code_hash: str) -> Optional[DeviceAuth]: ...
    def device_auth_by_user_code(self, tenant: str, user_code: str) -> Optional[DeviceAuth]: ...

    def put_device(self, d: Device) -> None: ...
    def get_device(self, id: str) -> Optional[Device]: ...
    def list_devices(self, tenant: str, owner: Optional[str] = None) -> list[Device]: ...

    def get_idempotency(self, key: str) -> Optional[dict]: ...
    def put_idempotency(self, key: str, rec: dict) -> None: ...

    def put_task(self, t: TaskRecord) -> None: ...
    def get_task(self, id: str) -> Optional[TaskRecord]: ...
    def append_event(self, task_id: str, ev: TaskEvent) -> None: ...

    def close(self) -> None: ...


class MemoryStore:
    """In-memory store (the default). State is lost on restart."""

    def __init__(self) -> None:
        self.secrets: dict[str, bytes] = {}
        self.auths: dict[str, DeviceAuth] = {}
        self.devices: dict[str, Device] = {}
        self.idem: dict[str, dict] = {}
        self.tasks: dict[str, TaskRecord] = {}

    def get_secret(self, name):
        return self.secrets.get(name)

    def put_secret(self, name, value):
        self.secrets[name] = value

    def put_device_auth(self, a):
        self.auths[a["code_hash"]] = a

    def get_device_auth(self, code_hash):
        return self.auths.get(code_hash)

    def device_auth_by_user_code(self, tenant, user_code):
        return next((a for a in self.auths.values() if a["tenant"] == tenant and a["user_code"] == user_code), None)

    def put_device(self, d):
        self.devices[d["id"]] = d

    def get_device(self, id):
        return self.devices.get(id)

    def list_devices(self, tenant, owner=None):
        return [d for d in self.devices.values() if d["tenant"] == tenant and (owner is None or d["owner"] == owner)]

    def get_idempotency(self, key):
        return self.idem.get(key)

    def put_idempotency(self, key, rec):
        self.idem.setdefault(key, rec)

    def put_task(self, t):
        cur = self.tasks.get(t["id"])
        if cur is None:
            self.tasks[t["id"]] = t
        elif cur is not t:
            cur.update({k: v for k, v in t.items() if k != "events"})

    def get_task(self, id):
        return self.tasks.get(id)

    def append_event(self, task_id, ev):
        t = self.tasks.get(task_id)
        if t is not None and not any(e is ev for e in t["events"]):
            t["events"].append(ev)

    def close(self):
        pass


def _enc(v: Any) -> str:
    def default(x):
        if isinstance(x, (bytes, bytearray)):
            return {"$b64": base64.b64encode(x).decode()}
        if isinstance(x, datetime.datetime):
            return {"$dt": x.isoformat()}
        raise TypeError(type(x))
    return json.dumps(v, default=default, separators=(",", ":"))


def _dec(s: str) -> Any:
    def hook(o):
        if "$b64" in o and len(o) == 1:
            return base64.b64decode(o["$b64"])
        if "$dt" in o and len(o) == 1:
            return datetime.datetime.fromisoformat(o["$dt"])
        return o
    return json.loads(s, object_hook=hook)


class SQLiteStore:
    """SQLite store (stdlib sqlite3). Records are stored as JSON documents."""

    def __init__(self, path: str = ":memory:") -> None:
        self.db = sqlite3.connect(path, check_same_thread=False, isolation_level=None)
        self._lock = threading.Lock()
        self.db.execute("PRAGMA journal_mode=WAL")
        self.db.execute("PRAGMA busy_timeout=5000")
        self.db.executescript("""
          CREATE TABLE IF NOT EXISTS lsp2_secrets (name TEXT PRIMARY KEY, value BLOB NOT NULL);
          CREATE TABLE IF NOT EXISTS lsp2_device_auths (code_hash TEXT PRIMARY KEY, tenant TEXT NOT NULL, user_code TEXT NOT NULL, doc TEXT NOT NULL);
          CREATE INDEX IF NOT EXISTS lsp2_device_auths_code ON lsp2_device_auths (tenant, user_code);
          CREATE TABLE IF NOT EXISTS lsp2_devices (id TEXT PRIMARY KEY, tenant TEXT NOT NULL, owner TEXT NOT NULL, doc TEXT NOT NULL);
          CREATE INDEX IF NOT EXISTS lsp2_devices_owner ON lsp2_devices (tenant, owner);
          CREATE TABLE IF NOT EXISTS lsp2_idempotency (key TEXT PRIMARY KEY, doc TEXT NOT NULL);
          CREATE TABLE IF NOT EXISTS lsp2_tasks (id TEXT PRIMARY KEY, doc TEXT NOT NULL);
          CREATE TABLE IF NOT EXISTS lsp2_events (task_id TEXT NOT NULL, seq INTEGER NOT NULL, doc TEXT NOT NULL, PRIMARY KEY (task_id, seq));
        """)

    def _x(self, sql: str, *a: Any) -> list:
        with self._lock:
            return self.db.execute(sql, a).fetchall()

    def _one(self, sql: str, *a: Any) -> Any:
        r = self._x(sql, *a)
        return _dec(r[0][0]) if r else None

    def get_secret(self, name):
        r = self._x("SELECT value FROM lsp2_secrets WHERE name = ?", name)
        return bytes(r[0][0]) if r else None

    def put_secret(self, name, value):
        self._x("INSERT OR REPLACE INTO lsp2_secrets (name, value) VALUES (?, ?)", name, bytes(value))

    def put_device_auth(self, a):
        self._x("INSERT OR REPLACE INTO lsp2_device_auths (code_hash, tenant, user_code, doc) VALUES (?, ?, ?, ?)",
                a["code_hash"], a["tenant"], a["user_code"], _enc(a))

    def get_device_auth(self, code_hash):
        return self._one("SELECT doc FROM lsp2_device_auths WHERE code_hash = ?", code_hash)

    def device_auth_by_user_code(self, tenant, user_code):
        return self._one("SELECT doc FROM lsp2_device_auths WHERE tenant = ? AND user_code = ? ORDER BY rowid DESC LIMIT 1", tenant, user_code)

    def put_device(self, d):
        self._x("INSERT OR REPLACE INTO lsp2_devices (id, tenant, owner, doc) VALUES (?, ?, ?, ?)", d["id"], d["tenant"], d["owner"], _enc(d))

    def get_device(self, id):
        return self._one("SELECT doc FROM lsp2_devices WHERE id = ?", id)

    def list_devices(self, tenant, owner=None):
        if owner is None:
            rows = self._x("SELECT doc FROM lsp2_devices WHERE tenant = ? ORDER BY rowid", tenant)
        else:
            rows = self._x("SELECT doc FROM lsp2_devices WHERE tenant = ? AND owner = ? ORDER BY rowid", tenant, owner)
        return [_dec(r[0]) for r in rows]

    def get_idempotency(self, key):
        return self._one("SELECT doc FROM lsp2_idempotency WHERE key = ?", key)

    def put_idempotency(self, key, rec):
        self._x("INSERT OR IGNORE INTO lsp2_idempotency (key, doc) VALUES (?, ?)", key, _enc(rec))

    def put_task(self, t):
        self._x("INSERT OR REPLACE INTO lsp2_tasks (id, doc) VALUES (?, ?)", t["id"], _enc({**t, "events": []}))

    def get_task(self, id):
        t = self._one("SELECT doc FROM lsp2_tasks WHERE id = ?", id)
        if t is not None:
            t["events"] = [_dec(r[0]) for r in self._x("SELECT doc FROM lsp2_events WHERE task_id = ? ORDER BY seq", id)]
        return t

    def append_event(self, task_id, ev):
        self._x("INSERT OR IGNORE INTO lsp2_events (task_id, seq, doc) VALUES (?, ?, ?)", task_id, ev["seq"], _enc(ev))

    def close(self):
        with self._lock:
            self.db.close()
