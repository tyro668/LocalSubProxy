package lmbp;

import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * Minimal JSON (RFC 8259) for the mock: objects are LinkedHashMap (key order
 * kept, like JSON.stringify), arrays ArrayList, numbers Long (integral
 * literals that fit) or Double, plus String, Boolean and null. stringify
 * matches JavaScript's JSON.stringify output for these values.
 */
public final class Json {
  private Json() {}

  public static final class ParseError extends RuntimeException {
    ParseError(String m) { super(m); }
  }

  public static Object parse(String s) {
    P p = new P(s);
    p.ws();
    Object v = p.value(0);
    p.ws();
    if (p.i != s.length()) throw new ParseError("trailing data");
    return v;
  }

  public static Object parse(byte[] b) { return parse(new String(b, StandardCharsets.UTF_8)); }

  /** parse or null on any error. */
  public static Object tryParse(String s) {
    try { return parse(s); } catch (RuntimeException e) { return null; }
  }

  private static final class P {
    final String s; int i;
    P(String s) { this.s = s; }
    void ws() { while (i < s.length()) { char c = s.charAt(i); if (c == ' ' || c == '\t' || c == '\n' || c == '\r') i++; else break; } }
    Object value(int depth) {
      if (depth > 512) throw new ParseError("too deep");
      if (i >= s.length()) throw new ParseError("unexpected end");
      char c = s.charAt(i);
      switch (c) {
        case '{': {
          i++; Map<String, Object> m = new LinkedHashMap<>(); ws();
          if (peek() == '}') { i++; return m; }
          for (;;) {
            ws(); if (peek() != '"') throw new ParseError("expected key");
            String k = str(); ws(); expect(':'); ws();
            m.put(k, value(depth + 1)); ws();
            char d = next();
            if (d == '}') return m;
            if (d != ',') throw new ParseError("expected , or }");
          }
        }
        case '[': {
          i++; List<Object> l = new ArrayList<>(); ws();
          if (peek() == ']') { i++; return l; }
          for (;;) {
            ws(); l.add(value(depth + 1)); ws();
            char d = next();
            if (d == ']') return l;
            if (d != ',') throw new ParseError("expected , or ]");
          }
        }
        case '"': return str();
        case 't': lit("true"); return Boolean.TRUE;
        case 'f': lit("false"); return Boolean.FALSE;
        case 'n': lit("null"); return null;
        default: return num();
      }
    }
    char peek() { if (i >= s.length()) throw new ParseError("unexpected end"); return s.charAt(i); }
    char next() { char c = peek(); i++; return c; }
    void expect(char c) { if (next() != c) throw new ParseError("expected " + c); }
    void lit(String w) { if (!s.startsWith(w, i)) throw new ParseError("bad literal"); i += w.length(); }
    String str() {
      expect('"');
      StringBuilder b = new StringBuilder();
      for (;;) {
        char c = next();
        if (c == '"') return b.toString();
        if (c < 0x20) throw new ParseError("control character in string");
        if (c != '\\') { b.append(c); continue; }
        char e = next();
        switch (e) {
          case '"': b.append('"'); break;
          case '\\': b.append('\\'); break;
          case '/': b.append('/'); break;
          case 'b': b.append('\b'); break;
          case 'f': b.append('\f'); break;
          case 'n': b.append('\n'); break;
          case 'r': b.append('\r'); break;
          case 't': b.append('\t'); break;
          case 'u': {
            if (i + 4 > s.length()) throw new ParseError("bad escape");
            b.append((char) Integer.parseInt(s.substring(i, i + 4), 16)); i += 4; break;
          }
          default: throw new ParseError("bad escape");
        }
      }
    }
    Object num() {
      int st = i;
      if (i < s.length() && s.charAt(i) == '-') i++;
      if (i >= s.length()) throw new ParseError("bad number");
      if (s.charAt(i) == '0') i++;
      else if (Character.isDigit(s.charAt(i))) while (i < s.length() && Character.isDigit(s.charAt(i))) i++;
      else throw new ParseError("bad number");
      boolean frac = false;
      if (i < s.length() && s.charAt(i) == '.') {
        frac = true; i++;
        if (i >= s.length() || !Character.isDigit(s.charAt(i))) throw new ParseError("bad number");
        while (i < s.length() && Character.isDigit(s.charAt(i))) i++;
      }
      if (i < s.length() && (s.charAt(i) == 'e' || s.charAt(i) == 'E')) {
        frac = true; i++;
        if (i < s.length() && (s.charAt(i) == '+' || s.charAt(i) == '-')) i++;
        if (i >= s.length() || !Character.isDigit(s.charAt(i))) throw new ParseError("bad number");
        while (i < s.length() && Character.isDigit(s.charAt(i))) i++;
      }
      String t = s.substring(st, i);
      if (!frac && t.length() < 19) return Long.parseLong(t);
      double d = Double.parseDouble(t);
      if (Double.isInfinite(d)) throw new ParseError("number out of range");
      return d;
    }
  }

  public static String stringify(Object v) {
    StringBuilder b = new StringBuilder();
    write(b, v);
    return b.toString();
  }

  public static byte[] bytes(Object v) { return stringify(v).getBytes(StandardCharsets.UTF_8); }

  public static int utf8Len(String s) { return s.getBytes(StandardCharsets.UTF_8).length; }

  @SuppressWarnings("unchecked")
  static void write(StringBuilder b, Object v) {
    if (v == null) b.append("null");
    else if (v instanceof String s) quote(b, s);
    else if (v instanceof Boolean x) b.append(x ? "true" : "false");
    else if (v instanceof Long || v instanceof Integer || v instanceof Short) b.append(((Number) v).longValue());
    else if (v instanceof Number n) num(b, n.doubleValue());
    else if (v instanceof Map<?, ?> m) {
      b.append('{'); boolean first = true;
      for (Map.Entry<?, ?> e : m.entrySet()) {
        if (!first) b.append(','); first = false;
        quote(b, String.valueOf(e.getKey())); b.append(':'); write(b, e.getValue());
      }
      b.append('}');
    } else if (v instanceof List<?> l) {
      b.append('['); boolean first = true;
      for (Object x : l) { if (!first) b.append(','); first = false; write(b, x); }
      b.append(']');
    } else quote(b, v.toString());
  }

  static void num(StringBuilder b, double d) {
    if (Double.isNaN(d) || Double.isInfinite(d)) { b.append("null"); return; }
    if (d == Math.rint(d) && Math.abs(d) < 1e21) { b.append(new java.math.BigDecimal(d).toPlainString()); return; }
    // JS shortest round-trip form; Java's Double.toString is close enough for the mock.
    String s = Double.toString(d);
    if (s.contains("E")) {
      String[] p = s.split("E");
      String mant = p[0].endsWith(".0") ? p[0].substring(0, p[0].length() - 2) : p[0];
      int exp = Integer.parseInt(p[1]);
      s = mant + "e" + (exp > 0 ? "+" : "") + exp;
    }
    b.append(s);
  }

  static void quote(StringBuilder b, String s) {
    b.append('"');
    for (int i = 0; i < s.length(); i++) {
      char c = s.charAt(i);
      switch (c) {
        case '"': b.append("\\\""); break;
        case '\\': b.append("\\\\"); break;
        case '\b': b.append("\\b"); break;
        case '\f': b.append("\\f"); break;
        case '\n': b.append("\\n"); break;
        case '\r': b.append("\\r"); break;
        case '\t': b.append("\\t"); break;
        default:
          if (c < 0x20) b.append(String.format("\\u%04x", (int) c));
          else if (Character.isSurrogate(c)) {
            boolean paired = Character.isHighSurrogate(c) && i + 1 < s.length() && Character.isLowSurrogate(s.charAt(i + 1));
            if (paired) { b.append(c).append(s.charAt(++i)); } else b.append(String.format("\\u%04x", (int) c));
          } else b.append(c);
      }
    }
    b.append('"');
  }

  // ---------- JS-like accessors ----------
  public static Map<String, Object> obj() { return new LinkedHashMap<>(); }

  public static Map<String, Object> obj(Object... kv) {
    Map<String, Object> m = new LinkedHashMap<>();
    for (int i = 0; i < kv.length; i += 2) m.put((String) kv[i], kv[i + 1]);
    return m;
  }

  @SuppressWarnings("unchecked")
  public static Map<String, Object> asObj(Object o) { return o instanceof Map ? (Map<String, Object>) o : null; }

  @SuppressWarnings("unchecked")
  public static List<Object> asList(Object o) { return o instanceof List ? (List<Object>) o : null; }

  public static boolean isObj(Object o) { return o instanceof Map; }

  public static Object get(Object o, String k) { Map<String, Object> m = asObj(o); return m == null ? null : m.get(k); }

  public static String str(Object o, String k) { Object v = get(o, k); return v instanceof String s ? s : null; }

  /** JavaScript truthiness. */
  public static boolean truthy(Object v) {
    if (v == null) return false;
    if (v instanceof Boolean b) return b;
    if (v instanceof String s) return !s.isEmpty();
    if (v instanceof Long l) return l != 0;
    if (v instanceof Number n) { double d = n.doubleValue(); return d != 0 && !Double.isNaN(d); }
    return true;
  }

  public static boolean isNumber(Object v) { return v instanceof Long || v instanceof Double; }

  /** Number.isInteger. */
  public static boolean isInteger(Object v) {
    if (v instanceof Long) return true;
    return v instanceof Double d && !d.isInfinite() && d == Math.rint(d);
  }

  /** Number.isSafeInteger. */
  public static boolean isSafeInteger(Object v) {
    return isInteger(v) && Math.abs(((Number) v).doubleValue()) <= 9007199254740991.0;
  }

  public static long longOf(Object v) { return v instanceof Number n ? (v instanceof Long l ? l : (long) n.doubleValue()) : 0; }

  /** Deep copy (structuredClone/JSON round trip). */
  public static Object copy(Object v) {
    if (v instanceof Map<?, ?> m) { Map<String, Object> o = new LinkedHashMap<>(); for (Map.Entry<?, ?> e : m.entrySet()) o.put((String) e.getKey(), copy(e.getValue())); return o; }
    if (v instanceof List<?> l) { List<Object> o = new ArrayList<>(); for (Object x : l) o.add(copy(x)); return o; }
    return v;
  }
}
