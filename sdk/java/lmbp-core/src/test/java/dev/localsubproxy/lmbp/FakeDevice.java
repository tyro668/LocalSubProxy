package dev.localsubproxy.lmbp;

import java.net.URI;
import java.net.URLEncoder;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.net.http.WebSocket;
import java.nio.charset.StandardCharsets;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CompletionStage;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.TimeUnit;

/** A minimal connector for tests: pairs over HTTP, connects the lmbp WebSocket and answers tasks with a scripted reply. */
final class FakeDevice implements AutoCloseable {
  enum Mode { REPLY, HANG }

  final String base, tenant;
  final Lmbp.SigningKey key = Lmbp.signingKey(LMBPServer.generateSigningSeed());
  final HttpClient http = HttpClient.newHttpClient();
  final List<Map<String, Object>> received = new CopyOnWriteArrayList<>();
  volatile Mode mode = Mode.REPLY;
  volatile List<String> reply = List.of("hello", " world");
  String deviceId, token;
  WebSocket ws;

  FakeDevice(String base, String tenant) { this.base = base; this.tenant = tenant; }

  String proof(String iss, String aud, String cred) {
    return Lmbp.signJWS(key.priv(), Lmbp.TYP_PROOF, Lmbp.thumbprint(key.pub()),
        Json.obj("iss", iss, "aud", aud, "jti", Lmbp.randomID("j_"), "ath", Lmbp.ath(cred), "iat", Lmbp.now()));
  }

  Map<String, Object> post(String path, String form) throws Exception {
    HttpResponse<String> r = http.send(HttpRequest.newBuilder(URI.create(base + path)).header("Content-Type", "application/x-www-form-urlencoded")
        .POST(HttpRequest.BodyPublishers.ofString(form)).build(), HttpResponse.BodyHandlers.ofString());
    return Json.asObj(Json.parse(r.body()));
  }

  static String enc(String s) { return URLEncoder.encode(s, StandardCharsets.UTF_8); }

  /** Starts pairing; returns the user code to approve. */
  String authorize() throws Exception {
    Map<String, Object> a = post("/lmbp/v1/device/authorize", "tenant=" + enc(tenant) + "&device_name=test-pc&public_key=" + Lmbp.b64(key.pub()));
    dc = (String) a.get("device_code");
    return (String) a.get("user_code");
  }

  String dc;
  /** Runs after the WebSocket upgrade, before the hello (e.g. a revoke). */
  volatile Runnable beforeHello;

  void token() throws Exception {
    Map<String, Object> t = post("/lmbp/v1/token", "grant_type=" + enc("urn:ietf:params:oauth:grant-type:device_code") + "&device_code=" + enc(dc) + "&proof=" + enc(proof("", Lmbp.PROOF_TOKEN, dc)));
    if (t.get("access_token") == null) throw new IllegalStateException("token: " + Json.stringify(t));
    token = (String) t.get("access_token");
    deviceId = (String) t.get("device_id");
  }

  void connect(String model) throws Exception {
    CompletableFuture<Void> welcome = new CompletableFuture<>();
    StringBuilder buf = new StringBuilder();
    ws = http.newWebSocketBuilder().header("Authorization", "Bearer " + token).subprotocols("lmbp")
        .buildAsync(URI.create("ws" + base.substring(4) + "/lmbp/v1/connect"), new WebSocket.Listener() {
          @Override public CompletionStage<?> onText(WebSocket w, CharSequence data, boolean last) {
            buf.append(data);
            if (last) {
              Map<String, Object> m = Json.asObj(Json.parse(buf.toString()));
              buf.setLength(0);
              received.add(m);
              if ("welcome".equals(m.get("type"))) welcome.complete(null);
              if ("task.run".equals(m.get("type"))) Thread.ofVirtual().start(() -> run((String) m.get("task_id")));
            }
            w.request(1);
            return null;
          }
        }).get(5, TimeUnit.SECONDS);
    if (beforeHello != null) beforeHello.run();
    send(Json.obj("v", 1L, "type", "hello", "id", "h1", "data", Json.obj("device_id", deviceId, "proof", proof(deviceId, Lmbp.PROOF_HELLO, token),
        "capabilities", List.of("tools"), "models", List.of(model), "model_capabilities", Json.obj(model, List.of("tools")), "max_concurrency", 4L)));
    welcome.get(5, TimeUnit.SECONDS);
  }

  synchronized void send(Map<String, Object> m) {
    ws.sendText(Json.stringify(m), true).join();
  }

  void run(String taskId) {
    if (mode == Mode.HANG) {
      send(Json.obj("v", 1L, "type", "task.delta", "task_id", taskId, "seq", 1L, "data", Json.obj("text", "partial")));
      return;
    }
    long seq = 0;
    for (String part : reply) send(Json.obj("v", 1L, "type", "task.delta", "task_id", taskId, "seq", ++seq, "data", Json.obj("text", part)));
    send(Json.obj("v", 1L, "type", "task.done", "task_id", taskId, "seq", ++seq, "data", Json.obj("finish_reason", "stop", "usage", Json.obj("prompt_tokens", 1L, "completion_tokens", 2L, "total_tokens", 3L))));
  }

  boolean sawCancel(String taskId) {
    for (Map<String, Object> m : received) if ("task.cancel".equals(m.get("type")) && taskId.equals(m.get("task_id"))) return true;
    return false;
  }

  static void waitFor(java.util.function.BooleanSupplier c, long ms) throws InterruptedException {
    long end = System.currentTimeMillis() + ms;
    while (!c.getAsBoolean() && System.currentTimeMillis() < end) Thread.sleep(20);
  }

  @Override public void close() { if (ws != null) ws.abort(); }
}
