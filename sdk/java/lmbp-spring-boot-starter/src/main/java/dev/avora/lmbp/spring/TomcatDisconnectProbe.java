package dev.avora.lmbp.spring;

import jakarta.servlet.ServletException;
import java.io.IOException;
import java.util.function.BooleanSupplier;
import org.apache.catalina.connector.Request;
import org.apache.catalina.connector.Response;
import org.apache.catalina.valves.ValveBase;
import org.apache.coyote.ActionCode;
import org.apache.coyote.http11.Http11InputBuffer;

/**
 * Lets {@link LmbpFilter} notice a client that went away while a request is still waiting
 * (spec §6.5: the gateway MUST send {@code task.cancel(client_disconnected)}).
 *
 * <p>The Servlet API reports a disconnect only when a write fails, and a non-streaming response
 * writes nothing before the task ends, so Tomcat on its own never tells us. This valve exposes a
 * probe as a request attribute: a non-blocking read of the connection past the (already consumed)
 * request body. Only end of stream or a read error means the client closed the connection: bytes
 * waiting on the connection (an HTTP/1.1 keep-alive client that pipelines its next request while
 * inference runs) are not a disconnect.
 * Only used when Tomcat is the servlet container; other containers keep the write-failure path.
 */
final class TomcatDisconnectProbe extends ValveBase {
  TomcatDisconnectProbe() { super(true); }

  @Override public void invoke(Request request, Response response) throws IOException, ServletException {
    org.apache.coyote.Request coyote = request.getCoyoteRequest();
    request.setAttribute(LmbpFilter.PROBE_ATTR, (BooleanSupplier) () -> gone(coyote));
    getNext().invoke(request, response);
  }

  /**
   * True when the peer closed the connection. Tomcat's non-blocking AVAILABLE read reports 1 both
   * for real data and for EOF/error (it swallows the IOException and returns 1 with nothing read),
   * so the count alone cannot tell them apart. Real data lands in the input buffer; EOF/error leaves
   * it empty. Once a pipelined request is buffered, the probe stops reading (the client is alive and
   * waiting) and a later close is noticed by the failing write, as for other containers.
   */
  static boolean gone(org.apache.coyote.Request coyote) {
    try {
      if (!(coyote.getInputBuffer() instanceof Http11InputBuffer in)) return false; // HTTP/2 etc.: no probe
      if (buffered(in)) return false;
      coyote.action(ActionCode.AVAILABLE, Boolean.TRUE);
      return coyote.getAvailable() > 0 && !buffered(in);
    } catch (RuntimeException e) {
      return true;
    }
  }

  private static boolean buffered(Http11InputBuffer in) {
    java.nio.ByteBuffer b = in.getByteBuffer();
    return b != null && b.hasRemaining();
  }
}
