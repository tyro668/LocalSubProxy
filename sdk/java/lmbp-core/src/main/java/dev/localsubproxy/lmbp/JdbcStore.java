package dev.localsubproxy.lmbp;

import java.io.PrintWriter;
import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.sql.Statement;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.logging.Logger;
import javax.sql.DataSource;

/**
 * {@link Store} over JDBC. Tested with SQLite (xerial sqlite-jdbc); the SQL is plain
 * {@code CREATE TABLE IF NOT EXISTS} / {@code ALTER TABLE ADD COLUMN} / {@code INSERT} / {@code UPDATE} / {@code DELETE} / {@code SELECT}
 * with a configurable large-text column type so other databases can work, but only SQLite
 * is verified in this repository.
 *
 * <p><b>Scale limits (stated honestly):</b> single gateway instance per database (live
 * sessions, fan-out and JTI replay protection are in memory); every call runs in its own
 * short transaction on a connection from the {@link DataSource}; finished tasks with their
 * events and idempotency records are deleted after the gateway's retention period
 * ({@link #purge}, default 7 days).
 * Byte values are stored base64url-encoded in text columns.
 */
public final class JdbcStore implements Store {
  private final DataSource ds;

  /** Creates the {@code lmbp_*} tables if missing. {@code textType} is the large-text column type ({@code TEXT} for SQLite/PostgreSQL/MySQL). */
  public JdbcStore(DataSource ds, String textType) {
    this.ds = ds;
    String tx = textType == null || textType.isBlank() ? "TEXT" : textType;
    String[] ddl = {
        "CREATE TABLE IF NOT EXISTS lmbp_secrets (name VARCHAR(128) PRIMARY KEY, val " + tx + " NOT NULL)",
        "CREATE TABLE IF NOT EXISTS lmbp_device_auths (code_hash VARCHAR(64) PRIMARY KEY, user_code VARCHAR(32) NOT NULL, tenant VARCHAR(64) NOT NULL, doc " + tx + " NOT NULL)",
        "CREATE TABLE IF NOT EXISTS lmbp_devices (id VARCHAR(64) PRIMARY KEY, tenant VARCHAR(64) NOT NULL, owner VARCHAR(128) NOT NULL, created BIGINT NOT NULL, doc " + tx + " NOT NULL)",
        "CREATE TABLE IF NOT EXISTS lmbp_idempotency (tenant VARCHAR(64) NOT NULL, owner VARCHAR(128) NOT NULL, ikey VARCHAR(255) NOT NULL, task_id VARCHAR(64) NOT NULL, body_hash VARCHAR(64) NOT NULL, PRIMARY KEY (tenant, owner, ikey))",
        "CREATE TABLE IF NOT EXISTS lmbp_tasks (id VARCHAR(64) PRIMARY KEY, tenant VARCHAR(64) NOT NULL, owner VARCHAR(128) NOT NULL, device VARCHAR(64) NOT NULL, model VARCHAR(512) NOT NULL, state VARCHAR(16) NOT NULL)",
        "CREATE TABLE IF NOT EXISTS lmbp_task_events (task_id VARCHAR(64) NOT NULL, seq BIGINT NOT NULL, type VARCHAR(32) NOT NULL, data " + tx + " NOT NULL, PRIMARY KEY (task_id, seq))"};
    tx(c -> { try (Statement s = c.createStatement()) { for (String d : ddl) s.executeUpdate(d); } return null; });
    // added later: whether a task was end-to-end encrypted (replays stay sealed)
    try { tx(c -> { try (Statement s = c.createStatement()) { s.executeUpdate("ALTER TABLE lmbp_tasks ADD COLUMN e2ee INTEGER DEFAULT 0 NOT NULL"); } return null; }); }
    catch (RuntimeException e) { /* column exists */ }
    // added later: retention (spec §14.4, errata E6) needs when a task finished and when a pairing expires (ms since epoch; 0 = unknown, kept)
    try { tx(c -> { try (Statement s = c.createStatement()) { s.executeUpdate("ALTER TABLE lmbp_tasks ADD COLUMN finished BIGINT DEFAULT 0 NOT NULL"); } return null; }); }
    catch (RuntimeException e) { /* column exists */ }
    try { tx(c -> { try (Statement s = c.createStatement()) { s.executeUpdate("ALTER TABLE lmbp_device_auths ADD COLUMN expires BIGINT DEFAULT 0 NOT NULL"); } return null; }); }
    catch (RuntimeException e) { /* column exists */ }
  }

  public JdbcStore(DataSource ds) { this(ds, "TEXT"); }

  /** Convenience: a {@link DriverManager} connection per call, e.g. {@code jdbc:sqlite:lmbp.db}. */
  public static JdbcStore open(String jdbcUrl) { return new JdbcStore(new UrlDataSource(jdbcUrl)); }

  interface Body<T> { T run(Connection c) throws SQLException; }

  private <T> T tx(Body<T> b) {
    try (Connection c = ds.getConnection()) {
      boolean auto = c.getAutoCommit();
      c.setAutoCommit(false);
      try {
        T r = b.run(c);
        c.commit();
        return r;
      } catch (SQLException | RuntimeException e) {
        c.rollback();
        throw e;
      } finally { c.setAutoCommit(auto); }
    } catch (SQLException e) {
      throw new IllegalStateException("lmbp jdbc store: " + e.getMessage(), e);
    }
  }

  private static PreparedStatement ps(Connection c, String sql, Object... args) throws SQLException {
    PreparedStatement p = c.prepareStatement(sql);
    for (int i = 0; i < args.length; i++) p.setObject(i + 1, args[i]);
    return p;
  }

  private static int exec(Connection c, String sql, Object... args) throws SQLException {
    try (PreparedStatement p = ps(c, sql, args)) { return p.executeUpdate(); }
  }

  private static String one(Connection c, String sql, Object... args) throws SQLException {
    try (PreparedStatement p = ps(c, sql, args); ResultSet r = p.executeQuery()) { return r.next() ? r.getString(1) : null; }
  }

  private static void upsert(Connection c, String update, Object[] uargs, String insert, Object[] iargs) throws SQLException {
    if (exec(c, update, uargs) == 0) exec(c, insert, iargs);
  }

  // ---- secrets ----
  @Override public byte[] getSecret(String n) {
    String v = tx(c -> one(c, "SELECT val FROM lmbp_secrets WHERE name = ?", n));
    return v == null ? null : Lmbp.unb64(v);
  }

  @Override public void putSecret(String n, byte[] v) {
    tx(c -> { upsert(c, "UPDATE lmbp_secrets SET val = ? WHERE name = ?", new Object[] {Lmbp.b64(v), n}, "INSERT INTO lmbp_secrets (name, val) VALUES (?, ?)", new Object[] {n, Lmbp.b64(v)}); return null; });
  }

  private static String s(Map<String, Object> m, String k) { String v = Json.str(m, k); return v == null ? "" : v; }

  // ---- device authorizations ----
  private static String authDoc(DeviceAuth a) {
    return Json.stringify(Json.obj("code_hash", a.codeHash, "user_code", a.userCode, "tenant", a.tenant, "name", a.name, "status", a.status, "device_id", a.deviceID,
        "owner", a.owner, "pub", a.pub == null ? "" : Lmbp.b64(a.pub), "expires", a.expires, "last_poll", a.lastPoll, "interval", (long) a.interval));
  }

  private static DeviceAuth authOf(String doc) {
    if (doc == null) return null;
    Map<String, Object> m = Json.asObj(Json.parse(doc));
    DeviceAuth a = new DeviceAuth();
    a.codeHash = s(m, "code_hash"); a.userCode = s(m, "user_code"); a.tenant = s(m, "tenant"); a.name = s(m, "name");
    a.status = s(m, "status"); a.deviceID = s(m, "device_id"); a.owner = s(m, "owner");
    String pub = s(m, "pub");
    a.pub = pub.isEmpty() ? null : Lmbp.unb64(pub);
    a.expires = Json.longOf(m.get("expires")); a.lastPoll = Json.longOf(m.get("last_poll")); a.interval = (int) Json.longOf(m.get("interval"));
    return a;
  }

  @Override public void putDeviceAuth(DeviceAuth a) {
    String doc = authDoc(a);
    tx(c -> { upsert(c, "UPDATE lmbp_device_auths SET user_code = ?, tenant = ?, doc = ?, expires = ? WHERE code_hash = ?", new Object[] {a.userCode, a.tenant, doc, a.expires, a.codeHash},
        "INSERT INTO lmbp_device_auths (code_hash, user_code, tenant, doc, expires) VALUES (?, ?, ?, ?, ?)", new Object[] {a.codeHash, a.userCode, a.tenant, doc, a.expires}); return null; });
  }

  @Override public DeviceAuth getDeviceAuth(String h) { return authOf(tx(c -> one(c, "SELECT doc FROM lmbp_device_auths WHERE code_hash = ?", h))); }

  @Override public DeviceAuth deviceAuthByUserCode(String tenant, String code) {
    return authOf(tx(c -> one(c, "SELECT doc FROM lmbp_device_auths WHERE tenant = ? AND user_code = ?", tenant, code)));
  }

  // ---- devices ----
  private static String devDoc(Device d) {
    return Json.stringify(Json.obj("id", d.id, "tenant", d.tenant, "owner", d.owner, "name", d.name, "status", d.status, "e2ee_key", d.e2eeKey == null ? "" : d.e2eeKey,
        "pub", Lmbp.b64(d.pub), "created_ms", d.created == null ? 0L : d.created.toEpochMilli()));
  }

  private static Device devOf(String doc) {
    if (doc == null) return null;
    Map<String, Object> m = Json.asObj(Json.parse(doc));
    Device d = new Device();
    d.id = s(m, "id"); d.tenant = s(m, "tenant"); d.owner = s(m, "owner"); d.name = s(m, "name"); d.status = s(m, "status");
    d.e2eeKey = s(m, "e2ee_key"); d.pub = Lmbp.unb64(s(m, "pub")); d.created = Instant.ofEpochMilli(Json.longOf(m.get("created_ms")));
    return d;
  }

  @Override public void putDevice(Device d) {
    String doc = devDoc(d);
    long created = d.created == null ? 0 : d.created.toEpochMilli();
    tx(c -> { upsert(c, "UPDATE lmbp_devices SET tenant = ?, owner = ?, doc = ? WHERE id = ?", new Object[] {d.tenant, d.owner, doc, d.id},
        "INSERT INTO lmbp_devices (id, tenant, owner, created, doc) VALUES (?, ?, ?, ?, ?)", new Object[] {d.id, d.tenant, d.owner, created, doc}); return null; });
  }

  @Override public boolean setDeviceE2EEKey(String id, String key) {
    // optimistic compare-and-swap on the whole doc: only the key changes, and
    // only if nobody (e.g. a revoke) wrote the record since we read it
    for (int i = 0; i < 5; i++) {
      Boolean done = tx(c -> {
        String doc = one(c, "SELECT doc FROM lmbp_devices WHERE id = ?", id);
        Device d = devOf(doc);
        if (d == null || !"active".equals(d.status)) return Boolean.FALSE;
        d.e2eeKey = key;
        try (PreparedStatement p = ps(c, "UPDATE lmbp_devices SET doc = ? WHERE id = ? AND doc = ?", devDoc(d), id, doc)) {
          return p.executeUpdate() == 1 ? Boolean.TRUE : null;
        }
      });
      if (done != null) return done;
    }
    return false;
  }

  @Override public Device getDevice(String id) { return id == null ? null : devOf(tx(c -> one(c, "SELECT doc FROM lmbp_devices WHERE id = ?", id))); }

  @Override public List<Device> listDevices(String tenant, String owner) {
    return tx(c -> {
      List<Device> out = new ArrayList<>();
      String sql = "SELECT doc FROM lmbp_devices WHERE tenant = ?" + (owner == null ? "" : " AND owner = ?") + " ORDER BY created, id";
      try (PreparedStatement p = owner == null ? ps(c, sql, tenant) : ps(c, sql, tenant, owner); ResultSet r = p.executeQuery()) {
        while (r.next()) out.add(devOf(r.getString(1)));
      }
      return out;
    });
  }

  // ---- idempotency + tasks ----
  @Override public Idempotency getIdempotency(String t, String o, String k) {
    return tx(c -> {
      try (PreparedStatement p = ps(c, "SELECT task_id, body_hash FROM lmbp_idempotency WHERE tenant = ? AND owner = ? AND ikey = ?", t, o, k); ResultSet r = p.executeQuery()) {
        return r.next() ? new Idempotency(t, o, r.getString(1), r.getString(2)) : null;
      }
    });
  }

  @Override public void createTask(TaskRecord t, String idemKey, Idempotency rec) {
    tx(c -> {
      if (one(c, "SELECT id FROM lmbp_tasks WHERE id = ?", t.id) != null) throw new Conflict("task " + t.id + " exists");
      if (idemKey != null && one(c, "SELECT task_id FROM lmbp_idempotency WHERE tenant = ? AND owner = ? AND ikey = ?", rec.tenant(), rec.owner(), idemKey) != null)
        throw new Conflict("idempotency key taken");
      exec(c, "INSERT INTO lmbp_tasks (id, tenant, owner, device, model, state, e2ee) VALUES (?, ?, ?, ?, ?, ?, ?)", t.id, t.tenant, t.owner, t.device, t.model, t.state, t.e2ee ? 1 : 0);
      if (idemKey != null) exec(c, "INSERT INTO lmbp_idempotency (tenant, owner, ikey, task_id, body_hash) VALUES (?, ?, ?, ?, ?)", rec.tenant(), rec.owner(), idemKey, rec.taskID(), rec.bodyHash());
      for (Event e : t.events) insertEvent(c, t.id, e);
      return null;
    });
  }

  private static void insertEvent(Connection c, String id, Event e) throws SQLException {
    if (one(c, "SELECT type FROM lmbp_task_events WHERE task_id = ? AND seq = ?", id, e.seq()) != null) throw new Conflict("task " + id + ": seq " + e.seq() + " already stored");
    exec(c, "INSERT INTO lmbp_task_events (task_id, seq, type, data) VALUES (?, ?, ?, ?)", id, e.seq(), e.type(), Json.stringify(e.data() == null ? Json.obj() : e.data()));
  }

  @Override public TaskRecord getTask(String id) {
    return tx(c -> {
      TaskRecord t;
      try (PreparedStatement p = ps(c, "SELECT tenant, owner, device, model, state, e2ee, finished FROM lmbp_tasks WHERE id = ?", id); ResultSet r = p.executeQuery()) {
        if (!r.next()) return null;
        t = new TaskRecord();
        t.id = id; t.tenant = r.getString(1); t.owner = r.getString(2); t.device = r.getString(3); t.model = r.getString(4); t.state = r.getString(5);
        t.e2ee = r.getInt(6) != 0;
        t.finished = r.getLong(7);
      }
      try (PreparedStatement p = ps(c, "SELECT seq, type, data FROM lmbp_task_events WHERE task_id = ? ORDER BY seq", id); ResultSet r = p.executeQuery()) {
        while (r.next()) t.events.add(new Event(r.getLong(1), r.getString(2), Json.asObj(Json.parse(r.getString(3)))));
      }
      return t;
    });
  }

  @Override public void setState(String id, String state) { tx(c -> exec(c, "UPDATE lmbp_tasks SET state = ? WHERE id = ?", state, id)); }

  @Override public void appendEvent(String id, Event ev) {
    tx(c -> {
      if (one(c, "SELECT id FROM lmbp_tasks WHERE id = ?", id) == null) return null;
      insertEvent(c, id, ev);
      return null;
    });
  }

  @Override public void finishTask(String id, Event ev, String state) {
    tx(c -> {
      String cur = one(c, "SELECT state FROM lmbp_tasks WHERE id = ?", id);
      if (cur == null) throw new NotFound("task " + id + " not found");
      if (TERMINAL.contains(cur)) throw new Conflict("task " + id + " already " + cur);
      insertEvent(c, id, ev);
      exec(c, "UPDATE lmbp_tasks SET state = ?, finished = ? WHERE id = ?", state, System.currentTimeMillis(), id);
      return null;
    });
  }

  @Override public List<String> unfinishedTasks() {
    return tx(c -> {
      List<String> out = new java.util.ArrayList<>();
      try (PreparedStatement p = ps(c, "SELECT id FROM lmbp_tasks WHERE state NOT IN ('SUCCEEDED','FAILED','CANCELLED')"); ResultSet r = p.executeQuery()) {
        while (r.next()) out.add(r.getString(1));
      }
      return out;
    });
  }

  /** One transaction: finished tasks before the cutoff with their events and idempotency records, and expired pairings. */
  @Override public int purge(long cutoff) {
    String old = "SELECT id FROM lmbp_tasks WHERE state IN ('SUCCEEDED','FAILED','CANCELLED') AND finished > 0 AND finished < ?";
    return tx(c -> {
      exec(c, "DELETE FROM lmbp_task_events WHERE task_id IN (" + old + ")", cutoff);
      // Idempotency records go with their task: never a key without its task.
      exec(c, "DELETE FROM lmbp_idempotency WHERE task_id IN (" + old + ")", cutoff);
      int n = exec(c, "DELETE FROM lmbp_tasks WHERE id IN (" + old + ")", cutoff);
      exec(c, "DELETE FROM lmbp_device_auths WHERE expires > 0 AND expires < ?", cutoff);
      return n;
    });
  }

  /** Minimal DataSource over {@link DriverManager}. */
  static final class UrlDataSource implements DataSource {
    private final String url;
    UrlDataSource(String url) { this.url = url; }
    @Override public Connection getConnection() throws SQLException { return DriverManager.getConnection(url); }
    @Override public Connection getConnection(String u, String p) throws SQLException { return DriverManager.getConnection(url, u, p); }
    @Override public PrintWriter getLogWriter() { return null; }
    @Override public void setLogWriter(PrintWriter out) { }
    @Override public void setLoginTimeout(int seconds) { }
    @Override public int getLoginTimeout() { return 0; }
    @Override public Logger getParentLogger() { return Logger.getGlobal(); }
    @Override public <T> T unwrap(Class<T> iface) throws SQLException { throw new SQLException("not a wrapper"); }
    @Override public boolean isWrapperFor(Class<?> iface) { return false; }
  }
}
