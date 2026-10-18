package dev.avora.lmbp;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.Test;

/** Review9 P2-10: tool_result images are converted, unsupported blocks rejected, never dropped. */
class AnthropicToolResultTest {
  static Map<String, Object> conv(List<Object> content) {
    return Json.obj("model", "m", "max_tokens", 8L, "messages", List.of(
        Json.obj("role", "user", "content", "take a screenshot"),
        Json.obj("role", "assistant", "content", List.of(Json.obj("type", "tool_use", "id", "toolu_1", "name", "screenshot", "input", Json.obj()))),
        Json.obj("role", "user", "content", List.of(Json.obj("type", "tool_result", "tool_use_id", "toolu_1", "content", content)))));
  }

  @Test void imageConverted() {
    Map<String, Object> out = Anthropic.toOpenAI(conv(List.of(Json.obj("type", "text", "text", "here"),
        Json.obj("type", "image", "source", Json.obj("type", "base64", "media_type", "image/png", "data", "QUJD")))));
    List<Object> msgs = Json.asList(out.get("messages"));
    Object last = msgs.get(msgs.size() - 1);
    assertEquals("user", Json.get(last, "role"));
    boolean found = false;
    for (Object p : Json.asList(Json.get(last, "content")))
      if ("image_url".equals(Json.get(p, "type")) && "data:image/png;base64,QUJD".equals(Json.get(Json.get(p, "image_url"), "url"))) found = true;
    assertTrue(found, Json.stringify(out));
  }

  @Test void unsupportedRejected() {
    IllegalArgumentException e = assertThrows(IllegalArgumentException.class,
        () -> Anthropic.toOpenAI(conv(List.of(Json.obj("type", "document", "source", Json.obj("type", "text", "data", "x"))))));
    assertTrue(e.getMessage().contains("tool_result content type document"), e.getMessage());
  }
}
