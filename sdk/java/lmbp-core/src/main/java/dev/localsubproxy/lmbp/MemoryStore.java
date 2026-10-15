package dev.localsubproxy.lmbp;

import java.util.ArrayList;
import java.util.HashMap;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * In-memory {@link Store} (the default). Everything is lost on restart, including the
 * generated signing key unless {@code signingSeed} is configured. Single process only.
 * Tasks and their events are kept until the process exits; for long-running services
 * use {@link JdbcStore}.
 */
public final class MemoryStore implements Store {
  private final Map<String, byte[]> secrets = new HashMap<>();
  private final Map<String, DeviceAuth> auths = new LinkedHashMap<>();
  private final Map<String, Device> devices = new LinkedHashMap<>();
  private final Map<List<String>, Idempotency> idem = new HashMap<>();
  private final Map<String, TaskRecord> tasks = new HashMap<>();

  @Override public synchronized byte[] getSecret(String n) { return secrets.get(n); }
  @Override public synchronized void putSecret(String n, byte[] v) { secrets.put(n, v); }
  @Override public synchronized void putDeviceAuth(DeviceAuth a) { auths.put(a.codeHash, a); }
  @Override public synchronized DeviceAuth getDeviceAuth(String h) { return auths.get(h); }
  @Override public synchronized DeviceAuth deviceAuthByUserCode(String tenant, String code) {
    for (DeviceAuth a : auths.values()) if (a.tenant.equals(tenant) && a.userCode.equals(code)) return a;
    return null;
  }
  @Override public synchronized void putDevice(Device d) { devices.put(d.id, d); }
  @Override public synchronized Device getDevice(String id) { return id == null ? null : devices.get(id); }
  @Override public synchronized List<Device> listDevices(String tenant, String owner) {
    List<Device> out = new ArrayList<>();
    for (Device d : devices.values()) if (d.tenant.equals(tenant) && (owner == null || d.owner.equals(owner))) out.add(d);
    return out;
  }
  @Override public synchronized Idempotency getIdempotency(String t, String o, String k) { return idem.get(List.of(t, o, k)); }
  @Override public synchronized void createTask(TaskRecord t, String idemKey, Idempotency rec) {
    if (tasks.containsKey(t.id)) throw new Conflict("task " + t.id + " exists");
    if (idemKey != null) {
      List<String> k = List.of(rec.tenant(), rec.owner(), idemKey);
      if (idem.containsKey(k)) throw new Conflict("idempotency key taken");
      idem.put(k, rec);
    }
    tasks.put(t.id, t);
  }
  @Override public synchronized TaskRecord getTask(String id) { return tasks.get(id); }
  @Override public synchronized void setState(String id, String state) { TaskRecord t = tasks.get(id); if (t != null) t.state = state; }
  @Override public synchronized void appendEvent(String id, Event ev) {
    TaskRecord t = tasks.get(id);
    if (t == null) return;
    for (Event e : t.events) if (e.seq() == ev.seq()) throw new Conflict("task " + id + ": seq " + ev.seq() + " already stored");
    t.events.add(ev);
  }
  @Override public synchronized void finishTask(String id, Event ev, String state) {
    TaskRecord t = tasks.get(id);
    if (t == null) throw new NotFound("task " + id + " not found");
    if (TERMINAL.contains(t.state)) throw new Conflict("task " + id + " already " + t.state);
    for (Event e : t.events) if (e.seq() == ev.seq()) throw new Conflict("task " + id + ": seq " + ev.seq() + " already stored");
    t.events.add(ev);
    t.state = state;
  }
}
