"""Pluggable persistence for the gateway.

Mirrors the Go ``store.Store`` (internal/lmbp/store): pairings, devices,
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


class StoreConflict(Exception):
    """A seq or a terminal state already exists."""


class StoreNotFound(Exception):
    """The task does not exist."""


TERMINAL_STATES = frozenset({"SUCCEEDED", "FAILED", "CANCELLED"})


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

    # Records are keyed by the (tenant, owner, key) triple; never join the parts ambiguously.
    def get_idempotency(self, tenant: str, owner: str, key: str) -> Optional[dict]: ...
    def create_task(self, t: TaskRecord, idem: Optional[dict] = None) -> None:
        """Store a new task and, when ``idem`` ({tenant, owner, key, rec}) is
        given, its Idempotency-Key record, atomically: both or neither, so a key
        never exists without its task. Raises StoreConflict if the key is taken
        (then nothing is written)."""

    def put_task(self, t: TaskRecord) -> None: ...
    def get_task(self, id: str) -> Optional[TaskRecord]: ...
    # append_event stores a non-terminal event; raises StoreConflict if the
    # seq is already stored (a seq is never reused).
    def append_event(self, task_id: str, ev: TaskEvent) -> None: ...
    # finish_task atomically stores the terminal event (task.done/task.error)
    # AND the terminal state (SUCCEEDED/FAILED/CANCELLED): both or neither.
    # Raises StoreConflict if the seq is taken or the task already has a
    # terminal state, StoreNotFound if the task does not exist. put_task is for
    # non-terminal states only. Recovery relies on this: a stored terminal
    # event always agrees with the stored state.
    def finish_task(self, task_id: str, ev: TaskEvent, state: str) -> None: ...

    def close(self) -> None: ...


class MemoryStore:
    """In-memory store (the default). State is lost on restart."""

    def __init__(self) -> None:
        self.secrets: dict[str, bytes] = {}
        self.auths: dict[str, DeviceAuth] = {}
        self.devices: dict[str, Device] = {}
        self.idem: dict[tuple[str, str, str], dict] = {}
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

    def get_idempotency(self, tenant, owner, key):
        return self.idem.get((tenant, owner, key))

    def create_task(self, t, idem=None):
        if t["id"] in self.tasks:
            raise StoreConflict(f"task {t['id']} exists")
        if idem is not None:
            k = (idem["tenant"], idem["owner"], idem["key"])
            if k in self.idem:
                raise StoreConflict("idempotency key taken")
            self.idem[k] = idem["rec"]
        self.tasks[t["id"]] = t

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
        if t is None:
            return
        if any(e["seq"] == ev["seq"] for e in t["events"]):
            raise StoreConflict(f"task {task_id}: seq {ev['seq']} already stored")
        t["events"].append(ev)

    def finish_task(self, task_id, ev, state):
        t = self.tasks.get(task_id)
        if t is None:
            raise StoreNotFound(f"task {task_id} not found")
        if t.get("state") in TERMINAL_STATES:
            raise StoreConflict(f"task {task_id} already {t['state']}")
        if any(e["seq"] == ev["seq"] for e in t["events"]):
            raise StoreConflict(f"task {task_id}: seq {ev['seq']} already stored")
        # One synchronous step: event and state change together.
        t["events"].append(ev)
        t["state"] = state

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
          CREATE TABLE IF NOT EXISTS lmbp_secrets (name TEXT PRIMARY KEY, value BLOB NOT NULL);
          CREATE TABLE IF NOT EXISTS lmbp_device_auths (code_hash TEXT PRIMARY KEY, tenant TEXT NOT NULL, user_code TEXT NOT NULL, doc TEXT NOT NULL);
          CREATE INDEX IF NOT EXISTS lmbp_device_auths_code ON lmbp_device_auths (tenant, user_code);
          CREATE TABLE IF NOT EXISTS lmbp_devices (id TEXT PRIMARY KEY, tenant TEXT NOT NULL, owner TEXT NOT NULL, doc TEXT NOT NULL);
          CREATE INDEX IF NOT EXISTS lmbp_devices_owner ON lmbp_devices (tenant, owner);
          CREATE TABLE IF NOT EXISTS lmbp_idempotency (tenant TEXT NOT NULL, owner TEXT NOT NULL, idem_key TEXT NOT NULL, doc TEXT NOT NULL, PRIMARY KEY (tenant, owner, idem_key));
          CREATE TABLE IF NOT EXISTS lmbp_tasks (id TEXT PRIMARY KEY, doc TEXT NOT NULL);
          CREATE TABLE IF NOT EXISTS lmbp_events (task_id TEXT NOT NULL, seq INTEGER NOT NULL, doc TEXT NOT NULL, PRIMARY KEY (task_id, seq));
        """)

    def _x(self, sql: str, *a: Any) -> list:
        with self._lock:
            return self.db.execute(sql, a).fetchall()

    def _one(self, sql: str, *a: Any) -> Any:
        r = self._x(sql, *a)
        return _dec(r[0][0]) if r else None

    def get_secret(self, name):
        r = self._x("SELECT value FROM lmbp_secrets WHERE name = ?", name)
        return bytes(r[0][0]) if r else None

    def put_secret(self, name, value):
        self._x("INSERT OR REPLACE INTO lmbp_secrets (name, value) VALUES (?, ?)", name, bytes(value))

    def put_device_auth(self, a):
        self._x("INSERT OR REPLACE INTO lmbp_device_auths (code_hash, tenant, user_code, doc) VALUES (?, ?, ?, ?)",
                a["code_hash"], a["tenant"], a["user_code"], _enc(a))

    def get_device_auth(self, code_hash):
        return self._one("SELECT doc FROM lmbp_device_auths WHERE code_hash = ?", code_hash)

    def device_auth_by_user_code(self, tenant, user_code):
        return self._one("SELECT doc FROM lmbp_device_auths WHERE tenant = ? AND user_code = ? ORDER BY rowid DESC LIMIT 1", tenant, user_code)

    def put_device(self, d):
        self._x("INSERT OR REPLACE INTO lmbp_devices (id, tenant, owner, doc) VALUES (?, ?, ?, ?)", d["id"], d["tenant"], d["owner"], _enc(d))

    def get_device(self, id):
        return self._one("SELECT doc FROM lmbp_devices WHERE id = ?", id)

    def list_devices(self, tenant, owner=None):
        if owner is None:
            rows = self._x("SELECT doc FROM lmbp_devices WHERE tenant = ? ORDER BY rowid", tenant)
        else:
            rows = self._x("SELECT doc FROM lmbp_devices WHERE tenant = ? AND owner = ? ORDER BY rowid", tenant, owner)
        return [_dec(r[0]) for r in rows]

    def get_idempotency(self, tenant, owner, key):
        return self._one("SELECT doc FROM lmbp_idempotency WHERE tenant = ? AND owner = ? AND idem_key = ?", tenant, owner, key)

    def create_task(self, t, idem=None):
        """One transaction: the idempotency record and the task row."""
        with self._lock:
            try:
                self.db.execute("BEGIN IMMEDIATE")
                if idem is not None:
                    cur = self.db.execute("INSERT OR IGNORE INTO lmbp_idempotency (tenant, owner, idem_key, doc) VALUES (?, ?, ?, ?)",
                                          (idem["tenant"], idem["owner"], idem["key"], _enc(idem["rec"])))
                    if cur.rowcount == 0:
                        raise StoreConflict("idempotency key taken")
                self.db.execute("INSERT INTO lmbp_tasks (id, doc) VALUES (?, ?)", (t["id"], _enc({**t, "events": []})))
                self.db.execute("COMMIT")
            except BaseException:
                self.db.execute("ROLLBACK")
                raise

    def put_task(self, t):
        self._x("INSERT OR REPLACE INTO lmbp_tasks (id, doc) VALUES (?, ?)", t["id"], _enc({**t, "events": []}))

    def get_task(self, id):
        t = self._one("SELECT doc FROM lmbp_tasks WHERE id = ?", id)
        if t is not None:
            t["events"] = [_dec(r[0]) for r in self._x("SELECT doc FROM lmbp_events WHERE task_id = ? ORDER BY seq", id)]
        return t

    def append_event(self, task_id, ev):
        with self._lock:
            cur = self.db.execute("INSERT OR IGNORE INTO lmbp_events (task_id, seq, doc) VALUES (?, ?, ?)", (task_id, ev["seq"], _enc(ev)))
            if cur.rowcount == 0:
                raise StoreConflict(f"task {task_id}: seq {ev['seq']} already stored")

    def finish_task(self, task_id, ev, state):
        """One transaction: the terminal event row and the task's terminal state."""
        with self._lock:
            self.db.execute("BEGIN IMMEDIATE")
            try:
                r = self.db.execute("SELECT doc FROM lmbp_tasks WHERE id = ?", (task_id,)).fetchone()
                if r is None:
                    raise StoreNotFound(f"task {task_id} not found")
                t = _dec(r[0])
                if t.get("state") in TERMINAL_STATES:
                    raise StoreConflict(f"task {task_id} already {t['state']}")
                if self.db.execute("SELECT 1 FROM lmbp_events WHERE task_id = ? AND seq = ?", (task_id, ev["seq"])).fetchone():
                    raise StoreConflict(f"task {task_id}: seq {ev['seq']} already stored")
                self.db.execute("INSERT INTO lmbp_events (task_id, seq, doc) VALUES (?, ?, ?)", (task_id, ev["seq"], _enc(ev)))
                self.db.execute("UPDATE lmbp_tasks SET doc = ? WHERE id = ?", (_enc({**t, "events": [], "state": state}), task_id))
                self.db.execute("COMMIT")
            except BaseException:
                self.db.execute("ROLLBACK")
                raise

    def close(self):
        with self._lock:
            self.db.close()
