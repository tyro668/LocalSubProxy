package dev.localsubproxy.lmbp;

import java.io.ByteArrayOutputStream;
import java.net.URLEncoder;
import java.nio.charset.StandardCharsets;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * In-process client for one end user of one tenant: the same engine path as an HTTP request
 * with the tenant API key and {@code X-End-User}, without the HTTP hop. Requests and responses
 * are the OpenAI / Anthropic JSON shapes as {@code Map}s (see {@link Json} to convert).
 *
 * <p>Every call ends with a definite result: a response, an {@link LmbpApiException} carrying the
 * LMBP error code, or (streams) an {@link IncompleteStreamException} with the resume cursor.
 */
public final class InProcessClient {
  private final LMBPServer s;
  private final String tenant, user;

  InProcessClient(LMBPServer s, String tenant, String user) { this.s = s; this.tenant = tenant; this.user = user; }

  /** Per-call options: {@code idempotencyKey} makes the call retry-safe (same key + same body = same task). */
  public record Options(String idempotencyKey, String lastEventId) {
    public static final Options NONE = new Options(null, null);
    public static Options idempotent(String key) { return new Options(key, null); }
  }

  /** Models on this user's online devices ({@code dev_xxx/model}). */
  public List<Object> models() {
    Object v = json("GET", "/v1/models", null, Options.NONE);
    List<Object> d = Json.asList(Json.get(v, "data"));
    return d == null ? List.of() : d;
  }

  /** Non-streaming chat completion (OpenAI shape). */
  public Map<String, Object> call(Map<String, ?> request) { return call(request, Options.NONE); }

  public Map<String, Object> call(Map<String, ?> request, Options o) {
    return Json.asObj(json("POST", "/v1/chat/completions", with(request, false), o));
  }

  /** Streaming chat completion; close the stream (try-with-resources) to stop early. */
  public LmbpStream stream(Map<String, ?> request) { return stream(request, Options.NONE); }

  public LmbpStream stream(Map<String, ?> request, Options o) { return sse("POST", "/v1/chat/completions", with(request, true), o, false); }

  /** Anthropic Messages, non-streaming. */
  public Map<String, Object> messages(Map<String, ?> request) { return messages(request, Options.NONE); }

  public Map<String, Object> messages(Map<String, ?> request, Options o) { return Json.asObj(json("POST", "/v1/messages", with(request, false), o)); }

  /** Anthropic Messages, streaming (named events, {@code ping} filtered). */
  public LmbpStream messagesStream(Map<String, ?> request) { return messagesStream(request, Options.NONE); }

  public LmbpStream messagesStream(Map<String, ?> request, Options o) { return sse("POST", "/v1/messages", with(request, true), o, true); }

  /** Re-attach to a task's chat-completion stream after {@code lastEventId} ("" replays everything). */
  public LmbpStream resume(String taskId, String lastEventId) {
    return sse("GET", "/lmbp/v1/tenant/tasks/" + URLEncoder.encode(taskId, StandardCharsets.UTF_8) + "/events", null, new Options(null, lastEventId), false);
  }

  /** Cancels a running task of this user; true if this call decided its outcome (CANCELLED). */
  public boolean cancel(String taskId) {
    boolean[] r = {false};
    s.g.locked(() -> r[0] = s.g.cancelTask(tenant, user, taskId));
    return r[0];
  }

  // ---- plumbing ----
  private static Map<String, Object> with(Map<String, ?> req, boolean stream) {
    Map<String, Object> m = new LinkedHashMap<>(req);
    m.put("stream", stream);
    return m;
  }

  private HttpRequest request(String method, String path, Object body, Options o) {
    Map<String, String> h = new LinkedHashMap<>();
    h.put("content-type", "application/json");
    if (o.idempotencyKey() != null && !o.idempotencyKey().isEmpty()) h.put("idempotency-key", o.idempotencyKey());
    if (o.lastEventId() != null && !o.lastEventId().isEmpty()) h.put("last-event-id", o.lastEventId());
    HttpRequest r = new HttpRequest(method, path, "", h, body == null ? new byte[0] : Json.bytes(body));
    r.trustedTenant = tenant;
    r.trustedUser = user;
    return r;
  }

  private Object json(String method, String path, Object body, Options o) {
    HttpRequest r = request(method, path, body, o);
    int[] st = {0};
    ByteArrayOutputStream out = new ByteArrayOutputStream();
    s.handle(r, new HttpSink() {
      @Override public void start(int status, Map<String, String> headers) { st[0] = status; }
      @Override public void write(byte[] b) { out.writeBytes(b); }
    });
    Object v = Json.tryParse(out.toString(StandardCharsets.UTF_8));
    if (st[0] != 200) throw LmbpApiException.from(st[0], v);
    if (v == null) throw new LmbpApiException(502, "INTERNAL", "unparseable gateway response", null);
    return v;
  }

  private LmbpStream sse(String method, String path, Object body, Options o, boolean anthropic) {
    HttpRequest r = request(method, path, body, o);
    LmbpStream st = new LmbpStream(r, anthropic, o.lastEventId());
    Thread.ofVirtual().name("lmbp-inproc").start(() -> {
      try { s.handle(r, st.sink); } finally { st.end(); }
    });
    return st;
  }
}
