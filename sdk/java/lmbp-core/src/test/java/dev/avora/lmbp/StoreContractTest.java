package dev.avora.lmbp;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertThrows;

import java.nio.file.Path;
import java.time.Instant;
import java.util.stream.Stream;
import org.junit.jupiter.api.io.TempDir;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.Arguments;
import org.junit.jupiter.params.provider.MethodSource;

/** The Store contract, run against MemoryStore and JdbcStore (SQLite). */
class StoreContractTest {
  @TempDir static Path dir;

  static Stream<Arguments> stores() {
    return Stream.of(Arguments.of("memory", new MemoryStore()), Arguments.of("sqlite", JdbcStore.open("jdbc:sqlite:" + dir.resolve("c.db"))));
  }

  static Store.TaskRecord task(String id) {
    Store.TaskRecord t = new Store.TaskRecord();
    t.id = id; t.tenant = "acme"; t.owner = "u1"; t.device = "dev_1"; t.model = "m"; t.state = "QUEUED";
    return t;
  }

  @ParameterizedTest(name = "{0}") @MethodSource("stores")
  void contract(String name, Store s) {
    s.putSecret("k", new byte[] {1, 2, 3});
    assertArrayEquals(new byte[] {1, 2, 3}, s.getSecret("k"));
    assertNull(s.getSecret("missing"));

    Store.Device d = new Store.Device();
    d.id = "dev_1"; d.tenant = "acme"; d.owner = "u1"; d.name = "pc"; d.status = "active"; d.pub = new byte[32]; d.created = Instant.ofEpochSecond(1000);
    s.putDevice(d);
    d.status = "revoked";
    s.putDevice(d);
    assertEquals("revoked", s.getDevice("dev_1").status);
    assertEquals("", s.getDevice("dev_1").e2eeKey);
    assertFalse(s.setDeviceE2EEKey("dev_1", "k1"), "E2EE key written to a revoked device");
    assertEquals("revoked", s.getDevice("dev_1").status);
    assertFalse(s.setDeviceE2EEKey("missing", "k1"));
    Store.Device d2 = new Store.Device();
    d2.id = "dev_2"; d2.tenant = "acme"; d2.owner = "u9"; d2.name = "pc2"; d2.status = "active"; d2.pub = new byte[32]; d2.created = Instant.ofEpochSecond(1000);
    s.putDevice(d2);
    assertTrue(s.setDeviceE2EEKey("dev_2", "k2"));
    assertEquals("k2", s.getDevice("dev_2").e2eeKey);
    assertEquals("active", s.getDevice("dev_2").status);
    assertEquals(1, s.listDevices("acme", "u1").size());
    assertEquals(0, s.listDevices("acme", "u2").size());
    assertEquals(2, s.listDevices("acme", null).size());

    Store.DeviceAuth a = new Store.DeviceAuth();
    a.codeHash = "h"; a.userCode = "BCDFGHJK"; a.tenant = "acme"; a.name = "pc"; a.status = "pending"; a.pub = new byte[32]; a.expires = 5; a.interval = 2;
    s.putDeviceAuth(a);
    assertEquals("pending", s.deviceAuthByUserCode("acme", "BCDFGHJK").status);
    assertNull(s.deviceAuthByUserCode("other", "BCDFGHJK"));

    Store.TaskRecord sealed = task("t_e2ee");
    sealed.e2ee = true;
    s.createTask(sealed, null, null);
    assertTrue(s.getTask("t_e2ee").e2ee, "E2EE mode must survive a restart (review7 #6)");
    assertFalse(s.getTask("t_e2ee") == null);
    s.createTask(task("t1"), "key", new Store.Idempotency("acme", "u1", "t1", "hash"));
    assertThrows(Store.Conflict.class, () -> s.createTask(task("t1"), null, null));
    assertThrows(Store.Conflict.class, () -> s.createTask(task("t2"), "key", new Store.Idempotency("acme", "u1", "t2", "hash")));
    assertNull(s.getTask("t2"), "a failed createTask must not leave a task behind");
    assertEquals("t1", s.getIdempotency("acme", "u1", "key").taskID());
    assertNull(s.getIdempotency("acme", "u2", "key"));
    s.setState("t1", "RUNNING");
    s.appendEvent("t1", new Store.Event(1, "task.delta", Json.obj("text", "a")));
    assertThrows(Store.Conflict.class, () -> s.appendEvent("t1", new Store.Event(1, "task.delta", Json.obj("text", "b"))));
    s.finishTask("t1", new Store.Event(2, "task.done", Json.obj("finish_reason", "stop")), "SUCCEEDED");
    assertThrows(Store.Conflict.class, () -> s.finishTask("t1", new Store.Event(3, "task.error", Json.obj()), "FAILED"));
    assertThrows(Store.NotFound.class, () -> s.finishTask("nope", new Store.Event(1, "task.done", Json.obj()), "SUCCEEDED"));
    Store.TaskRecord r = s.getTask("t1");
    assertNotNull(r);
    assertEquals("SUCCEEDED", r.state);
    assertEquals(2, r.events.size());
    assertEquals("a", r.events.get(0).data().get("text"));
  }
}
