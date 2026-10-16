package dev.localsubproxy.lmbp;

import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

/**
 * Anthropic Messages compatibility (/v1/messages, LMBP spec §6.7): request
 * translation to the OpenAI chat shape and rendering of task events as an
 * Anthropic message or named-event SSE stream. Port of sdk/node/src/anthropic.ts.
 */
final class Anthropic {
  private Anthropic() {}

  static final Map<Integer, String> STATUS_TYPE = Map.of(400, "invalid_request_error", 422, "invalid_request_error", 401, "authentication_error",
      403, "permission_error", 404, "not_found_error", 413, "request_too_large", 429, "rate_limit_error", 503, "overloaded_error", 529, "overloaded_error");

  static String errorType(int status) { return STATUS_TYPE.getOrDefault(status, "api_error"); }

  static Map<String, Object> errorBody(int status, String code, String message) {
    return Json.obj("type", "error", "error", Json.obj("type", errorType(status), "message", message, "lmbp_code", code));
  }

  static String stopReason(Object finish) {
    return "length".equals(finish) ? "max_tokens" : "tool_calls".equals(finish) ? "tool_use" : "end_turn";
  }

  static String s(Object v) { return v == null ? "" : (v instanceof String str ? str : Json.stringify(v)); }

  /** Translate a Messages request into a chat-completions request; IllegalArgumentException → 400. */
  static Map<String, Object> toOpenAI(Map<String, Object> in) {
    if (!(in.get("model") instanceof String m) || m.isEmpty()) throw new IllegalArgumentException("model: field required");
    Object mt = in.get("max_tokens");
    if (!Json.isInteger(mt) || ((Number) mt).doubleValue() < 1) throw new IllegalArgumentException("max_tokens: field required (positive integer)");
    if (in.containsKey("lmbp_e2ee")) throw new IllegalArgumentException("lmbp_e2ee is only supported on /v1/chat/completions");
    Map<String, Object> out = Json.obj("model", in.get("model"), "max_tokens", mt, "stream", Boolean.TRUE.equals(in.get("stream")));
    for (String k : new String[] {"temperature", "top_p"}) if (in.get(k) != null) out.put(k, in.get(k));
    if (in.get("stop_sequences") != null) out.put("stop", in.get("stop_sequences"));
    List<Object> msgs = new ArrayList<>();
    Object sys = in.get("system");
    if (sys instanceof String ss) { if (!ss.isEmpty()) msgs.add(Json.obj("role", "system", "content", ss)); }
    else if (sys instanceof List<?> l) {
      List<String> parts = new ArrayList<>();
      for (Object b : l) { if (!"text".equals(Json.get(b, "type"))) throw new IllegalArgumentException("system: only text blocks are supported"); parts.add(s(Json.get(b, "text"))); }
      msgs.add(Json.obj("role", "system", "content", String.join("\n\n", parts)));
    } else if (sys != null) throw new IllegalArgumentException("system: must be a string or an array of text blocks");
    List<Object> list = Json.asList(in.get("messages"));
    if (list == null || list.isEmpty()) throw new IllegalArgumentException("messages: at least one message is required");
    for (int i = 0; i < list.size(); i++) {
      Object mo = list.get(i);
      Object role = Json.get(mo, "role");
      if (!"user".equals(role) && !"assistant".equals(role)) throw new IllegalArgumentException("messages." + i + ".role: must be user or assistant");
      Object content = Json.get(mo, "content");
      if (content instanceof String cs) { msgs.add(Json.obj("role", role, "content", cs)); continue; }
      List<Object> blocks = Json.asList(content);
      if (blocks == null) throw new IllegalArgumentException("messages." + i + ".content: must be a string or an array of blocks");
      List<Object> parts = new ArrayList<>(), calls = new ArrayList<>(), results = new ArrayList<>();
      List<String> text = new ArrayList<>();
      for (int j = 0; j < blocks.size(); j++) {
        Object b = blocks.get(j);
        Object type = Json.get(b, "type");
        String ty = type instanceof String t ? t : "";
        switch (ty) {
          case "text" -> { String t = s(Json.get(b, "text")); parts.add(Json.obj("type", "text", "text", t)); text.add(t); }
          case "image" -> {
            Object src = Json.get(b, "source");
            String url;
            if ("base64".equals(Json.get(src, "type"))) url = "data:" + s(Json.get(src, "media_type")) + ";base64," + s(Json.get(src, "data"));
            else if ("url".equals(Json.get(src, "type"))) url = s(Json.get(src, "url"));
            else throw new IllegalArgumentException("messages." + i + ".content." + j + ".source.type: base64 or url required");
            parts.add(Json.obj("type", "image_url", "image_url", Json.obj("url", url)));
          }
          case "tool_use" -> {
            if (!"assistant".equals(role)) throw new IllegalArgumentException("messages." + i + ": tool_use blocks belong to assistant messages");
            Object input = Json.get(b, "input");
            calls.add(Json.obj("id", s(Json.get(b, "id")), "type", "function", "function", Json.obj("name", s(Json.get(b, "name")), "arguments", input == null ? "{}" : Json.stringify(input))));
          }
          case "tool_result" -> {
            if (!"user".equals(role)) throw new IllegalArgumentException("messages." + i + ": tool_result blocks belong to user messages");
            String c = "";
            Object bc = Json.get(b, "content");
            if (bc instanceof String bs) c = bs;
            else if (bc instanceof List<?> zl) {
              List<String> ts = new ArrayList<>();
              for (Object z : zl) if ("text".equals(Json.get(z, "type"))) ts.add(s(Json.get(z, "text")));
              c = String.join("\n", ts);
            }
            if (Boolean.TRUE.equals(Json.get(b, "is_error"))) c = "Error: " + c;
            results.add(Json.obj("role", "tool", "tool_call_id", s(Json.get(b, "tool_use_id")), "content", c));
          }
          case "thinking", "redacted_thinking" -> { }
          default -> throw new IllegalArgumentException("messages." + i + ".content." + j + ": unsupported block type " + (type == null ? "undefined" : s(type)));
        }
      }
      msgs.addAll(results);
      if ("assistant".equals(role)) {
        if (text.isEmpty() && calls.isEmpty()) continue;
        Map<String, Object> am = Json.obj("role", "assistant", "content", String.join("", text));
        if (!calls.isEmpty()) { am.put("tool_calls", calls); if (text.isEmpty()) am.put("content", null); }
        msgs.add(am);
      } else if (!parts.isEmpty()) msgs.add(Json.obj("role", "user", "content", parts));
    }
    out.put("messages", msgs);
    List<Object> tools = Json.asList(in.get("tools"));
    if (tools != null && !tools.isEmpty()) {
      List<Object> ot = new ArrayList<>();
      for (int i = 0; i < tools.size(); i++) {
        Object t = tools.get(i);
        Object type = Json.get(t, "type");
        if (Json.asObj(t) != null && Json.asObj(t).containsKey("type") && !"custom".equals(type)) throw new IllegalArgumentException("tools." + i + ": server tool " + s(type) + " is not supported");
        if (!Json.truthy(Json.get(t, "name"))) throw new IllegalArgumentException("tools." + i + ".name: field required");
        Map<String, Object> fn = Json.obj("name", Json.get(t, "name"), "parameters", Json.get(t, "input_schema"));
        if (Json.get(t, "description") instanceof String d) fn.put("description", d);
        ot.add(Json.obj("type", "function", "function", fn));
      }
      out.put("tools", ot);
    }
    Object tc = in.get("tool_choice");
    if (Json.isObj(tc)) {
      Object type = Json.get(tc, "type");
      if ("auto".equals(type)) out.put("tool_choice", "auto");
      else if ("any".equals(type)) out.put("tool_choice", "required");
      else if ("none".equals(type)) out.put("tool_choice", "none");
      else if ("tool".equals(type)) out.put("tool_choice", Json.obj("type", "function", "function", Json.obj("name", Json.get(tc, "name"))));
    }
    return out;
  }

  static Map<String, Object> modelsList(List<Object> data) {
    List<Object> out = new ArrayList<>();
    for (Object m : data) out.add(Json.obj("type", "model", "id", Json.get(m, "id"), "display_name", Json.get(m, "display_name") instanceof String dn && !dn.isEmpty() ? dn : Json.get(m, "id"), "created_at", "1970-01-01T00:00:00Z"));
    return Json.obj("data", out, "has_more", false, "first_id", data.isEmpty() ? null : Json.get(data.get(0), "id"),
        "last_id", data.isEmpty() ? null : Json.get(data.get(data.size() - 1), "id"));
  }

  static Object toolInput(String str) {
    try { Object v = str.isEmpty() ? Json.obj() : Json.parse(str); return v instanceof Map || v instanceof List ? v : Json.obj(); } catch (RuntimeException e) { return Json.obj(); }
  }

  static final Map<String, Integer> STATUS_OF = new HashMap<>(Gateway.STATUS_OF);
  static { STATUS_OF.put("OUTCOME_UNKNOWN", 502); }

  static int statusOf(Object code) { Integer v = code instanceof String c ? STATUS_OF.get(c) : null; return v == null ? 502 : v; }

  static Map<String, Object> usageOf(Object u) {
    Object in = Json.get(u, "prompt_tokens"), out = Json.get(u, "completion_tokens");
    return Json.obj("input_tokens", in != null ? in : 0L, "output_tokens", out != null ? out : 0L);
  }

  static final class Block { boolean tool; String id, name; StringBuilder buf = new StringBuilder(); }

  /** Render a task as an Anthropic response (see Go serveAnthropic). */
  static void serve(Gateway g, Http.Res res, Gateway.Task t, long after, boolean stream, Gateway.Abort signal) {
    String msgID = "msg_" + t.id.replaceFirst("^task_", "");
    if (!stream) {
      StringBuilder text = new StringBuilder();
      List<Map<String, Object>> calls = new ArrayList<>();
      Object[] done = {null}, fail = {null};
      boolean ok = t.stream(0, signal, null, e -> {
        if (e.type().equals("task.delta")) {
          Object tx = Json.get(e.data(), "text");
          if (Json.truthy(tx)) text.append(s(tx));
          Gateway.mergeCalls(calls, Json.asList(Json.get(e.data(), "tool_calls")));
        } else if (e.type().equals("task.done")) done[0] = e.data() != null ? e.data() : Json.obj();
        else fail[0] = e.data() != null ? e.data() : Json.obj();
        return true;
      });
      if (!ok) return;
      if (fail[0] != null) {
        int st = statusOf(Json.get(fail[0], "code"));
        Gateway.json(res, st, errorBody(st, s(Json.get(fail[0], "code")), Json.str(fail[0], "message")));
        return;
      }
      List<Object> content = new ArrayList<>();
      if (text.length() > 0) content.add(Json.obj("type", "text", "text", text.toString()));
      for (Map<String, Object> c : calls) {
        Map<String, Object> fn = Json.asObj(c.get("function"));
        content.add(Json.obj("type", "tool_use", "id", c.get("id") != null ? c.get("id") : "", "name", fn.get("name"), "input", toolInput((String) fn.get("arguments"))));
      }
      Gateway.json(res, 200, Json.obj("id", msgID, "type", "message", "role", "assistant", "model", t.reqModel, "content", content,
          "stop_reason", stopReason(Json.get(done[0], "finish_reason")), "stop_sequence", null, "usage", usageOf(Json.get(done[0], "usage"))));
      return;
    }
    res.writeHead(200, Gateway.sseHeaders());
    new Streamer(res, t, after, msgID).run(signal);
  }

  /** Named-event SSE renderer with the strictly sequential content-block state machine (audit finding 5). */
  static final class Streamer {
    final Http.Res res;
    final Gateway.Task t;
    final long after;
    final String msgID;
    long cur;
    boolean wrote;
    final List<Object[]> group = new ArrayList<>();
    int idx = -1, liveTI = -1;
    String liveKind = "";
    List<Block> pending = new ArrayList<>();
    final Map<Integer, Block> pendingTool = new HashMap<>();

    Streamer(Http.Res res, Gateway.Task t, long after, String msgID) { this.res = res; this.t = t; this.after = after; this.msgID = msgID; }

    void send(String type, Object data) { group.add(new Object[] {type, data}); }

    boolean flush() {
      List<Object[]> g = new ArrayList<>(group);
      group.clear();
      if (after > 0 && cur <= after) return true;
      wrote = true;
      StringBuilder b = new StringBuilder();
      for (int i = 0; i < g.size(); i++) {
        b.append("event: ").append(g.get(i)[0]).append('\n');
        if (i == g.size() - 1 && cur > 0) b.append("id: ").append(cur).append('\n');
        b.append("data: ").append(Json.stringify(g.get(i)[1])).append("\n\n");
      }
      return b.length() == 0 || res.write(b.toString());
    }

    String toolID(int ti, Object id) { return Json.truthy(id) ? s(id) : "toolu_" + t.id.replaceFirst("^task_", "") + "_" + ti; }
    void start(Object cb) { idx++; send("content_block_start", Json.obj("type", "content_block_start", "index", (long) idx, "content_block", cb)); }
    void textDelta(String str) { send("content_block_delta", Json.obj("type", "content_block_delta", "index", (long) idx, "delta", Json.obj("type", "text_delta", "text", str))); }
    void argsDelta(String str) { send("content_block_delta", Json.obj("type", "content_block_delta", "index", (long) idx, "delta", Json.obj("type", "input_json_delta", "partial_json", str))); }
    void stop() { send("content_block_stop", Json.obj("type", "content_block_stop", "index", (long) idx)); }
    void closeLive() { if (!liveKind.isEmpty()) { stop(); liveKind = ""; liveTI = -1; } }

    void finishBlocks() {
      closeLive();
      for (Block b : pending) {
        if (b.tool) { start(Json.obj("type", "tool_use", "id", b.id, "name", b.name, "input", Json.obj())); if (b.buf.length() > 0) argsDelta(b.buf.toString()); }
        else { start(Json.obj("type", "text", "text", "")); textDelta(b.buf.toString()); }
        stop();
      }
      pending = new ArrayList<>();
    }

    void onText(String str) {
      if (liveKind.equals("tool")) {
        Block last = pending.isEmpty() ? null : pending.get(pending.size() - 1);
        if (last != null && !last.tool) last.buf.append(str);
        else { Block b = new Block(); b.id = ""; b.name = ""; b.buf.append(str); pending.add(b); }
        return;
      }
      if (liveKind.isEmpty()) { start(Json.obj("type", "text", "text", "")); liveKind = "text"; }
      textDelta(str);
    }

    void onTool(int ti, Object c) {
      Object fn = Json.get(c, "function");
      String args = Json.truthy(Json.get(fn, "arguments")) ? s(Json.get(fn, "arguments")) : "";
      String name = Json.truthy(Json.get(fn, "name")) ? s(Json.get(fn, "name")) : "";
      if (liveKind.equals("tool") && liveTI == ti) { if (!args.isEmpty()) argsDelta(args); return; }
      Block p = pendingTool.get(ti);
      if (p != null) { if (p.name.isEmpty()) p.name = name; p.buf.append(args); return; }
      if (liveKind.equals("tool")) {
        Block b = new Block(); b.tool = true; b.id = toolID(ti, Json.get(c, "id")); b.name = name; b.buf.append(args);
        pending.add(b); pendingTool.put(ti, b); return;
      }
      closeLive();
      start(Json.obj("type", "tool_use", "id", toolID(ti, Json.get(c, "id")), "name", name, "input", Json.obj()));
      liveKind = "tool"; liveTI = ti;
      if (!args.isEmpty()) argsDelta(args);
    }

    void run(Gateway.Abort signal) {
      send("message_start", Json.obj("type", "message_start", "message", Json.obj("id", msgID, "type", "message", "role", "assistant", "model", t.reqModel,
          "content", new ArrayList<>(), "stop_reason", null, "stop_sequence", null, "usage", Json.obj("input_tokens", 0L, "output_tokens", 0L))));
      flush();
      boolean ok = t.stream(0, signal, () -> res.write("event: ping\ndata: {\"type\":\"ping\"}\n\n"), e -> {
        cur = e.seq();
        if (e.type().equals("task.delta")) {
          Map<String, Object> d = e.data() != null ? e.data() : Json.obj();
          if (Json.truthy(d.get("text"))) onText(s(d.get("text")));
          List<Object> tcs = Json.asList(d.get("tool_calls"));
          if (tcs != null) for (Object c : tcs) { Object ix = Json.get(c, "index"); onTool(Json.isInteger(ix) ? (int) Json.longOf(ix) : 0, c); }
        } else if (e.type().equals("task.done")) {
          finishBlocks();
          Object u = Json.get(e.data(), "usage");
          send("message_delta", Json.obj("type", "message_delta", "delta", Json.obj("stop_reason", stopReason(Json.get(e.data(), "finish_reason")), "stop_sequence", null),
              "usage", Json.truthy(u) ? usageOf(u) : Json.obj("output_tokens", 0L)));
          send("message_stop", Json.obj("type", "message_stop"));
        } else {
          Object code = Json.get(e.data(), "code");
          send("error", Json.obj("type", "error", "error", Json.obj("type", errorType(statusOf(code)), "message", Json.get(e.data(), "message"), "lmbp_code", code)));
        }
        return flush();
      });
      if (ok && !wrote) res.write("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n");
      res.end();
    }
  }
}
