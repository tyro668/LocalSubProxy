package lmbp;

import java.time.Instant;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;

/** In-memory persistence, same contract as the Node MemoryStore (sdk/node/src/store.ts). Callers hold the gateway lock. */
final class Store {
  static final class Conflict extends RuntimeException { Conflict(String m) { super(m); } }
  static final class NotFound extends RuntimeException { NotFound(String m) { super(m); } }
  static final Set<String> TERMINAL = Set.of("SUCCEEDED", "FAILED", "CANCELLED");

  static final class DeviceAuth {
    String codeHash, userCode, tenant, name, status, deviceID = "", owner = "";
    byte[] pub;
    long expires, lastPoll;
    int interval;
  }

  static final class Device {
    String id, tenant, owner, name, status, e2eeKey = "";
    byte[] pub;
    Instant created;
  }

  record Idempotency(String tenant, String owner, String taskID, String bodyHash) {}

  record Event(long seq, String type, Map<String, Object> data) {}

  static final class TaskRecord {
    String id, tenant, owner, device, model, state;
    long finished; // ms since epoch when the task ended (retention), 0 while running
    final List<Event> events = new ArrayList<>();
  }

  final Map<String, byte[]> secrets = new HashMap<>();
  final Map<String, DeviceAuth> auths = new LinkedHashMap<>();
  final Map<String, Device> devices = new LinkedHashMap<>();
  final Map<List<String>, Idempotency> idem = new HashMap<>();
  final Map<String, TaskRecord> tasks = new HashMap<>();

  byte[] getSecret(String n) { return secrets.get(n); }
  void putSecret(String n, byte[] v) { secrets.put(n, v); }
  void putDeviceAuth(DeviceAuth a) { auths.put(a.codeHash, a); }
  DeviceAuth getDeviceAuth(String h) { return auths.get(h); }
  DeviceAuth deviceAuthByUserCode(String tenant, String code) {
    for (DeviceAuth a : auths.values()) if (a.tenant.equals(tenant) && a.userCode.equals(code)) return a;
    return null;
  }
  void putDevice(Device d) { devices.put(d.id, d); }
  Device getDevice(String id) { return id == null ? null : devices.get(id); }
  List<Device> listDevices(String tenant, String owner) {
    List<Device> out = new ArrayList<>();
    for (Device d : devices.values()) if (d.tenant.equals(tenant) && (owner == null || d.owner.equals(owner))) out.add(d);
    return out;
  }
  Idempotency getIdempotency(String t, String o, String k) { return idem.get(List.of(t, o, k)); }
  void createTask(TaskRecord t, String idemKey, Idempotency rec) {
    if (tasks.containsKey(t.id)) throw new Conflict("task " + t.id + " exists");
    if (idemKey != null) {
      List<String> k = List.of(rec.tenant(), rec.owner(), idemKey);
      if (idem.containsKey(k)) throw new Conflict("idempotency key taken");
      idem.put(k, rec);
    }
    tasks.put(t.id, t);
  }
  TaskRecord getTask(String id) { return tasks.get(id); }
  void setState(String id, String state) { TaskRecord t = tasks.get(id); if (t != null) t.state = state; }
  void appendEvent(String id, Event ev) {
    TaskRecord t = tasks.get(id);
    if (t == null) return;
    for (Event e : t.events) if (e.seq() == ev.seq()) throw new Conflict("task " + id + ": seq " + ev.seq() + " already stored");
    t.events.add(ev);
  }
  void finishTask(String id, Event ev, String state) {
    TaskRecord t = tasks.get(id);
    if (t == null) throw new NotFound("task " + id + " not found");
    if (TERMINAL.contains(t.state)) throw new Conflict("task " + id + " already " + t.state);
    for (Event e : t.events) if (e.seq() == ev.seq()) throw new Conflict("task " + id + ": seq " + ev.seq() + " already stored");
    t.events.add(ev);
    t.state = state;
    t.finished = System.currentTimeMillis();
  }

  /** Retention (spec §14.4, errata E6): finished tasks older than cutoff go with their idempotency records; expired pairings too. */
  int purge(long cutoff) {
    int n = 0;
    for (var it = tasks.values().iterator(); it.hasNext(); ) {
      TaskRecord t = it.next();
      if (TERMINAL.contains(t.state) && t.finished > 0 && t.finished < cutoff) { it.remove(); n++; }
    }
    idem.values().removeIf(r -> !tasks.containsKey(r.taskID()));
    auths.values().removeIf(a -> a.expires > 0 && a.expires < cutoff);
    return n;
  }
}
