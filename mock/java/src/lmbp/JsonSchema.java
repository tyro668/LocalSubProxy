package lmbp;

import java.net.URI;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.regex.Pattern;

/**
 * Minimal JSON Schema (draft 2020-12) validator implementing exactly the
 * keywords the LMBP schemas use; it fails closed on any other keyword.
 * Port of sdk/node/src/engine/jsonschema.js.
 */
final class JsonSchema {
  static final Set<String> ANNOTATIONS = Set.of("$schema", "$id", "$defs", "title", "description", "default", "$comment");
  static final Set<String> KEYWORDS = Set.of("$ref", "type", "required", "properties", "additionalProperties", "items", "minItems", "maxItems",
      "minimum", "maximum", "minLength", "maxLength", "enum", "const", "pattern", "oneOf", "anyOf", "allOf", "if", "then", "else", "uniqueItems");

  record Err(List<Object> path, String message) {}

  private final Map<String, Map<String, Object>> docs = new HashMap<>();
  private final Map<String, Pattern> patterns = new HashMap<>();

  JsonSchema(List<Object> list) {
    for (Object d : list) { check(d); Map<String, Object> m = Json.asObj(d); docs.put((String) m.get("$id"), m); }
  }

  void check(Object s) {
    Map<String, Object> m = Json.asObj(s);
    if (m == null) return;
    for (Map.Entry<String, Object> e : m.entrySet()) {
      String k = e.getKey(); Object v = e.getValue();
      if (ANNOTATIONS.contains(k)) { if (k.equals("$defs")) for (Object x : Json.asObj(v).values()) check(x); continue; }
      if (!KEYWORDS.contains(k)) throw new IllegalStateException("jsonschema: unsupported keyword " + k);
      if (k.equals("properties")) for (Object x : Json.asObj(v).values()) check(x);
      else if (Set.of("items", "additionalProperties", "if", "then", "else").contains(k)) check(v);
      else if (Set.of("oneOf", "anyOf", "allOf").contains(k)) for (Object x : Json.asList(v)) check(x);
    }
  }

  static String typeOf(Object v) {
    if (v == null) return "null";
    if (v instanceof List) return "array";
    if (v instanceof Map) return "object";
    if (v instanceof String) return "string";
    if (v instanceof Boolean) return "boolean";
    return Json.isInteger(v) ? "integer" : "number";
  }

  static boolean isType(Object v, String t) { String a = typeOf(v); return a.equals(t) || (t.equals("number") && a.equals("integer")); }

  static boolean deepEqual(Object a, Object b) {
    if (a == null || b == null) return a == b;
    if (Json.isNumber(a) && Json.isNumber(b)) return ((Number) a).doubleValue() == ((Number) b).doubleValue();
    if (a instanceof List<?> x && b instanceof List<?> y) {
      if (x.size() != y.size()) return false;
      for (int i = 0; i < x.size(); i++) if (!deepEqual(x.get(i), y.get(i))) return false;
      return true;
    }
    if (a instanceof Map<?, ?> x && b instanceof Map<?, ?> y) {
      if (x.size() != y.size()) return false;
      for (Object k : x.keySet()) if (!y.containsKey(k) || !deepEqual(x.get(k), y.get(k))) return false;
      return true;
    }
    return a.equals(b);
  }

  record Resolved(Object s, String docID) {}

  Resolved resolve(String ref, String docID) {
    int h = ref.indexOf('#');
    String file = h < 0 ? ref : ref.substring(0, h);
    String frag = h < 0 ? "" : ref.substring(h + 1);
    String id = file.isEmpty() ? docID : URI.create(docID).resolve(file).toString();
    Object s = docs.get(id);
    if (s == null) throw new IllegalStateException("jsonschema: unknown $ref " + ref);
    for (String p : frag.split("/")) {
      if (p.isEmpty()) continue;
      s = Json.get(s, java.net.URLDecoder.decode(p, java.nio.charset.StandardCharsets.UTF_8));
    }
    if (s == null) throw new IllegalStateException("jsonschema: unresolvable $ref " + ref);
    return new Resolved(s, id);
  }

  Err validate(String id, Object value) {
    Map<String, Object> d = docs.get(id);
    if (d == null) throw new IllegalStateException("jsonschema: unknown schema " + id);
    return v(d, value, new ArrayList<>(), id);
  }

  Pattern re(String p) { return patterns.computeIfAbsent(p, Pattern::compile); }

  static List<Object> plus(List<Object> path, Object k) { List<Object> o = new ArrayList<>(path); o.add(k); return o; }

  Err v(Object so, Object x, List<Object> path, String docID) {
    Map<String, Object> s = Json.asObj(so);
    if (s == null) return null;
    if (s.get("$ref") instanceof String ref) { Resolved r = resolve(ref, docID); Err e = v(r.s(), x, path, r.docID()); if (e != null) return e; }
    if (s.containsKey("type")) {
      Object t = s.get("type");
      List<Object> ts = t instanceof List ? Json.asList(t) : List.of(t);
      boolean any = false;
      for (Object tt : ts) if (isType(x, (String) tt)) any = true;
      if (!any) { List<String> names = new ArrayList<>(); for (Object tt : ts) names.add((String) tt); return new Err(path, "must be " + String.join(" or ", names)); }
    }
    if (s.containsKey("const") && !deepEqual(x, s.get("const"))) return new Err(path, "must be " + Json.stringify(s.get("const")));
    if (s.get("enum") instanceof List<?> en) {
      boolean any = false;
      for (Object e : en) if (deepEqual(e, x)) any = true;
      if (!any) return new Err(path, "must be one of " + Json.stringify(en));
    }
    if (Json.isNumber(x)) {
      double n = ((Number) x).doubleValue();
      if (s.get("minimum") instanceof Number m && n < m.doubleValue()) return new Err(path, "must be >= " + Json.stringify(m));
      if (s.get("maximum") instanceof Number m && n > m.doubleValue()) return new Err(path, "must be <= " + Json.stringify(m));
    }
    if (x instanceof String str) {
      long n = str.codePointCount(0, str.length());
      if (s.get("minLength") instanceof Number m && n < m.longValue()) return new Err(path, "must have at least " + m + " characters");
      if (s.get("maxLength") instanceof Number m && n > m.longValue()) return new Err(path, "must have at most " + m + " characters");
      if (s.get("pattern") instanceof String p && !re(p).matcher(str).find()) return new Err(path, "must match " + p);
    }
    if (x instanceof List<?> l) {
      if (s.get("minItems") instanceof Number m && l.size() < m.longValue()) return new Err(path, "must have at least " + m + " items");
      if (s.get("maxItems") instanceof Number m && l.size() > m.longValue()) return new Err(path, "must have at most " + m + " items");
      if (Boolean.TRUE.equals(s.get("uniqueItems")))
        for (int i = 0; i < l.size(); i++) for (int j = 0; j < i; j++) if (deepEqual(l.get(i), l.get(j))) return new Err(path, "items must be unique");
      if (s.containsKey("items") && s.get("items") != null)
        for (int i = 0; i < l.size(); i++) { Err e = v(s.get("items"), l.get(i), plus(path, (long) i), docID); if (e != null) return e; }
    }
    if (x instanceof Map<?, ?> mo) {
      if (s.get("required") instanceof List<?> req) for (Object k : req) if (!mo.containsKey(k)) return new Err(path, "missing required property " + k);
      Map<String, Object> props = Json.asObj(s.get("properties"));
      Object ap = s.get("additionalProperties");
      for (Map.Entry<?, ?> en : mo.entrySet()) {
        String k = (String) en.getKey();
        if (props != null && props.containsKey(k)) { Err e = v(props.get(k), en.getValue(), plus(path, k), docID); if (e != null) return e; }
        else if (Boolean.FALSE.equals(ap)) return new Err(plus(path, k), "is not allowed");
        else if (ap instanceof Map) { Err e = v(ap, en.getValue(), plus(path, k), docID); if (e != null) return e; }
      }
    }
    if (s.get("allOf") instanceof List<?> all) for (Object sub : all) { Err e = v(sub, x, path, docID); if (e != null) return e; }
    if (s.get("anyOf") instanceof List<?> any) {
      boolean ok = false;
      for (Object sub : any) if (v(sub, x, path, docID) == null) { ok = true; break; }
      if (!ok) return new Err(path, "does not match any allowed form");
    }
    if (s.get("oneOf") instanceof List<?> one) {
      int n = 0;
      for (Object sub : one) if (v(sub, x, path, docID) == null) n++;
      if (n != 1) return new Err(path, "must match exactly one allowed form");
    }
    if (s.containsKey("if")) {
      Object branch = v(s.get("if"), x, path, docID) != null ? s.get("else") : s.get("then");
      if (branch != null) { Err e = v(branch, x, path, docID); if (e != null) return e; }
    }
    return null;
  }
}
