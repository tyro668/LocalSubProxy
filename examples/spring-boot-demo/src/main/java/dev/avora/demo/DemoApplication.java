package dev.avora.demo;

import dev.avora.lmbp.LmbpApiException;
import dev.avora.lmbp.Pairing;
import dev.avora.lmbp.spring.LmbpClient;
import dev.avora.lmbp.spring.LmbpUserResolver;
import java.util.List;
import java.util.Map;
import org.springframework.boot.SpringApplication;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.context.annotation.Bean;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.ExceptionHandler;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PathVariable;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RestController;
import org.springframework.web.servlet.mvc.method.annotation.SseEmitter;

/**
 * Spring Boot SaaS example. The starter serves every protocol route (/.well-known/*, /lmbp/v1/*,
 * /v1/chat/completions, /v1/models, /v1/messages) and the connector WebSocket. Business code does:
 *   1. pairing   GET /app/pair, POST /app/pair/approve {user_code}, GET /app/devices, POST /app/devices/{id}/revoke
 *   2. LLM calls POST /app/ask {model, prompt}, POST /app/ask/stream (SSE) — or the official OpenAI /
 *                Anthropic clients pointed at this server with the tenant API key and X-End-User.
 *
 *   TENANT_API_KEY=$(openssl rand -hex 24) java -jar target/spring-boot-demo-0.1.0-SNAPSHOT.jar
 */
@SpringBootApplication
@RestController
public class DemoApplication {
  private final LmbpClient lmbp;

  DemoApplication(LmbpClient lmbp) { this.lmbp = lmbp; }

  public static void main(String[] args) { SpringApplication.run(DemoApplication.class, args); }

  /** Your login decides who the end user is. Demo: a header. */
  @Bean LmbpUserResolver users() {
    return req -> { String u = req.getHeader("X-Demo-User"); return u == null || u.isEmpty() ? "demo-user" : u; };
  }

  // ---- 1. pairing ----
  @GetMapping("/app/pair")
  Map<String, Object> pair() { return Map.of("link", lmbp.pairLink(lmbp.currentUser())); }

  @PostMapping("/app/pair/approve")
  Pairing.Decision approve(@RequestBody Map<String, String> body) { return lmbp.approve(lmbp.currentUser(), body.get("user_code")); }

  @GetMapping("/app/devices")
  List<Pairing.Device> devices() { return lmbp.devices(lmbp.currentUser()); }

  @PostMapping("/app/devices/{id}/revoke")
  Map<String, Object> revoke(@PathVariable String id) { return Map.of("revoked", lmbp.revoke(lmbp.currentUser(), id)); }

  // ---- 2. model calls ----
  static Map<String, Object> chatRequest(Map<String, Object> body) {
    return Map.of("model", String.valueOf(body.get("model")), "messages", List.of(Map.of("role", "user", "content", String.valueOf(body.get("prompt")))));
  }

  @PostMapping("/app/ask")
  Map<String, Object> ask(@RequestBody Map<String, Object> body) {
    Map<String, Object> r = lmbp.chat(chatRequest(body));
    Object msg = ((Map<?, ?>) ((List<?>) r.get("choices")).get(0)).get("message");
    return Map.of("answer", String.valueOf(((Map<?, ?>) msg).get("content")));
  }

  @PostMapping("/app/ask/stream")
  SseEmitter askStream(@RequestBody Map<String, Object> body) { return lmbp.stream(chatRequest(body)); }

  @ExceptionHandler(LmbpApiException.class)
  ResponseEntity<Map<String, Object>> apiError(LmbpApiException e) {
    return ResponseEntity.status(e.status() > 0 ? e.status() : 502).body(Map.of("error", Map.of("code", e.code(), "message", String.valueOf(e.getMessage()))));
  }

  @ExceptionHandler(Pairing.PairingException.class)
  ResponseEntity<Map<String, Object>> pairError(Pairing.PairingException e) {
    int st = switch (e.code()) { case "NOT_FOUND" -> 404; case "RATE_LIMITED" -> 429; case "FORBIDDEN", "CONSENT_DENIED" -> 403; default -> 400; };
    return ResponseEntity.status(st).body(Map.of("error", Map.of("code", e.code(), "message", String.valueOf(e.getMessage()))));
  }
}
