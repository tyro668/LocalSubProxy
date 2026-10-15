package dev.localsubproxy.lmbp;

import java.nio.charset.StandardCharsets;
import java.util.concurrent.LinkedBlockingQueue;

/**
 * Engine side of one accepted connector WebSocket. Obtained from
 * {@link WebSocketHandshake#accept(WebSocketPeer)}; the transport reports inbound events here.
 * Outbound frames are queued and written in order by one writer thread.
 */
public final class WebSocketSession {
  interface Handler { void message(String text, boolean binary, int size); }

  final Gateway g;
  final WebSocketPeer peer;
  private final LinkedBlockingQueue<Object> queue = new LinkedBlockingQueue<>();
  private static final Object PING = new Object(), END = new Object();
  private record Close(int code, String reason) {}
  volatile boolean open = true;
  boolean closeFired;
  Handler onMessage;
  Runnable onPong, onClose;

  WebSocketSession(Gateway g, WebSocketPeer peer) {
    this.g = g;
    this.peer = peer;
    Thread.ofVirtual().name("lmbp-ws-writer").start(this::writer);
  }

  private void writer() {
    try {
      for (;;) {
        Object f = queue.take();
        if (f == END) { peer.terminate(); break; }
        if (f instanceof Close c) { peer.close(c.code(), c.reason()); break; }
        if (f == PING) peer.sendPing();
        else peer.sendText((String) f);
      }
    } catch (Exception e) {
      peer.terminate();
    }
  }

  // ---- transport → engine ----
  /** A complete inbound text message. */
  public void onText(String text) {
    Handler h = onMessage;
    int size = text.getBytes(StandardCharsets.UTF_8).length;
    // Some containers cap text messages in characters; the protocol limit is in UTF-8 bytes.
    if (size > Lmbp.MAX_FRAME_BYTES) { g.locked(() -> close(1009, "message too big")); return; }
    if (h != null && open) g.locked(() -> h.message(text, false, size));
  }

  /** A complete inbound binary message (not allowed by LMBP; the engine closes 1003). */
  public void onBinary(int size) {
    Handler h = onMessage;
    if (h != null && open) g.locked(() -> h.message("", true, size));
  }

  public void onPong() { Runnable r = onPong; if (r != null) g.locked(r); }

  /** The connection is gone (closed by either side or lost). Idempotent. */
  public void onClose() {
    open = false;
    queue.add(END);
    g.locked(() -> {
      if (closeFired) return;
      closeFired = true;
      if (onClose != null) onClose.run();
    });
  }

  // ---- engine → transport (called with the engine lock held; never blocks) ----
  boolean isOpen() { return open; }

  void send(String text) {
    if (!open) throw new IllegalStateException("closed");
    queue.add(text);
  }

  void ping() { if (open) queue.add(PING); }

  void close() { close(1000, ""); }

  void close(int code, String reason) {
    if (!open) return;
    open = false;
    queue.add(new Close(code, reason));
  }

  void terminate() {
    open = false;
    queue.add(END);
  }
}
