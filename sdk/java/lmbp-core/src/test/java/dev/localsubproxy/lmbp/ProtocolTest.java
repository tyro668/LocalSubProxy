package dev.localsubproxy.lmbp;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.Set;
import org.junit.jupiter.api.Test;

/** Shared JOSE vectors (incl. every negative), JWK, JSON, origins and parameter validation. */
class ProtocolTest {
  static final Path ROOT = Path.of(System.getProperty("lmbp.root", "../../.."));
  static final Set<String> KINDS = Set.of("proof", "client_assertion", "access_token", "e2ee_key");

  static void verify(Map<String, Object> f, Map<String, Object> v) {
    byte[] key = Lmbp.unb64(Json.str(Json.get(Json.get(f, "keys"), Json.str(v, "verify_with_key")), "public_b64url"));
    long now = Json.longOf(f.get("now"));
    Object c = v.get("context"), compact = v.get("compact");
    switch (Json.str(v, "kind")) {
      case "proof" -> Lmbp.verifyProof(key, compact, Json.str(c, "iss"), Json.str(c, "aud"), Json.str(c, "credential"), now);
      case "client_assertion" -> Lmbp.verifyJWT(key, compact, Lmbp.TYP_ASSERTION, Json.str(c, "aud"), now);
      case "access_token" -> Lmbp.verifyJWT(key, compact, Lmbp.TYP_ACCESS, Json.str(c, "aud"), now);
      case "e2ee_key" -> Lmbp.verifyE2EEKey(key, Json.str(c, "device_id"), compact, now);
      default -> throw new IllegalStateException();
    }
  }

  @Test void joseVectors() throws Exception {
    Map<String, Object> f = Json.asObj(Json.parse(Files.readString(ROOT.resolve("spec/testvectors/jose.json"))));
    List<String> bad = new ArrayList<>();
    int n = 0;
    for (Object o : Json.asList(f.get("valid"))) {
      Map<String, Object> v = Json.asObj(o);
      if (!KINDS.contains(Json.str(v, "kind"))) continue;
      try { verify(f, v); } catch (RuntimeException e) { bad.add("valid " + v.get("name") + " rejected: " + e.getMessage()); }
    }
    for (Object o : Json.asList(f.get("negative"))) {
      Map<String, Object> v = Json.asObj(o);
      if (!KINDS.contains(Json.str(v, "kind"))) continue;
      n++;
      try { verify(f, v); bad.add("negative " + v.get("name") + " accepted"); } catch (RuntimeException e) { /* expected */ }
    }
    assertTrue(n > 50, "only " + n + " negative vectors");
    assertTrue(bad.isEmpty(), String.join("\n", bad));
    Object d = Json.get(f.get("keys"), "device");
    Lmbp.SigningKey k = Lmbp.signingKey(Lmbp.unb64(Json.str(d, "seed_b64url")));
    assertEquals(Json.str(d, "public_b64url"), Lmbp.b64(k.pub()));
    assertEquals(Json.str(d, "jkt"), Lmbp.thumbprint(k.pub()));
    assertEquals(Json.stringify(Json.get(d, "jwk")), Json.stringify(Lmbp.publicJWK(k.pub())));
  }

  @Test void signVerifyRoundTrip() {
    Lmbp.SigningKey k = Lmbp.signingKey(new byte[32]);
    long t = Lmbp.now();
    String jws = Lmbp.signJWS(k.priv(), Lmbp.TYP_ACCESS, Lmbp.thumbprint(k.pub()), Json.obj("iss", "i", "sub", "s", "aud", "a", "jti", "j", "iat", t, "exp", t + 60));
    Lmbp.verifyJWT(k.pub(), jws, Lmbp.TYP_ACCESS, "a", t);
    assertThrows(IllegalArgumentException.class, () -> Lmbp.verifyJWT(k.pub(), jws, Lmbp.TYP_ACCESS, "b", t));
  }

  @Test void jsonLikeJavaScript() {
    assertEquals("{\"b\":1,\"a\":[1,0.5,0,1e+21,\"\u2028x\"],\"c\":null}", Json.stringify(Json.parse("{\"b\":1,\"a\":[1.0,0.5,-0.0,1e21,\"\\u2028x\"],\"c\":null}")));
    assertTrue(Json.truthy(0.5) && !Json.truthy(0L) && !Json.truthy(""));
  }

  @Test void origins() {
    assertEquals("https://gw.example.com", Lmbp.origin("HTTPS://GW.Example.com:443/"));
    assertEquals("http://127.0.0.1:8080", Lmbp.origin("http://127.0.0.1:8080"));
    for (String bad : new String[] {"http://gw.example.com", "https://gw.example.com/path", "https://u@gw.example.com"})
      assertThrows(IllegalArgumentException.class, () -> Lmbp.origin(bad), bad);
  }

  @Test void chatParameters() {
    Params.ParamError e = assertThrows(Params.ParamError.class,
        () -> Params.validateChatParams(Json.asObj(Json.parse("{\"model\":\"x\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"temperature\":3}"))));
    assertEquals("temperature", e.param);
    Params.Sampling p = Params.validateChatParams(Json.asObj(Json.parse("{\"model\":\"x\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"max_completion_tokens\":5,\"stop\":\"x\"}")));
    assertEquals(5L, p.maxTokens);
    assertEquals(1, p.stop.size());
  }
}
