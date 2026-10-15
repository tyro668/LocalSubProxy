package dev.localsubproxy.lmbp;

import java.time.Instant;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.Set;

/**
 * Persistence for pairing, devices, idempotency keys and task event logs. Same contract
 * as the Node SDK's {@code Store} (sdk/node/src/store.ts). The engine calls every method
 * while holding its single lock, so implementations need not be thread-safe against the
 * engine, but each call must be atomic on its own.
 *
 * <p>Implementations: {@link MemoryStore} (default) and {@link JdbcStore}. Both are
 * <b>single-instance</b>: live sessions, task fan-out and replay protection are in process
 * memory, so run one gateway process per store.
 *
 * <p>The nested types are plain data holders. Values handed in by the engine may be kept;
 * values returned are mutated by the engine and written back with the matching
 * {@code put*} call before the change has to be durable.
 */
public interface Store {
  /** A uniqueness rule was violated (duplicate task id, idempotency key, event seq or second terminal). */
  final class Conflict extends RuntimeException { public Conflict(String m) { super(m); } }
  /** The referenced task does not exist. */
  final class NotFound extends RuntimeException { public NotFound(String m) { super(m); } }

  Set<String> TERMINAL = Set.of("SUCCEEDED", "FAILED", "CANCELLED");

  /** RFC 8628 device authorization (pairing) in progress. Keyed by {@code codeHash}. */
  final class DeviceAuth {
    public String codeHash, userCode, tenant, name, status, deviceID = "", owner = "";
    public byte[] pub;
    public long expires, lastPoll;
    public int interval;
  }

  /** A paired device. {@code status} is {@code active} or {@code revoked}. */
  final class Device {
    public String id, tenant, owner, name, status, e2eeKey = "";
    public byte[] pub;
    public Instant created;
  }

  record Idempotency(String tenant, String owner, String taskID, String bodyHash) {}

  record Event(long seq, String type, Map<String, Object> data) {}

  final class TaskRecord {
    public String id, tenant, owner, device, model, state;
    public final List<Event> events = new ArrayList<>();
  }

  byte[] getSecret(String name);
  void putSecret(String name, byte[] value);

  void putDeviceAuth(DeviceAuth a);
  DeviceAuth getDeviceAuth(String codeHash);
  DeviceAuth deviceAuthByUserCode(String tenant, String userCode);

  void putDevice(Device d);
  Device getDevice(String id);
  /** Devices of a tenant; {@code owner == null} lists every owner. */
  List<Device> listDevices(String tenant, String owner);

  Idempotency getIdempotency(String tenant, String owner, String key);
  /** Atomically stores the task and (if {@code idemKey != null}) its idempotency record; {@link Conflict} if either exists. */
  void createTask(TaskRecord t, String idemKey, Idempotency rec);
  /** Task with its events in seq order, or null. */
  TaskRecord getTask(String id);
  /** Non-terminal state change. */
  void setState(String id, String state);
  /** Appends a non-terminal event; {@link Conflict} if the seq is already stored. */
  void appendEvent(String id, Event ev);
  /** Atomically appends the terminal event and sets the terminal state; {@link Conflict} if already terminal or seq taken, {@link NotFound} if missing. */
  void finishTask(String id, Event ev, String state);
}
