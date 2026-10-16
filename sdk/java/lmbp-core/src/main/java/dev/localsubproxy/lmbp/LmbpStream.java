package dev.localsubproxy.lmbp;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.Iterator;
import java.util.Map;
import java.util.NoSuchElementException;
import java.util.concurrent.ArrayBlockingQueue;
import java.util.concurrent.TimeUnit;

/**
 * A streaming response. Iterate the parsed chunks (OpenAI {@code chat.completion.chunk}, or
 * Anthropic events with {@code ping} filtered). The iterator ends normally only at the
 * terminator; it throws {@link LmbpApiException} for an error event and
 * {@link IncompleteStreamException} for a truncated stream. {@link #close()} (or
 * {@link #cancel()}) stops reading; a non-idempotent task with no other reader is cancelled.
 *
 * <p>Buffering is bounded: when the consumer stops reading, the engine's writer blocks,
 * the device's flow-control window fills and the device pauses (or the task fails with
 * SLOW_CONSUMER after 60 s).
 */
public final class LmbpStream implements Iterator<Map<String, Object>>, Iterable<Map<String, Object>>, AutoCloseable {
  static final Object EOF = new Object();
  /** Put by close(): wakes a reader blocked in take(); the stream then ends. */
  private static final Object CLOSED = new Object();
  private static final int QUEUE = 64;

  final HttpRequest req;
  private final boolean anthropic;
  private final ArrayBlockingQueue<Object> q = new ArrayBlockingQueue<>(QUEUE);
  volatile int status;
  private volatile String taskId = "";
  private String lastEventId = "";
  private final StringBuilder buf = new StringBuilder();
  private final ByteArrayOutputStream pending = new ByteArrayOutputStream();
  private Map<String, Object> next;
  private boolean finished;
  private RuntimeException failure;
  private volatile boolean closed;

  LmbpStream(HttpRequest req, boolean anthropic, String lastEventId) {
    this.req = req;
    this.anthropic = anthropic;
    this.lastEventId = lastEventId == null ? "" : lastEventId;
  }

  /** Gateway task id (X-Task-Id), known once the response has started. */
  public String taskId() { return taskId; }

  /** SSE id of the last chunk returned: the resume cursor. */
  public String lastEventId() { return lastEventId; }

  // ---- engine side ----
  final HttpSink sink = new HttpSink() {
    @Override public void start(int st, Map<String, String> headers) {
      status = st;
      String t = headers.get("X-Task-Id");
      if (t != null) taskId = t;
    }

    @Override public void write(byte[] b) throws IOException { put(b); }
  };

  void put(Object o) throws IOException {
    try {
      while (!q.offer(o, 200, TimeUnit.MILLISECONDS)) if (closed) throw new IOException("stream closed by the consumer");
    } catch (InterruptedException e) {
      Thread.currentThread().interrupt();
      throw new IOException("interrupted");
    }
    if (closed && o != EOF) throw new IOException("stream closed by the consumer");
  }

  void end() {
    try { put(EOF); } catch (IOException e) { /* consumer gone */ }
    producerEnded = true;
  }

  /** Test observation: the engine side has finished producing. */
  volatile boolean producerEnded;

  // ---- consumer side ----
  @Override public Iterator<Map<String, Object>> iterator() { return this; }

  @Override public boolean hasNext() {
    if (failure != null) throw failure;
    while (next == null && !finished) pump();
    if (failure != null) throw failure;
    if (next == null) close();
    return next != null;
  }

  @Override public Map<String, Object> next() {
    if (!hasNext()) throw new NoSuchElementException();
    Map<String, Object> n = next;
    next = null;
    return n;
  }

  private Object take() {
    try { return q.take(); } catch (InterruptedException e) { Thread.currentThread().interrupt(); close(); throw new LmbpApiException(0, "CANCELLED", "interrupted", null); }
  }

  private void pump() {
    int i;
    while (next == null && !finished && status == 200 && (i = buf.indexOf("\n\n")) >= 0) {
      String ev = buf.substring(0, i);
      buf.delete(0, i + 2);
      event(ev);
    }
    if (next != null || finished) return;
    if (closed) { finished = true; return; } // explicit terminal state: never wait on a closed stream
    Object o = take();
    if (o == CLOSED) { finished = true; return; }
    if (o == EOF) {
      if (status >= 400 || (status != 0 && status != 200 && pending.size() > 0)) fail(errorFromBody());
      else fail(new IncompleteStreamException(taskId, lastEventId));
      return;
    }
    byte[] b = (byte[]) o;
    if (status != 200) pending.writeBytes(b);
    else buf.append(new String(b, StandardCharsets.UTF_8));
  }

  private LmbpApiException errorFromBody() {
    String s = pending.toString(StandardCharsets.UTF_8);
    Object v = Json.tryParse(s);
    return LmbpApiException.from(status, v);
  }

  private void event(String raw) {
    String id = null, type = null;
    StringBuilder data = new StringBuilder();
    for (String line : raw.split("\n")) {
      if (line.startsWith(":")) continue;
      if (line.startsWith("id:")) id = line.substring(3).trim();
      else if (line.startsWith("event:")) type = line.substring(6).trim();
      else if (line.startsWith("data:")) { if (data.length() > 0) data.append('\n'); data.append(line.substring(5).replaceFirst("^ ", "")); }
    }
    if (data.length() == 0) return;
    String d = data.toString();
    if (!anthropic && d.equals("[DONE]")) { done(); return; }
    Map<String, Object> v = Json.asObj(Json.tryParse(d));
    if (v == null) return;
    if (anthropic) {
      String t = type != null ? type : Json.str(v, "type");
      if ("ping".equals(t)) return;
      if ("error".equals(t)) { fail(LmbpApiException.from(502, v)); return; }
      if (id != null && !id.isEmpty()) lastEventId = id;
      next = v;
      if ("message_stop".equals(t)) finished = true;
      return;
    }
    if (v.containsKey("error")) { fail(LmbpApiException.from(502, v)); return; }
    if (id != null && !id.isEmpty()) lastEventId = id;
    next = v;
  }

  private void done() { finished = true; close(); }

  private void fail(RuntimeException e) { failure = e; finished = true; close(); }

  /** Stops the stream early; same as {@link #close()}. */
  public void cancel() { close(); }

  /** Releases the stream. Idempotent; returns at once. */
  @Override public void close() {
    if (closed) return;
    closed = true;
    req.abort();
    // Clearing may drop an EOF the producer already put; once it has exited nothing else
    // arrives. The closed flag ends the stream, and CLOSED wakes a reader blocked in take().
    q.clear();
    q.offer(CLOSED);
  }
}
