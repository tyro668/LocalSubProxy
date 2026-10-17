package dev.localsubproxy.lmbp.spring;

import static org.junit.jupiter.api.Assertions.assertTrue;

import dev.localsubproxy.lmbp.FakeDevice;
import dev.localsubproxy.lmbp.LMBPServer;
import java.io.OutputStream;
import java.net.Socket;
import java.nio.charset.StandardCharsets;
import java.util.Map;
import org.junit.jupiter.api.Test;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.boot.test.web.server.LocalServerPort;

/**
 * SDK consistency review (2026-10, main@52f48ae) P1: behind Tomcat, a non-streaming caller that
 * disconnected was never noticed (the servlet API reports a disconnect only on a failed write and
 * nothing is written before the task ends), so the device kept running until the task timeout.
 * Spec §6.5: the gateway MUST send task.cancel(client_disconnected).
 */
@SpringBootTest(webEnvironment = SpringBootTest.WebEnvironment.RANDOM_PORT, properties = {
    "lmbp.tenant=acme", "lmbp.api-key=0123456789abcdef0123456789abcdef", "lmbp.issuer=http://127.0.0.1"})
class DisconnectTest {
  static final String KEY = "0123456789abcdef0123456789abcdef";

  @SpringBootApplication
  static class App { }

  @LocalServerPort int port;
  @Autowired LMBPServer lsp;

  @Test void nonStreamingDisconnectCancelsTheTask() throws Exception {
    try (FakeDevice dev = new FakeDevice("http://127.0.0.1:" + port, "acme")) {
      String code = dev.authorize();
      lsp.pairing().approve("acme", "alice", code);
      dev.token();
      dev.connect("tiny");
      dev.mode = FakeDevice.Mode.HANG;
      String body = "{\"model\":\"" + dev.deviceId + "/tiny\",\"stream\":false,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}";
      byte[] b = body.getBytes(StandardCharsets.UTF_8);
      String tid;
      try (Socket s = new Socket("127.0.0.1", port)) {
        OutputStream out = s.getOutputStream();
        out.write(("POST /v1/chat/completions HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer " + KEY + "\r\nX-End-User: alice\r\n"
            + "Content-Type: application/json\r\nContent-Length: " + b.length + "\r\n\r\n").getBytes(StandardCharsets.ISO_8859_1));
        out.write(b);
        out.flush();
        FakeDevice.waitFor(() -> dev.received.stream().anyMatch(m -> "task.run".equals(m.get("type"))), 5000);
        tid = dev.received.stream().filter(m -> "task.run".equals(m.get("type"))).map(m -> (String) m.get("task_id")).findFirst().orElseThrow();
        Thread.sleep(300);
      } // the caller goes away before any byte of the response
      long t0 = System.currentTimeMillis();
      FakeDevice.waitFor(() -> dev.sawCancel(tid), 5000);
      long took = System.currentTimeMillis() - t0;
      assertTrue(dev.cancels(tid).contains("client_disconnected"), "no task.cancel(client_disconnected) within 5s: " + dev.cancels(tid));
      assertTrue(took < 3000, "task.cancel took " + took + "ms");
    }
  }
}
