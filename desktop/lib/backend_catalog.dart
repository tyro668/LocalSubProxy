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
    this.limits = const BackendLimits(),
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
      limits: BackendLimits.fromJson(
        (j['limits'] as Map?)?.cast<String, dynamic>() ?? const {},
      ),
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

  /// Which scheme token limits this backend type enforces.
  final BackendLimits limits;
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

/// The kernel's backend.Limits: a limit the backend cannot enforce is not
/// offered (and the kernel refuses to store it).
class BackendLimits {
  const BackendLimits({
    this.maxOutput = false,
    this.maxOutputVia = '',
    this.defaultMaxOutput = 0,
    this.contextWindow = false,
    this.contextVia = '',
    this.contextDetect = '',
  });

  factory BackendLimits.fromJson(Map<String, dynamic> j) => BackendLimits(
    maxOutput: j['max_output'] == true,
    maxOutputVia: j['max_output_via'] as String? ?? '',
    defaultMaxOutput: j['default_max_output'] as int? ?? 0,
    contextWindow: j['context_window'] == true,
    contextVia: j['context_via'] as String? ?? '',
    contextDetect: j['context_detect'] as String? ?? '',
  );

  final bool maxOutput;
  final String maxOutputVia;
  final int defaultMaxOutput;
  final bool contextWindow;
  final String contextVia;
  final String contextDetect;
  bool get any => maxOutput || contextWindow;

  /// Placeholder of an empty field: what applies when the user sets nothing.
  String get maxOutputPlaceholder =>
      defaultMaxOutput > 0 ? 'Avora 默认 $defaultMaxOutput' : '厂商默认';
  String get contextPlaceholder =>
      contextDetect.isNotEmpty ? '自动检测（$contextDetect）' : '未知，不预检';
}

/// Display text of one kernel limit state ({state, value, source, estimate}),
/// keeping unknown, vendor default, detected and user-set apart.
String limitStateLabel(Map<String, dynamic>? s) {
  if (s == null) return '未知';
  final v = s['value'] as int? ?? 0;
  final src = s['source'] as String? ?? '';
  final est = s['estimate'] == true ? ' · 按估算预检' : '';
  switch (s['state']) {
    case 'user':
      return '$v（已设置$est）';
    case 'detected':
      return '$v（自动检测，来源 $src$est）';
    case 'vendor_default':
      return '厂商默认（未设置）';
    case 'avora_default':
      return 'Avora 默认 $v（未设置）';
    case 'unsupported':
      return '此方案不支持';
    case 'unknown':
      return src.isEmpty ? '未知（未设置，不预检）' : '未设置 · 执行时从 $src 自动检测';
  }
  return '未知';
}
