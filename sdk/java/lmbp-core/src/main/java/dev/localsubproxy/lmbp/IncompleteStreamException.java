package dev.localsubproxy.lmbp;

/**
 * The stream ended before its terminator ({@code [DONE]} / {@code message_stop} / an error).
 * The response is NOT complete: resume with {@link InProcessClient#resume(String, String)} from
 * {@link #lastEventId()}, or repeat the request with the same idempotency key.
 */
public final class IncompleteStreamException extends LmbpApiException {
  private final String taskId, lastEventId;

  public IncompleteStreamException(String taskId, String lastEventId) {
    super(0, "INCOMPLETE_STREAM", "stream ended before its terminator (task " + taskId + ", last event id " + lastEventId + ")", null);
    this.taskId = taskId;
    this.lastEventId = lastEventId;
  }

  public String taskId() { return taskId; }
  public String lastEventId() { return lastEventId; }
}
