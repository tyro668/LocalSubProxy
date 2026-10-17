package dev.localsubproxy.lmbp.spring;

import jakarta.servlet.ServletException;
import java.io.IOException;
import java.util.function.BooleanSupplier;
import org.apache.catalina.connector.Request;
import org.apache.catalina.connector.Response;
import org.apache.catalina.valves.ValveBase;
import org.apache.coyote.ActionCode;

/**
 * Lets {@link LmbpFilter} notice a client that went away while a request is still waiting
 * (spec §6.5: the gateway MUST send {@code task.cancel(client_disconnected)}).
 *
 * <p>The Servlet API reports a disconnect only when a write fails, and a non-streaming response
 * writes nothing before the task ends, so Tomcat on its own never tells us. This valve exposes a
 * probe as a request attribute: a non-blocking read of the connection past the (already consumed)
 * request body. End of stream or a read error means the client closed the connection.
 * Only used when Tomcat is the servlet container; other containers keep the write-failure path.
 */
final class TomcatDisconnectProbe extends ValveBase {
  TomcatDisconnectProbe() { super(true); }

  @Override public void invoke(Request request, Response response) throws IOException, ServletException {
    org.apache.coyote.Request coyote = request.getCoyoteRequest();
    request.setAttribute(LmbpFilter.PROBE_ATTR, (BooleanSupplier) () -> gone(coyote));
    getNext().invoke(request, response);
  }

  /** True when the peer closed the connection (EOF/error on a non-blocking read past the body). */
  static boolean gone(org.apache.coyote.Request coyote) {
    try {
      coyote.action(ActionCode.AVAILABLE, Boolean.TRUE);
      return coyote.getAvailable() > 0;
    } catch (RuntimeException e) {
      return true;
    }
  }
}
