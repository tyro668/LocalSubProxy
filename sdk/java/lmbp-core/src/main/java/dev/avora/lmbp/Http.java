package dev.avora.lmbp;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.LinkedHashMap;
import java.util.Map;

/** Response writer over an {@link HttpSink}; every sink call runs with the gateway lock released. */
final class Http {
  private Http() {}

  static final class Res {
    final Gateway g;
    final HttpSink sink;
    final HttpRequest req;
    final Map<String, String> headers = new LinkedHashMap<>();
    boolean headersSent, ended;

    Res(Gateway g, HttpSink sink, HttpRequest req) { this.g = g; this.sink = sink; this.req = req; }

    void setHeader(String k, String v) { headers.put(k, v); }

    String getHeader(String k) { return headers.get(k); }

    boolean writeHead(int status, Map<String, String> extra) {
      if (headersSent) return false;
      headersSent = true;
      if (extra != null) headers.putAll(extra);
      Map<String, String> h = new LinkedHashMap<>(headers);
      boolean[] ok = {true};
      g.unlocked(() -> { try { sink.start(status, h); } catch (IOException | RuntimeException e) { ok[0] = false; } });
      if (!ok[0]) g.abortReq(req.abort);
      return ok[0];
    }

    boolean write(String s) { return raw(s.getBytes(StandardCharsets.UTF_8)); }

    boolean raw(byte[] b) {
      if (ended || req.abort.aborted) return false;
      boolean[] ok = {true};
      g.unlocked(() -> { try { sink.write(b); } catch (IOException | RuntimeException e) { ok[0] = false; } });
      if (!ok[0]) g.abortReq(req.abort);
      return ok[0];
    }

    void end(byte[] body) {
      if (ended) return;
      if (body != null && body.length > 0) raw(body);
      ended = true;
    }

    void end() { end(null); }

    void send(int status, String contentType, byte[] body) {
      if (headers.get("Content-Type") == null) headers.put("Content-Type", contentType);
      headers.put("Content-Length", String.valueOf(body.length));
      writeHead(status, null);
      if (!"HEAD".equals(req.method)) end(body); else end();
    }
  }
}
