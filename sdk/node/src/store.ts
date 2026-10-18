/**
 * Pluggable persistence for the gateway. Mirrors the Go `store.Store`
 * (internal/lmbp/store): pairings, devices, idempotency keys, tasks and their
 * events, and secrets such as the gateway signing seed. The interface is
 * synchronous so the engine can call it inline; implement it over any
 * embedded or synchronous database. Live WebSocket sessions stay in-process,
 * so one Store serves one gateway instance (persistence across restarts).
 */
import { createRequire } from 'node:module';

export interface DeviceAuth {
  codeHash: string;
  userCode: string;
  tenant: string;
  pub: Buffer;
  name: string;
  status: 'pending' | 'approved' | 'denied' | 'consumed';
  expires: number; // ms since epoch
  interval: number;
  lastPoll: number;
  deviceID: string;
  owner: string;
}

export interface Device {
  id: string;
  tenant: string;
  owner: string;
  name: string;
  pub: Buffer;
  status: 'active' | 'revoked';
  created: Date;
  e2eeKey: string;
}

/** An Idempotency-Key record; tenant and owner are re-verified on every replay. */
export interface Idempotency { tenant: string; owner: string; taskID: string; bodyHash: string }

export interface TaskEvent { seq: number; type: string; data: unknown }

export interface TaskRecord {
  id: string;
  tenant: string;
  owner: string;
  device: string;
  model: string;
  state: string;
  events: TaskEvent[];
  /** The task was end-to-end encrypted: replays render its frames sealed. */
  e2ee?: boolean;
  /** When the terminal state was stored (ms since epoch); set by finishTask, used by purge. */
  finished?: number;
}

export interface Store {
  getSecret(name: string): Buffer | undefined;
  putSecret(name: string, value: Buffer): void;

  putDeviceAuth(a: DeviceAuth): void;
  getDeviceAuth(codeHash: string): DeviceAuth | undefined;
  deviceAuthByUserCode(tenant: string, userCode: string): DeviceAuth | undefined;

  putDevice(d: Device): void;
  getDevice(id: string): Device | undefined;
  /** Optional: set only e2eeKey, and only while the device is active; returns
   * false otherwise. Without it the gateway does a synchronous re-read +
   * putDevice (safe in one process, not across processes). */
  setDeviceE2EEKey?(id: string, key: string): boolean;
  listDevices(tenant: string, owner?: string): Device[];

  /** Records are keyed by the (tenant, owner, key) triple; implementations MUST NOT join the parts ambiguously. */
  getIdempotency(tenant: string, owner: string, key: string): Idempotency | undefined;
  /**
   * createTask stores a new task and, when idem is given, its Idempotency-Key
   * record, atomically: both are written or neither is, so a key never exists
   * without its task. Throws StoreConflict if the key is already taken (then
   * nothing is written).
   */
  createTask(t: TaskRecord, idem?: NewIdempotency): void;

  /** putTask writes the task's metadata; events are written with appendEvent / finishTask.
   *  It is used for non-terminal states (QUEUED, RUNNING) only. */
  putTask(t: TaskRecord): void;
  /** getTask returns the task with all its events. */
  getTask(id: string): TaskRecord | undefined;
  /** appendEvent stores a non-terminal event; throws StoreConflict if the seq is already stored (a seq is never reused). */
  appendEvent(taskID: string, ev: TaskEvent): void;
  /**
   * finishTask atomically stores the terminal event (task.done / task.error)
   * AND the terminal state (SUCCEEDED, FAILED, CANCELLED): both or neither.
   * Throws StoreConflict if the seq is taken or the task already has a
   * terminal state, StoreNotFound if the task does not exist. Recovery relies
   * on this: a stored terminal event always agrees with the stored state.
   */
  finishTask(taskID: string, ev: TaskEvent, state: string): void;

  /**
   * Optional retention (spec §14.4, errata E6): delete tasks that finished before cutoff
   * (ms since epoch) together with their events and Idempotency-Key records,
   * and pairings that expired before cutoff. Returns the number of tasks
   * removed. A key never outlives its task, so a replay after the purge runs
   * again. Stores without it keep everything (the gateway logs a warning).
   */
  purge?(cutoff: number): number;

  /**
   * Optional: ids of tasks without a terminal state (QUEUED, RUNNING or
   * unknown). A starting gateway owns no task, so these are orphans of a
   * previous process; it stores OUTCOME_UNKNOWN for them (finishTask) so
   * replays agree and retention applies (audit3 P2-5).
   */
  unfinishedTasks?(): string[];

  close?(): void;
}

/** Thrown by appendEvent / finishTask when a seq or a terminal state already exists. */
/** The Idempotency-Key record written together with its task by createTask. */
export interface NewIdempotency { tenant: string; owner: string; key: string; rec: Idempotency }

export class StoreConflict extends Error { constructor(m: string) { super(m); this.name = 'StoreConflict'; } }
/** Thrown by finishTask when the task does not exist. */
export class StoreNotFound extends Error { constructor(m: string) { super(m); this.name = 'StoreNotFound'; } }
const TERMINAL_STATES = new Set(['SUCCEEDED', 'FAILED', 'CANCELLED']);

/** In-memory store (the default). State is lost on restart. */
export class MemoryStore implements Store {
  private secrets = new Map<string, Buffer>();
  private auths = new Map<string, DeviceAuth>();
  private devices = new Map<string, Device>();
  private idem = new Map<string, Idempotency>();
  private tasks = new Map<string, TaskRecord>();
  getSecret(n: string) { return this.secrets.get(n); }
  putSecret(n: string, v: Buffer) { this.secrets.set(n, v); }
  putDeviceAuth(a: DeviceAuth) { this.auths.set(a.codeHash, a); }
  getDeviceAuth(h: string) { return this.auths.get(h); }
  deviceAuthByUserCode(tenant: string, code: string) {
    for (const a of this.auths.values()) if (a.tenant === tenant && a.userCode === code) return a;
    return undefined;
  }
  putDevice(d: Device) { this.devices.set(d.id, d); }
  getDevice(id: string) { return this.devices.get(id); }
  listDevices(tenant: string, owner?: string) {
    return [...this.devices.values()].filter((d) => d.tenant === tenant && (owner === undefined || d.owner === owner));
  }
  getIdempotency(t: string, o: string, k: string) { return this.idem.get(JSON.stringify([t, o, k])); }
  createTask(t: TaskRecord, i?: NewIdempotency) {
    if (this.tasks.has(t.id)) throw new StoreConflict(`task ${t.id} exists`);
    if (i) {
      const id = JSON.stringify([i.tenant, i.owner, i.key]);   // unambiguous encoding of the triple
      if (this.idem.has(id)) throw new StoreConflict('idempotency key taken');
      this.idem.set(id, i.rec);
    }
    this.tasks.set(t.id, t);
  }
  putTask(t: TaskRecord) {
    const cur = this.tasks.get(t.id);
    if (cur && cur !== t) Object.assign(cur, { ...t, events: cur.events });
    else if (!cur) this.tasks.set(t.id, t);
  }
  getTask(id: string) { return this.tasks.get(id); }
  appendEvent(id: string, ev: TaskEvent) {
    const t = this.tasks.get(id);
    if (!t) return;
    if (t.events.some((e) => e.seq === ev.seq)) throw new StoreConflict(`task ${id}: seq ${ev.seq} already stored`);
    t.events.push(ev);
  }
  finishTask(id: string, ev: TaskEvent, state: string) {
    const t = this.tasks.get(id);
    if (!t) throw new StoreNotFound(`task ${id} not found`);
    if (TERMINAL_STATES.has(t.state)) throw new StoreConflict(`task ${id} already ${t.state}`);
    if (t.events.some((e) => e.seq === ev.seq)) throw new StoreConflict(`task ${id}: seq ${ev.seq} already stored`);
    // Single synchronous step: event and state change together.
    t.events.push(ev);
    t.state = state;
    t.finished = Date.now();
  }
  unfinishedTasks() { return [...this.tasks.values()].filter((t) => !TERMINAL_STATES.has(t.state)).map((t) => t.id); }
  purge(cutoff: number) {
    let n = 0;
    for (const [id, t] of this.tasks) {
      if (TERMINAL_STATES.has(t.state) && (t.finished ?? Infinity) < cutoff) { this.tasks.delete(id); n++; }
    }
    // Idempotency records go with their task: never a key without its task.
    for (const [k, i] of this.idem) if (!this.tasks.has(i.taskID)) this.idem.delete(k);
    for (const [h, a] of this.auths) if (a.expires < cutoff) this.auths.delete(h);
    return n;
  }
}

/** Minimal synchronous SQL driver surface (better-sqlite3 compatible). */
export interface SqlDatabase {
  exec(sql: string): unknown;
  prepare(sql: string): { run(...a: unknown[]): unknown; get(...a: unknown[]): any; all(...a: unknown[]): any[] };
  close(): unknown;
}

const enc = (v: unknown) => JSON.stringify(v, (_k, x) => {
  if (x && x.type === 'Buffer' && Array.isArray(x.data)) return { $b64: Buffer.from(x.data).toString('base64') };
  return x;
});
const dec = <T>(s: string): T => JSON.parse(s, (_k, x) => (x && typeof x === 'object' && typeof x.$b64 === 'string' ? Buffer.from(x.$b64, 'base64') : x));

/**
 * SQLite store over better-sqlite3 (an optional dependency), or any driver
 * with the same synchronous API. Records are stored as JSON documents.
 */
export class SqliteStore implements Store {
  readonly db: SqlDatabase;
  constructor(pathOrDb: string | SqlDatabase = ':memory:') {
    if (typeof pathOrDb === 'string') {
      let Database: any;
      try { Database = createRequire(import.meta.url)('better-sqlite3'); } catch {
        throw new Error('SqliteStore needs the optional dependency better-sqlite3 (npm install better-sqlite3)');
      }
      this.db = new Database(pathOrDb);
      this.db.exec('PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000');
    } else this.db = pathOrDb;
    this.db.exec(`
      CREATE TABLE IF NOT EXISTS lmbp_secrets (name TEXT PRIMARY KEY, value BLOB NOT NULL);
      CREATE TABLE IF NOT EXISTS lmbp_device_auths (code_hash TEXT PRIMARY KEY, tenant TEXT NOT NULL, user_code TEXT NOT NULL, doc TEXT NOT NULL);
      CREATE INDEX IF NOT EXISTS lmbp_device_auths_code ON lmbp_device_auths (tenant, user_code);
      CREATE TABLE IF NOT EXISTS lmbp_devices (id TEXT PRIMARY KEY, tenant TEXT NOT NULL, owner TEXT NOT NULL, doc TEXT NOT NULL);
      CREATE INDEX IF NOT EXISTS lmbp_devices_owner ON lmbp_devices (tenant, owner);
      CREATE TABLE IF NOT EXISTS lmbp_idempotency (tenant TEXT NOT NULL, owner TEXT NOT NULL, idem_key TEXT NOT NULL, doc TEXT NOT NULL, PRIMARY KEY (tenant, owner, idem_key));
      CREATE TABLE IF NOT EXISTS lmbp_tasks (id TEXT PRIMARY KEY, doc TEXT NOT NULL);
      CREATE TABLE IF NOT EXISTS lmbp_events (task_id TEXT NOT NULL, seq INTEGER NOT NULL, doc TEXT NOT NULL, PRIMARY KEY (task_id, seq));`);
  }
  private one<T>(sql: string, ...a: unknown[]): T | undefined {
    const r = this.db.prepare(sql).get(...a);
    return r ? dec<T>(r.doc) : undefined;
  }
  getSecret(n: string) {
    const r = this.db.prepare('SELECT value FROM lmbp_secrets WHERE name = ?').get(n);
    return r ? Buffer.from(r.value) : undefined;
  }
  putSecret(n: string, v: Buffer) { this.db.prepare('INSERT OR REPLACE INTO lmbp_secrets (name, value) VALUES (?, ?)').run(n, v); }
  putDeviceAuth(a: DeviceAuth) {
    this.db.prepare('INSERT OR REPLACE INTO lmbp_device_auths (code_hash, tenant, user_code, doc) VALUES (?, ?, ?, ?)').run(a.codeHash, a.tenant, a.userCode, enc(a));
  }
  getDeviceAuth(h: string) { return this.one<DeviceAuth>('SELECT doc FROM lmbp_device_auths WHERE code_hash = ?', h); }
  deviceAuthByUserCode(t: string, c: string) {
    return this.one<DeviceAuth>('SELECT doc FROM lmbp_device_auths WHERE tenant = ? AND user_code = ? ORDER BY rowid DESC LIMIT 1', t, c);
  }
  putDevice(d: Device) {
    this.db.prepare('INSERT OR REPLACE INTO lmbp_devices (id, tenant, owner, doc) VALUES (?, ?, ?, ?)').run(d.id, d.tenant, d.owner, enc({ ...d, created: d.created.toISOString() }));
  }
  setDeviceE2EEKey(id: string, key: string) {
    // field-only and conditional: a revoked record is never rewritten
    const r = this.db.prepare("UPDATE lmbp_devices SET doc = json_set(doc, '$.e2eeKey', ?) WHERE id = ? AND json_extract(doc, '$.status') = 'active'").run(key, id);
    return (r as { changes: number }).changes > 0;
  }
  private dev(doc: string): Device { const d = dec<any>(doc); d.created = new Date(d.created); return d; }
  getDevice(id: string) {
    const r = this.db.prepare('SELECT doc FROM lmbp_devices WHERE id = ?').get(id);
    return r ? this.dev(r.doc) : undefined;
  }
  listDevices(t: string, o?: string) {
    const rows = o === undefined ? this.db.prepare('SELECT doc FROM lmbp_devices WHERE tenant = ? ORDER BY rowid').all(t)
      : this.db.prepare('SELECT doc FROM lmbp_devices WHERE tenant = ? AND owner = ? ORDER BY rowid').all(t, o);
    return rows.map((r) => this.dev(r.doc));
  }
  getIdempotency(t: string, o: string, k: string) {
    return this.one<Idempotency>('SELECT doc FROM lmbp_idempotency WHERE tenant = ? AND owner = ? AND idem_key = ?', t, o, k);
  }
  /** One transaction: the idempotency record and the task row. */
  createTask(t: TaskRecord, i?: NewIdempotency) {
    this.db.exec('BEGIN IMMEDIATE');
    try {
      if (i) {
        const r: any = this.db.prepare('INSERT OR IGNORE INTO lmbp_idempotency (tenant, owner, idem_key, doc) VALUES (?, ?, ?, ?)').run(i.tenant, i.owner, i.key, enc(i.rec));
        if (r && r.changes === 0) throw new StoreConflict('idempotency key taken');
      }
      this.db.prepare('INSERT INTO lmbp_tasks (id, doc) VALUES (?, ?)').run(t.id, enc({ ...t, events: [] }));
      this.db.exec('COMMIT');
    } catch (e) {
      try { this.db.exec('ROLLBACK'); } catch { /* already rolled back */ }
      throw e;
    }
  }
  putTask(t: TaskRecord) { this.db.prepare('INSERT OR REPLACE INTO lmbp_tasks (id, doc) VALUES (?, ?)').run(t.id, enc({ ...t, events: [] })); }
  getTask(id: string) {
    const t = this.one<TaskRecord>('SELECT doc FROM lmbp_tasks WHERE id = ?', id);
    if (t) t.events = this.db.prepare('SELECT doc FROM lmbp_events WHERE task_id = ? ORDER BY seq').all(id).map((r) => dec<TaskEvent>(r.doc));
    return t;
  }
  appendEvent(id: string, ev: TaskEvent) {
    const r: any = this.db.prepare('INSERT OR IGNORE INTO lmbp_events (task_id, seq, doc) VALUES (?, ?, ?)').run(id, ev.seq, enc(ev));
    if (r && r.changes === 0) throw new StoreConflict(`task ${id}: seq ${ev.seq} already stored`);
  }
  /** One transaction: the terminal event row and the task's terminal state. */
  finishTask(id: string, ev: TaskEvent, state: string) {
    this.db.exec('BEGIN IMMEDIATE');
    try {
      const t = this.one<TaskRecord>('SELECT doc FROM lmbp_tasks WHERE id = ?', id);
      if (!t) throw new StoreNotFound(`task ${id} not found`);
      if (TERMINAL_STATES.has(t.state)) throw new StoreConflict(`task ${id} already ${t.state}`);
      if (this.db.prepare('SELECT 1 FROM lmbp_events WHERE task_id = ? AND seq = ?').get(id, ev.seq)) throw new StoreConflict(`task ${id}: seq ${ev.seq} already stored`);
      this.db.prepare('INSERT INTO lmbp_events (task_id, seq, doc) VALUES (?, ?, ?)').run(id, ev.seq, enc(ev));
      this.db.prepare('UPDATE lmbp_tasks SET doc = ? WHERE id = ?').run(enc({ ...t, events: [], state, finished: Date.now() }), id);
      this.db.exec('COMMIT');
    } catch (e) {
      try { this.db.exec('ROLLBACK'); } catch { /* already rolled back */ }
      throw e;
    }
  }
  unfinishedTasks(): string[] {
    return this.db.prepare("SELECT id FROM lmbp_tasks WHERE json_extract(doc, '$.state') IS NULL OR json_extract(doc, '$.state') NOT IN ('SUCCEEDED','FAILED','CANCELLED')").all().map((r: any) => r.id);
  }
  /** One transaction: finished tasks before cutoff with their events and idempotency records, and expired pairings. */
  purge(cutoff: number) {
    const old = "SELECT id FROM lmbp_tasks WHERE json_extract(doc, '$.state') IN ('SUCCEEDED','FAILED','CANCELLED') AND json_extract(doc, '$.finished') < ?";
    this.db.exec('BEGIN IMMEDIATE');
    try {
      this.db.prepare(`DELETE FROM lmbp_events WHERE task_id IN (${old})`).run(cutoff);
      // Idempotency records go with their task: never a key without its task.
      this.db.prepare(`DELETE FROM lmbp_idempotency WHERE json_extract(doc, '$.taskID') IN (${old})`).run(cutoff);
      const r: any = this.db.prepare(`DELETE FROM lmbp_tasks WHERE id IN (${old})`).run(cutoff);
      this.db.prepare("DELETE FROM lmbp_device_auths WHERE json_extract(doc, '$.expires') < ?").run(cutoff);
      this.db.exec('COMMIT');
      return r?.changes ?? 0;
    } catch (e) {
      try { this.db.exec('ROLLBACK'); } catch { /* already rolled back */ }
      throw e;
    }
  }
  close() { this.db.close(); }
}
