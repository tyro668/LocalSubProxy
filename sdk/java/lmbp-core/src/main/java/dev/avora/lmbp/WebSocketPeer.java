package dev.avora.lmbp;

import java.io.IOException;

/**
 * The transport side of one accepted WebSocket (subprotocol {@code lmbp}). The engine calls
 * these from a single writer thread per connection, in order, never with an engine lock held.
 * The transport must reject inbound messages larger than {@link LMBPServer#MAX_FRAME_BYTES}
 * (close 1009) and report events to the {@link WebSocketSession} it was attached to.
 */
public interface WebSocketPeer {
  void sendText(String text) throws IOException;

  void sendPing() throws IOException;

  /** Close handshake with a status code; the transport should then call {@link WebSocketSession#onClose()}. */
  void close(int code, String reason);

  /** Drop the connection without a close handshake. */
  void terminate();
}
