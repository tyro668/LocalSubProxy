package dev.avora.lmbp.spring;

import dev.avora.lmbp.HttpRequest;
import dev.avora.lmbp.LMBPServer;
import dev.avora.lmbp.WebSocketHandshake;
import dev.avora.lmbp.WebSocketPeer;
import java.io.IOException;
import java.nio.ByteBuffer;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import org.springframework.http.HttpStatusCode;
import org.springframework.http.server.ServerHttpRequest;
import org.springframework.http.server.ServerHttpResponse;
import org.springframework.web.socket.BinaryMessage;
import org.springframework.web.socket.CloseStatus;
import org.springframework.web.socket.PingMessage;
import org.springframework.web.socket.PongMessage;
import org.springframework.web.socket.TextMessage;
import org.springframework.web.socket.WebSocketHandler;
import org.springframework.web.socket.WebSocketSession;
import org.springframework.web.socket.handler.AbstractWebSocketHandler;
import org.springframework.web.socket.server.HandshakeInterceptor;

/** Bridges Spring WebSocket to the engine: the interceptor runs the LMBP handshake checks, the handler relays frames. */
public class LmbpWebSocketHandler extends AbstractWebSocketHandler implements HandshakeInterceptor {
  static final String HS = "lmbp.handshake", SESSION = "lmbp.session";
  private final LMBPServer lmbp;

  public LmbpWebSocketHandler(LMBPServer lmbp) { this.lmbp = lmbp; }

  @Override
  public boolean beforeHandshake(ServerHttpRequest request, ServerHttpResponse response, WebSocketHandler wsHandler, Map<String, Object> attributes) throws IOException {
    Map<String, String> h = new LinkedHashMap<>();
    for (Map.Entry<String, List<String>> e : request.getHeaders().entrySet()) h.put(e.getKey(), String.join(", ", e.getValue()));
    HttpRequest r = new HttpRequest("GET", request.getURI().getPath(), request.getURI().getRawQuery(), h, new byte[0]);
    WebSocketHandshake hs = lmbp.handshake(r);
    if (!hs.accepted()) {
      response.setStatusCode(HttpStatusCode.valueOf(hs.status()));
      hs.headers().forEach((k, v) -> response.getHeaders().set(k, v));
      response.getBody().write(hs.body());
      response.flush();
      return false;
    }
    attributes.put(HS, hs);
    return true;
  }

  @Override public void afterHandshake(ServerHttpRequest request, ServerHttpResponse response, WebSocketHandler wsHandler, Exception exception) { }

  @Override public void afterConnectionEstablished(WebSocketSession s) {
    WebSocketHandshake hs = (WebSocketHandshake) s.getAttributes().get(HS);
    dev.avora.lmbp.WebSocketSession ls = hs.accept(new WebSocketPeer() {
      @Override public void sendText(String text) throws IOException { s.sendMessage(new TextMessage(text)); }
      @Override public void sendPing() throws IOException { s.sendMessage(new PingMessage(ByteBuffer.allocate(0))); }
      @Override public void close(int code, String reason) { try { s.close(new CloseStatus(code, reason)); } catch (IOException | RuntimeException e) { terminate(); } }
      @Override public void terminate() { try { s.close(CloseStatus.SESSION_NOT_RELIABLE); } catch (IOException | RuntimeException e) { /* gone */ } }
    });
    s.getAttributes().put(SESSION, ls);
  }

  private static dev.avora.lmbp.WebSocketSession ls(WebSocketSession s) { return (dev.avora.lmbp.WebSocketSession) s.getAttributes().get(SESSION); }

  @Override protected void handleTextMessage(WebSocketSession s, TextMessage m) { ls(s).onText(m.getPayload()); }

  @Override protected void handleBinaryMessage(WebSocketSession s, BinaryMessage m) { ls(s).onBinary(m.getPayloadLength()); }

  @Override protected void handlePongMessage(WebSocketSession s, PongMessage m) { ls(s).onPong(); }

  @Override public void handleTransportError(WebSocketSession s, Throwable e) { var l = ls(s); if (l != null) l.onClose(); }

  @Override public void afterConnectionClosed(WebSocketSession s, CloseStatus st) { var l = ls(s); if (l != null) l.onClose(); }
}
