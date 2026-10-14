package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS secrets(name TEXT PRIMARY KEY, value BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS tenants(id TEXT PRIMARY KEY, name TEXT NOT NULL, api_key_hash TEXT NOT NULL UNIQUE, profile TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS device_auths(device_code_hash TEXT PRIMARY KEY, user_code TEXT NOT NULL, tenant_id TEXT NOT NULL,
  public_key BLOB NOT NULL, device_name TEXT NOT NULL, status TEXT NOT NULL, owner_id TEXT NOT NULL DEFAULT '',
  device_id TEXT NOT NULL DEFAULT '', expires_at INTEGER NOT NULL, interval_s INTEGER NOT NULL, last_poll INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX IF NOT EXISTS device_auths_user_code ON device_auths(tenant_id, user_code);
CREATE TABLE IF NOT EXISTS devices(id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, owner_id TEXT NOT NULL, name TEXT NOT NULL,
  public_key BLOB NOT NULL, status TEXT NOT NULL, created_at INTEGER NOT NULL, e2ee_key TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS devices_owner ON devices(tenant_id, owner_id);
CREATE TABLE IF NOT EXISTS tasks(id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, owner_id TEXT NOT NULL, device_id TEXT NOT NULL,
  model TEXT NOT NULL, state TEXT NOT NULL, request BLOB NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS tasks_updated ON tasks(state, updated_at);
CREATE TABLE IF NOT EXISTS events(task_id TEXT NOT NULL, seq INTEGER NOT NULL, kind TEXT NOT NULL, data BLOB, at INTEGER NOT NULL,
  PRIMARY KEY(task_id, seq));
CREATE TABLE IF NOT EXISTS idempotency(tenant_id TEXT NOT NULL, owner_id TEXT NOT NULL, idem_key TEXT NOT NULL, task_id TEXT NOT NULL,
  body_hash TEXT NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY(tenant_id, owner_id, idem_key));
`

type SQLite struct {
	db *sql.DB
	c  *Cipher
}

// OpenSQLite opens (or creates) a database file; use ":memory:" for tests.
func OpenSQLite(path string, c *Cipher) (*SQLite, error) {
	dsn := path
	if path != ":memory:" {
		dsn = "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single writer; fine for the reference implementation
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &SQLite{db: db, c: c}, nil
}

func (s *SQLite) Close() error { return s.db.Close() }

// GetSecret implements SecretStore. Secrets are sealed when a Cipher is set.
func (s *SQLite) GetSecret(ctx context.Context, name string) ([]byte, error) {
	var v []byte
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM secrets WHERE name = ?`, name).Scan(&v); err != nil {
		return nil, nf(err)
	}
	return s.c.Open(v)
}

// PutSecret implements SecretStore.
func (s *SQLite) PutSecret(ctx context.Context, name string, v []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO secrets(name, value) VALUES(?, ?)`, name, s.c.Seal(v))
	return err
}

func nf(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *SQLite) PutTenant(ctx context.Context, t Tenant) error {
	pb, _ := json.Marshal(t.Profile)
	_, err := s.db.ExecContext(ctx, `INSERT INTO tenants(id,name,api_key_hash,profile) VALUES(?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, api_key_hash=excluded.api_key_hash, profile=excluded.profile`, t.ID, t.Name, t.APIKeyHash, string(pb))
	return err
}

func scanTenant(r interface{ Scan(...any) error }) (Tenant, error) {
	var t Tenant
	var prof string
	if err := r.Scan(&t.ID, &t.Name, &t.APIKeyHash, &prof); err != nil {
		return t, nf(err)
	}
	if prof != "" {
		_ = json.Unmarshal([]byte(prof), &t.Profile)
	}
	return t, nil
}

func (s *SQLite) TenantByAPIKeyHash(ctx context.Context, h string) (Tenant, error) {
	return scanTenant(s.db.QueryRowContext(ctx, `SELECT id,name,api_key_hash,profile FROM tenants WHERE api_key_hash=?`, h))
}

func (s *SQLite) GetTenant(ctx context.Context, id string) (Tenant, error) {
	return scanTenant(s.db.QueryRowContext(ctx, `SELECT id,name,api_key_hash,profile FROM tenants WHERE id=?`, id))
}

const daCols = `device_code_hash,user_code,tenant_id,public_key,device_name,status,owner_id,device_id,expires_at,interval_s,last_poll,attempts`

func scanDA(r interface{ Scan(...any) error }) (DeviceAuth, error) {
	var d DeviceAuth
	var exp, lp int64
	err := r.Scan(&d.DeviceCodeHash, &d.UserCode, &d.TenantID, &d.PublicKey, &d.DeviceName, &d.Status, &d.OwnerID, &d.DeviceID, &exp, &d.Interval, &lp, &d.Attempts)
	d.ExpiresAt, d.LastPoll = time.Unix(exp, 0), time.Unix(0, lp)
	return d, nf(err)
}

func (s *SQLite) CreateDeviceAuth(ctx context.Context, d DeviceAuth) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO device_auths(`+daCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.DeviceCodeHash, d.UserCode, d.TenantID, d.PublicKey, d.DeviceName, d.Status, d.OwnerID, d.DeviceID, d.ExpiresAt.Unix(), d.Interval, d.LastPoll.UnixNano(), d.Attempts)
	return err
}

func (s *SQLite) DeviceAuthByCodeHash(ctx context.Context, h string) (DeviceAuth, error) {
	return scanDA(s.db.QueryRowContext(ctx, `SELECT `+daCols+` FROM device_auths WHERE device_code_hash=?`, h))
}

func (s *SQLite) DeviceAuthByUserCode(ctx context.Context, tenantID, code string) (DeviceAuth, error) {
	return scanDA(s.db.QueryRowContext(ctx, `SELECT `+daCols+` FROM device_auths WHERE tenant_id=? AND user_code=?`, tenantID, code))
}

func (s *SQLite) UpdateDeviceAuth(ctx context.Context, d DeviceAuth) error {
	_, err := s.db.ExecContext(ctx, `UPDATE device_auths SET status=?,owner_id=?,device_id=?,last_poll=?,attempts=?,interval_s=? WHERE device_code_hash=?`,
		d.Status, d.OwnerID, d.DeviceID, d.LastPoll.UnixNano(), d.Attempts, d.Interval, d.DeviceCodeHash)
	return err
}

func (s *SQLite) CreateDevice(ctx context.Context, d Device) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO devices(id,tenant_id,owner_id,name,public_key,status,created_at) VALUES(?,?,?,?,?,?,?)`,
		d.ID, d.TenantID, d.OwnerID, d.Name, d.PublicKey, d.Status, d.CreatedAt.Unix())
	return err
}

func scanDev(r interface{ Scan(...any) error }) (Device, error) {
	var d Device
	var c int64
	err := r.Scan(&d.ID, &d.TenantID, &d.OwnerID, &d.Name, &d.PublicKey, &d.Status, &c, &d.E2EEKey)
	d.CreatedAt = time.Unix(c, 0)
	return d, nf(err)
}

func (s *SQLite) GetDevice(ctx context.Context, id string) (Device, error) {
	return scanDev(s.db.QueryRowContext(ctx, `SELECT id,tenant_id,owner_id,name,public_key,status,created_at,e2ee_key FROM devices WHERE id=?`, id))
}

func (s *SQLite) ListDevices(ctx context.Context, tenantID, ownerID string) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,tenant_id,owner_id,name,public_key,status,created_at,e2ee_key FROM devices WHERE tenant_id=? AND owner_id=? ORDER BY created_at`, tenantID, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		d, err := scanDev(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *SQLite) SetDeviceStatus(ctx context.Context, id, status string) error {
	r, err := s.db.ExecContext(ctx, `UPDATE devices SET status=? WHERE id=?`, status, id)
	if err == nil {
		if n, _ := r.RowsAffected(); n == 0 {
			return ErrNotFound
		}
	}
	return err
}

func (s *SQLite) SetDeviceE2EEKey(ctx context.Context, id, stmt string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE devices SET e2ee_key=? WHERE id=?`, stmt, id)
	return err
}

func (s *SQLite) GetIdempotency(ctx context.Context, tenantID, ownerID, key string) (Idempotency, error) {
	var i Idempotency
	var c int64
	err := s.db.QueryRowContext(ctx, `SELECT tenant_id,owner_id,idem_key,task_id,body_hash,created_at FROM idempotency WHERE tenant_id=? AND owner_id=? AND idem_key=?`,
		tenantID, ownerID, key).Scan(&i.TenantID, &i.OwnerID, &i.Key, &i.TaskID, &i.BodyHash, &c)
	i.CreatedAt = time.Unix(0, c)
	return i, nf(err)
}

// CreateTask writes the task row and the idempotency record in one
// transaction (see Store).
func (s *SQLite) CreateTask(ctx context.Context, t Task, i *Idempotency) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if i != nil {
		r, err := tx.ExecContext(ctx, `INSERT INTO idempotency(tenant_id,owner_id,idem_key,task_id,body_hash,created_at) VALUES(?,?,?,?,?,?)
			ON CONFLICT DO NOTHING`, i.TenantID, i.OwnerID, i.Key, i.TaskID, i.BodyHash, i.CreatedAt.UnixNano())
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return ErrExists
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(id,tenant_id,owner_id,device_id,model,state,request,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		t.ID, t.TenantID, t.OwnerID, t.DeviceID, t.Model, t.State, s.c.Seal(t.Request), t.CreatedAt.UnixNano(), t.UpdatedAt.UnixNano()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) SetTaskState(ctx context.Context, id, state string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET state=?, updated_at=? WHERE id=? AND state IN ('QUEUED','RUNNING')`, state, time.Now().UnixNano(), id)
	return err
}

// FinishTask writes the terminal event and the terminal state in one
// transaction (see Store).
func (s *SQLite) FinishTask(ctx context.Context, e Event, state string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var cur string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id=?`, e.TaskID).Scan(&cur); err != nil {
		return nf(err)
	}
	if cur != "QUEUED" && cur != "RUNNING" {
		return ErrExists
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE task_id=? AND seq=?`, e.TaskID, e.Seq).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrExists
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(task_id,seq,kind,data,at) VALUES(?,?,?,?,?)`, e.TaskID, e.Seq, e.Kind, s.c.Seal(e.Data), e.At.UnixNano()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state=?, updated_at=? WHERE id=?`, state, time.Now().UnixNano(), e.TaskID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) GetTask(ctx context.Context, id string) (Task, error) {
	var t Task
	var c, u int64
	err := s.db.QueryRowContext(ctx, `SELECT id,tenant_id,owner_id,device_id,model,state,request,created_at,updated_at FROM tasks WHERE id=?`, id).
		Scan(&t.ID, &t.TenantID, &t.OwnerID, &t.DeviceID, &t.Model, &t.State, &t.Request, &c, &u)
	if err != nil {
		return t, nf(err)
	}
	t.CreatedAt, t.UpdatedAt = time.Unix(0, c), time.Unix(0, u)
	t.Request, err = s.c.Open(t.Request)
	return t, err
}

func (s *SQLite) AppendEvent(ctx context.Context, e Event) error {
	r, err := s.db.ExecContext(ctx, `INSERT INTO events(task_id,seq,kind,data,at) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, e.TaskID, e.Seq, e.Kind, s.c.Seal(e.Data), e.At.UnixNano())
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrExists // a seq is never reused
	}
	return nil
}

func (s *SQLite) ListEvents(ctx context.Context, taskID string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT task_id,seq,kind,data,at FROM events WHERE task_id=? ORDER BY seq`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&e.TaskID, &e.Seq, &e.Kind, &e.Data, &at); err != nil {
			return nil, err
		}
		e.At = time.Unix(0, at)
		if e.Data, err = s.c.Open(e.Data); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *SQLite) Purge(ctx context.Context, cutoff time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	c := cutoff.UnixNano()
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE task_id IN (SELECT id FROM tasks WHERE state<>'RUNNING' AND updated_at<?)`, c); err != nil {
		return 0, err
	}
	r, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE state<>'RUNNING' AND updated_at<?`, c)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM idempotency WHERE created_at<?`, time.Now().Add(-24*time.Hour).UnixNano()); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM device_auths WHERE expires_at<?`, cutoff.Unix()); err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	return int(n), tx.Commit()
}
