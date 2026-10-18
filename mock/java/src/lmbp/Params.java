package lmbp;

import java.io.InputStream;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.HashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.regex.Pattern;

/**
 * Request parameter validation, the same rules as the Go reference
 * (internal/lmbp/gateway/params.go) and the Node/Python SDKs, pinned by
 * spec/testvectors/params.json and conformance Z8.
 */
final class Params {
  static final int MAX_STOP_SEQUENCES = 4, MAX_STOP_LENGTH = 256, MAX_TOOLS = 128;
  static final Pattern TOOL_NAME = Pattern.compile("^[A-Za-z0-9_-]{1,64}$");
  static final String BASE = "https://localsubproxy.dev/spec/lmbp/";
  static final Set<String> ROLES = Set.of("system", "user", "assistant", "tool", "developer");

  static final class ParamError extends RuntimeException {
    final String param;
    ParamError(String param, String message) { super(message); this.param = param; }
  }

  static ParamError perr(String param, String msg) { return new ParamError(param, param + ": " + msg); }

  static Map<String, Object> paramErrorBody(ParamError e, boolean anthropic) {
    return anthropic
        ? Json.obj("type", "error", "error", Json.obj("type", "invalid_request_error", "message", e.getMessage(), "lmbp_code", "BAD_REQUEST", "lmbp_param", e.param))
        : Json.obj("error", Json.obj("message", e.getMessage(), "type", "invalid_request_error", "code", "BAD_REQUEST", "param", e.param));
  }

  static boolean present(Map<String, Object> m, String k) { return m.containsKey(k) && m.get(k) != null; }

  static String fmt(Object n) { return Json.stringify(n); }

  static Object numberIn(Map<String, Object> m, String k, double lo, double hi) {
    if (!present(m, k)) return null;
    Object f = m.get(k);
    if (!Json.isNumber(f)) throw perr(k, "must be a number");
    double d = ((Number) f).doubleValue();
    if (d < lo || d > hi) throw perr(k, "must be between " + fmt(lo) + " and " + fmt(hi) + ", got " + fmt(f));
    return f;
  }

  static long positiveInt(Map<String, Object> m, String k) {
    if (!present(m, k)) return 0;
    Object f = m.get(k);
    if (!Json.isInteger(f) || ((Number) f).doubleValue() < 1 || ((Number) f).doubleValue() > 2147483647) throw perr(k, "must be a positive integer, got " + Json.stringify(f));
    return Json.longOf(f);
  }

  static void bool(Map<String, Object> m, String k) { if (present(m, k) && !(m.get(k) instanceof Boolean)) throw perr(k, "must be a boolean"); }

  static List<Object> stopList(Map<String, Object> m, String k, boolean allowString) {
    if (!present(m, k)) return null;
    Object v = m.get(k);
    List<Object> list;
    if (v instanceof String) {
      if (!allowString) throw perr(k, "must be an array of strings");
      list = new ArrayList<>(List.of(v));
    } else if (v instanceof List) list = Json.asList(v);
    else throw perr(k, allowString ? "must be a string or an array of strings" : "must be an array of strings");
    if (list.size() < 1 || list.size() > MAX_STOP_SEQUENCES) throw perr(k, "must have 1 to " + MAX_STOP_SEQUENCES + " sequences, got " + list.size());
    for (int i = 0; i < list.size(); i++) {
      Object s = list.get(i);
      if (!(s instanceof String str) || str.isEmpty() || Lmbp.charLen(str) > MAX_STOP_LENGTH)
        throw perr(k, "sequence " + i + " must be a non-empty string of at most " + MAX_STOP_LENGTH + " characters");
    }
    return new ArrayList<>(list);
  }

  /** Normalized sampling parameters (null = absent; maxTokens 0 = absent). */
  static final class Sampling { Object temperature, topP; long maxTokens; List<Object> stop; }

  static Sampling validateChatParams(Map<String, Object> m) {
    if (!(m.get("model") instanceof String s) || s.isEmpty()) throw perr("model", "field required (non-empty string)");
    bool(m, "stream");
    Sampling p = new Sampling();
    p.temperature = numberIn(m, "temperature", 0, 2);
    p.topP = numberIn(m, "top_p", 0, 1);
    long mt = positiveInt(m, "max_tokens");
    long mct = positiveInt(m, "max_completion_tokens");
    p.maxTokens = mt != 0 ? mt : mct;
    if (present(m, "n")) {
      if (!Json.isInteger(m.get("n"))) throw perr("n", "must be an integer");
      if (((Number) m.get("n")).doubleValue() != 1) throw perr("n", "only n=1 is supported, got " + fmt(m.get("n")));
    }
    p.stop = stopList(m, "stop", true);
    if (!m.containsKey("lmbp_e2ee")) validateOpenAIMessages(m);
    Set<String> names = validateOpenAITools(m);
    validateOpenAIToolChoice(m, names);
    return p;
  }

  static void validateOpenAIMessages(Map<String, Object> m) {
    if (!present(m, "messages")) throw perr("messages", "field required");
    List<Object> list = Json.asList(m.get("messages"));
    if (list == null) throw perr("messages", "must be an array");
    if (list.isEmpty() || list.size() > Lmbp.MAX_MESSAGES) throw perr("messages", "1-" + Lmbp.MAX_MESSAGES + " messages required, got " + list.size());
    for (int i = 0; i < list.size(); i++) {
      Map<String, Object> msg = Json.asObj(list.get(i));
      if (msg == null) throw perr("messages", "message " + i + " must be an object");
      if (!(msg.get("role") instanceof String r) || !ROLES.contains(r))
        throw perr("messages", "message " + i + ": unsupported role " + (msg.containsKey("role") ? Json.stringify(msg.get("role")) : "undefined"));
      Object c = msg.get("content");
      if (!(c == null || c instanceof String || c instanceof List)) throw perr("messages", "message " + i + ": content must be a string, an array of content parts or null");
      if ("tool".equals(msg.get("role"))) {
        if (!(msg.get("tool_call_id") instanceof String tc) || tc.isEmpty()) throw perr("messages", "message " + i + ": tool messages need tool_call_id");
        if (c == null) throw perr("messages", "message " + i + ": tool messages need content");
      }
      if (msg.get("tool_calls") != null) {
        List<Object> tcs = Json.asList(msg.get("tool_calls"));
        if (tcs == null || !"assistant".equals(msg.get("role"))) throw perr("messages", "message " + i + ": tool_calls must be an array on an assistant message");
        for (int j = 0; j < tcs.size(); j++) if (!Json.isObj(Json.get(tcs.get(j), "function"))) throw perr("messages", "message " + i + ": tool_calls[" + j + "] needs a function");
      }
    }
  }

  static Set<String> validateOpenAITools(Map<String, Object> m) {
    Set<String> names = new HashSet<>();
    if (!present(m, "tools")) return names;
    List<Object> tools = Json.asList(m.get("tools"));
    if (tools == null) throw perr("tools", "must be an array");
    if (tools.size() > MAX_TOOLS) throw perr("tools", "at most " + MAX_TOOLS + " tools, got " + tools.size());
    for (int i = 0; i < tools.size(); i++) {
      Map<String, Object> t = Json.asObj(tools.get(i));
      if (t == null || !"function".equals(t.get("type"))) throw perr("tools", "tool " + i + " must have type function");
      Map<String, Object> fn = Json.asObj(t.get("function"));
      if (fn == null) throw perr("tools", "tool " + i + " needs a function object");
      if (!(fn.get("name") instanceof String name) || !TOOL_NAME.matcher(name).matches())
        throw perr("tools", "tool " + i + ": function name must match ^[A-Za-z0-9_-]{1,64}$, got " + (fn.containsKey("name") ? Json.stringify(fn.get("name")) : "undefined"));
      if (present(fn, "description") && !(fn.get("description") instanceof String)) throw perr("tools", "tool " + i + ": description must be a string");
      if (present(fn, "parameters") && !Json.isObj(fn.get("parameters"))) throw perr("tools", "tool " + i + ": parameters must be a JSON Schema object");
      names.add((String) fn.get("name"));
    }
    return names;
  }

  static void validateOpenAIToolChoice(Map<String, Object> m, Set<String> names) {
    if (!present(m, "tool_choice")) return;
    Object v = m.get("tool_choice");
    if (v instanceof String s) {
      if (s.equals("none")) return;
      if (!s.equals("auto") && !s.equals("required")) throw perr("tool_choice", "must be none, auto, required or a function object, got " + Json.stringify(s));
    } else if (Json.isObj(v)) {
      Object name = Json.get(Json.get(v, "function"), "name");
      if (!"function".equals(Json.get(v, "type")) || !(name instanceof String ns) || ns.isEmpty())
        throw perr("tool_choice", "object form must be {\"type\":\"function\",\"function\":{\"name\":...}}");
      if (!names.contains(name)) throw perr("tool_choice", "names function " + Json.stringify(name) + " that is not in tools");
    } else throw perr("tool_choice", "must be a string or an object");
    if (names.isEmpty()) throw perr("tool_choice", "requires tools");
  }

  static void validateAnthropicParams(Map<String, Object> m) {
    if (!(m.get("model") instanceof String s) || s.isEmpty()) throw perr("model", "field required (non-empty string)");
    if (!present(m, "max_tokens")) throw perr("max_tokens", "field required (positive integer)");
    positiveInt(m, "max_tokens");
    bool(m, "stream");
    numberIn(m, "temperature", 0, 1);
    numberIn(m, "top_p", 0, 1);
    stopList(m, "stop_sequences", false);
    if (!present(m, "messages")) throw perr("messages", "field required");
    List<Object> msgs = Json.asList(m.get("messages"));
    if (msgs == null) throw perr("messages", "must be an array");
    if (msgs.isEmpty() || msgs.size() > Lmbp.MAX_MESSAGES) throw perr("messages", "1-" + Lmbp.MAX_MESSAGES + " messages required, got " + msgs.size());
    for (int i = 0; i < msgs.size(); i++) {
      Map<String, Object> msg = Json.asObj(msgs.get(i));
      if (msg == null) throw perr("messages", "message " + i + " must be an object");
      if (!"user".equals(msg.get("role")) && !"assistant".equals(msg.get("role"))) throw perr("messages", "message " + i + ": role must be user or assistant");
      if (!(msg.get("content") instanceof String) && !(msg.get("content") instanceof List)) throw perr("messages", "message " + i + ": content must be a string or an array of blocks");
    }
    Set<String> names = new HashSet<>();
    if (present(m, "tools")) {
      List<Object> tools = Json.asList(m.get("tools"));
      if (tools == null) throw perr("tools", "must be an array");
      if (tools.size() > MAX_TOOLS) throw perr("tools", "at most " + MAX_TOOLS + " tools, got " + tools.size());
      for (int i = 0; i < tools.size(); i++) {
        Map<String, Object> t = Json.asObj(tools.get(i));
        if (t == null) throw perr("tools", "tool " + i + " must be an object");
        if (present(t, "type") && !"custom".equals(t.get("type"))) throw perr("tools", "tool " + i + ": server tool " + jsStr(t.get("type")) + " is not supported");
        if (!(t.get("name") instanceof String name) || !TOOL_NAME.matcher(name).matches())
          throw perr("tools", "tool " + i + ": name must match ^[A-Za-z0-9_-]{1,64}$, got " + (t.containsKey("name") ? Json.stringify(t.get("name")) : "undefined"));
        if (!Json.isObj(t.get("input_schema"))) throw perr("tools", "tool " + i + ": input_schema (object) is required");
        names.add((String) t.get("name"));
      }
    }
    if (present(m, "tool_choice")) {
      Object tc = m.get("tool_choice");
      if (!Json.isObj(tc)) throw perr("tool_choice", "must be an object");
      Object type = Json.get(tc, "type");
      if ("none".equals(type)) return;
      if ("tool".equals(type)) {
        Object name = Json.get(tc, "name");
        if (!(name instanceof String ns) || ns.isEmpty()) throw perr("tool_choice", "type tool needs a name");
        if (!names.contains(name)) throw perr("tool_choice", "names tool " + Json.stringify(name) + " that is not in tools");
      } else if (!"auto".equals(type) && !"any".equals(type))
        throw perr("tool_choice", "type must be auto, any, tool or none, got " + (type == null && !Json.asObj(tc).containsKey("type") ? "undefined" : Json.stringify(type)));
      if (names.isEmpty()) throw perr("tool_choice", "requires tools");
    }
  }

  static String jsStr(Object v) { return v instanceof String s ? s : Json.stringify(v); }

  private static JsonSchema schemas;

  static synchronized JsonSchema schemaSet() {
    if (schemas == null) {
      List<Object> docs = new ArrayList<>();
      for (String n : new String[] {"defs.json", "task.run.payload.json", "hello.json", "models.update.json", "task.done.json", "task.error.json"}) {
        try (InputStream in = Params.class.getResourceAsStream("/schemas/" + n)) {
          if (in == null) throw new IllegalStateException("bundled schema " + n + " missing");
          docs.add(Json.parse(new String(in.readAllBytes(), StandardCharsets.UTF_8)));
        } catch (java.io.IOException e) { throw new IllegalStateException(e); }
      }
      schemas = new JsonSchema(docs);
    }
    return schemas;
  }

  /** Validates hello / models.update data against its schema (spec §7.4): null when valid, else a message. */
  /**
   * Validates a device data frame's envelope and, for task.done / task.error, its body against
   * task.done.json / task.error.json: null when acceptable (checked before the seq is accepted,
   * persisted or ACKed).
   */
  static String deviceDataProblem(Map<String, Object> inp) {
    Object type = inp.get("type"), tid = inp.get("task_id"), seq = inp.get("seq"), data = inp.get("data");
    if (!(tid instanceof String s) || s.isEmpty()) return type + " without task_id";
    if (!Json.isInteger(seq) || ((Number) seq).doubleValue() < 1) return type + " seq must be an integer >= 1";
    if (!(data instanceof Map)) return type + " data must be a JSON object";
    if (!"task.done".equals(type) && !"task.error".equals(type)) return null;
    JsonSchema.Err e = schemaSet().validate(BASE + type + ".json", Json.parse(Json.stringify(data)));
    if (e == null) return null;
    List<String> ps = new ArrayList<>();
    for (Object p : e.path()) ps.add(String.valueOf(p));
    return type + (ps.isEmpty() ? "" : "." + String.join(".", ps)) + ": " + e.message();
  }

  static String announceProblem(Object data, boolean hello) {
    String name = hello ? "hello" : "models.update";
    JsonSchema.Err e = schemaSet().validate(BASE + name + ".json", Json.parse(Json.stringify(data)));
    if (e == null) return null;
    List<String> ps = new ArrayList<>();
    for (Object p : e.path()) ps.add(String.valueOf(p));
    return name + (ps.isEmpty() ? "" : "." + String.join(".", ps)) + ": " + e.message();
  }

  static void validateTaskRun(Map<String, Object> payload) {
    JsonSchema.Err e = schemaSet().validate(BASE + "task.run.payload.json", Json.parse(Json.stringify(payload)));
    if (e != null) {
      List<String> ps = new ArrayList<>();
      for (Object p : e.path()) ps.add(String.valueOf(p));
      throw new ParamError(e.path().isEmpty() ? "" : String.valueOf(e.path().get(0)),
          "request does not satisfy the task.run schema at /" + String.join("/", ps) + ": " + e.message());
    }
  }
}
