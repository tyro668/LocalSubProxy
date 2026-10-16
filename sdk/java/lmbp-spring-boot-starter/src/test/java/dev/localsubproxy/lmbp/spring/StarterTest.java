package dev.localsubproxy.lmbp.spring;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import dev.localsubproxy.lmbp.LMBPServer;
import dev.localsubproxy.lmbp.LmbpApiException;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.net.http.WebSocket;
import java.net.http.WebSocketHandshakeException;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CompletionException;
import org.junit.jupiter.api.Test;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.boot.test.web.server.LocalServerPort;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.RestController;

/** Auto-configuration wires the filter, WebSocket and client; app routes still work. */
@SpringBootTest(webEnvironment = SpringBootTest.WebEnvironment.RANDOM_PORT, properties = {
    "lmbp.tenant=acme", "lmbp.api-key=0123456789abcdef0123456789abcdef", "lmbp.issuer=http://127.0.0.1", "lmbp.display-name=Acme Test"})
class StarterTest {
  static final String KEY = "0123456789abcdef0123456789abcdef";

  @SpringBootApplication
  @RestController
  static class App {
    @GetMapping("/hello") String hello() { return "app"; }
  }

  @LocalServerPort int port;
  @Autowired LmbpClient lmbp;
  @Autowired LMBPServer lsp;
  final HttpClient http = HttpClient.newHttpClient();

  HttpResponse<String> get(String path, String... headers) throws Exception {
    HttpRequest.Builder b = HttpRequest.newBuilder(URI.create("http://127.0.0.1:" + port + path));
    for (int i = 0; i < headers.length; i += 2) b.header(headers[i], headers[i + 1]);
    return http.send(b.build(), HttpResponse.BodyHandlers.ofString());
  }

  @Test void protocolRoutesAndAppRoutes() throws Exception {
    HttpResponse<String> r = get("/.well-known/lmbp?tenant=acme");
    assertEquals(200, r.statusCode());
    assertTrue(r.body().contains("\"signed_metadata\"") && r.body().contains("Acme Test"), r.body());
    assertEquals(200, get("/.well-known/jwks.json").statusCode());
    assertEquals("app", get("/hello").body());
    r = get("/v1/models", "Authorization", "Bearer " + KEY, "X-End-User", "alice");
    assertEquals(200, r.statusCode());
    assertTrue(r.body().contains("\"data\":[]"), r.body());
    r = get("/v1/models", "Authorization", "Bearer nope", "X-End-User", "alice");
    assertEquals(401, r.statusCode());
    r = get("/lmbp/v1/connect");
    assertEquals(400, r.statusCode());
    assertTrue(r.headers().firstValue("Content-Type").orElse("").startsWith("application/problem+json"));
  }

  @Test void websocketHandshakeIsCheckedByTheEngine() {
    CompletionException e = assertThrows(CompletionException.class, () -> http.newWebSocketBuilder().subprotocols("lmbp").header("Authorization", "Bearer bad")
        .buildAsync(URI.create("ws://127.0.0.1:" + port + "/lmbp/v1/connect"), new WebSocket.Listener() { }).join());
    assertTrue(e.getCause() instanceof WebSocketHandshakeException);
    assertEquals(401, ((WebSocketHandshakeException) e.getCause()).getResponse().statusCode());
  }

  @Test void clientFacade() {
    assertEquals("lmbp://pair?v=1&gateway=http%3A%2F%2F127.0.0.1&tenant=acme", lmbp.pairLink("alice"));
    assertEquals(List.of(), lmbp.models("alice"));
    LmbpApiException e = assertThrows(LmbpApiException.class, () -> lmbp.chat("alice", Map.of("model", "dev_x/m", "messages", List.of(Map.of("role", "user", "content", "hi")))));
    assertEquals("MODEL_NOT_FOUND", e.code());
    assertThrows(IllegalArgumentException.class, () -> lmbp.chat("", Map.of()));
    assertEquals(lsp.kid(), lmbp.server().kid());
  }
}
