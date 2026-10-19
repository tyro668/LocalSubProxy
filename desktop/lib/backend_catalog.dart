/// Scheme types as described by the kernel's `backends` action. The desktop
/// draws its forms from these; capabilities and validation stay in Go.
class BackendProfile {
  const BackendProfile({
    required this.provider,
    required this.type,
    required this.label,
    required this.short,
    this.executable,
    this.loginCommand,
    this.needsApiKey = false,
    this.cloud = false,
    this.defaultBaseURL = '',
    this.efforts = const [],
    this.known = true,
  });

  factory BackendProfile.fromJson(Map<String, dynamic> j) {
    String? text(String key) {
      final v = j[key] as String?;
      return v == null || v.isEmpty ? null : v;
    }

    return BackendProfile(
      provider: j['short'] as String,
      type: j['type'] as String,
      label: j['label'] as String,
      short: j['name'] as String,
      executable: text('executable'),
      loginCommand: text('login_command'),
      needsApiKey: j['needs_api_key'] == true,
      cloud: j['cloud'] == true,
      defaultBaseURL: j['default_base_url'] as String? ?? '',
      efforts: [
        for (final e in (j['efforts'] as List? ?? const []))
          if (e != 'default') e as String,
      ],
    );
  }

  /// A stored scheme whose type this build does not know: shown, not edited.
  factory BackendProfile.unknown(String? type) => BackendProfile(
    provider: type ?? '',
    type: type ?? '',
    label: '未知类型（${type ?? '空'}）',
    short: type ?? '未知',
    known: false,
  );

  final String provider;
  final String type;
  final String label;
  final String short;
  final String? executable;
  final String? loginCommand;
  final bool needsApiKey;
  final bool cloud;
  final String defaultBaseURL;

  /// Reasoning efforts a scheme may store, besides default.
  final List<String> efforts;
  final bool known;
  bool get isCLI => executable != null;
}

List<BackendProfile> backendProfiles = const [];

/// Replaces the catalog with the kernel's `backends` answer.
void loadBackendProfiles(List<dynamic> data) {
  backendProfiles = [
    for (final b in data)
      BackendProfile.fromJson((b as Map).cast<String, dynamic>()),
  ];
}

BackendProfile backendForProvider(String provider) =>
    backendProfiles.firstWhere(
      (p) => p.provider == provider,
      orElse: () => BackendProfile.unknown(provider),
    );
BackendProfile backendForType(String? type) => backendProfiles.firstWhere(
  (p) => p.type == type,
  orElse: () => BackendProfile.unknown(type),
);
