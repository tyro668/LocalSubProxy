package dev.avora.lmbp;

import java.util.Map;

/**
 * Result of {@link LMBPServer#handshake(HttpRequest)} for an upgrade to <code>/lmbp/v1/connect</code>.
 * If {@link #accepted()} is false, answer the upgrade with {@link #status()}, {@link #headers()}
 * and {@link #body()} (an RFC 9457 problem) instead of switching protocols. Otherwise complete
 * the upgrade with subprotocol {@link #subprotocol()} and call {@link #accept(WebSocketPeer)} once.
 */
public final class WebSocketHandshake {
  final Gateway g;
  final int status;
  final byte[] body;
  final Store.Device dev;
  final String tok, iss;
  final Map<String, Object> claims;
  private boolean used;

  WebSocketHandshake(Gateway g, int status, byte[] body, Store.Device dev, String tok, Map<String, Object> claims, String iss) {
    this.g = g; this.status = status; this.body = body; this.dev = dev; this.tok = tok; this.claims = claims; this.iss = iss;
  }

  public boolean accepted() { return status == 101; }

  public int status() { return status; }

  public Map<String, String> headers() { return Map.of("Content-Type", "application/problem+json", "Cache-Control", "no-store"); }

  public byte[] body() { return body == null ? new byte[0] : body.clone(); }

  public String subprotocol() { return Lmbp.SUBPROTOCOL; }

  /** Attach the upgraded connection; the hello must arrive within 15 s. */
  public synchronized WebSocketSession accept(WebSocketPeer peer) {
    if (!accepted()) throw new IllegalStateException("handshake was rejected");
    if (used) throw new IllegalStateException("handshake already accepted");
    used = true;
    WebSocketSession ws = new WebSocketSession(g, peer);
    g.locked(() -> g.session(ws, dev, tok, claims, iss));
    return ws;
  }
}
