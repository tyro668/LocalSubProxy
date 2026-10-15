package dev.localsubproxy.lmbp;

import java.io.IOException;
import java.util.Map;

/**
 * Where the engine writes one HTTP response. Called from the request thread, never with
 * an engine lock held. {@link #start} is called exactly once before any {@link #write};
 * every write must be flushed to the client (SSE). A throwing write marks the request
 * as disconnected.
 */
public interface HttpSink {
  void start(int status, Map<String, String> headers) throws IOException;

  void write(byte[] bytes) throws IOException;
}
