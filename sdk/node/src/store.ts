/**
 * Pluggable persistence for the gateway. Mirrors the Go `store.Store`
 * (internal/lsp2/store): pairings, devices, idempotency keys, tasks and their
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

export interface Idempotency { taskID: string; bodyHash: string }

export interface TaskEvent { seq: number; type: string; data: unknown }

export interface TaskRecord {
  id: string;
  tenant: string;
  owner: string;
  device: string;
  model: string;
  state: string;
  events: TaskEvent[];
}

export interface Store {
  getSecret(name: string): Buffer | undefined;
  putSecret(name: string, value: Buffer): void;

  putDeviceAuth(a: DeviceAuth): void;
  getDeviceAuth(codeHash: string): DeviceAuth | undefined;
  deviceAuthByUserCode(tenant: string, userCode: string): DeviceAuth | undefined;

  putDevice(d: Device): void;
  getDevice(id: string): Device | undefined;
  listDevices(tenant: string, owner?: string): Device[];

  /** key is `${tenant}|${owner}|${Idempotency-Key}`. */
  getIdempotency(key: string): Idempotency | undefined;
  putIdempotency(key: string, rec: Idempotency): void;

  /** putTask writes the task's metadata; events are written with appendEvent. */
  putTask(t: TaskRecord): void;
  /** getTask returns the task with all its events. */
  getTask(id: string): TaskRecord | undefined;
  appendEvent(taskID: string, ev: TaskEvent): void;

  close?(): void;
}

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
  getIdempotency(k: string) { return this.idem.get(k); }
  putIdempotency(k: string, r: Idempotency) { this.idem.set(k, r); }
  putTask(t: TaskRecord) {
    const cur = this.tasks.get(t.id);
    if (cur && cur !== t) Object.assign(cur, { ...t, events: cur.events });
    else if (!cur) this.tasks.set(t.id, t);
  }
  getTask(id: string) { return this.tasks.get(id); }
  appendEvent(id: string, ev: TaskEvent) {
    const t = this.tasks.get(id);
    if (t && !t.events.includes(ev)) t.events.push(ev);
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
      CREATE TABLE IF NOT EXISTS lsp2_secrets (name TEXT PRIMARY KEY, value BLOB NOT NULL);
      CREATE TABLE IF NOT EXISTS lsp2_device_auths (code_hash TEXT PRIMARY KEY, tenant TEXT NOT NULL, user_code TEXT NOT NULL, doc TEXT NOT NULL);
      CREATE INDEX IF NOT EXISTS lsp2_device_auths_code ON lsp2_device_auths (tenant, user_code);
      CREATE TABLE IF NOT EXISTS lsp2_devices (id TEXT PRIMARY KEY, tenant TEXT NOT NULL, owner TEXT NOT NULL, doc TEXT NOT NULL);
      CREATE INDEX IF NOT EXISTS lsp2_devices_owner ON lsp2_devices (tenant, owner);
      CREATE TABLE IF NOT EXISTS lsp2_idempotency (key TEXT PRIMARY KEY, doc TEXT NOT NULL);
      CREATE TABLE IF NOT EXISTS lsp2_tasks (id TEXT PRIMARY KEY, doc TEXT NOT NULL);
      CREATE TABLE IF NOT EXISTS lsp2_events (task_id TEXT NOT NULL, seq INTEGER NOT NULL, doc TEXT NOT NULL, PRIMARY KEY (task_id, seq));`);
  }
  private one<T>(sql: string, ...a: unknown[]): T | undefined {
    const r = this.db.prepare(sql).get(...a);
    return r ? dec<T>(r.doc) : undefined;
  }
  getSecret(n: string) {
    const r = this.db.prepare('SELECT value FROM lsp2_secrets WHERE name = ?').get(n);
    return r ? Buffer.from(r.value) : undefined;
  }
  putSecret(n: string, v: Buffer) { this.db.prepare('INSERT OR REPLACE INTO lsp2_secrets (name, value) VALUES (?, ?)').run(n, v); }
  putDeviceAuth(a: DeviceAuth) {
    this.db.prepare('INSERT OR REPLACE INTO lsp2_device_auths (code_hash, tenant, user_code, doc) VALUES (?, ?, ?, ?)').run(a.codeHash, a.tenant, a.userCode, enc(a));
  }
  getDeviceAuth(h: string) { return this.one<DeviceAuth>('SELECT doc FROM lsp2_device_auths WHERE code_hash = ?', h); }
  deviceAuthByUserCode(t: string, c: string) {
    return this.one<DeviceAuth>('SELECT doc FROM lsp2_device_auths WHERE tenant = ? AND user_code = ? ORDER BY rowid DESC LIMIT 1', t, c);
  }
  putDevice(d: Device) {
    this.db.prepare('INSERT OR REPLACE INTO lsp2_devices (id, tenant, owner, doc) VALUES (?, ?, ?, ?)').run(d.id, d.tenant, d.owner, enc({ ...d, created: d.created.toISOString() }));
  }
  private dev(doc: string): Device { const d = dec<any>(doc); d.created = new Date(d.created); return d; }
  getDevice(id: string) {
    const r = this.db.prepare('SELECT doc FROM lsp2_devices WHERE id = ?').get(id);
    return r ? this.dev(r.doc) : undefined;
  }
  listDevices(t: string, o?: string) {
    const rows = o === undefined ? this.db.prepare('SELECT doc FROM lsp2_devices WHERE tenant = ? ORDER BY rowid').all(t)
      : this.db.prepare('SELECT doc FROM lsp2_devices WHERE tenant = ? AND owner = ? ORDER BY rowid').all(t, o);
    return rows.map((r) => this.dev(r.doc));
  }
  getIdempotency(k: string) { return this.one<Idempotency>('SELECT doc FROM lsp2_idempotency WHERE key = ?', k); }
  putIdempotency(k: string, r: Idempotency) { this.db.prepare('INSERT OR IGNORE INTO lsp2_idempotency (key, doc) VALUES (?, ?)').run(k, enc(r)); }
  putTask(t: TaskRecord) { this.db.prepare('INSERT OR REPLACE INTO lsp2_tasks (id, doc) VALUES (?, ?)').run(t.id, enc({ ...t, events: [] })); }
  getTask(id: string) {
    const t = this.one<TaskRecord>('SELECT doc FROM lsp2_tasks WHERE id = ?', id);
    if (t) t.events = this.db.prepare('SELECT doc FROM lsp2_events WHERE task_id = ? ORDER BY seq').all(id).map((r) => dec<TaskEvent>(r.doc));
    return t;
  }
  appendEvent(id: string, ev: TaskEvent) { this.db.prepare('INSERT OR IGNORE INTO lsp2_events (task_id, seq, doc) VALUES (?, ?, ?)').run(id, ev.seq, enc(ev)); }
  close() { this.db.close(); }
}
