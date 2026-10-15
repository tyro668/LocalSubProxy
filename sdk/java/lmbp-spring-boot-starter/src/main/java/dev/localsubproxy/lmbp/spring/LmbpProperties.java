package dev.localsubproxy.lmbp.spring;

import org.springframework.boot.context.properties.ConfigurationProperties;

/** {@code lmbp.*} configuration. Only {@code lmbp.api-key} is required. */
@ConfigurationProperties("lmbp")
public class LmbpProperties {
  /** Public origin of this app (e.g. https://saas.example.com). Empty = dev mode (request Host decides; never in production). */
  private String issuer = "";
  /** Tenant id this app serves. */
  private String tenant = "default";
  /** Tenant API key (>= 32 chars, server-side secret) used by the official OpenAI/Anthropic clients. */
  private String apiKey = "";
  /** Hex Ed25519 seed (32 bytes). Empty = generated once and kept in the store. */
  private String signingSeed = "";
  /** {@code memory} (default), {@code jdbc} (uses the application's DataSource) or a JDBC URL such as {@code jdbc:sqlite:lmbp.db}. */
  private String store = "memory";
  /** Large-text column type for the JDBC store. */
  private String jdbcTextType = "TEXT";
  /** Per-device concurrent task cap. */
  private int maxConcurrency = 16;
  /** Tenant display name shown by the desktop app during pairing. */
  private String displayName = "";
  /** Optional JSON file of tenant profiles keyed by tenant id (same format as mock/tenant-profiles.example.json). */
  private String profileFile = "";
  /** Directory with the demo chat page (mock/web); empty disables /chat. */
  private String chatUiDir = "";
  /** Idle SSE keep-alive interval (ms). Servlet containers detect a vanished client only on a write, so this bounds how fast an abandoned stream is cancelled. */
  private long keepaliveMs = 1000;
  /** Timeout for SseEmitter streams returned by LmbpClient, in milliseconds. */
  private long sseTimeoutMs = 15 * 60_000L;

  public String getIssuer() { return issuer; }
  public void setIssuer(String v) { issuer = v; }
  public String getTenant() { return tenant; }
  public void setTenant(String v) { tenant = v; }
  public String getApiKey() { return apiKey; }
  public void setApiKey(String v) { apiKey = v; }
  public String getSigningSeed() { return signingSeed; }
  public void setSigningSeed(String v) { signingSeed = v; }
  public String getStore() { return store; }
  public void setStore(String v) { store = v; }
  public String getJdbcTextType() { return jdbcTextType; }
  public void setJdbcTextType(String v) { jdbcTextType = v; }
  public int getMaxConcurrency() { return maxConcurrency; }
  public void setMaxConcurrency(int v) { maxConcurrency = v; }
  public String getDisplayName() { return displayName; }
  public void setDisplayName(String v) { displayName = v; }
  public String getProfileFile() { return profileFile; }
  public void setProfileFile(String v) { profileFile = v; }
  public String getChatUiDir() { return chatUiDir; }
  public void setChatUiDir(String v) { chatUiDir = v; }
  public long getKeepaliveMs() { return keepaliveMs; }
  public void setKeepaliveMs(long v) { keepaliveMs = v; }
  public long getSseTimeoutMs() { return sseTimeoutMs; }
  public void setSseTimeoutMs(long v) { sseTimeoutMs = v; }
}
