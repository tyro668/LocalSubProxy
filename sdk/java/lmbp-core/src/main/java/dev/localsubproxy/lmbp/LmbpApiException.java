package dev.localsubproxy.lmbp;

import java.util.Map;

/** A request the gateway refused or a task that ended in an error. {@code code} is the LMBP error code (e.g. DEVICE_OFFLINE, MODEL_NOT_FOUND, CANCELLED). */
public class LmbpApiException extends RuntimeException {
  private final int status;
  private final String code;
  private final transient Object body;

  public LmbpApiException(int status, String code, String message, Object body) {
    super(message);
    this.status = status;
    this.code = code;
    this.body = body;
  }

  public int status() { return status; }
  public String code() { return code; }
  /** Parsed error body (OpenAI/Anthropic error shape), may be null. */
  public Object body() { return body; }

  static LmbpApiException from(int status, Object v) {
    Map<String, Object> e = Json.asObj(Json.get(v, "error"));
    String code = null, msg = null;
    if (e != null) {
      code = Json.str(e, "code");
      if (code == null) code = Json.str(e, "lmbp_code");
      msg = Json.str(e, "message");
    }
    if (code == null) code = Json.str(v, "code");
    if (msg == null) msg = Json.str(v, "detail");
    return new LmbpApiException(status, code == null ? "ERROR" : code, msg == null ? "request failed" : msg, v);
  }
}
