/// Presentation metadata only. Runtime capabilities and validation belong to Go.
class BackendProfile {
  const BackendProfile(
    this.provider,
    this.type,
    this.label,
    this.short, {
    this.executable,
    this.loginCommand,
  });
  final String provider;
  final String type;
  final String label;
  final String short;
  final String? executable;
  final String? loginCommand;
  bool get isCLI => executable != null;
}

const backendProfiles = [
  BackendProfile(
    'codex',
    'codex_app_server',
    'OpenAI Codex（订阅）',
    'Codex',
    executable: 'codex',
  ),
  BackendProfile(
    'grok',
    'grok_build',
    'Grok Build（CLI）',
    'Grok Build',
    executable: 'grok',
    loginCommand: 'grok login',
  ),
  BackendProfile(
    'antigravity',
    'antigravity_cli',
    'Antigravity CLI',
    'Antigravity',
    executable: 'agy',
    loginCommand: 'agy',
  ),
  BackendProfile('openai', 'openai_compatible', 'OpenAI 兼容协议', 'OpenAI 兼容'),
  BackendProfile(
    'anthropic',
    'anthropic_compatible',
    'Anthropic 兼容协议',
    'Anthropic 兼容',
  ),
  BackendProfile('ollama', 'ollama_http', 'Ollama 协议（本地模型）', 'Ollama'),
];

BackendProfile backendForProvider(String provider) =>
    backendProfiles.firstWhere(
      (p) => p.provider == provider,
      orElse: () => backendProfiles.firstWhere((p) => p.provider == 'openai'),
    );
BackendProfile backendForType(String? type) => backendProfiles.firstWhere(
  (p) => p.type == type,
  orElse: () => backendProfiles.firstWhere((p) => p.provider == 'openai'),
);
